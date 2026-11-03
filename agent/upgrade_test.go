package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
