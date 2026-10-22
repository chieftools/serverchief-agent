package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"go.chief.tools/serverchief-agent/inventory"
)

func TestCommandFixturesRoundTrip(t *testing.T) {
	fixture := readFixture(t, "commands-response.json")
	var response CommandsResponse
	if err := json.Unmarshal(fixture, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Commands) != 2 || response.Commands[0].Kind != CommandRefreshInventory || response.Commands[1].Kind != CommandResync ||
		response.Commands[0].ExpiresAt.Sub(response.Commands[0].CreatedAt) != 24*time.Hour {
		t.Fatalf("commands = %+v", response.Commands)
	}
	if encoded, _ := json.Marshal(response); !sameJSON(t, encoded, fixture) {
		t.Fatalf("CommandsResponse does not round-trip the fixture:\n%s", encoded)
	}

	fixture = readFixture(t, "command-result-request.json")
	var result CommandResult
	if err := json.Unmarshal(fixture, &result); err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(result); !sameJSON(t, encoded, fixture) {
		t.Fatalf("CommandResult does not round-trip the fixture:\n%s", encoded)
	}
}

func TestHTTPClientCommandEndpoints(t *testing.T) {
	commands := string(readFixture(t, "commands-response.json"))
	server, requests := newRecordingServer(t, func(writer http.ResponseWriter, request *recordedRequest) {
		switch request.path {
		case "/agent/v1/commands":
			writeJSONResponse(writer, 200, commands)
		case "/agent/v1/commands/01jb9a2c3d4e5f6g7h8j9k0m1n/ack", "/agent/v1/commands/01jb9a2c3d4e5f6g7h8j9k0m1n/result":
			writer.WriteHeader(http.StatusNoContent)
		default:
			writeJSONResponse(writer, 404, `{"type":"not_found","title":"Not found","status":404}`)
		}
	})
	ctx := context.Background()
	client := NewClient(server.URL+"/agent/v1", "scagt_secret", "1.2.3", server.Client())

	fetched, err := client.FetchCommands(ctx)
	if err != nil || len(fetched) != 2 || fetched[0].ID != "01jb9a2c3d4e5f6g7h8j9k0m1n" || string(fetched[1].Args) != `{
        "datasets": [
          "packages"
        ]
      }` {
		t.Fatalf("FetchCommands() = %+v, %v", fetched, err)
	}
	if err := client.AckCommand(ctx, "01jb9a2c3d4e5f6g7h8j9k0m1n"); err != nil {
		t.Fatalf("AckCommand() = %v", err)
	}
	result := CommandResult{Status: CommandSucceeded, FinishedAt: time.Date(2026, 10, 22, 14, 10, 3, 0, time.UTC)}
	if err := client.ReportCommandResult(ctx, "01jb9a2c3d4e5f6g7h8j9k0m1n", result); err != nil {
		t.Fatalf("ReportCommandResult() = %v", err)
	}
	err = client.AckCommand(ctx, "someone-elses")
	var problem *ProblemError
	if !errors.As(err, &problem) || problem.Status != 404 || problem.Type != "not_found" {
		t.Fatalf("AckCommand(unknown) = %v", err)
	}
	if kind, _ := classify(err); kind != failureRejected {
		t.Fatalf("404 classified %v", kind)
	}

	if len(*requests) != 4 {
		t.Fatalf("%d requests", len(*requests))
	}
	get, ack, report := (*requests)[0], (*requests)[1], (*requests)[2]
	if get.method != http.MethodGet || get.header.Get("Authorization") != "Bearer scagt_secret" || get.header.Get(ProtocolHeader) != ProtocolRevision {
		t.Fatalf("GET /commands = %s %v", get.method, get.header)
	}
	if ack.method != http.MethodPost || len(ack.body) != 0 || ack.header.Get("Content-Type") != "" {
		t.Fatalf("ack = %s %q %q", ack.method, ack.body, ack.header.Get("Content-Type"))
	}
	if report.method != http.MethodPost || report.header.Get("Content-Type") != "application/json" ||
		!sameJSON(t, report.body, readFixture(t, "command-result-request.json")) {
		t.Fatalf("result = %s %s", report.method, report.body)
	}
}

// commandsSetup is a daemon with a system and a keyed packages dataset,
// backed by the fake protocol server and a command journal on disk.
type commandsSetup struct {
	testDaemonSetup
	server      *protocolServer
	system      *fakeCollector
	packages    *keyedCollector
	journalPath string
}

func newCommandsSetup(t *testing.T) *commandsSetup {
	t.Helper()
	system := &fakeCollector{name: "system", data: map[string]any{"hostname": "web-01"}}
	packages := &keyedCollector{fakeCollector{name: "packages", data: map[string]any{
		"manager": "dpkg",
		"items":   []any{map[string]any{"name": "nginx", "version": "1.0", "arch": "amd64"}},
	}}}
	return &commandsSetup{
		testDaemonSetup: newTestSetup(t, system, packages),
		server:          newProtocolServer(t),
		system:          system,
		packages:        packages,
		journalPath:     filepath.Join(t.TempDir(), "commands.json"),
	}
}

// start opens the journal and returns a daemon that already completed its
// startup round, as after a (re)start.
func (setup *commandsSetup) start(t *testing.T) *daemon {
	t.Helper()
	options := setup.options(t, setup.server.client(), "system", "packages")
	journal, err := OpenCommandJournal(setup.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	options.Commands = journal
	return newDaemon(options)
}

func tick(t *testing.T, daemon *daemon) {
	t.Helper()
	if err := daemon.tick(context.Background()); err != nil {
		t.Fatalf("tick() = %v", err)
	}
}

// heartbeatNow makes the next tick heartbeat.
func heartbeatNow(daemon *daemon) { daemon.heartbeatAt = time.Now() }

// requestsSince returns the request paths after the first count.
func requestsSince(server *protocolServer, count int) []string {
	return server.requestLog()[count:]
}

func TestHeartbeatFlagFetchesAcknowledgesExecutesAndReports(t *testing.T) {
	setup := newCommandsSetup(t)
	server := setup.server
	daemon := setup.start(t)
	tick(t, daemon) // heartbeat and first push
	if sent := server.sent(); len(sent) != 1 || !reflect.DeepEqual(envelopeNames(sent[0]), []string{"packages", "system"}) {
		t.Fatalf("startup envelopes = %+v", sent)
	}

	server.addCommand("c1", CommandRefreshInventory, `{"datasets":null}`)
	server.addCommand("c2", CommandResync, `{"datasets":["packages"]}`)
	server.addCommand("c3", CommandRefreshConfig, `{}`)
	setup.system.set(map[string]any{"hostname": "web-02"}, nil)
	before := len(server.requestLog())
	heartbeatNow(daemon)
	tick(t, daemon) // heartbeat → commands → acks → push → results
	tick(t, daemon) // the config fetch refresh_config asked for

	want := []string{
		"/heartbeat", "/commands",
		"/commands/c1/ack", "/commands/c2/ack", "/commands/c3/ack",
		"/inventory",
		"/commands/c1/result", "/commands/c2/result",
		"/config", "/commands/c3/result",
	}
	if got := requestsSince(server, before); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests =\n%q\nwant\n%q", got, want)
	}
	sent := server.sent()
	pushed := sent[len(sent)-1]
	// packages is unchanged but resync sends it in full.
	if !reflect.DeepEqual(envelopeNames(pushed), []string{"packages", "system"}) || pushed.Datasets["packages"].Mode != inventory.ModeFull {
		t.Fatalf("command push = %+v", pushed)
	}
	for _, id := range []string{"c1", "c2", "c3"} {
		results := server.resultsOf(id)
		if len(results) != 1 || results[0].Status != CommandSucceeded || results[0].Detail != nil || results[0].FinishedAt.IsZero() {
			t.Fatalf("%s results = %+v", id, results)
		}
		if state := server.commandState(id); state != CommandSucceeded {
			t.Fatalf("%s is %s", id, state)
		}
	}

	// Nothing is pending any more: the next heartbeat fetches nothing.
	before = len(server.requestLog())
	heartbeatNow(daemon)
	tick(t, daemon)
	if got := requestsSince(server, before); !reflect.DeepEqual(got, []string{"/heartbeat"}) {
		t.Fatalf("idle requests = %q", got)
	}
}

func TestUnknownCommandKindIsAcknowledgedAndFailed(t *testing.T) {
	setup := newCommandsSetup(t)
	daemon := setup.start(t)
	tick(t, daemon)

	setup.server.addCommand("c9", "update_agent", `{"version":"2.0.0"}`)
	before := len(setup.server.requestLog())
	heartbeatNow(daemon)
	tick(t, daemon)
	if got := requestsSince(setup.server, before); !reflect.DeepEqual(got, []string{"/heartbeat", "/commands", "/commands/c9/ack", "/commands/c9/result"}) {
		t.Fatalf("requests = %q", got)
	}
	results := setup.server.resultsOf("c9")
	if len(results) != 1 || results[0].Status != CommandFailed || results[0].Detail == nil || *results[0].Detail != `unsupported command kind "update_agent"` {
		t.Fatalf("results = %+v", results)
	}
}

func TestFailedCommandsReportTheirError(t *testing.T) {
	setup := newCommandsSetup(t)
	server := setup.server
	daemon := setup.start(t)
	tick(t, daemon)

	// The push the command triggered fails.
	server.addCommand("c1", CommandResync, `{"datasets":["system"]}`)
	server.fail("/inventory", failure{status: 500, body: `{"type":"server_error","title":"Oops","status":500,"detail":"database unavailable"}`})
	// None of the requested datasets is collected.
	server.addCommand("c2", CommandRefreshInventory, `{"datasets":["vhosts"]}`)
	// Malformed arguments.
	server.addCommand("c3", CommandRefreshInventory, `{"datasets":"all"}`)
	heartbeatNow(daemon)
	tick(t, daemon)

	detail := func(id string) string {
		t.Helper()
		results := server.resultsOf(id)
		if len(results) != 1 || results[0].Status != CommandFailed || results[0].Detail == nil {
			t.Fatalf("%s results = %+v", id, results)
		}
		return *results[0].Detail
	}
	if got := detail("c1"); !strings.Contains(got, "system: ") || !strings.Contains(got, "500 server_error: database unavailable") {
		t.Fatalf("c1 detail = %q", got)
	}
	if got := detail("c2"); got != "no requested dataset is enabled: vhosts" {
		t.Fatalf("c2 detail = %q", got)
	}
	if got := detail("c3"); !strings.HasPrefix(got, "invalid args: ") {
		t.Fatalf("c3 detail = %q", got)
	}
}

func TestCommandAcksAndResultsSurviveTransientErrors(t *testing.T) {
	setup := newCommandsSetup(t)
	server := setup.server
	daemon := setup.start(t)
	tick(t, daemon)

	server.addCommand("c1", CommandResync, `{"datasets":["system"]}`)
	server.fail("/commands/c1/ack", failure{status: 503, body: `{"type":"temporarily_unavailable","status":503,"title":"Down"}`})
	server.fail("/commands/c1/result", failure{status: 502, body: `bad gateway`})
	envelopes := len(server.sent())
	heartbeatNow(daemon)
	tick(t, daemon)
	if len(server.sent()) != envelopes || server.commandState("c1") != "delivered" {
		t.Fatalf("executed before the ack succeeded: %d envelopes, %s", len(server.sent()), server.commandState("c1"))
	}

	tick(t, daemon) // ack retried, executed, result fails
	if len(server.sent()) != envelopes+1 || server.commandState("c1") != "acknowledged" || len(server.resultsOf("c1")) != 0 {
		t.Fatalf("after ack retry: %d envelopes, %s", len(server.sent()), server.commandState("c1"))
	}
	tick(t, daemon) // result retried
	if results := server.resultsOf("c1"); len(results) != 1 || results[0].Status != CommandSucceeded {
		t.Fatalf("results = %+v", results)
	}
	if len(server.sent()) != envelopes+1 {
		t.Fatal("the command was executed twice")
	}
	acks := 0
	for _, path := range server.requestLog() {
		if path == "/commands/c1/ack" {
			acks++
		}
	}
	if acks != 2 {
		t.Fatalf("%d acks", acks)
	}
}

// TestRestartDoesNotExecuteACommandAgain covers a crash after a command
// ran but before its result arrived, combined with an ack the backend
// lost, so GET /commands lists the command again after the restart.
func TestRestartDoesNotExecuteACommandAgain(t *testing.T) {
	setup := newCommandsSetup(t)
	server := setup.server
	first := setup.start(t)
	tick(t, first)

	server.addCommand("c1", CommandResync, `{"datasets":["packages"]}`)
	server.fail("/commands/c1/result", failure{status: 500, body: `{"type":"server_error","status":500,"title":"Oops"}`})
	heartbeatNow(first)
	tick(t, first) // executed (packages resent in full), result lost
	envelopes := len(server.sent())
	if last := server.sent()[envelopes-1]; !reflect.DeepEqual(envelopeNames(last), []string{"packages"}) || len(server.resultsOf("c1")) != 0 {
		t.Fatalf("first run: %+v, results %+v", last, server.resultsOf("c1"))
	}

	// The process dies here. The backend lost the ack as well.
	server.mu.Lock()
	server.commandStates["c1"] = "delivered"
	server.mu.Unlock()

	before := len(server.requestLog())
	second := setup.start(t)
	tick(t, second)
	if len(server.sent()) != envelopes {
		t.Fatalf("the command ran again after the restart: %+v", server.sent()[envelopes:])
	}
	results := server.resultsOf("c1")
	if len(results) != 1 || results[0].Status != CommandSucceeded {
		t.Fatalf("results = %+v", results)
	}
	got := requestsSince(server, before)
	if !slices.Contains(got, "/commands/c1/ack") || slices.Index(got, "/commands/c1/result") < 0 {
		t.Fatalf("requests after restart = %q", got)
	}

	// A third start finds the command completed and sends nothing for it.
	before = len(server.requestLog())
	third := setup.start(t)
	tick(t, third)
	for _, path := range requestsSince(server, before) {
		if strings.HasPrefix(path, "/commands/") {
			t.Fatalf("third start sent %s", path)
		}
	}
}

func TestRestartRecoversInterruptedAndUnacknowledgedCommands(t *testing.T) {
	setup := newCommandsSetup(t)
	server := setup.server
	server.addCommand("running", CommandResync, `{"datasets":null}`)
	server.addCommand("received", CommandRefreshConfig, `{}`)
	server.mu.Lock()
	server.commandStates["running"] = "acknowledged"
	server.mu.Unlock()

	// The previous process acknowledged "running" and was executing it,
	// and journaled "received" without acknowledging it yet.
	journal, err := OpenCommandJournal(setup.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, record := range []commandRecord{
		{ID: "running", Kind: CommandResync, State: commandAcknowledged, UpdatedAt: now},
		{ID: "received", Kind: CommandRefreshConfig, Args: json.RawMessage(`{}`), State: commandReceived, UpdatedAt: now},
	} {
		if err := journal.put(record); err != nil {
			t.Fatal(err)
		}
	}

	daemon := setup.start(t)
	tick(t, daemon)
	tick(t, daemon)
	running := server.resultsOf("running")
	if len(running) != 1 || running[0].Status != CommandFailed || running[0].Detail == nil || *running[0].Detail != "the agent restarted before the command finished" {
		t.Fatalf("running results = %+v", running)
	}
	if received := server.resultsOf("received"); len(received) != 1 || received[0].Status != CommandSucceeded {
		t.Fatalf("received results = %+v", received)
	}
	if server.commandState("received") != CommandSucceeded {
		t.Fatalf("received is %s", server.commandState("received"))
	}
}

func TestRealtimeSubscriptionChecksCommands(t *testing.T) {
	setup := newCommandsSetup(t)
	daemon := setup.start(t)
	daemon.onRealtimeEvent(eventSubscribed)
	select {
	case <-daemon.commandsWake:
	default:
		t.Fatal("a fresh subscription did not check for commands")
	}
	daemon.onRealtimeEvent("inventory.requested")
	select {
	case <-daemon.commandsWake:
		t.Fatal("inventory.requested is no longer an event")
	default:
	}
}

func TestCommandJournalPrunesReportedRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commands.json")
	journal, err := OpenCommandJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	result := &CommandResult{Status: CommandSucceeded, FinishedAt: now}
	_ = journal.put(commandRecord{ID: "stale", State: commandFinished, Result: result, Reported: true, UpdatedAt: now.Add(-49 * time.Hour)})
	_ = journal.put(commandRecord{ID: "unreported", State: commandFinished, Result: result, UpdatedAt: now.Add(-72 * time.Hour)})
	for index := range maximumJournalRecords + 5 {
		_ = journal.put(commandRecord{ID: fmt.Sprintf("c%03d", index), State: commandFinished, Result: result, Reported: true, UpdatedAt: now.Add(time.Duration(index) * time.Second)})
	}

	reopened, err := OpenCommandJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	records := reopened.snapshot()
	if len(records) != maximumJournalRecords {
		t.Fatalf("%d records", len(records))
	}
	if reopened.get("stale") != nil || reopened.get("c000") != nil || reopened.get("unreported") == nil || reopened.get("c104") == nil {
		t.Fatal("pruned the wrong records")
	}
}
