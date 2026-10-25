package metrics

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"go.chief.tools/serverchief-agent/agent"
)

const (
	// liveInterval is how often live samples are read.
	liveInterval = time.Second
	// liveBatch is how many live samples go out together, every 2 seconds.
	liveBatch = 2
	// maximumLive caps how far ahead one metrics.live event keeps sampling;
	// the panel repeats the event while someone watches.
	maximumLive = 5 * time.Minute
	liveTimeout = 5 * time.Second
	// liveIdleBatches without a browser streaming the server (10 seconds)
	// end live sampling early.
	liveIdleBatches = 5
)

// liveSkipped are metrics live charts never need: capacities and file
// systems change over minutes, not seconds.
var liveSkipped = map[string]bool{"memory.total": true, "swap.total": true, "cpu.cores": true}

// Live samples every second until until, capped at five minutes from now.
// A later window extends the current one; an earlier one never shortens it.
func (runner *Runner) Live(until time.Time) {
	if latest := runner.Now().Add(maximumLive); until.After(latest) {
		until = latest
	}

	runner.mutex.Lock()
	if until.After(runner.liveUntil) {
		runner.liveUntil = until
	}
	runner.mutex.Unlock()

	signal(runner.liveWake)
}

func (runner *Runner) liveWindow() time.Time {
	runner.mutex.Lock()
	defer runner.mutex.Unlock()
	return runner.liveUntil
}

func (runner *Runner) stopLive() {
	runner.mutex.Lock()
	runner.liveUntil = time.Time{}
	runner.mutex.Unlock()
}

// liveLoop samples while a live window is open, next to the regular
// samples, which keep going unchanged.
func (runner *Runner) liveLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-runner.liveWake:
		}
		runner.live(ctx)
	}
}

func (runner *Runner) live(ctx context.Context) {
	config, paused := runner.current()
	if !config.Enabled || paused || config.Endpoint == "" || !runner.Now().Before(runner.liveWindow()) {
		return
	}
	log.Printf("[metrics] live samples until %s", runner.liveWindow().UTC().Format(time.TimeOnly))

	read := runner.ReadLive
	if read == nil {
		read = runner.Read
	}
	previous := read()
	var pending []Sample
	var last int64
	idle := 0

	tick := liveInterval
	if runner.liveTick > 0 {
		tick = runner.liveTick
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		config, paused = runner.current()
		if !config.Enabled || paused || !runner.Now().Before(runner.liveWindow()) {
			log.Printf("[metrics] live samples stopped")
			return
		}

		usage := read()
		points := livePoints(Derive(&previous, usage))
		previous = usage
		// Two readings within one second would repeat a timestamp.
		if at := usage.At.Unix(); len(points) > 0 && at > last {
			pending = append(pending, Sample{At: at, Points: points})
			last = at
		}
		if len(pending) < liveBatch {
			continue
		}

		samples := pending
		pending = nil
		watched, keep := runner.sendLive(ctx, config.Endpoint, samples)
		if watched {
			idle = 0
		} else {
			idle++
		}
		if !keep || idle >= liveIdleBatches {
			runner.stopLive()
			log.Printf("[metrics] live samples stopped until the next request")
			return
		}
	}
}

// sendLive posts live samples and reports whether a browser streams the
// server and whether to keep sampling. Live samples are never spooled or
// retried; a failed batch counts as watched, so a short outage does not end
// the window.
func (runner *Runner) sendLive(ctx context.Context, endpoint string, samples []Sample) (watched, keep bool) {
	ctx, cancel := context.WithTimeout(ctx, liveTimeout)
	defer cancel()

	response, err := runner.Client.SendLive(ctx, endpoint, samples)
	if err == nil {
		return response.Watchers > 0, true
	}

	var problem *agent.ProblemError
	if errors.As(err, &problem) && (problem.Status == http.StatusUnauthorized || problem.Status == http.StatusForbidden) {
		log.Printf("[metrics] live samples: %v", err)
		return false, false
	}
	return true, true
}

func livePoints(points []Point) []Point {
	kept := points[:0]
	for _, point := range points {
		if !liveSkipped[point.Metric] && !strings.HasPrefix(point.Metric, "fs.") {
			kept = append(kept, point)
		}
	}
	return kept
}
