package metrics

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"go.chief.tools/serverchief-agent/agent"
	"go.chief.tools/serverchief-agent/collector"
)

const (
	DefaultInterval = time.Minute
	minimumInterval = 10 * time.Second
	maximumInterval = time.Hour

	initialBackoff = 2 * time.Second
	maximumBackoff = 5 * time.Minute
	// unknownRetry waits for the panel to publish a newly enrolled server.
	unknownRetry = 5 * time.Minute
	// maximumSendOffset spreads a fleet's deliveries over the start of each
	// interval instead of every agent sending on the minute.
	maximumSendOffset = 30 * time.Second
)

// Runner samples usage on the configured interval and delivers the spool.
// It runs next to the daemon's loop, which can be busy collecting a dataset
// for minutes, and only takes configuration from it.
type Runner struct {
	Read func() collector.Usage
	// ReadLive reads usage for live samples, without file systems; nil
	// uses Read.
	ReadLive func() collector.Usage
	Spool    *Spool
	Client   *Client
	Now      func() time.Time

	mutex      sync.Mutex
	config     agent.MetricsConfig
	paused     bool
	lastSentAt time.Time
	lastError  string
	changed    chan struct{}
	wake       chan struct{}
	offset     time.Duration
	liveUntil  time.Time
	liveWake   chan struct{}
	liveTick   time.Duration
}

// NewRunner returns a runner that is disabled until Configure enables it.
func NewRunner(read func() collector.Usage, spool *Spool, client *Client) *Runner {
	return &Runner{
		Read:     read,
		Spool:    spool,
		Client:   client,
		Now:      time.Now,
		changed:  make(chan struct{}, 1),
		wake:     make(chan struct{}, 1),
		liveWake: make(chan struct{}, 1),
		offset:   rand.N(maximumSendOffset),
	}
}

// Configure applies the backend's metrics setting; nil (an older backend)
// disables metrics. Turning metrics off drops the spool.
func (runner *Runner) Configure(config *agent.MetricsConfig) {
	next := agent.MetricsConfig{}
	if config != nil {
		next = *config
	}
	runner.mutex.Lock()
	previous := runner.config
	runner.config = next
	runner.paused = false
	runner.mutex.Unlock()

	if previous.Enabled && !next.Enabled {
		if err := runner.Spool.Clear(); err != nil {
			log.Printf("[metrics] clear the spool: %v", err)
		}
	}
	if previous != next {
		if next.Enabled {
			log.Printf("[metrics] sending every %s to %s", runner.interval(next), next.Endpoint)
		} else if previous.Enabled {
			log.Printf("[metrics] turned off")
		}
	}
	signal(runner.changed)
	signal(runner.wake)
}

// Status is the heartbeat's metrics capability.
func (runner *Runner) Status() agent.MetricsCapability {
	runner.mutex.Lock()
	defer runner.mutex.Unlock()

	status := agent.MetricsCapability{Enabled: runner.config.Enabled, SpoolBytes: runner.Spool.Bytes(), Live: true}
	if !runner.lastSentAt.IsZero() {
		at := runner.lastSentAt.UTC().Truncate(time.Second)
		status.LastSentAt = &at
	}
	if runner.lastError != "" {
		message := runner.lastError
		status.Error = &message
	}
	return status
}

// Run samples, delivers and answers live requests until ctx ends.
func (runner *Runner) Run(ctx context.Context) {
	var group sync.WaitGroup
	group.Add(3)
	go func() {
		defer group.Done()
		runner.liveLoop(ctx)
	}()
	go func() {
		defer group.Done()
		runner.sample(ctx)
	}()
	go func() {
		defer group.Done()
		runner.deliverLoop(ctx)
	}()
	group.Wait()
}

func (runner *Runner) current() (agent.MetricsConfig, bool) {
	runner.mutex.Lock()
	defer runner.mutex.Unlock()
	return runner.config, runner.paused
}

func (runner *Runner) interval(config agent.MetricsConfig) time.Duration {
	interval := time.Duration(config.Interval) * time.Second
	if interval == 0 {
		return DefaultInterval
	}
	return min(max(interval, minimumInterval), maximumInterval)
}

// sample reads usage at every interval boundary, so samples of a fleet line
// up (at % interval == 0) and rates cover the interval before.
func (runner *Runner) sample(ctx context.Context) {
	var previous *collector.Usage
	for {
		config, _ := runner.current()
		if !config.Enabled {
			previous = nil
			select {
			case <-ctx.Done():
				return
			case <-runner.changed:
				continue
			}
		}
		if previous == nil {
			// A first reading now gives the first sample its rates.
			usage := runner.Read()
			previous = &usage
		}

		interval := runner.interval(config)
		boundary := nextBoundary(runner.Now(), interval)
		timer := time.NewTimer(boundary.Sub(runner.Now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-runner.changed:
			timer.Stop()
			continue
		case <-timer.C:
		}

		usage := runner.Read()
		points := Derive(previous, usage)
		previous = &usage
		if len(points) == 0 {
			continue
		}
		if err := runner.Spool.Add(Sample{At: boundary.Unix(), Points: points}); err != nil {
			log.Printf("[metrics] spool a sample: %v", err)
			continue
		}
		time.AfterFunc(min(runner.offset, interval/2), func() { signal(runner.wake) })
	}
}

// nextBoundary is the first multiple of interval after now, in Unix time.
func nextBoundary(now time.Time, interval time.Duration) time.Time {
	seconds := int64(interval / time.Second)
	return time.Unix((now.Unix()/seconds+1)*seconds, 0)
}

func (runner *Runner) deliverLoop(ctx context.Context) {
	backoff := initialBackoff
	for {
		wait := runner.deliver(ctx, &backoff)
		var retry <-chan time.Time
		var timer *time.Timer
		if wait > 0 {
			timer = time.NewTimer(wait)
			retry = timer.C
		}
		select {
		case <-ctx.Done():
		case <-runner.wake:
		case <-retry:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// deliver sends batches until the spool is empty (0) or a failure says how
// long to wait before the next attempt.
func (runner *Runner) deliver(ctx context.Context, backoff *time.Duration) time.Duration {
	for ctx.Err() == nil {
		config, paused := runner.current()
		if !config.Enabled || config.Endpoint == "" || paused {
			return 0
		}
		prepared, err := runner.Spool.Next()
		if err != nil {
			return runner.retry("read the spool", err, backoff)
		}
		if prepared == nil {
			return 0
		}

		response, err := runner.Client.Send(ctx, config.Endpoint, prepared)
		if err == nil {
			if err := runner.Spool.Done(); err != nil {
				return runner.retry("update the spool", err, backoff)
			}
			*backoff = initialBackoff
			runner.mutex.Lock()
			runner.lastSentAt = runner.Now()
			runner.lastError = ""
			runner.mutex.Unlock()
			if response.Dropped > 0 {
				log.Printf("[metrics] the gateway dropped %d samples older than the server's retention", response.Dropped)
			}
			continue
		}
		if ctx.Err() != nil {
			return 0
		}

		var problem *agent.ProblemError
		if !errors.As(err, &problem) {
			return runner.retry("send samples", err, backoff)
		}
		switch {
		case problem.Status == http.StatusForbidden:
			// Turned off for this server; the next config says so too.
			runner.fail(err, true)
			if err := runner.Spool.Clear(); err != nil {
				log.Printf("[metrics] clear the spool: %v", err)
			}
			return 0
		case problem.Status == http.StatusUpgradeRequired:
			runner.fail(err, true)
			return 0
		case problem.Status == http.StatusUnauthorized:
			runner.fail(err, false)
			return unknownRetry
		case problem.Status == http.StatusTooManyRequests || problem.Status == http.StatusServiceUnavailable:
			if problem.RetryAfter > 0 {
				runner.fail(err, false)
				return problem.RetryAfter
			}
			return runner.retry("send samples", err, backoff)
		case problem.Status >= 400 && problem.Status < 500:
			// This batch can never be stored; the rest of the spool can.
			runner.fail(err, false)
			if err := runner.Spool.Done(); err != nil {
				return runner.retry("update the spool", err, backoff)
			}
		default:
			return runner.retry("send samples", err, backoff)
		}
	}
	return 0
}

func (runner *Runner) fail(err error, pause bool) {
	runner.mutex.Lock()
	changed := runner.lastError != err.Error()
	runner.lastError = err.Error()
	if pause {
		runner.paused = true
	}
	runner.mutex.Unlock()
	if changed {
		log.Printf("[metrics] %v", err)
	}
}

func (runner *Runner) retry(operation string, err error, backoff *time.Duration) time.Duration {
	wait := *backoff/2 + rand.N(*backoff/2+1)
	*backoff = min(*backoff*2, maximumBackoff)
	runner.mutex.Lock()
	runner.lastError = err.Error()
	runner.mutex.Unlock()
	log.Printf("[metrics] %s failed: %v; retrying in %s", operation, err, wait.Round(time.Second))
	return wait
}

func signal(channel chan struct{}) {
	select {
	case channel <- struct{}{}:
	default:
	}
}
