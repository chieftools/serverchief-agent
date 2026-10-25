package metrics

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.chief.tools/serverchief-agent/agent"
	"go.chief.tools/serverchief-agent/collector"
)

const fakeCredential = "scagt_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type gateway struct {
	mutex    sync.Mutex
	answers  []func(http.ResponseWriter)
	requests []*http.Request
	bodies   [][]byte
}

func (fake *gateway) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	body, _ := io.ReadAll(request.Body)
	fake.requests = append(fake.requests, request)
	fake.bodies = append(fake.bodies, body)
	answer := stored
	if len(fake.answers) > 0 {
		answer, fake.answers = fake.answers[0], fake.answers[1:]
	}
	answer(writer)
}

func stored(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write([]byte(`{"protocol_version":1,"batch_id":"x","accepted":1,"dropped":0,"points":2}`))
}

func problem(status int, kind string, retryAfter string) func(http.ResponseWriter) {
	return func(writer http.ResponseWriter) {
		writer.Header().Set("Content-Type", "application/problem+json")
		if retryAfter != "" {
			writer.Header().Set("Retry-After", retryAfter)
		}
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(`{"type":"` + kind + `","title":"t","status":` + http.StatusText(status)[:0] + itoa(status) + `,"detail":"d"}`))
	}
}

func itoa(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func newTestRunner(t *testing.T, fake *gateway) (*Runner, *Spool, string) {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	now := time.Unix(1793001600, 0)
	spool := newTestSpool(t, now)
	runner := NewRunner(func() collector.Usage { return collector.Usage{} }, spool, &Client{
		Credential: fakeCredential,
		UserAgent:  agent.UserAgent("0.3.0", "01jb8z5k2m3n4p5q6r7s8t9v0w"),
		HTTP:       server.Client(),
	})
	runner.Now = func() time.Time { return now }
	runner.Configure(&agent.MetricsConfig{Enabled: true, Endpoint: server.URL, Interval: 60})
	return runner, spool, server.URL
}

func TestDeliverDrainsTheSpoolInBatches(t *testing.T) {
	fake := &gateway{}
	runner, spool, _ := newTestRunner(t, fake)
	for minute := range 61 {
		// About as large as a real sample, so a full batch is compressed.
		sample := sampleAt(1793001600 - int64(61-minute)*60)
		for index := range 40 {
			sample.Points = append(sample.Points, Point{Metric: "net.rx_bps", Instance: "eth" + itoa(index), Value: 1234.5})
		}
		_ = spool.Add(sample)
	}

	backoff := initialBackoff
	if wait := runner.deliver(context.Background(), &backoff); wait != 0 {
		t.Fatalf("wait = %s, want an idle spool", wait)
	}
	if len(fake.requests) != 2 || spool.Bytes() != 0 {
		t.Fatalf("%d requests, %d bytes left", len(fake.requests), spool.Bytes())
	}

	request := fake.requests[0]
	if request.URL.Path != "/v1/samples" || request.Header.Get("Authorization") != "Bearer "+fakeCredential ||
		request.Header.Get("Content-Encoding") != "gzip" || !strings.HasSuffix(request.Header.Get("User-Agent"), " server/01jb8z5k2m3n4p5q6r7s8t9v0w") {
		t.Fatalf("request %s %v", request.URL.Path, request.Header)
	}
	status := runner.Status()
	if !status.Enabled || status.LastSentAt == nil || status.Error != nil || status.SpoolBytes != 0 {
		t.Fatalf("status = %+v", status)
	}
}

func TestDeliverRetriesTheSameBatchAfterAmbiguousFailures(t *testing.T) {
	fake := &gateway{answers: []func(http.ResponseWriter){
		problem(http.StatusBadGateway, "", ""),
		problem(http.StatusServiceUnavailable, "temporarily_unavailable", "7"),
	}}
	runner, spool, _ := newTestRunner(t, fake)
	_ = spool.Add(sampleAt(1793001540))

	backoff := initialBackoff
	if wait := runner.deliver(context.Background(), &backoff); wait < time.Second || wait > initialBackoff || backoff != 2*initialBackoff {
		t.Fatalf("after a 502: wait %s, backoff %s", wait, backoff)
	}
	if status := runner.Status(); status.Error == nil || status.SpoolBytes == 0 {
		t.Fatalf("status after a failure = %+v", status)
	}
	if wait := runner.deliver(context.Background(), &backoff); wait != 7*time.Second {
		t.Fatalf("after a 503 with Retry-After: wait %s", wait)
	}
	if wait := runner.deliver(context.Background(), &backoff); wait != 0 || spool.Bytes() != 0 {
		t.Fatalf("after recovery: wait %s, %d bytes", wait, spool.Bytes())
	}
	if len(fake.bodies) != 3 || string(fake.bodies[0]) != string(fake.bodies[2]) {
		t.Fatal("a retry must resend the same batch")
	}
}

func TestDeliverDropsRejectedBatchesAndKeepsTheRest(t *testing.T) {
	fake := &gateway{answers: []func(http.ResponseWriter){problem(http.StatusUnprocessableEntity, "validation_failed", "")}}
	runner, spool, _ := newTestRunner(t, fake)
	for minute := range 61 {
		_ = spool.Add(sampleAt(1793001600 - int64(61-minute)*60))
	}

	backoff := initialBackoff
	if wait := runner.deliver(context.Background(), &backoff); wait != 0 || len(fake.requests) != 2 || spool.Bytes() != 0 {
		t.Fatalf("wait %s, %d requests, %d bytes", wait, len(fake.requests), spool.Bytes())
	}
}

func TestDeliverWaitsForAnUnknownCredential(t *testing.T) {
	fake := &gateway{answers: []func(http.ResponseWriter){problem(http.StatusUnauthorized, "unauthenticated", "")}}
	runner, spool, _ := newTestRunner(t, fake)
	_ = spool.Add(sampleAt(1793001540))

	backoff := initialBackoff
	if wait := runner.deliver(context.Background(), &backoff); wait != unknownRetry || spool.Bytes() == 0 {
		t.Fatalf("wait %s, %d bytes; the spool must be kept", wait, spool.Bytes())
	}
}

func TestDeliverStopsWhenMetricsAreDisabledOrTheProtocolIsUnsupported(t *testing.T) {
	for _, test := range []struct {
		status    int
		kind      string
		keepSpool bool
	}{
		{http.StatusForbidden, "metrics_disabled", false},
		{http.StatusUpgradeRequired, "unsupported_protocol", true},
	} {
		t.Run(test.kind, func(t *testing.T) {
			fake := &gateway{answers: []func(http.ResponseWriter){problem(test.status, test.kind, "")}}
			runner, spool, endpoint := newTestRunner(t, fake)
			_ = spool.Add(sampleAt(1793001540))

			backoff := initialBackoff
			runner.deliver(context.Background(), &backoff)
			runner.deliver(context.Background(), &backoff)
			if len(fake.requests) != 1 || (spool.Bytes() > 0) != test.keepSpool {
				t.Fatalf("%d requests, %d bytes", len(fake.requests), spool.Bytes())
			}

			// A new configuration resumes delivery.
			runner.Configure(&agent.MetricsConfig{Enabled: true, Endpoint: endpoint, Interval: 60})
			_ = spool.Add(sampleAt(1793001600))
			runner.deliver(context.Background(), &backoff)
			want := 2
			if test.keepSpool {
				want = 3 // the kept batch, then the new sample
			}
			if len(fake.requests) != want {
				t.Fatalf("%d requests after a new configuration, want %d", len(fake.requests), want)
			}
		})
	}
}

func TestConfigureOffClearsTheSpool(t *testing.T) {
	fake := &gateway{}
	runner, spool, _ := newTestRunner(t, fake)
	_ = spool.Add(sampleAt(1793001540))

	runner.Configure(nil)
	backoff := initialBackoff
	runner.deliver(context.Background(), &backoff)
	if spool.Bytes() != 0 || len(fake.requests) != 0 || runner.Status().Enabled {
		t.Fatalf("%d bytes, %d requests", spool.Bytes(), len(fake.requests))
	}
}

func TestIntervalAndBoundaries(t *testing.T) {
	runner := &Runner{}
	for seconds, want := range map[int]time.Duration{0: time.Minute, 1: 10 * time.Second, 60: time.Minute, 300: 5 * time.Minute, 86400: time.Hour} {
		if got := runner.interval(agent.MetricsConfig{Interval: seconds}); got != want {
			t.Errorf("interval(%d) = %s, want %s", seconds, got, want)
		}
	}
	if got := nextBoundary(time.Unix(1793001601, 0), time.Minute); got.Unix() != 1793001660 {
		t.Errorf("next boundary = %d", got.Unix())
	}
	if got := nextBoundary(time.Unix(1793001600, 0), time.Minute); got.Unix() != 1793001660 {
		t.Errorf("next boundary from a boundary = %d", got.Unix())
	}
}
