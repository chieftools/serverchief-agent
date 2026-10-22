package agent

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"go.chief.tools/serverchief-agent/collector"
	"go.chief.tools/serverchief-agent/inventory"
)

type fakeCollector struct {
	name string
	mu   sync.Mutex
	data any
	err  error
	fail bool
}

func (fake *fakeCollector) Name() string { return fake.name }

func (fake *fakeCollector) Collect(context.Context) (any, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.fail {
		panic("boom")
	}
	return fake.data, fake.err
}

func (fake *fakeCollector) set(data any) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.data = data
}

type recordingClient struct {
	mu         sync.Mutex
	envelopes  []*inventory.Envelope
	heartbeats []Heartbeat
	sendErr    error
	resync     []string
	sent       chan struct{}
}

func (client *recordingClient) SendInventory(_ context.Context, envelope *inventory.Envelope) (SendResult, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.envelopes = append(client.envelopes, envelope)
	if client.sent != nil {
		select {
		case client.sent <- struct{}{}:
		default:
		}
	}
	if client.sendErr != nil {
		return SendResult{}, client.sendErr
	}
	result := SendResult{Resync: client.resync}
	client.resync = nil
	return result, nil
}

func (client *recordingClient) Heartbeat(_ context.Context, heartbeat Heartbeat) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.heartbeats = append(client.heartbeats, heartbeat)
	return nil
}

func (client *recordingClient) datasetNames(index int) []string {
	client.mu.Lock()
	defer client.mu.Unlock()
	var names []string
	for name := range client.envelopes[index].Datasets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func newTestDaemon(t *testing.T, client Client, collectors ...collector.Collector) (*daemon, *inventory.StateStore) {
	t.Helper()
	registry := collector.NewRegistry()
	for _, instance := range collectors {
		registry.MustRegister(instance)
	}
	store, err := inventory.OpenStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	options := RunOptions{
		Registry: registry, Store: store, Client: client, Config: DefaultConfig(),
		Now: time.Now, Jitter: func(time.Duration) time.Duration { return 0 }, CollectTimeout: time.Second,
	}
	return &daemon{options: options, next: make(map[string]time.Time), backoff: initialBackoff}, store
}

func TestCycleSendsOnlyChangedDatasets(t *testing.T) {
	system := &fakeCollector{name: "system", data: map[string]string{"hostname": "web-01"}}
	network := &fakeCollector{name: "network", data: []string{"eth0"}}
	client := &recordingClient{}
	daemon, store := newTestDaemon(t, client, system, network)
	ctx := context.Background()

	if _, err := daemon.cycle(ctx, []string{"network", "system"}); err != nil {
		t.Fatal(err)
	}
	if len(client.envelopes) != 1 || !reflect.DeepEqual(client.datasetNames(0), []string{"network", "system"}) {
		t.Fatalf("first cycle sent %d envelopes", len(client.envelopes))
	}
	first := client.envelopes[0]
	if first.Seq != 1 || first.Datasets["system"].Mode != inventory.ModeFull {
		t.Fatalf("first envelope = %#v", first)
	}

	if _, err := daemon.cycle(ctx, []string{"network", "system"}); err != nil {
		t.Fatal(err)
	}
	if len(client.envelopes) != 1 {
		t.Fatalf("unchanged datasets were resent")
	}

	network.set([]string{"eth0", "eth1"})
	if _, err := daemon.cycle(ctx, []string{"network", "system"}); err != nil {
		t.Fatal(err)
	}
	if len(client.envelopes) != 2 || !reflect.DeepEqual(client.datasetNames(1), []string{"network"}) || client.envelopes[1].Seq != 2 {
		t.Fatalf("second envelope = %#v", client.envelopes[1])
	}
	if store.Hash("network") != client.envelopes[1].Datasets["network"].Hash {
		t.Fatal("state does not record the delivered hash")
	}
}

func TestCycleKeepsStateOnDeliveryFailure(t *testing.T) {
	system := &fakeCollector{name: "system", data: "a"}
	client := &recordingClient{sendErr: errors.New("backend down")}
	daemon, store := newTestDaemon(t, client, system)

	if _, err := daemon.cycle(context.Background(), []string{"system"}); err == nil {
		t.Fatal("cycle succeeded despite delivery failure")
	}
	if store.Hash("system") != "" {
		t.Fatal("hash recorded for an undelivered dataset")
	}
	client.sendErr = nil
	if _, err := daemon.cycle(context.Background(), []string{"system"}); err != nil {
		t.Fatal(err)
	}
	if len(client.envelopes) != 2 || client.envelopes[1].Seq != 2 {
		t.Fatalf("retry was not sent with a new sequence number: %d envelopes", len(client.envelopes))
	}
}

func TestCycleReportsCollectorFailuresAlongsideChanges(t *testing.T) {
	broken := &fakeCollector{name: "network", err: errors.New("sysfs unavailable")}
	panicking := &fakeCollector{name: "hardware", fail: true}
	healthy := &fakeCollector{name: "system", data: "ok"}
	client := &recordingClient{}
	daemon, _ := newTestDaemon(t, client, broken, panicking, healthy)

	if _, err := daemon.cycle(context.Background(), []string{"hardware", "network", "system"}); err != nil {
		t.Fatal(err)
	}
	envelope := client.envelopes[0]
	if len(envelope.Datasets) != 1 || envelope.Errors["network"] != "sysfs unavailable" || envelope.Errors["hardware"] == "" {
		t.Fatalf("envelope = %#v", envelope)
	}

	// Only failures and no changes: nothing is sent.
	if _, err := daemon.cycle(context.Background(), []string{"hardware", "network", "system"}); err != nil {
		t.Fatal(err)
	}
	if len(client.envelopes) != 1 {
		t.Fatal("an error-only envelope was sent")
	}
}

func TestCycleResync(t *testing.T) {
	system := &fakeCollector{name: "system", data: "a"}
	client := &recordingClient{resync: []string{"system"}}
	daemon, store := newTestDaemon(t, client, system)

	resync, err := daemon.cycle(context.Background(), []string{"system"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resync, []string{"system"}) || store.Hash("system") != "" {
		t.Fatalf("resync = %v, hash = %q", resync, store.Hash("system"))
	}
	if _, err := daemon.cycle(context.Background(), []string{"system"}); err != nil {
		t.Fatal(err)
	}
	if len(client.envelopes) != 2 || store.Hash("system") == "" {
		t.Fatal("resynced dataset was not resent")
	}
}

func TestRunDeliversReloadsAndStops(t *testing.T) {
	system := &fakeCollector{name: "system", data: "a"}
	hardware := &fakeCollector{name: "hardware", data: "b"}
	client := &recordingClient{sent: make(chan struct{}, 10)}
	registry := collector.NewRegistry()
	registry.MustRegister(system)
	registry.MustRegister(hardware)
	store, err := inventory.OpenStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	reload := make(chan Config)
	readyCalls := 0
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{
			Registry: registry, Store: store, Client: client, Config: DefaultConfig(), Version: "1.2.3",
			Reload: reload, Ready: func() { readyCalls++ },
		})
	}()

	waitFor := func(what string) {
		t.Helper()
		select {
		case <-client.sent:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	waitFor("the initial envelope")

	// Reload restricts collection to one dataset, whose payload changed.
	system.set("changed")
	reload <- Config{Datasets: []string{"system"}}
	waitFor("the post-reload envelope")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil on shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}

	if readyCalls != 1 {
		t.Fatalf("Ready called %d times", readyCalls)
	}
	if got := client.datasetNames(0); !reflect.DeepEqual(got, []string{"hardware", "system"}) {
		t.Fatalf("initial datasets = %v", got)
	}
	if got := client.datasetNames(1); !reflect.DeepEqual(got, []string{"system"}) {
		t.Fatalf("post-reload datasets = %v", got)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.heartbeats) == 0 || client.heartbeats[0].AgentVersion != "1.2.3" {
		t.Fatalf("heartbeats = %#v", client.heartbeats)
	}
}

func TestRunRequiresDatasets(t *testing.T) {
	store, _ := inventory.OpenStateStore(filepath.Join(t.TempDir(), "state.json"))
	err := Run(context.Background(), RunOptions{Registry: collector.NewRegistry(), Store: store, Client: LogClient{}})
	if err == nil {
		t.Fatal("Run() with no datasets succeeded")
	}
}

func TestNormalJitterBounds(t *testing.T) {
	interval := 5 * time.Minute
	if got := normalJitter(interval, 0.1, func(time.Duration) time.Duration { return 0 }); got != 270*time.Second {
		t.Fatalf("lower bound = %s", got)
	}
	if got := normalJitter(interval, 0.1, func(maximum time.Duration) time.Duration { return maximum }); got != 330*time.Second {
		t.Fatalf("upper bound = %s", got)
	}
	for range 100 {
		if value := fullJitter(time.Second); value < 0 || value > time.Second {
			t.Fatalf("fullJitter out of range: %s", value)
		}
	}
}
