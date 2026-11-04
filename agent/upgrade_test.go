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

	"go.chief.tools/serverchief-agent/optout"
	"go.chief.tools/serverchief-agent/updater"
	"go.chief.tools/serverchief-agent/upgrader"
)

// fakeUpgrades is a real upgrader.Controller on a temporary directory with
// a scripted capability and launch. The tests play the helper by writing
// its result files.
type fakeUpgrades struct {
	mu         sync.Mutex
	controller *upgrader.Controller
	capable    upgrader.Capability
	launches   int
	launchErr  error
	running    bool
}

func newFakeUpgrades(t *testing.T) *fakeUpgrades {
	t.Helper()
	fake := &fakeUpgrades{capable: upgrader.Capability{Available: true}}
	controller := upgrader.NewController(t.TempDir(), -1)
	controller.Detect = func(context.Context) upgrader.Capability {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.capable
	}
	controller.Launch = func(context.Context) error {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.launches++
		return fake.launchErr
	}
	controller.HelperRunning = func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.running
	}
	fake.controller = controller
	return fake
}

func (fake *fakeUpgrades) launchCount() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.launches
}

func (fake *fakeUpgrades) store() updater.Store { return fake.controller.Files.Store }

func (fake *fakeUpgrades) request(t *testing.T) *upgrader.Request {
	t.Helper()
	request, err := fake.controller.PendingRequest()
	if err != nil {
		t.Fatal(err)
	}
	return request
}

// finish plays the helper: output, result, then the request goes.
func (fake *fakeUpgrades) finish(t *testing.T, result upgrader.Result, output string) {
	t.Helper()
	if err := fake.store().WriteText(upgrader.OutputFile, output); err != nil {
		t.Fatal(err)
	}
	if err := fake.store().WriteJSON(upgrader.ResultFile, result); err != nil {
		t.Fatal(err)
	}
	if err := fake.store().Remove(upgrader.RequestFile); err != nil {
		t.Fatal(err)
	}
}

func (fake *fakeUpgrades) exists(name string) bool {
	_, err := os.Stat(filepath.Join(fake.store().Directory, name))
	return err == nil
}

type upgradeSetup struct {
	*rebootSetup
	upgrades *fakeUpgrades
}

func newUpgradeSetup(t *testing.T) *upgradeSetup {
	t.Helper()
	return &upgradeSetup{rebootSetup: newRebootSetup(t), upgrades: newFakeUpgrades(t)}
}

func (setup *upgradeSetup) start(t *testing.T) *daemon {
	t.Helper()
	daemon := setup.rebootSetup.start(t)
	daemon.options.Upgrades = setup.upgrades.controller
	daemon.options.UpdatePoll = time.Second
	return daemon
}

func (setup *upgradeSetup) command(t *testing.T, daemon *daemon, id, args string) {
	t.Helper()
	setup.server.addCommand(id, CommandUpgradePackages, args)
	daemon.heartbeatAt = setup.clock.Now()
	tick(t, daemon)
}

func upgradeResultData(t *testing.T, result CommandResult) UpgradePackagesData {
	t.Helper()
	var data UpgradePackagesData
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatalf("result data %s: %v", result.Data, err)
	}
	return data
}

const securityArgs = `{"scope":"security","restart_services":true,"agent_version":"0.4.1"}`

func TestUpgradeResultFixtureRoundTrips(t *testing.T) {
	fixture := readFixture(t, "upgrade-packages-result-request.json")
	var result CommandResult
	if err := json.Unmarshal(fixture, &result); err != nil {
		t.Fatal(err)
	}
	data := upgradeResultData(t, result)
	if data.Code != upgrader.CodeUpgraded || data.Scope != upgrader.ScopeSecurity || *data.Upgraded != 3 || *data.HeldBack != 0 || !*data.RebootRequired ||
		!reflect.DeepEqual(data.ServicesRestarted, []string{"nginx.service", "php8.3-fpm.service"}) ||
		data.AgentUpdate == nil || *data.AgentUpdate != (upgrader.AgentUpdate{Code: "updated", PreviousVersion: "0.4.0", InstalledVersion: "0.4.1"}) {
		t.Fatalf("data = %+v", data)
	}
	if encoded, _ := json.Marshal(data); !sameJSON(t, encoded, result.Data) {
		t.Fatalf("UpgradePackagesData does not round-trip the fixture:\n%s", encoded)
	}
	if encoded, _ := json.Marshal(result); !sameJSON(t, encoded, fixture) {
		t.Fatalf("upgrade_packages CommandResult does not round-trip the fixture:\n%s", encoded)
	}
	// Early refusals carry every key, null where unknown.
	if encoded := string(upgradeData(upgrader.CodeDisabled, upgrader.ScopeAll)); encoded != `{"code":"disabled","scope":"all","upgraded":null,"held_back":null,"reboot_required":null,"services_restarted":null,"agent_update":null}` {
		t.Fatalf("refusal data = %s", encoded)
	}
}

func TestHeartbeatReportsUpgradePackages(t *testing.T) {
	setup := newUpgradeSetup(t)
	daemon := setup.start(t)
	tick(t, daemon)
	capabilities := setup.server.heartbeatLog()[0].Capabilities
	if capabilities.UpgradePackages == nil || !capabilities.UpgradePackages.Available || capabilities.UpgradePackages.Reason != nil {
		t.Fatalf("capabilities = %+v", capabilities)
	}

	setup.upgrades.capable = upgrader.Capability{Reason: "the root helper does not answer"}
	daemon.heartbeatAt = setup.clock.Now()
	tick(t, daemon)
	heartbeats := setup.server.heartbeatLog()
	encoded, _ := json.Marshal(heartbeats[len(heartbeats)-1].Capabilities)
	if !strings.Contains(string(encoded), `"upgrade_packages":{"available":false,"reason":"the root helper does not answer"}`) {
		t.Fatalf("capabilities = %s", encoded)
	}

	setup.disabled = []string{optout.UpgradePackages}
	daemon.heartbeatAt = setup.clock.Now()
	tick(t, daemon)
	heartbeats = setup.server.heartbeatLog()
	encoded, _ = json.Marshal(heartbeats[len(heartbeats)-1].Capabilities)
	if strings.Contains(string(encoded), `"upgrade_packages":{`) || !strings.Contains(string(encoded), `"disabled":["upgrade_packages"]`) {
		t.Fatalf("capabilities = %s", encoded)
	}
}

func TestUpgradeArgumentsAreValidatedStrictly(t *testing.T) {
	setup := newUpgradeSetup(t)
	daemon := setup.start(t)
	tick(t, daemon)
	for index, args := range []string{
		`{}`,
		`{"scope":"everything","restart_services":true}`,
		`{"scope":"all"}`,
		`{"scope":"all","restart_services":"yes"}`,
		`{"scope":"all","restart_services":true,"agent_version":"latest"}`,
		`{"scope":"all","restart_services":true,"packages":["openssl"]}`,
		`{"scope":"all","restart_services":true,"not_after":"in an hour"}`,
		`{"scope":"all","restart_services":true,"not_after":1767225600}`,
	} {
		id := "bad" + string(rune('a'+index))
		setup.command(t, daemon, id, args)
		results := setup.server.resultsOf(id)
		if len(results) != 1 || results[0].Status != CommandFailed || results[0].Detail == nil || !strings.HasPrefix(*results[0].Detail, "invalid args: ") || results[0].Data != nil {
			t.Fatalf("%s: results = %+v", args, results)
		}
	}
	if setup.upgrades.launchCount() != 0 {
		t.Fatal("launched an invalid upgrade")
	}
}

func TestUpgradeRefusalsAreReportedWithTheirCodes(t *testing.T) {
	setup := newUpgradeSetup(t)
	daemon := setup.start(t)
	tick(t, daemon)

	setup.disabled = []string{optout.UpgradePackages}
	setup.command(t, daemon, "u1", securityArgs)
	setup.disabled = nil
	setup.upgrades.capable = upgrader.Capability{Reason: "systemd-run is not installed"}
	setup.command(t, daemon, "u2", `{"scope":"all","restart_services":false,"agent_version":null}`)

	for id, want := range map[string][2]string{
		"u1": {upgrader.CodeDisabled, "serverchief enable upgrade_packages"},
		"u2": {upgrader.CodeUnsupported, "systemd-run is not installed"},
	} {
		results := setup.server.resultsOf(id)
		if len(results) != 1 || results[0].Status != CommandFailed || results[0].Detail == nil || !strings.Contains(*results[0].Detail, want[1]) {
			t.Fatalf("%s: results = %+v", id, results)
		}
		if data := upgradeResultData(t, results[0]); data.Code != want[0] || data.Scope == "" || data.Upgraded != nil {
			t.Fatalf("%s: data = %+v", id, data)
		}
	}
	if setup.upgrades.launchCount() != 0 {
		t.Fatal("launched a refused upgrade")
	}
}

func TestUpgradeIsNotStartedAfterItsDeadline(t *testing.T) {
	setup := newUpgradeSetup(t)
	setup.packages = &fakePackages{lock: "/var/lib/dpkg/lock-frontend"}
	daemon := setup.start(t)
	tick(t, daemon)

	past := setup.clock.Now().Add(-time.Second).UTC().Format(time.RFC3339)
	setup.command(t, daemon, "u1", `{"scope":"security","restart_services":true,"agent_version":null,"not_after":"`+past+`"}`)
	if results := setup.server.resultsOf("u1"); len(results) != 1 || results[0].Status != CommandFailed || upgradeResultData(t, results[0]).Code != upgrader.CodeTooLate {
		t.Fatalf("u1 results = %+v", results)
	}

	// Waiting for the package manager stops at the deadline, not after an hour.
	deadline := setup.clock.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339)
	setup.command(t, daemon, "u2", `{"scope":"all","restart_services":false,"not_after":"`+deadline+`"}`)
	setup.clock.advance(9 * time.Minute)
	tick(t, daemon)
	if len(setup.server.resultsOf("u2")) != 0 {
		t.Fatal("gave up before the deadline")
	}
	setup.clock.advance(time.Minute)
	tick(t, daemon)
	results := setup.server.resultsOf("u2")
	if len(results) != 1 || upgradeResultData(t, results[0]).Code != upgrader.CodeTooLate || upgradeResultData(t, results[0]).Scope != "all" || daemon.upgrade.waiting != nil {
		t.Fatalf("u2 results = %+v", results)
	}

	// Before the deadline the upgrade is handed over as usual.
	setup.packages.mu.Lock()
	setup.packages.lock = ""
	setup.packages.mu.Unlock()
	future := setup.clock.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	setup.command(t, daemon, "u3", `{"scope":"all","restart_services":true,"not_after":"`+future+`"}`)
	if setup.upgrades.launchCount() != 1 {
		t.Fatalf("launches = %d", setup.upgrades.launchCount())
	}
}

func TestUpgradeIsHandedOffAndReportedByPolling(t *testing.T) {
	setup := newUpgradeSetup(t)
	daemon := setup.start(t)
	tick(t, daemon)

	setup.command(t, daemon, "u1", securityArgs)
	request := setup.upgrades.request(t)
	if setup.upgrades.launchCount() != 1 || request == nil ||
		*request != (upgrader.Request{CommandID: "u1", Scope: "security", RestartServices: true, AgentVersion: "0.4.1", RequestedAt: request.RequestedAt}) {
		t.Fatalf("launches = %d, request %+v", setup.upgrades.launchCount(), request)
	}
	if record := daemon.commands.get("u1"); record == nil || record.State != commandHandedOff {
		t.Fatalf("record = %+v", record)
	}

	// A second upgrade while one runs is refused, and the first is never
	// executed twice when the backend lists it again.
	setup.command(t, daemon, "u2", securityArgs)
	if results := setup.server.resultsOf("u2"); len(results) != 1 || upgradeResultData(t, results[0]).Code != upgrader.CodeUpgradeFailed ||
		!strings.Contains(*results[0].Detail, "another package upgrade (u1)") {
		t.Fatalf("second upgrade = %+v", results)
	}
	setup.command(t, daemon, "u1", securityArgs)
	if setup.upgrades.launchCount() != 1 || len(setup.server.resultsOf("u1")) != 0 {
		t.Fatalf("launches = %d, results %+v", setup.upgrades.launchCount(), setup.server.resultsOf("u1"))
	}

	upgraded, held, reboot := 3, 0, true
	setup.upgrades.finish(t, upgrader.Result{CommandID: "u1", Status: upgrader.StatusSucceeded, FinishedAt: setup.clock.Now(), Data: upgrader.Data{
		Code: upgrader.CodeUpgraded, Scope: "security", Upgraded: &upgraded, HeldBack: &held, RebootRequired: &reboot, ServicesRestarted: []string{"nginx.service"},
	}}, "$ apt-get install -y --only-upgrade openssl\nDone.\n")
	setup.clock.advance(2 * time.Second)
	tick(t, daemon)

	results := setup.server.resultsOf("u1")
	if len(results) != 1 || results[0].Status != CommandSucceeded || results[0].Output == nil || *results[0].Output != "$ apt-get install -y --only-upgrade openssl\nDone.\n" {
		t.Fatalf("results = %+v", results)
	}
	if data := upgradeResultData(t, results[0]); data.Code != upgrader.CodeUpgraded || *data.Upgraded != 3 || !reflect.DeepEqual(data.ServicesRestarted, []string{"nginx.service"}) {
		t.Fatalf("data = %+v", data)
	}
	if setup.upgrades.exists(upgrader.ResultFile) || setup.upgrades.exists(upgrader.OutputFile) {
		t.Fatal("result files kept")
	}
	// Packages and updates are collected again right away.
	if at, scheduled := daemon.next["packages"]; !scheduled || at.After(setup.clock.Now()) {
		t.Fatalf("packages next at %v (scheduled %v)", at, scheduled)
	}
}

func TestUpgradeWaitsForThePackageManager(t *testing.T) {
	setup := newUpgradeSetup(t)
	setup.packages = &fakePackages{lock: "/var/lib/dpkg/lock-frontend"}
	daemon := setup.start(t)
	tick(t, daemon)

	setup.command(t, daemon, "u1", securityArgs)
	setup.clock.advance(30 * time.Minute)
	tick(t, daemon)
	if setup.upgrades.launchCount() != 0 || len(setup.server.resultsOf("u1")) != 0 {
		t.Fatal("upgraded while the package manager ran")
	}
	setup.packages.mu.Lock()
	setup.packages.lock = ""
	setup.packages.mu.Unlock()
	setup.clock.advance(time.Minute)
	tick(t, daemon)
	if setup.upgrades.launchCount() != 1 {
		t.Fatalf("launches = %d", setup.upgrades.launchCount())
	}

	setup.packages.mu.Lock()
	setup.packages.lock = "/var/lib/dpkg/lock-frontend"
	setup.packages.mu.Unlock()
	setup.command(t, daemon, "u2", securityArgs)
	if results := setup.server.resultsOf("u2"); len(results) != 1 || upgradeResultData(t, results[0]).Code != upgrader.CodeUpgradeFailed {
		t.Fatalf("second while handed off = %+v", results)
	}
	setup.upgrades.finish(t, upgrader.Result{CommandID: "u1", Status: upgrader.StatusSucceeded, Data: upgrader.Data{Code: upgrader.CodeNothingToUpgrade, Scope: "security"}}, "")
	setup.clock.advance(2 * time.Second)
	tick(t, daemon)
	setup.command(t, daemon, "u3", securityArgs)
	setup.clock.advance(time.Hour)
	tick(t, daemon)
	if results := setup.server.resultsOf("u3"); len(results) != 1 || upgradeResultData(t, results[0]).Code != upgrader.CodePackageManagerBusy {
		t.Fatalf("busy = %+v", results)
	}
}

func TestUpgradeResultIsReportedAfterARestartAndAMissingHelperRelaunched(t *testing.T) {
	setup := newUpgradeSetup(t)
	first := setup.start(t)
	tick(t, first)
	setup.command(t, first, "u1", securityArgs)

	// The daemon restarts while the helper runs: a pending request without
	// a running helper is relaunched once.
	second := setup.start(t)
	tick(t, second)
	if setup.upgrades.launchCount() != 2 || len(setup.server.resultsOf("u1")) != 0 {
		t.Fatalf("launches = %d, results %+v", setup.upgrades.launchCount(), setup.server.resultsOf("u1"))
	}

	setup.upgrades.finish(t, upgrader.Result{CommandID: "u1", Status: upgrader.StatusFailed, Detail: "apt-get exited with status 100", Data: upgrader.Data{Code: upgrader.CodeUpgradeFailed, Scope: "security"}}, "E: broken\n")
	third := setup.start(t)
	tick(t, third)
	results := setup.server.resultsOf("u1")
	if len(results) != 1 || results[0].Status != CommandFailed || *results[0].Detail != "apt-get exited with status 100" || *results[0].Output != "E: broken\n" {
		t.Fatalf("results = %+v", results)
	}
}

func TestUpgradeWithoutRequestOrResultIsReportedFailed(t *testing.T) {
	setup := newUpgradeSetup(t)
	setup.upgrades.launchErr = errors.New("systemd-run exited with status 1")
	daemon := setup.start(t)
	tick(t, daemon)
	setup.command(t, daemon, "u1", securityArgs)
	if results := setup.server.resultsOf("u1"); len(results) != 1 || upgradeResultData(t, results[0]).Code != upgrader.CodeUpgradeFailed ||
		!strings.Contains(*results[0].Detail, "start the package upgrade helper: systemd-run exited") || setup.upgrades.request(t) != nil {
		t.Fatalf("launch failure = %+v", results)
	}

	// A handed-off command whose helper vanished with its request.
	setup.upgrades.launchErr = nil
	setup.command(t, daemon, "u2", securityArgs)
	if err := setup.upgrades.store().Remove(upgrader.RequestFile); err != nil {
		t.Fatal(err)
	}
	restarted := setup.start(t)
	tick(t, restarted)
	if results := setup.server.resultsOf("u2"); len(results) != 1 || !strings.Contains(*results[0].Detail, "stopped without a result") {
		t.Fatalf("vanished = %+v", results)
	}
}

func TestRebootWaitsForAPackageUpgrade(t *testing.T) {
	setup := newUpgradeSetup(t)
	daemon := setup.start(t)
	tick(t, daemon)
	setup.command(t, daemon, "u1", securityArgs)

	setup.rebootSetup.command(t, daemon, "r1", `{}`)
	if setup.reboots.count() != 0 {
		t.Fatal("rebooted during a package upgrade")
	}
	setup.upgrades.finish(t, upgrader.Result{CommandID: "u1", Status: upgrader.StatusSucceeded, Data: upgrader.Data{Code: upgrader.CodeUpgraded, Scope: "security"}}, "")
	setup.clock.advance(time.Minute)
	tick(t, daemon)
	if setup.reboots.count() != 1 {
		t.Fatalf("reboots = %d after the upgrade", setup.reboots.count())
	}
}

func TestAgentUpdatePollLeavesTheNestedResultToTheUpgrade(t *testing.T) {
	setup := newUpgradeSetup(t)
	updates := newFakeUpdates(t, "0.4.0")
	nested := updater.NestedCommandID("u1")
	if err := updates.controller.Store.WriteRequest(updater.Request{CommandID: nested, PreviousVersion: "0.4.0", TargetVersion: "0.4.1"}); err != nil {
		t.Fatal(err)
	}
	daemon := setup.start(t)
	daemon.options.Updates = updates.controller
	daemon.initUpdates(setup.clock.Now())
	if daemon.update.readyFor != nested {
		t.Fatalf("ready for %q", daemon.update.readyFor)
	}
	if err := updates.controller.Store.WriteJSON(updater.ResultFile, updater.Result{CommandID: nested, Code: updater.CodeUpdated, PreviousVersion: "0.4.0", TargetVersion: "0.4.1", InstalledVersion: "0.4.1"}); err != nil {
		t.Fatal(err)
	}
	_ = updates.controller.Store.RemoveRequest()
	tick(t, daemon)
	if results := setup.server.resultsOf(nested); len(results) != 0 || daemon.commands.get(nested) != nil {
		t.Fatalf("nested result reported: %+v", results)
	}
	if result, _ := updates.controller.ReadResult(); result == nil {
		t.Fatal("nested result removed by the daemon")
	}
}

// appendOutput plays the helper writing its live output.
func (fake *fakeUpgrades) appendOutput(t *testing.T, data string) {
	t.Helper()
	file, err := fake.store().OpenAppend(upgrader.OutputFile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(data); err != nil {
		t.Fatal(err)
	}
}

// handOffUpgrade starts u1 and returns the daemon with it handed off.
func handOffUpgrade(t *testing.T, setup *upgradeSetup) *daemon {
	t.Helper()
	daemon := setup.start(t)
	tick(t, daemon)
	setup.command(t, daemon, "u1", securityArgs)
	if record := daemon.commands.get("u1"); record == nil || record.State != commandHandedOff {
		t.Fatalf("record = %+v", record)
	}
	return daemon
}

func TestCommandOutputFixtureRoundTrips(t *testing.T) {
	fixture := readFixture(t, "command-output-request.json")
	var chunk CommandOutputChunk
	if err := json.Unmarshal(fixture, &chunk); err != nil {
		t.Fatal(err)
	}
	if chunk.Offset != 1843 || chunk.Length != int64(len(chunk.Output)) || !strings.HasPrefix(chunk.Output, "# Installing 3 security updates\n") {
		t.Fatalf("chunk = %+v", chunk)
	}
	if encoded, _ := json.Marshal(chunk); !sameJSON(t, encoded, fixture) {
		t.Fatalf("CommandOutputChunk does not round-trip the fixture:\n%s", encoded)
	}
}

func TestOutputChunksEndAtCharacterBoundaries(t *testing.T) {
	for name, test := range map[string]struct {
		data     string
		complete bool
		length   int64
		output   string
		ok       bool
	}{
		"plain":                    {"abc", false, 3, "abc", true},
		"incomplete character":     {"abc\xc3", false, 3, "abc", true},
		"only an incomplete start": {"\xe2\x82", false, 0, "", false},
		"complete at the end":      {"abc\xc3", true, 4, "abc�", true},
		"invalid byte":             {"a\xffb", false, 3, "a�b", true},
		"multibyte":                {"é€", false, 5, "é€", true},
	} {
		chunk, ok := outputChunk(10, []byte(test.data), test.complete)
		if ok != test.ok || ok && (chunk.Offset != 10 || chunk.Length != test.length || chunk.Output != test.output) {
			t.Errorf("%s: chunk = %+v, ok %v", name, chunk, ok)
		}
	}
	// A replacement character takes three bytes for each invalid byte
	// between valid ones: the text stays within the cap by sending fewer
	// raw bytes.
	invalid := []byte(strings.Repeat("\xffa", MaximumOutputChunk/2))
	chunk, ok := outputChunk(0, invalid, false)
	if !ok || len(chunk.Output) > MaximumOutputChunk || chunk.Length == 0 || chunk.Length >= int64(len(invalid)) {
		t.Fatalf("length %d, text %d bytes", chunk.Length, len(chunk.Output))
	}
}

func TestUpgradeOutputIsStreamedWhileItRuns(t *testing.T) {
	setup := newUpgradeSetup(t)
	daemon := handOffUpgrade(t, setup)

	setup.upgrades.appendOutput(t, "# Refreshing the package lists\n$ apt-get update -qq\ncaf\xc3")
	setup.clock.advance(upgradeStreamCheck)
	tick(t, daemon)
	chunks := setup.server.outputOf("u1")
	if len(chunks) != 1 || chunks[0].Offset != 0 || chunks[0].Output != "# Refreshing the package lists\n$ apt-get update -qq\ncaf" || chunks[0].Length != int64(len(chunks[0].Output)) {
		t.Fatalf("chunks = %+v", chunks)
	}

	// Nothing new, nothing sent.
	for range 4 {
		setup.clock.advance(upgradeStreamCheck)
		tick(t, daemon)
	}
	if chunks := setup.server.outputOf("u1"); len(chunks) != 1 {
		t.Fatalf("sent without new output: %+v", chunks)
	}

	// New output goes out on the next check, not seconds later; the rest
	// of an incomplete character completes it.
	setup.upgrades.appendOutput(t, "\xa9\n")
	if at := daemon.wakeAt(); at.After(setup.clock.Now().Add(upgradeStreamCheck)) {
		t.Fatalf("wakes at %v", at)
	}
	setup.clock.advance(upgradeStreamCheck)
	tick(t, daemon)
	chunks = setup.server.outputOf("u1")
	if len(chunks) != 2 || chunks[1].Offset != chunks[0].Length || chunks[1].Output != "é\n" || chunks[1].Length != 3 {
		t.Fatalf("chunks = %+v", chunks)
	}

	// A backlog goes in chunks of at most 64 KiB, one after the other
	// without waiting for the next check.
	setup.upgrades.appendOutput(t, strings.Repeat("x", MaximumOutputChunk+10))
	setup.clock.advance(upgradeStreamCheck)
	tick(t, daemon)
	chunks = setup.server.outputOf("u1")
	if len(chunks) != 3 || chunks[2].Length != MaximumOutputChunk {
		t.Fatalf("third chunk length = %d", chunks[len(chunks)-1].Length)
	}
	if at := daemon.wakeAt(); at.After(setup.clock.Now()) {
		t.Fatalf("the rest waits until %v", at)
	}
	tick(t, daemon)
	if chunks = setup.server.outputOf("u1"); len(chunks) != 4 || chunks[3].Length != 10 {
		t.Fatalf("chunks = %d, last %+v", len(chunks), chunks[len(chunks)-1])
	}
}

func TestUpgradeOutputFollowsTheBackendsOffset(t *testing.T) {
	setup := newUpgradeSetup(t)
	daemon := handOffUpgrade(t, setup)
	setup.upgrades.appendOutput(t, "0123456789")
	setup.server.fail("/commands/u1/output", failure{status: 409, body: `{"type":"offset_mismatch","status":409,"title":"Offset mismatch","expected_offset":4}`})
	setup.clock.advance(upgradeStreamCheck)
	tick(t, daemon)
	setup.clock.advance(upgradeStreamCheck)
	tick(t, daemon)
	if chunks := setup.server.outputOf("u1"); len(chunks) != 1 || chunks[0].Offset != 4 || chunks[0].Output != "456789" {
		t.Fatalf("chunks = %+v", chunks)
	}

	// A restarted daemon continues where the backend is.
	setup.upgrades.appendOutput(t, "abc")
	restarted := setup.start(t)
	tick(t, restarted)
	setup.clock.advance(upgradeStreamCheck)
	tick(t, restarted)
	if chunks := setup.server.outputOf("u1"); len(chunks) != 2 || chunks[1].Offset != 10 || chunks[1].Output != "abc" {
		t.Fatalf("after a restart chunks = %+v", chunks)
	}
}

func TestUpgradeOutputStopsWhenTheBackendRejectsIt(t *testing.T) {
	setup := newUpgradeSetup(t)
	daemon := handOffUpgrade(t, setup)
	setup.upgrades.appendOutput(t, "first\n")
	setup.server.fail("/commands/u1/output", failure{status: 404, body: `{"type":"not_found","status":404,"title":"Not found"}`})
	setup.clock.advance(upgradeStreamCheck)
	tick(t, daemon)
	before := len(setup.server.requestLog())
	setup.upgrades.appendOutput(t, "second\n")
	for range 3 {
		setup.clock.advance(upgradeStreamCheck)
		tick(t, daemon)
	}
	if got := requestsSince(setup.server, before); slices.Contains(got, "/commands/u1/output") {
		t.Fatalf("streamed after a rejection: %q", got)
	}
}

func TestUpgradeOutputIsFlushedBeforeTheResult(t *testing.T) {
	setup := newUpgradeSetup(t)
	daemon := handOffUpgrade(t, setup)
	before := len(setup.server.requestLog())
	// The helper finished between two intervals, its output ending in an
	// incomplete character that never completes.
	setup.upgrades.finish(t, upgrader.Result{CommandID: "u1", Status: upgrader.StatusSucceeded, FinishedAt: setup.clock.Now(), Data: upgrader.Data{Code: upgrader.CodeNothingToUpgrade, Scope: "security"}},
		strings.Repeat("y", MaximumOutputChunk+5)+"\xc3")
	setup.clock.advance(time.Second)
	tick(t, daemon)

	got := requestsSince(setup.server, before)
	if want := []string{"/commands/u1/output", "/commands/u1/output", "/commands/u1/result"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %q", got)
	}
	chunks := setup.server.outputOf("u1")
	if last := chunks[len(chunks)-1]; !strings.HasSuffix(last.Output, "�") || last.Offset+last.Length != int64(MaximumOutputChunk+6) {
		t.Fatalf("last chunk = offset %d, length %d", last.Offset, last.Length)
	}
	if setup.upgrades.exists(upgrader.StreamFile) {
		t.Fatal("stream position kept")
	}
}
