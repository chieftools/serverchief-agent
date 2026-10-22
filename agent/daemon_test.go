package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
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

func (fake *fakeCollector) set(data any, err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.data, fake.err = data, err
}

// keyedCollector is a delta dataset keyed by manager/name/arch.
type keyedCollector struct{ fakeCollector }

func (*keyedCollector) DeltaKeyFields() []string { return []string{"manager", "name", "arch"} }

// changingCollector exposes a change token.
type changingCollector struct {
	fakeCollector
	token string
}

func (collector *changingCollector) ChangeToken() string {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return collector.token
}

// protocolServer is an in-memory backend following the spec: it stores
// hashes and payloads, applies deltas, fences replays by seq and answers
// heartbeats with resyncs.
type protocolServer struct {
	t      *testing.T
	server *httptest.Server

	mu         sync.Mutex
	hashes     map[string]string
	payloads   map[string]json.RawMessage
	seq        uint64
	envelopes  []inventory.Envelope
	heartbeats []HeartbeatRequest
	configs    []string // If-None-Match values
	config     ServerConfig

	// One-shot behaviour.
	heartbeatResync []string
	inventoryResync []string
	failures        map[string][]failure // path → queued failures
	wrongHash       map[string]bool      // accept with a different hash
	seqAhead        uint64               // pretend the server stored this seq
	ignoreUnknown   map[string]bool      // datasets answered as ignored

	events chan string
}

type failure struct {
	status int
	body   string
	header map[string]string
}

func newProtocolServer(t *testing.T) *protocolServer {
	t.Helper()
	server := &protocolServer{
		t:         t,
		hashes:    map[string]string{},
		payloads:  map[string]json.RawMessage{},
		failures:  map[string][]failure{},
		wrongHash: map[string]bool{},
		config:    DefaultServerConfig(),
		events:    make(chan string, 100),
	}
	server.config.ETag = `"c1"`
	server.server = httptest.NewServer(http.HandlerFunc(server.handle))
	t.Cleanup(server.server.Close)
	return server
}

func (server *protocolServer) client() *HTTPClient {
	return NewClient(server.server.URL+"/agent/v1", "scagt_test", "1.2.3", server.server.Client())
}

func (server *protocolServer) fail(path string, failures ...failure) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.failures[path] = append(server.failures[path], failures...)
}

func (server *protocolServer) handle(writer http.ResponseWriter, request *http.Request) {
	path := request.URL.Path[len("/agent/v1"):]
	if request.Header.Get("Authorization") != "Bearer scagt_test" || request.Header.Get(ProtocolHeader) != ProtocolRevision {
		writeJSONResponse(writer, 401, `{"type":"unauthenticated","title":"Unauthenticated","status":401}`)
		return
	}
	body, _ := io.ReadAll(request.Body)
	if request.Header.Get("Content-Encoding") == "gzip" {
		reader, _ := gzip.NewReader(bytes.NewReader(body))
		body, _ = io.ReadAll(reader)
	}

	server.mu.Lock()
	defer func() {
		server.mu.Unlock()
		select {
		case server.events <- path:
		default:
		}
	}()
	if queued := server.failures[path]; len(queued) > 0 {
		next := queued[0]
		server.failures[path] = queued[1:]
		for name, value := range next.header {
			writer.Header().Set(name, value)
		}
		writeJSONResponse(writer, next.status, next.body)
		return
	}

	switch path {
	case "/heartbeat":
		var heartbeat HeartbeatRequest
		_ = json.Unmarshal(body, &heartbeat)
		server.heartbeats = append(server.heartbeats, heartbeat)
		resync := []string{}
		for name, hash := range heartbeat.Hashes {
			if server.hashes[name] != hash {
				resync = append(resync, name)
			}
		}
		resync = append(resync, server.heartbeatResync...)
		server.heartbeatResync = nil
		slices.Sort(resync)
		encoded, _ := json.Marshal(HeartbeatResponse{Resync: slices.Compact(resync), ConfigETag: server.config.ETag})
		writeJSONResponse(writer, 200, string(encoded))
	case "/config":
		server.configs = append(server.configs, request.Header.Get("If-None-Match"))
		if request.Header.Get("If-None-Match") == server.config.ETag {
			writer.WriteHeader(http.StatusNotModified)
			return
		}
		encoded, _ := json.Marshal(server.config)
		writeJSONResponse(writer, 200, string(encoded))
	case "/inventory":
		server.inventory(writer, body)
	case "/broadcasting/auth":
		writeJSONResponse(writer, 200, `{"auth":"key:signature"}`)
	default:
		writeJSONResponse(writer, 404, `{"type":"not_found","status":404,"title":"Not found"}`)
	}
}

func (server *protocolServer) inventory(writer http.ResponseWriter, body []byte) {
	var envelope inventory.Envelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		writeJSONResponse(writer, 400, `{"type":"invalid_request","status":400,"title":"bad"}`)
		return
	}
	server.envelopes = append(server.envelopes, envelope)
	response := InventoryResponse{Accepted: map[string]string{}, Resync: []string{}, Ignored: []string{}}
	stored := max(server.seq, server.seqAhead)
	if envelope.Seq <= stored {
		for name := range envelope.Datasets {
			if hash, ok := server.hashes[name]; ok {
				response.Accepted[name] = hash
			}
		}
		response.Seq = stored
		encoded, _ := json.Marshal(response)
		writeJSONResponse(writer, 200, string(encoded))
		return
	}
	server.seq = envelope.Seq
	for name, dataset := range envelope.Datasets {
		if server.ignoreUnknown[name] {
			response.Ignored = append(response.Ignored, name)
			continue
		}
		switch dataset.Mode {
		case inventory.ModeFull:
			server.payloads[name] = dataset.Payload
			server.hashes[name] = dataset.Hash
		case inventory.ModeDelta:
			if dataset.BaseHash != server.hashes[name] {
				response.Resync = append(response.Resync, name)
				continue
			}
			applied, err := applyKeyedDelta(server.payloads[name], dataset.Payload)
			if err != nil {
				server.t.Errorf("apply %s delta: %v", name, err)
				continue
			}
			server.payloads[name] = applied
			server.hashes[name] = inventory.Hash(applied)
			if server.hashes[name] != dataset.Hash {
				response.Resync = append(response.Resync, name)
			}
		}
		if server.wrongHash[name] {
			server.hashes[name] = "server-side-" + dataset.Hash
			delete(server.wrongHash, name)
		}
		response.Accepted[name] = server.hashes[name]
	}
	response.Resync = append(response.Resync, server.inventoryResync...)
	server.inventoryResync = nil
	response.Seq = server.seq
	encoded, _ := json.Marshal(response)
	writeJSONResponse(writer, 200, string(encoded))
}

// applyKeyedDelta is the backend's side of a delta: apply it to the stored
// items, keep the other top-level fields and re-encode canonically.
func applyKeyedDelta(stored, payload json.RawMessage) ([]byte, error) {
	top, items, err := splitKeyed(stored)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var delta inventory.Delta[map[string]any]
	if err := decoder.Decode(&delta); err != nil {
		return nil, err
	}
	result, err := inventory.Apply(items, delta, itemKey(top, []string{"manager", "name", "arch"}))
	if err != nil {
		return nil, err
	}
	top[itemsField] = result
	return inventory.Canonical(top)
}

func (server *protocolServer) sent() []inventory.Envelope {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]inventory.Envelope(nil), server.envelopes...)
}

func envelopeNames(envelope inventory.Envelope) []string {
	return slices.Sorted(maps.Keys(envelope.Datasets))
}

type testDaemonSetup struct {
	statePath string
	payloads  *inventory.PayloadStore
	registry  *collector.Registry
}

func newTestSetup(t *testing.T, collectors ...collector.Collector) testDaemonSetup {
	t.Helper()
	registry := collector.NewRegistry()
	for _, instance := range collectors {
		registry.MustRegister(instance)
	}
	directory := t.TempDir()
	return testDaemonSetup{
		statePath: filepath.Join(directory, "state.json"),
		payloads:  inventory.NewPayloadStore(filepath.Join(directory, "payloads")),
		registry:  registry,
	}
}

func (setup testDaemonSetup) options(t *testing.T, client Client, enabled ...string) RunOptions {
	t.Helper()
	store, err := inventory.OpenStateStore(setup.statePath)
	if err != nil {
		t.Fatal(err)
	}
	config := DefaultServerConfig()
	config.ETag = `"c1"`
	if len(enabled) > 0 {
		config.Datasets = enabled
	}
	return RunOptions{
		Registry: setup.registry, Store: store, Payloads: setup.payloads, Client: client,
		Identity:     Identity{ServerID: "01jb8z5k2m3n4p5q6r7s8t9v0w", Generation: 3},
		ServerConfig: config,
		Version:      "1.2.3",
		Now:          time.Now, Jitter: func(time.Duration) time.Duration { return 0 }, CollectTimeout: time.Second,
	}
}

func (setup testDaemonSetup) daemon(t *testing.T, client Client, enabled ...string) *daemon {
	t.Helper()
	return newDaemon(setup.options(t, client, enabled...))
}

func TestFirstPushSendsFullDatasetsWithSeqOne(t *testing.T) {
	server := newProtocolServer(t)
	system := &fakeCollector{name: "system", data: map[string]string{"hostname": "web-01"}}
	network := &fakeCollector{name: "network", data: []string{"eth0"}}
	setup := newTestSetup(t, system, network)
	daemon := setup.daemon(t, server.client())
	ctx := context.Background()

	if err := daemon.deliver(ctx, []string{"network", "system"}); err != nil {
		t.Fatal(err)
	}
	sent := server.sent()
	if len(sent) != 1 || sent[0].Seq != 1 || !reflect.DeepEqual(envelopeNames(sent[0]), []string{"network", "system"}) {
		t.Fatalf("first envelope = %+v", sent)
	}
	for name, dataset := range sent[0].Datasets {
		if dataset.Mode != inventory.ModeFull || inventory.Hash(dataset.Payload) != dataset.Hash {
			t.Fatalf("%s = %+v", name, dataset)
		}
	}
	store := daemon.options.Store
	if store.Hash("system") != sent[0].Datasets["system"].Hash || store.AcknowledgedSeq() != 1 {
		t.Fatal("acknowledged state not recorded")
	}

	// Unchanged datasets are not resent; a changed one goes alone.
	if err := daemon.deliver(ctx, []string{"network", "system"}); err != nil {
		t.Fatal(err)
	}
	if len(server.sent()) != 1 {
		t.Fatal("unchanged datasets were resent")
	}
	network.set([]string{"eth0", "eth1"}, nil)
	if err := daemon.deliver(ctx, []string{"network", "system"}); err != nil {
		t.Fatal(err)
	}
	sent = server.sent()
	if len(sent) != 2 || sent[1].Seq != 2 || !reflect.DeepEqual(envelopeNames(sent[1]), []string{"network"}) {
		t.Fatalf("second envelope = %+v", sent[1])
	}
}

func TestOnlyServerEnabledDatasetsAreSentOnServerIntervals(t *testing.T) {
	server := newProtocolServer(t)
	system := &fakeCollector{name: "system", data: "a"}
	packages := &fakeCollector{name: "packages", data: "b"}
	setup := newTestSetup(t, system, packages)
	options := setup.options(t, server.client())
	options.ServerConfig.Intervals.Datasets["system"] = 120
	daemon := newDaemon(options)

	if !reflect.DeepEqual(slices.Sorted(maps.Keys(daemon.next)), []string{"system"}) {
		t.Fatalf("scheduled = %v; packages is not enabled by default", daemon.next)
	}
	before := time.Now()
	if err := daemon.deliver(context.Background(), daemon.due(time.Now())); err != nil {
		t.Fatal(err)
	}
	if got := daemon.next["system"].Sub(before); got < 108*time.Second || got > 121*time.Second {
		t.Fatalf("system rescheduled in %s, want about 120s minus jitter", got)
	}

	daemon.applyConfig(ServerConfig{ETag: `"c2"`, Datasets: []string{"packages"}, Intervals: ServerIntervals{Heartbeat: 60}})
	if !reflect.DeepEqual(slices.Sorted(maps.Keys(daemon.next)), []string{"packages"}) {
		t.Fatalf("after config: scheduled = %v", daemon.next)
	}
	if daemon.settings.Heartbeat() != time.Minute || daemon.heartbeatAt.After(time.Now().Add(time.Minute)) {
		t.Fatal("server heartbeat interval not applied")
	}
}

func TestAcknowledgedHashesAdvanceOnlyWhenAcceptedMatches(t *testing.T) {
	server := newProtocolServer(t)
	server.wrongHash["system"] = true
	system := &fakeCollector{name: "system", data: "a"}
	setup := newTestSetup(t, system)
	daemon := setup.daemon(t, server.client())

	if err := daemon.deliver(context.Background(), []string{"system"}); err != nil {
		t.Fatal(err)
	}
	if daemon.options.Store.Hash("system") != "" {
		t.Fatal("a mismatching accepted hash was recorded")
	}
	if err := daemon.deliver(context.Background(), []string{"system"}); err != nil {
		t.Fatal(err)
	}
	if len(server.sent()) != 2 || daemon.options.Store.Hash("system") == "" {
		t.Fatal("dataset was not resent and acknowledged")
	}
}

func TestResyncIsAnsweredWithFullResend(t *testing.T) {
	server := newProtocolServer(t)
	system := &fakeCollector{name: "system", data: "a"}
	packages := &keyedCollector{fakeCollector{name: "packages"}}
	packageList := func(versionOfA string, names ...string) map[string]any {
		items := []map[string]string{}
		for _, name := range names {
			version := "1"
			if name == "a" {
				version = versionOfA
			}
			items = append(items, map[string]string{"name": name, "arch": "amd64", "version": version})
		}
		return map[string]any{"manager": "dpkg", "items": items}
	}
	packages.set(packageList("1", "a", "b", "c", "d", "e", "f", "g", "h"), nil)
	setup := newTestSetup(t, system, packages)
	daemon := setup.daemon(t, server.client(), "system", "packages")
	ctx := context.Background()

	if err := daemon.deliver(ctx, []string{"packages", "system"}); err != nil {
		t.Fatal(err)
	}

	// A heartbeat resync forgets the hash and makes the dataset due now.
	server.mu.Lock()
	server.heartbeatResync = []string{"system"}
	server.mu.Unlock()
	daemon.next["system"] = time.Now().Add(time.Hour)
	if err := daemon.heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if daemon.options.Store.Hash("system") != "" || daemon.next["system"].After(time.Now()) {
		t.Fatal("heartbeat resync did not schedule a full resend")
	}
	if err := daemon.deliver(ctx, daemon.due(time.Now())); err != nil {
		t.Fatal(err)
	}
	sent := server.sent()
	if len(sent) != 2 || !reflect.DeepEqual(envelopeNames(sent[1]), []string{"system"}) || sent[1].Datasets["system"].Mode != inventory.ModeFull {
		t.Fatalf("resync envelope = %+v", sent[len(sent)-1])
	}

	// A changed keyed dataset goes as a delta the server can apply.
	packages.set(packageList("2", "a", "c", "d", "e", "f", "g", "h", "z"), nil)
	if err := daemon.deliver(ctx, []string{"packages"}); err != nil {
		t.Fatal(err)
	}
	sent = server.sent()
	delta := sent[2].Datasets["packages"]
	if delta.Mode != inventory.ModeDelta || delta.BaseHash != sent[0].Datasets["packages"].Hash ||
		string(delta.Payload) != `{"added":[{"arch":"amd64","name":"z","version":"1"}],"changed":[{"arch":"amd64","name":"a","version":"2"}],"removed":["dpkg/b/amd64"]}` {
		t.Fatalf("delta = %+v %s", delta, delta.Payload)
	}
	if daemon.options.Store.Hash("packages") != delta.Hash {
		t.Fatal("applied delta was not acknowledged")
	}

	// An inventory resync of a delta is answered by a full resend at once.
	server.mu.Lock()
	server.inventoryResync = []string{"packages"}
	server.mu.Unlock()
	packages.set(packageList("3", "a", "c", "d", "e", "f", "g", "h", "z"), nil)
	if err := daemon.deliver(ctx, []string{"packages"}); err != nil {
		t.Fatal(err)
	}
	if !daemon.next["packages"].Before(time.Now().Add(time.Second)) {
		t.Fatal("resynced delta dataset not due right away")
	}
	if err := daemon.deliver(ctx, daemon.due(time.Now())); err != nil {
		t.Fatal(err)
	}
	sent = server.sent()
	if last := sent[len(sent)-1].Datasets["packages"]; len(sent) != 5 || last.Mode != inventory.ModeFull {
		t.Fatalf("after inventory resync: %d envelopes, last %+v", len(sent), last)
	}
}

func TestIgnoredDatasetsWaitForAConfigChange(t *testing.T) {
	server := newProtocolServer(t)
	server.ignoreUnknown = map[string]bool{"packages": true}
	packages := &fakeCollector{name: "packages", data: "a"}
	setup := newTestSetup(t, packages)
	daemon := setup.daemon(t, server.client(), "packages")
	ctx := context.Background()

	for range 2 {
		if err := daemon.deliver(ctx, []string{"packages"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(server.sent()) != 1 || daemon.options.Store.Hash("packages") != "" {
		t.Fatalf("ignored dataset resent: %d envelopes", len(server.sent()))
	}
	daemon.applyConfig(ServerConfig{ETag: `"c2"`, Datasets: []string{"packages"}})
	server.mu.Lock()
	server.ignoreUnknown = nil
	server.mu.Unlock()
	if err := daemon.deliver(ctx, []string{"packages"}); err != nil {
		t.Fatal(err)
	}
	if len(server.sent()) != 2 || daemon.options.Store.Hash("packages") == "" {
		t.Fatal("dataset not sent after the configuration changed")
	}
}

func TestCollectorErrorsAreReportedWhenTheyChange(t *testing.T) {
	server := newProtocolServer(t)
	network := &fakeCollector{name: "network", err: errors.New("sysfs unavailable")}
	hardware := &fakeCollector{name: "hardware", fail: true}
	system := &fakeCollector{name: "system", data: "ok"}
	setup := newTestSetup(t, network, hardware, system)
	daemon := setup.daemon(t, server.client())
	ctx := context.Background()
	all := []string{"hardware", "network", "system"}

	if err := daemon.deliver(ctx, all); err != nil {
		t.Fatal(err)
	}
	first := server.sent()[0]
	if len(first.Datasets) != 1 || first.Errors["network"] != "sysfs unavailable" || first.Errors["hardware"] == "" {
		t.Fatalf("envelope = %+v", first)
	}
	// Same errors, no changes: nothing is sent.
	if err := daemon.deliver(ctx, all); err != nil {
		t.Fatal(err)
	}
	if len(server.sent()) != 1 {
		t.Fatal("unchanged errors were reported again")
	}
	// A new error message is reported on its own.
	network.set(nil, errors.New("permission denied"))
	if err := daemon.deliver(ctx, all); err != nil {
		t.Fatal(err)
	}
	sent := server.sent()
	if len(sent) != 2 || len(sent[1].Datasets) != 0 || sent[1].Errors["network"] != "permission denied" {
		t.Fatalf("error-only envelope = %+v", sent)
	}
	// Recovery sends the dataset so the backend can clear the error.
	network.set([]string{"eth0"}, nil)
	if err := daemon.deliver(ctx, []string{"network"}); err != nil {
		t.Fatal(err)
	}
	sent = server.sent()
	if len(sent) != 3 || !reflect.DeepEqual(envelopeNames(sent[2]), []string{"network"}) || sent[2].Errors != nil {
		t.Fatalf("recovery envelope = %+v", sent[2])
	}
}

func TestSeqSurvivesRestartAndCatchesUpWithTheServer(t *testing.T) {
	server := newProtocolServer(t)
	system := &fakeCollector{name: "system", data: "a"}
	setup := newTestSetup(t, system)

	if err := setup.daemon(t, server.client()).deliver(context.Background(), []string{"system"}); err != nil {
		t.Fatal(err)
	}
	system.set("b", nil)
	restarted := setup.daemon(t, server.client())
	if restarted.options.Store.Seq() != 1 {
		t.Fatalf("seq after restart = %d", restarted.options.Store.Seq())
	}
	if err := restarted.deliver(context.Background(), []string{"system"}); err != nil {
		t.Fatal(err)
	}
	if sent := server.sent(); len(sent) != 2 || sent[1].Seq != 2 {
		t.Fatalf("envelopes after restart = %+v", sent)
	}

	// A reinstall restarted the agent's seq below the server's: the server
	// does not apply the envelope, and the agent catches up and resends.
	server.mu.Lock()
	server.seqAhead = 40
	server.mu.Unlock()
	system.set("c", nil)
	if err := restarted.deliver(context.Background(), []string{"system"}); err != nil {
		t.Fatal(err)
	}
	if restarted.options.Store.Seq() != 40 || restarted.next["system"].After(time.Now()) {
		t.Fatalf("seq = %d, next = %s", restarted.options.Store.Seq(), restarted.next["system"])
	}
	if err := restarted.deliver(context.Background(), []string{"system"}); err != nil {
		t.Fatal(err)
	}
	sent := server.sent()
	if last := sent[len(sent)-1]; last.Seq != 41 || restarted.options.Store.Hash("system") != last.Datasets["system"].Hash {
		t.Fatalf("catch-up envelope = %+v", last)
	}
}

func TestDeliveryFailuresFollowTheBehaviourTable(t *testing.T) {
	server := newProtocolServer(t)
	system := &fakeCollector{name: "system", data: "a"}
	setup := newTestSetup(t, system)
	daemon := setup.daemon(t, server.client())
	ctx := context.Background()

	server.fail("/inventory",
		failure{status: 500, body: `{"type":"server_error","status":500,"title":"Oops"}`},
		failure{status: 429, body: `{"status":429,"title":"Slow"}`, header: map[string]string{"Retry-After": "90"}},
		failure{status: 422, body: `{"type":"validation_failed","status":422,"title":"Invalid"}`},
	)
	daemon.options.Jitter = func(maximum time.Duration) time.Duration { return maximum }

	// 5xx: exponential backoff.
	start := time.Now()
	if err := daemon.deliver(ctx, []string{"system"}); err != nil {
		t.Fatal(err)
	}
	if wait := daemon.next["system"].Sub(start); wait < initialBackoff || wait > initialBackoff+time.Second || daemon.inventoryBackoff != 2*initialBackoff {
		t.Fatalf("5xx retry in %s, backoff now %s", wait, daemon.inventoryBackoff)
	}
	// 429: Retry-After.
	if err := daemon.deliver(ctx, []string{"system"}); err != nil {
		t.Fatal(err)
	}
	if wait := daemon.next["system"].Sub(start); wait < 90*time.Second || wait > 91*time.Second {
		t.Fatalf("429 retry in %s", wait)
	}
	// 422: not retried unchanged; back on the normal interval.
	if err := daemon.deliver(ctx, []string{"system"}); err != nil {
		t.Fatal(err)
	}
	if wait := daemon.next["system"].Sub(start); wait < 5*time.Minute {
		t.Fatalf("422 retry in %s", wait)
	}
	if daemon.options.Store.Hash("system") != "" {
		t.Fatal("failed deliveries recorded a hash")
	}
	// Each attempt reserved a new seq.
	if sent := server.sent(); len(sent) != 0 || daemon.options.Store.Seq() != 3 {
		t.Fatalf("seq = %d", daemon.options.Store.Seq())
	}

	// 426: inventory pauses and heartbeats drop to hourly.
	server.fail("/heartbeat", failure{status: 426, body: `{"type":"unsupported_revision","status":426,"title":"Upgrade"}`})
	if err := daemon.heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if !daemon.unsupported || daemon.heartbeatAt.Sub(start) < 59*time.Minute {
		t.Fatalf("426: unsupported %v, next heartbeat %s", daemon.unsupported, daemon.heartbeatAt.Sub(start))
	}
	if err := daemon.heartbeat(ctx); err != nil || daemon.unsupported || daemon.next["system"].After(time.Now()) {
		t.Fatalf("recovery from 426: %v unsupported=%v", err, daemon.unsupported)
	}
}

func TestRevokedCredentialStopsTheDaemon(t *testing.T) {
	server := newProtocolServer(t)
	server.fail("/heartbeat", failure{status: 401, body: string(readFixtureNoT("problem.json"))})
	setup := newTestSetup(t, &fakeCollector{name: "system", data: "a"})
	options := setup.options(t, server.client())
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), options) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrStopped) {
			t.Fatalf("Run() = %v, want ErrStopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept going after 401 revoked")
	}
	if len(server.sent()) != 0 {
		t.Fatal("inventory was sent after the credential was revoked")
	}

	// A 401 on inventory stops it too.
	server.fail("/inventory", failure{status: 401, body: `{"type":"unauthenticated","status":401,"title":"Unauthenticated"}`})
	err := setup.daemon(t, server.client()).deliver(context.Background(), []string{"system"})
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("deliver() = %v", err)
	}
}

func TestRunHeartbeatsFetchesConfigAndReactsToRealtime(t *testing.T) {
	server := newProtocolServer(t)
	system := &fakeCollector{name: "system", data: "a"}
	hardware := &fakeCollector{name: "hardware", data: "b"}
	setup := newTestSetup(t, system, hardware)
	options := setup.options(t, server.client())
	options.ServerConfig.Realtime = &Realtime{Key: "k", Host: "socket.test", Port: 443, Scheme: "https", AuthEndpoint: server.server.URL + "/agent/v1/broadcasting/auth"}

	type connection struct {
		channel   string
		authorize RealtimeAuthorizer
		onEvent   func(string)
	}
	connections := make(chan connection, 4)
	stops := make(chan struct{}, 4)
	options.Realtime = func(config Realtime, channel string, authorize RealtimeAuthorizer, onEvent func(string)) func() {
		connections <- connection{channel, authorize, onEvent}
		return func() { stops <- struct{}{} }
	}
	reload := make(chan Config)
	options.Reload = reload
	readyCalls := 0
	options.Ready = func() { readyCalls++ }

	// The server's config moved on: heartbeat → GET /config.
	server.mu.Lock()
	server.config.ETag = `"c2"`
	server.config.Datasets = []string{"system"}
	server.config.Realtime = options.ServerConfig.Realtime
	server.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, options) }()

	waitFor := func(path string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case got := <-server.events:
				if got == path {
					return
				}
			case <-deadline:
				t.Fatalf("timed out waiting for %s", path)
			}
		}
	}
	waitFor("/heartbeat")
	waitFor("/config")
	waitFor("/inventory")

	var realtime connection
	select {
	case realtime = <-connections:
	case <-time.After(5 * time.Second):
		t.Fatal("realtime not connected")
	}
	if realtime.channel != "private-agent.server_01jb8z5k2m3n4p5q6r7s8t9v0w.generation_3" {
		t.Fatalf("channel = %s", realtime.channel)
	}
	if auth, err := realtime.authorize(ctx, "1.2", realtime.channel); err != nil || auth != "key:signature" {
		t.Fatalf("authorize = %q, %v", auth, err)
	}

	// inventory.requested collects everything now; only changes go out.
	system.set("changed", nil)
	realtime.onEvent(EventInventoryRequested)
	waitFor("/inventory")

	// config.updated fetches the config (304 now).
	realtime.onEvent(EventConfigUpdated)
	waitFor("/config")

	// SIGHUP-style reload collects again; nothing changed, nothing sent.
	reload <- Config{}
	time.Sleep(100 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v, want nil on shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	select {
	case <-stops:
	default:
		t.Fatal("realtime was not disconnected on shutdown")
	}

	if readyCalls != 1 {
		t.Fatalf("Ready called %d times", readyCalls)
	}
	sent := server.sent()
	if len(sent) != 2 || !reflect.DeepEqual(envelopeNames(sent[0]), []string{"system"}) || !reflect.DeepEqual(envelopeNames(sent[1]), []string{"system"}) {
		t.Fatalf("envelopes = %d (hardware is disabled by the fetched config)", len(sent))
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if !reflect.DeepEqual(server.configs, []string{`"c1"`, `"c2"`}) {
		t.Fatalf("If-None-Match values = %q", server.configs)
	}
	first := server.heartbeats[0]
	if first.AgentVersion != "1.2.3" || first.StartedAt.IsZero() || first.SentAt.IsZero() {
		t.Fatalf("heartbeat = %+v", first)
	}
}

func TestChangeTokenTriggersEarlyCollection(t *testing.T) {
	server := newProtocolServer(t)
	packages := &changingCollector{fakeCollector: fakeCollector{name: "packages", data: "a"}, token: "1"}
	setup := newTestSetup(t, packages)
	daemon := setup.daemon(t, server.client(), "packages")
	now := time.Now()
	daemon.next["packages"] = now.Add(time.Hour)

	daemon.pollChanges(now)
	if daemon.next["packages"].Before(now.Add(time.Minute)) {
		t.Fatal("the first token observation scheduled a collection")
	}
	packages.mu.Lock()
	packages.token = "2"
	packages.mu.Unlock()
	daemon.pollChanges(now)
	if daemon.next["packages"].After(now) {
		t.Fatal("a changed token did not make the dataset due")
	}
}

func TestBuildDeltaFallsBackToFull(t *testing.T) {
	fields := []string{"manager", "name", "arch"}
	base := []byte(`{"items":[{"arch":"amd64","name":"a","version":"1"}],"manager":"dpkg"}`)
	managerChanged := []byte(`{"items":[{"arch":"amd64","name":"a","version":"1"}],"manager":"rpm"}`)
	if _, ok, err := buildDelta(base, managerChanged, fields); err != nil || ok {
		t.Fatalf("top-level change produced a delta: %v %v", ok, err)
	}
	// Replacing everything is not smaller than the full payload.
	replaced := []byte(`{"items":[{"arch":"amd64","name":"b","version":"1"}],"manager":"dpkg"}`)
	if _, ok, _ := buildDelta(base, replaced, fields); ok {
		t.Fatal("a delta larger than the payload was used")
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

func TestRunRequiresDependencies(t *testing.T) {
	if err := Run(context.Background(), RunOptions{}); err == nil {
		t.Fatal("Run() without dependencies succeeded")
	}
}
