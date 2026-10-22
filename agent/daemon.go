package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"math/big"
	"runtime/debug"
	"slices"
	"time"

	"go.chief.tools/serverchief-agent/collector"
	"go.chief.tools/serverchief-agent/inventory"
)

const (
	initialBackoff        = 2 * time.Second
	maximumBackoff        = 5 * time.Minute
	defaultCollectTimeout = time.Minute
)

// RunOptions configures Run.
type RunOptions struct {
	Registry *collector.Registry
	Store    *inventory.StateStore
	Client   Client
	Config   Config
	Version  string

	// Reload delivers a new configuration (SIGHUP). Every dataset is
	// collected again right after a reload.
	Reload <-chan Config
	// Wake requests an immediate collection of every dataset, for the
	// refresh_inventory command and realtime wake-ups.
	Wake <-chan struct{}
	// Ready is called once, after the first collection round.
	Ready func()

	CollectTimeout time.Duration
	Now            func() time.Time
	// Jitter returns a random duration in [0, maximum].
	Jitter func(maximum time.Duration) time.Duration
}

type daemon struct {
	options RunOptions
	next    map[string]time.Time
	backoff time.Duration
}

// Run collects datasets on their schedules and delivers changes until ctx
// is cancelled. Cancellation is a clean shutdown and returns nil.
func Run(ctx context.Context, options RunOptions) error {
	if options.Registry == nil || options.Store == nil || options.Client == nil {
		return errors.New("run: registry, store and client are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Jitter == nil {
		options.Jitter = fullJitter
	}
	if options.CollectTimeout <= 0 {
		options.CollectTimeout = defaultCollectTimeout
	}

	daemon := &daemon{options: options, next: make(map[string]time.Time), backoff: initialBackoff}
	daemon.scheduleAll(options.Now())
	if len(daemon.next) == 0 {
		return errors.New("run: no datasets enabled")
	}
	heartbeatAt := options.Now()
	ready := false

	for {
		now := options.Now()
		if due := daemon.due(now); len(due) > 0 {
			resync, err := daemon.cycle(ctx, due)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				wait := options.Jitter(daemon.backoff)
				log.Printf("[inventory] delivery failed: %v; retrying in %s", err, wait.Round(time.Second))
				for _, name := range due {
					daemon.next[name] = now.Add(wait)
				}
				daemon.backoff = min(daemon.backoff*2, maximumBackoff)
			} else {
				daemon.backoff = initialBackoff
				for _, name := range due {
					daemon.next[name] = now.Add(normalJitter(options.Config.Interval(name), JitterFraction, options.Jitter))
				}
				// Resynced datasets go out in full on the next pass.
				for _, name := range resync {
					if _, scheduled := daemon.next[name]; scheduled {
						daemon.next[name] = now
					}
				}
			}
			if !ready {
				ready = true
				if options.Ready != nil {
					options.Ready()
				}
			}
		}

		if !now.Before(heartbeatAt) {
			heartbeat := Heartbeat{AgentVersion: options.Version, Hashes: options.Store.Hashes(), SentAt: now.UTC()}
			if err := options.Client.Heartbeat(ctx, heartbeat); err != nil && ctx.Err() == nil {
				log.Printf("[heartbeat] failed: %v", err)
			}
			heartbeatAt = now.Add(normalJitter(options.Config.Heartbeat(), JitterFraction, options.Jitter))
		}

		wakeAt := heartbeatAt
		for _, at := range daemon.next {
			if at.Before(wakeAt) {
				wakeAt = at
			}
		}
		timer := time.NewTimer(max(wakeAt.Sub(options.Now()), 0))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		case config := <-options.Reload:
			timer.Stop()
			daemon.options.Config = config
			options.Config = config
			daemon.scheduleAll(options.Now())
			log.Printf("[config] reloaded; collecting %d datasets now", len(daemon.next))
		case <-options.Wake:
			timer.Stop()
			daemon.scheduleAll(options.Now())
		}
	}
}

// scheduleAll makes every enabled dataset due at now and drops disabled ones.
func (daemon *daemon) scheduleAll(now time.Time) {
	clear(daemon.next)
	for _, name := range daemon.options.Registry.Names() {
		if daemon.options.Config.Enabled(name) {
			daemon.next[name] = now
		}
	}
}

func (daemon *daemon) due(now time.Time) []string {
	var due []string
	for name, at := range daemon.next {
		if !at.After(now) {
			due = append(due, name)
		}
	}
	slices.Sort(due)
	return due
}

// cycle collects names and sends every dataset whose hash differs from the
// last delivered one. Collector failures are logged and do not fail the
// cycle; a failed delivery does, leaving the state untouched for a retry.
func (daemon *daemon) cycle(ctx context.Context, names []string) (resync []string, err error) {
	options := daemon.options
	envelope := inventory.NewEnvelope(0)
	hashes := make(map[string]string)

	for _, name := range names {
		instance, ok := options.Registry.Get(name)
		if !ok {
			continue
		}
		dataset, err := collectDataset(ctx, instance, options.CollectTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			log.Printf("[collector] %s failed: %v", name, err)
			envelope.AddError(name, err)
			continue
		}
		if options.Store.Hash(name) == dataset.Hash {
			continue
		}
		envelope.AddFull(dataset)
		hashes[name] = dataset.Hash
	}

	// Errors alone are not worth a request; they ride along with the next
	// change and are always in the local log.
	if len(envelope.Datasets) == 0 {
		return nil, nil
	}

	seq, err := options.Store.NextSeq()
	if err != nil {
		return nil, fmt.Errorf("reserve sequence number: %w", err)
	}
	envelope.Seq = seq
	result, err := options.Client.SendInventory(ctx, envelope)
	if err != nil {
		return nil, err
	}

	for _, name := range result.Resync {
		delete(hashes, name)
	}
	if err := options.Store.MarkSent(hashes, options.Now()); err != nil {
		return nil, fmt.Errorf("record delivered hashes: %w", err)
	}
	if len(result.Resync) > 0 {
		if err := options.Store.Forget(result.Resync...); err != nil {
			return nil, fmt.Errorf("forget resynced datasets: %w", err)
		}
		log.Printf("[inventory] backend requested a resync of %v", result.Resync)
	}
	return result.Resync, nil
}

// CollectDatasets runs collectors once and returns their hashed datasets
// plus per-dataset errors. It backs `serverchief collect`.
func CollectDatasets(ctx context.Context, collectors []collector.Collector, timeout time.Duration) ([]inventory.Dataset, map[string]error) {
	if timeout <= 0 {
		timeout = defaultCollectTimeout
	}
	datasets := make([]inventory.Dataset, 0, len(collectors))
	failures := make(map[string]error)
	for _, instance := range collectors {
		dataset, err := collectDataset(ctx, instance, timeout)
		if err != nil {
			failures[instance.Name()] = err
			continue
		}
		datasets = append(datasets, dataset)
	}
	return datasets, failures
}

// collectDataset runs one collector with a timeout and turns a panic into
// an error, so one broken collector cannot take the daemon down.
func collectDataset(ctx context.Context, instance collector.Collector, timeout time.Duration) (dataset inventory.Dataset, err error) {
	collectContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("collector panicked: %v\n%s", recovered, debug.Stack())
		}
	}()
	payload, err := instance.Collect(collectContext)
	if err != nil {
		return inventory.Dataset{}, err
	}
	return inventory.NewDataset(instance.Name(), payload)
}

// normalJitter spreads interval uniformly over interval ± fraction.
func normalJitter(interval time.Duration, fraction float64, jitter func(time.Duration) time.Duration) time.Duration {
	window := time.Duration(float64(interval) * fraction)
	if window == 0 {
		return interval
	}
	return interval - window + jitter(2*window)
}

func fullJitter(maximum time.Duration) time.Duration {
	if maximum <= 0 {
		return 0
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(maximum)+1))
	if err != nil {
		return maximum
	}
	return time.Duration(value.Int64())
}
