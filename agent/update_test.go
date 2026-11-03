package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.chief.tools/serverchief-agent/inventory"
	"go.chief.tools/serverchief-agent/updater"
)

// fakeUpdates is an updater.Controller over a temporary directory whose
// capability, launches and helper lock are scripted.
type fakeUpdates struct {
	controller *updater.Controller
	store      updater.Store

	mu        sync.Mutex
	launches  int
	launchErr error
	running   bool
	capable   updater.Capability
}

func newFakeUpdates(t *testing.T, version string) *fakeUpdates {
	t.Helper()
	fake := &fakeUpdates{store: updater.Store{Directory: t.TempDir(), Owner: -1}, capable: updater.Capability{Available: true}}
	fake.controller = fake.forVersion(version)
	return fake
}

// forVersion returns a controller over the same files for a daemon
// running version (a restart after the package changed).
func (fake *fakeUpdates) forVersion(version string) *updater.Controller {
	return &updater.Controller{
		Store:   fake.store,
		Version: version,
		Detect: func(context.Context) updater.Capability {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			return fake.capable
		},
		Launch: func(context.Context) error {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			fake.launches++
			return fake.launchErr
		},
		HelperRunning: func() bool {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			return fake.running
		},
	}
}

func (fake *fakeUpdates) launchCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.launches
}

// helperFinishes writes the helper's result and removes the request, as
// the helper does.
func (fake *fakeUpdates) helperFinishes(t *testing.T, result updater.Result) {
	t.Helper()
	data, _ := json.Marshal(result)
	if err := inventory.WriteFileAtomic(filepath.Join(fake.store.Directory, updater.ResultFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fake.store.RemoveRequest(); err != nil {
		t.Fatal(err)
	}
}

func (fake *fakeUpdates) exists(name string) bool {
	_, err := os.Stat(filepath.Join(fake.store.Directory, name))
	return err == nil
}

// startWithUpdates starts a daemon running version with the fake updater.
func (setup *commandsSetup) startWithUpdates(t *testing.T, updates *updater.Controller, version string) *daemon {
	t.Helper()
	options := setup.options(t, setup.server.client(), "system", "packages")
	journal, err := OpenCommandJournal(setup.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	options.Commands = journal
	options.Version = version
	options.Updates = updates
	options.UpdatePoll = time.Nanosecond
	return newDaemon(options)
}

func updateResultData(t *testing.T, result CommandResult) AgentUpdateData {
	t.Helper()
	var data AgentUpdateData
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatalf("result data %s: %v", result.Data, err)
	}
	return data
}

func (server *protocolServer) heartbeatLog() []HeartbeatRequest {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]HeartbeatRequest(nil), server.heartbeats...)
}

func TestUpdateAgentHandsOverOnceAndReportsTheHelperResult(t *testing.T) {
	setup := newCommandsSetup(t)
	server := setup.server
	updates := newFakeUpdates(t, "1.2.3")
	first := setup.startWithUpdates(t, updates.controller, "1.2.3")
	tick(t, first)
	heartbeats := server.heartbeatLog()
	if capability := heartbeats[0].Capabilities; capability == nil || !capability.AgentUpdate.Available || capability.AgentUpdate.Reason != nil {
		t.Fatalf("heartbeat capabilities = %+v", capability)
	}

	server.addCommand("u1", CommandUpdateAgent, `{"version":"1.3.0"}`)
	before := len(server.requestLog())
	heartbeatNow(first)
	tick(t, first)
	if got := requestsSince(server, before); !reflect.DeepEqual(got, []string{"/heartbeat", "/commands", "/commands/u1/ack"}) {
		t.Fatalf("requests = %q", got)
	}
	request, err := updates.store.ReadRequest()
	if err != nil || request == nil || request.CommandID != "u1" || request.PreviousVersion != "1.2.3" || request.TargetVersion != "1.3.0" {
		t.Fatalf("request = %+v (%v)", request, err)
	}
	if updates.launchCount() != 1 {
		t.Fatalf("%d launches", updates.launchCount())
	}
	if record := first.commands.get("u1"); record == nil || record.State != commandHandedOff {
		t.Fatalf("journal = %+v", record)
	}
	// The metadata refresh stays out of the helper's way.
	if !first.update.inProgress {
		t.Fatal("the update is not in progress")
	}
	// The daemon that handed over never reports ready itself.
	heartbeatNow(first)
	tick(t, first)
	if updates.exists(updater.ReadyFile) || len(server.resultsOf("u1")) != 0 {
		t.Fatal("the handing-over daemon reported ready or a result")
	}

	// postinst restarts the daemon as 1.3.0 while the helper runs, and
	// the backend lost the ack, so GET /commands lists u1 again.
	updates.mu.Lock()
	updates.running = true
	updates.mu.Unlock()
	server.mu.Lock()
	server.commandStates["u1"] = "delivered"
	server.mu.Unlock()
	second := setup.startWithUpdates(t, updates.forVersion("1.3.0"), "1.3.0")
	if updates.exists(updater.ReadyFile) {
		t.Fatal("ready before a heartbeat")
	}
	before = len(server.requestLog())
	tick(t, second)
	readiness, err := updates.store.ReadReadiness()
	if err != nil || readiness == nil || readiness.CommandID != "u1" || readiness.Version != "1.3.0" || readiness.PID != os.Getpid() {
		t.Fatalf("readiness = %+v (%v)", readiness, err)
	}
	if got := requestsSince(server, before); !slices.Contains(got, "/commands/u1/ack") || slices.Contains(got, "/commands/u1/result") {
		t.Fatalf("requests after the restart = %q", got)
	}
	if updates.launchCount() != 1 {
		t.Fatalf("the restart launched the helper again (%d launches)", updates.launchCount())
	}

	updates.helperFinishes(t, updater.Result{CommandID: "u1", Code: updater.CodeUpdated, PreviousVersion: "1.2.3", TargetVersion: "1.3.0", InstalledVersion: "1.3.0", FinishedAt: time.Now().UTC()})
	tick(t, second)
	results := server.resultsOf("u1")
	if len(results) != 1 || results[0].Status != CommandSucceeded || results[0].Detail != nil {
		t.Fatalf("results = %+v", results)
	}
	if data := updateResultData(t, results[0]); data != (AgentUpdateData{Code: "updated", PreviousVersion: "1.2.3", TargetVersion: "1.3.0", InstalledVersion: "1.3.0"}) {
		t.Fatalf("data = %+v", data)
	}
	if updates.exists(updater.ResultFile) {
		t.Fatal("the reported result file was not removed")
	}
	tick(t, second)
	if second.update.inProgress || second.update.active {
		t.Fatalf("update state after the result = %+v", second.update)
	}

	// A third start neither reports nor launches anything for u1.
	before = len(server.requestLog())
	third := setup.startWithUpdates(t, updates.forVersion("1.3.0"), "1.3.0")
	tick(t, third)
	for _, path := range requestsSince(server, before) {
		if strings.HasPrefix(path, "/commands/") {
			t.Fatalf("third start sent %s", path)
		}
	}
	if updates.launchCount() != 1 || len(server.resultsOf("u1")) != 1 {
		t.Fatalf("%d launches, results %+v", updates.launchCount(), server.resultsOf("u1"))
	}
}

func TestUpdateAgentReportsAFailedHelperResult(t *testing.T) {
	setup := newCommandsSetup(t)
	updates := newFakeUpdates(t, "1.2.3")
	daemon := setup.startWithUpdates(t, updates.controller, "1.2.3")
	tick(t, daemon)
	setup.server.addCommand("u2", CommandUpdateAgent, `{"version":"1.3.0"}`)
	heartbeatNow(daemon)
	tick(t, daemon)

	// The result survives a lost POST: it is retried, and the file goes
	// only once the backend accepted it.
	setup.server.fail("/commands/u2/result", failure{status: 503, body: `{"type":"temporarily_unavailable","status":503,"title":"Down"}`})
	updates.helperFinishes(t, updater.Result{CommandID: "u2", Code: updater.CodeReadinessFailedRolledBack, Detail: "rolled back to 1.2.3; cause: agent 1.3.0 did not report ready within 2m0s",
		PreviousVersion: "1.2.3", TargetVersion: "1.3.0", InstalledVersion: "1.2.3"})
	tick(t, daemon)
	if len(setup.server.resultsOf("u2")) != 0 || !updates.exists(updater.ResultFile) {
		t.Fatal("the result was dropped after a failed POST")
	}
	daemon.results.at = time.Now()
	tick(t, daemon)
	results := setup.server.resultsOf("u2")
	if len(results) != 1 || results[0].Status != CommandFailed || results[0].Detail == nil || !strings.HasPrefix(*results[0].Detail, "rolled back to 1.2.3") {
		t.Fatalf("results = %+v", results)
	}
	if data := updateResultData(t, results[0]); data.Code != updater.CodeReadinessFailedRolledBack || data.InstalledVersion != "1.2.3" {
		t.Fatalf("data = %+v", data)
	}
	if updates.exists(updater.ResultFile) {
		t.Fatal("result file kept after the report")
	}
}

func TestUpdateAgentValidatesBeforeHandingOver(t *testing.T) {
	for _, test := range []struct {
		name, args string
		capable    updater.Capability
		launchErr  error
		status     string
		code       string
		detail     string
	}{
		{"two components", `{"version":"1.3"}`, updater.Capability{Available: true}, nil, CommandFailed, "", "invalid args: version must be a stable semantic version (X.Y.Z)"},
		{"shell fragment", `{"version":"1.3.0; rm -rf /"}`, updater.Capability{Available: true}, nil, CommandFailed, "", "invalid args: version must be a stable semantic version (X.Y.Z)"},
		{"pre-release", `{"version":"1.3.0-rc.1"}`, updater.Capability{Available: true}, nil, CommandFailed, "", "invalid args: version must be a stable semantic version (X.Y.Z)"},
		{"package name", `{"version":"1.3.0","package":"curl"}`, updater.Capability{Available: true}, nil, CommandFailed, "", `invalid args: json: unknown field "package"`},
		{"not a string", `{"version":130}`, updater.Capability{Available: true}, nil, CommandFailed, "", "invalid args: json: cannot unmarshal number"},
		{"tarball install", `{"version":"1.3.0"}`, updater.Capability{Reason: "installed from a tarball, not the serverchief package"}, nil, CommandFailed, updater.CodeUnsupportedInstall, "installed from a tarball, not the serverchief package"},
		{"already running", `{"version":"1.2.3"}`, updater.Capability{Available: true}, nil, CommandSucceeded, updater.CodeUpdated, "already running 1.2.3"},
		{"launch fails", `{"version":"1.3.0"}`, updater.Capability{Available: true}, errors.New("systemd-run exited with status 1: boom"), CommandFailed, updater.CodeInstallFailed, "start the update helper: systemd-run exited with status 1: boom"},
	} {
		t.Run(test.name, func(t *testing.T) {
			setup := newCommandsSetup(t)
			updates := newFakeUpdates(t, "1.2.3")
			updates.capable, updates.launchErr = test.capable, test.launchErr
			daemon := setup.startWithUpdates(t, updates.controller, "1.2.3")
			tick(t, daemon)
			setup.server.addCommand("u3", CommandUpdateAgent, test.args)
			heartbeatNow(daemon)
			tick(t, daemon)
			results := setup.server.resultsOf("u3")
			if len(results) != 1 || results[0].Status != test.status || results[0].Detail == nil || !strings.HasPrefix(*results[0].Detail, test.detail) {
				t.Fatalf("results = %+v (detail %q)", results, deref(results))
			}
			if test.code == "" {
				if results[0].Data != nil {
					t.Fatalf("data for invalid args = %s", results[0].Data)
				}
			} else if data := updateResultData(t, results[0]); data.Code != test.code || data.PreviousVersion != "1.2.3" || data.InstalledVersion != "1.2.3" {
				t.Fatalf("data = %+v", data)
			}
			if updates.exists(updater.RequestFile) {
				t.Fatal("a request file was left behind")
			}
			wantLaunches := 0
			if test.launchErr != nil {
				wantLaunches = 1
			}
			if updates.launchCount() != wantLaunches {
				t.Fatalf("%d launches", updates.launchCount())
			}
		})
	}
}

func deref(results []CommandResult) string {
	if len(results) == 0 || results[0].Detail == nil {
		return ""
	}
	return *results[0].Detail
}

func TestUpdateAgentRefusesASecondConcurrentUpdate(t *testing.T) {
	setup := newCommandsSetup(t)
	updates := newFakeUpdates(t, "1.2.3")
	daemon := setup.startWithUpdates(t, updates.controller, "1.2.3")
	tick(t, daemon)
	setup.server.addCommand("u4", CommandUpdateAgent, `{"version":"1.3.0"}`)
	setup.server.addCommand("u5", CommandUpdateAgent, `{"version":"1.4.0"}`)
	heartbeatNow(daemon)
	tick(t, daemon)
	results := setup.server.resultsOf("u5")
	if len(results) != 1 || results[0].Status != CommandFailed || deref(results) != "another agent update (u4) is in progress" ||
		updateResultData(t, results[0]).Code != updater.CodePackageManagerBusy {
		t.Fatalf("results = %+v", results)
	}
	if request, _ := updates.store.ReadRequest(); request == nil || request.CommandID != "u4" || updates.launchCount() != 1 {
		t.Fatalf("request = %+v, %d launches", request, updates.launchCount())
	}
}

func TestReadinessWaitsForASuccessfulHeartbeat(t *testing.T) {
	setup := newCommandsSetup(t)
	updates := newFakeUpdates(t, "1.3.0")
	updates.running = true
	if err := updates.store.WriteRequest(updater.Request{CommandID: "u6", PreviousVersion: "1.2.3", TargetVersion: "1.3.0"}); err != nil {
		t.Fatal(err)
	}
	setup.server.fail("/heartbeat", failure{status: 500, body: `{"type":"server_error","status":500,"title":"Oops"}`})
	daemon := setup.startWithUpdates(t, updates.controller, "1.3.0")
	tick(t, daemon)
	if updates.exists(updater.ReadyFile) {
		t.Fatal("ready after a failed heartbeat")
	}
	heartbeatNow(daemon)
	tick(t, daemon)
	if readiness, _ := updates.store.ReadReadiness(); readiness == nil || readiness.CommandID != "u6" || readiness.Version != "1.3.0" {
		t.Fatalf("readiness = %+v", readiness)
	}
	if updates.launchCount() != 0 {
		t.Fatal("launched a helper although one runs")
	}
}

func TestStartRelaunchesAHelperForAPendingRequestOnce(t *testing.T) {
	setup := newCommandsSetup(t)
	updates := newFakeUpdates(t, "1.2.3")
	if err := updates.store.WriteRequest(updater.Request{CommandID: "u7", PreviousVersion: "1.2.3", TargetVersion: "1.3.0"}); err != nil {
		t.Fatal(err)
	}
	journal, _ := OpenCommandJournal(setup.journalPath)
	if err := journal.put(commandRecord{ID: "u7", Kind: CommandUpdateAgent, Args: json.RawMessage(`{"version":"1.3.0"}`), State: commandHandedOff, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	daemon := setup.startWithUpdates(t, updates.controller, "1.2.3")
	tick(t, daemon)
	tick(t, daemon)
	if updates.launchCount() != 1 {
		t.Fatalf("%d launches", updates.launchCount())
	}
	// The interrupted command is neither reported "interrupted" nor run
	// again while its helper works.
	if results := setup.server.resultsOf("u7"); len(results) != 0 {
		t.Fatalf("results = %+v", results)
	}
}

func TestHandedOffCommandWithoutRequestOrResultFails(t *testing.T) {
	setup := newCommandsSetup(t)
	updates := newFakeUpdates(t, "1.2.3")
	journal, _ := OpenCommandJournal(setup.journalPath)
	// The agent crashed after journaling the hand-over, before it wrote
	// the request.
	if err := journal.put(commandRecord{ID: "u8", Kind: CommandUpdateAgent, Args: json.RawMessage(`{"version":"1.3.0"}`), State: commandHandedOff, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	setup.server.addCommand("u8", CommandUpdateAgent, `{"version":"1.3.0"}`)
	setup.server.mu.Lock()
	setup.server.commandStates["u8"] = "acknowledged"
	setup.server.mu.Unlock()
	daemon := setup.startWithUpdates(t, updates.controller, "1.2.3")
	tick(t, daemon)
	results := setup.server.resultsOf("u8")
	if len(results) != 1 || results[0].Status != CommandFailed || deref(results) != "the update helper stopped without a result" {
		t.Fatalf("results = %+v", results)
	}
	if data := updateResultData(t, results[0]); data != (AgentUpdateData{Code: updater.CodeInstallFailed, PreviousVersion: "1.2.3", TargetVersion: "1.3.0", InstalledVersion: "1.2.3"}) {
		t.Fatalf("data = %+v", data)
	}
	if updates.launchCount() != 0 {
		t.Fatal("launched a helper without a request")
	}
}

func TestResultFromBeforeAStartIsReportedEvenWithoutAJournalRecord(t *testing.T) {
	setup := newCommandsSetup(t)
	updates := newFakeUpdates(t, "1.2.3")
	updates.helperFinishes(t, updater.Result{CommandID: "u9", Code: updater.CodeTargetUnavailable, Detail: "the Server Chief repository does not offer 9.9.9",
		PreviousVersion: "1.2.3", TargetVersion: "9.9.9", InstalledVersion: "1.2.3"})
	daemon := setup.startWithUpdates(t, updates.controller, "1.2.3")
	// The backend knows u9 (it was acknowledged before the journal was lost).
	setup.server.addCommand("u9", CommandUpdateAgent, `{"version":"9.9.9"}`)
	setup.server.mu.Lock()
	setup.server.commandStates["u9"] = "acknowledged"
	setup.server.mu.Unlock()
	tick(t, daemon)
	results := setup.server.resultsOf("u9")
	if len(results) != 1 || results[0].Status != CommandFailed || updateResultData(t, results[0]).Code != updater.CodeTargetUnavailable {
		t.Fatalf("results = %+v", results)
	}
	if updates.exists(updater.ResultFile) {
		t.Fatal("result file kept")
	}
}

func TestHeartbeatReportsUpdatesUnavailableWithoutAController(t *testing.T) {
	setup := newCommandsSetup(t)
	daemon := setup.start(t)
	tick(t, daemon)
	heartbeat := setup.server.heartbeatLog()[0]
	encoded, _ := json.Marshal(heartbeat.Capabilities)
	if string(encoded) != `{"agent_update":{"available":false,"reason":"agent updates are not supported by this build"},"reboot":{"available":false,"reason":"reboots are not supported by this build"},"disabled":[]}` {
		t.Fatalf("capabilities = %s", encoded)
	}
}
