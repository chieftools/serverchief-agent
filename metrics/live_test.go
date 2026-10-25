package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"go.chief.tools/serverchief-agent/agent"
	"go.chief.tools/serverchief-agent/collector"
)

// liveRunner reads a second later on every call, so live samples get one timestamp each.
func liveRunner(t *testing.T, fake *gateway, now time.Time) *Runner {
	t.Helper()

	runner, _, endpoint := newTestRunner(t, fake)
	var reads atomic.Int64
	runner.ReadLive = func() collector.Usage {
		at := now.Add(time.Duration(reads.Add(1)) * time.Second)
		return collector.Usage{
			At:      at,
			Cores:   2,
			Load:    [3]float64{0.5, 0.4, 0.3},
			Memory:  collector.MemoryUsage{Total: 1 << 30, Available: 1 << 29},
			Threads: 120,
			Filesystems: []collector.FilesystemUsage{
				{MountPoint: "/", SizeBytes: 100, FreeBytes: 50, AvailableBytes: 40, Files: 10, FilesFree: 5},
			},
		}
	}
	runner.Now = func() time.Time { return now }
	runner.liveTick = 5 * time.Millisecond
	runner.Configure(&agent.MetricsConfig{Enabled: true, Endpoint: endpoint, Interval: 60})

	return runner
}

func liveRequests(fake *gateway) []LiveRequest {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()

	var requests []LiveRequest
	for index, request := range fake.requests {
		if request.URL.Path != "/v1/live" {
			continue
		}
		var body LiveRequest
		_ = json.Unmarshal(fake.bodies[index], &body)
		requests = append(requests, body)
	}
	return requests
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLiveExtendsTheWindowUpToFiveMinutes(t *testing.T) {
	now := time.Unix(1793001600, 0)
	runner := liveRunner(t, &gateway{}, now)

	runner.Live(now.Add(90 * time.Second))
	runner.Live(now.Add(30 * time.Second))
	if got := runner.liveWindow(); !got.Equal(now.Add(90 * time.Second)) {
		t.Fatalf("an earlier until shortened the window to %s", got)
	}

	runner.Live(now.Add(time.Hour))
	if got := runner.liveWindow(); !got.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("window = %s, want five minutes from now", got)
	}
}

func TestLiveSendsPairsOfSecondSamplesWithoutFileSystemsOrCapacities(t *testing.T) {
	now := time.Unix(1793001600, 0)
	fake := &gateway{}
	runner := liveRunner(t, fake, now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.liveLoop(ctx)

	runner.Live(now.Add(time.Minute))
	waitFor(t, func() bool { return len(liveRequests(fake)) >= 2 })
	cancel()

	first := liveRequests(fake)[0]
	if first.ProtocolVersion != 1 || len(first.Samples) != 2 || first.Samples[1].At != first.Samples[0].At+1 {
		t.Fatalf("request = %+v", first)
	}
	for _, point := range first.Samples[0].Points {
		if point.Metric == "fs.used_pct" || point.Metric == "memory.total" || point.Metric == "cpu.cores" {
			t.Fatalf("live samples carry %s", point.Metric)
		}
	}
	if fake.requests[0].Header.Get("Authorization") != "Bearer "+fakeCredential {
		t.Fatal("live samples are sent without the agent credential")
	}
}

func TestLiveStopsWhenTheGatewayRefusesTheServer(t *testing.T) {
	for name, answer := range map[string]func(http.ResponseWriter){
		"disabled": problem(http.StatusForbidden, "metrics_disabled", ""),
		"unknown":  problem(http.StatusUnauthorized, "unauthenticated", ""),
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Unix(1793001600, 0)
			fake := &gateway{answers: []func(http.ResponseWriter){answer}}
			runner := liveRunner(t, fake, now)

			runner.Live(now.Add(time.Minute))
			runner.live(context.Background())

			if len(liveRequests(fake)) != 1 || !runner.liveWindow().IsZero() {
				t.Fatalf("requests = %d, window = %s", len(liveRequests(fake)), runner.liveWindow())
			}
		})
	}
}

func TestLiveDropsABatchTheGatewayCannotTakeAndKeepsSampling(t *testing.T) {
	now := time.Unix(1793001600, 0)
	fake := &gateway{answers: []func(http.ResponseWriter){problem(http.StatusTooManyRequests, "rate_limited", "1")}}
	runner := liveRunner(t, fake, now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.liveLoop(ctx)

	runner.Live(now.Add(time.Minute))
	waitFor(t, func() bool { return len(liveRequests(fake)) >= 2 })
	cancel()

	if liveRequests(fake)[1].Samples[0].At == liveRequests(fake)[0].Samples[0].At {
		t.Fatal("a dropped batch was retried")
	}
}

func TestLiveDoesNothingWhileMetricsAreOff(t *testing.T) {
	now := time.Unix(1793001600, 0)
	fake := &gateway{}
	runner := liveRunner(t, fake, now)
	runner.Configure(nil)

	runner.Live(now.Add(time.Minute))
	runner.live(context.Background())

	if len(liveRequests(fake)) != 0 {
		t.Fatal("sent live samples with metrics off")
	}
}

func watched(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write([]byte(`{"protocol_version":1,"forwarded":2,"watchers":1}`))
}

func TestLiveStopsAfterTenSecondsWithoutWatchers(t *testing.T) {
	now := time.Unix(1793001600, 0)
	fake := &gateway{answers: []func(http.ResponseWriter){watched, stored, stored, stored, stored, stored}}
	runner := liveRunner(t, fake, now)

	runner.Live(now.Add(time.Minute))
	runner.live(context.Background())

	// One watched batch, then five without a browser
	if requests := len(liveRequests(fake)); requests != 6 || !runner.liveWindow().IsZero() {
		t.Fatalf("requests = %d, window = %s", requests, runner.liveWindow())
	}
}

func TestLiveKeepsSamplingThroughAShortGatewayOutage(t *testing.T) {
	now := time.Unix(1793001600, 0)
	answers := []func(http.ResponseWriter){}
	for range 6 {
		answers = append(answers, problem(http.StatusServiceUnavailable, "temporarily_unavailable", ""))
	}
	fake := &gateway{answers: append(answers, stored, stored, stored, stored, stored)}
	runner := liveRunner(t, fake, now)

	runner.Live(now.Add(time.Minute))
	runner.live(context.Background())

	if requests := len(liveRequests(fake)); requests != 11 {
		t.Fatalf("requests = %d, want the outage to count as watched", requests)
	}
}
