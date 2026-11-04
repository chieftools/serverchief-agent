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
)

// fakeReboots scripts the capability, the root helper's reboot action and
// the boot ID.
type fakeReboots struct {
	mu        sync.Mutex
	bootID    string
	bootTime  time.Time
	capable   ActionCapability
	output    string
	rebootErr error
	reboots   int
	path      string
	// required is what the host says about needing a reboot, nil when it
	// cannot tell.
	required *bool
}

func newFakeReboots(t *testing.T) *fakeReboots {
	t.Helper()
	return &fakeReboots{
		bootID:   "11111111-1111-1111-1111-111111111111",
		bootTime: time.Date(2026, 10, 18, 5, 37, 22, 0, time.UTC),
		capable:  ActionCapability{Available: true},
		output:   "$ /sbin/shutdown -r +1 'Server Chief: rebooting in 1 minute, requested from the panel'\nShutdown scheduled for Thu 2026-10-22 14:13:03 UTC, use 'shutdown -c' to cancel.\n",
		path:     filepath.Join(t.TempDir(), "reboot-request.json"),
	}
}

func (fake *fakeReboots) control() *Reboots {
	return &Reboots{
		RequestPath: fake.path,
		Detect: func(context.Context) ActionCapability {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			return fake.capable
		},
		Reboot: func(context.Context) (string, error) {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			fake.reboots++
			return fake.output, fake.rebootErr
		},
		BootID: func() (string, error) {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			return fake.bootID, nil
		},
		BootTime: func() *time.Time {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			at := fake.bootTime
			return &at
		},
		Required: func(context.Context) *bool {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			return fake.required
		},
	}
}

// rebooted is the server coming back with a new boot.
func (fake *fakeReboots) rebooted(at time.Time) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.bootID = "22222222-2222-2222-2222-222222222222"
	fake.bootTime = at
}

func (fake *fakeReboots) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.reboots
}

func (fake *fakeReboots) requestExists() bool {
	_, err := os.Stat(fake.path)
	return err == nil
}

// rebootClock is a settable clock for the daemons of a test.
type rebootClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *rebootClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *rebootClock) advance(by time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(by)
}

type rebootSetup struct {
	*commandsSetup
	reboots  *fakeReboots
	clock    *rebootClock
	disabled []string
	packages *fakePackages
}

func newRebootSetup(t *testing.T) *rebootSetup {
	t.Helper()
	return &rebootSetup{
		commandsSetup: newCommandsSetup(t),
		reboots:       newFakeReboots(t),
		clock:         &rebootClock{now: time.Now()},
	}
}

// start starts a daemon, again after a restart or reboot.
func (setup *rebootSetup) start(t *testing.T) *daemon {
	t.Helper()
	options := setup.options(t, setup.server.client(), "system", "packages")
	journal, err := OpenCommandJournal(setup.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	options.Commands = journal
	options.Now = setup.clock.Now
	options.Reboots = setup.reboots.control()
	options.OptOut = func() optout.State { return optout.State{Disabled: append([]string{}, setup.disabled...)} }
	if setup.packages != nil {
		options.Packages = setup.packages
	}
	return newDaemon(options)
}

// command delivers a command and runs the ticks that execute it.
func (setup *rebootSetup) command(t *testing.T, daemon *daemon, id, args string) {
	t.Helper()
	setup.server.addCommand(id, CommandReboot, args)
	daemon.heartbeatAt = setup.clock.Now()
	tick(t, daemon)
}

func rebootResultData(t *testing.T, result CommandResult) RebootData {
	t.Helper()
	var data RebootData
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatalf("result data %s: %v", result.Data, err)
	}
	return data
}

func TestRebootResultFixtureRoundTrips(t *testing.T) {
	fixture := readFixture(t, "reboot-result-request.json")
	var result CommandResult
	if err := json.Unmarshal(fixture, &result); err != nil {
		t.Fatal(err)
	}
	data := rebootResultData(t, result)
	if data.Code != RebootCodeRebooted || data.PreviousBootTime == nil || !data.PreviousBootTime.Equal(time.Date(2026, 10, 18, 5, 37, 22, 0, time.UTC)) || data.BootTime == nil {
		t.Fatalf("data = %+v", data)
	}
	if result.Output == nil || !strings.HasPrefix(*result.Output, "$ /sbin/shutdown -r +1 ") || result.OutputTruncated == nil || *result.OutputTruncated {
		t.Fatalf("output = %v, truncated %v", result.Output, result.OutputTruncated)
	}
	if encoded, _ := json.Marshal(result); !sameJSON(t, encoded, fixture) {
		t.Fatalf("reboot CommandResult does not round-trip the fixture:\n%s", encoded)
	}
}

func TestHeartbeatReportsRebootAndTheOptOut(t *testing.T) {
	setup := newRebootSetup(t)
	setup.disabled = []string{optout.UpgradePackages}
	daemon := setup.start(t)
	tick(t, daemon)
	capabilities := setup.server.heartbeatLog()[0].Capabilities
	if capabilities.Reboot == nil || !capabilities.Reboot.Available || capabilities.Reboot.Reason != nil ||
		!reflect.DeepEqual(capabilities.Disabled, []string{"upgrade_packages"}) {
		t.Fatalf("capabilities = %+v", capabilities)
	}

	// The opt-out is read before every heartbeat; a turned-off reboot is
	// left out and listed.
	setup.disabled = []string{optout.Reboot, optout.UpgradePackages}
	daemon.heartbeatAt = setup.clock.Now()
	tick(t, daemon)
	heartbeats := setup.server.heartbeatLog()
	encoded, _ := json.Marshal(heartbeats[len(heartbeats)-1].Capabilities)
	if strings.Contains(string(encoded), `"reboot":`) || strings.Contains(string(encoded), `"upgrade_packages":`) || !strings.Contains(string(encoded), `"disabled":["reboot","upgrade_packages"]`) {
		t.Fatalf("capabilities = %s", encoded)
	}

	setup.disabled = nil
	setup.reboots.capable = ActionCapability{Reason: new("running in a docker container, which has no init system to reboot")}
	daemon.heartbeatAt = setup.clock.Now()
	tick(t, daemon)
	heartbeats = setup.server.heartbeatLog()
	encoded, _ = json.Marshal(heartbeats[len(heartbeats)-1].Capabilities)
	if !strings.Contains(string(encoded), `"reboot":{"available":false,"reason":"running in a docker container, which has no init system to reboot"},`) || !strings.Contains(string(encoded), `"disabled":[]`) {
		t.Fatalf("capabilities = %s", encoded)
	}
}

func TestRebootIsHandedOffOnceAndReportedAfterTheReboot(t *testing.T) {
	setup := newRebootSetup(t)
	updates := newFakeUpdates(t, "1.2.3")
	first := setup.start(t)
	first.options.Updates = updates.controller
	first.options.UpdatePoll = time.Nanosecond
	first.update.active = true
	tick(t, first)

	setup.command(t, first, "r1", `{}`)
	if setup.reboots.count() != 1 || !setup.reboots.requestExists() {
		t.Fatalf("reboots = %d, request %v", setup.reboots.count(), setup.reboots.requestExists())
	}
	if record := first.commands.get("r1"); record == nil || record.State != commandHandedOff {
		t.Fatalf("record = %+v", record)
	}
	if results := setup.server.resultsOf("r1"); len(results) != 0 {
		t.Fatalf("results before the reboot = %+v", results)
	}
	// The update poll leaves a handed-off reboot alone, and the command is
	// never executed twice when the backend lists it again.
	setup.clock.advance(time.Second)
	tick(t, first)
	setup.server.addCommand("r1", CommandReboot, `{}`)
	first.heartbeatAt = setup.clock.Now()
	tick(t, first)
	if setup.reboots.count() != 1 || first.commands.get("r1").State != commandHandedOff {
		t.Fatalf("reboots = %d, record %+v", setup.reboots.count(), first.commands.get("r1"))
	}

	setup.clock.advance(90 * time.Second)
	setup.reboots.rebooted(setup.clock.Now().Add(-30 * time.Second).UTC().Truncate(time.Second))
	second := setup.start(t)
	tick(t, second)
	results := setup.server.resultsOf("r1")
	if len(results) != 1 || results[0].Status != CommandSucceeded || results[0].Detail != nil {
		t.Fatalf("results = %+v", results)
	}
	data := rebootResultData(t, results[0])
	if data.Code != RebootCodeRebooted || !data.PreviousBootTime.Equal(time.Date(2026, 10, 18, 5, 37, 22, 0, time.UTC)) || data.BootTime == nil || !data.BootTime.After(*data.PreviousBootTime) {
		t.Fatalf("data = %+v", data)
	}
	if results[0].Output == nil || *results[0].Output != setup.reboots.output || results[0].OutputTruncated == nil || *results[0].OutputTruncated {
		t.Fatalf("output = %v", results[0].Output)
	}
	if setup.reboots.requestExists() || setup.reboots.count() != 1 {
		t.Fatal("request kept or rebooted twice")
	}
}

func TestRebootThatDoesNotHappenIsReportedAfterTenMinutes(t *testing.T) {
	setup := newRebootSetup(t)
	first := setup.start(t)
	tick(t, first)
	setup.command(t, first, "r1", `{}`)

	// A restart within the minute (or a cancelled shutdown) keeps the boot.
	setup.clock.advance(2 * time.Minute)
	second := setup.start(t)
	tick(t, second)
	if results := setup.server.resultsOf("r1"); len(results) != 0 {
		t.Fatalf("results while young = %+v", results)
	}
	if at := second.wakeAt(); at.After(setup.clock.Now().Add(8 * time.Minute)) {
		t.Fatalf("wakes at %v, not by the verification", at)
	}
	setup.clock.advance(8 * time.Minute)
	tick(t, second)
	results := setup.server.resultsOf("r1")
	if len(results) != 1 || results[0].Status != CommandFailed || rebootResultData(t, results[0]).Code != RebootCodeNotRebooted ||
		results[0].Detail == nil || !strings.Contains(*results[0].Detail, "did not reboot within 10 minutes") || results[0].Output == nil {
		t.Fatalf("results = %+v", results)
	}
	if setup.reboots.requestExists() {
		t.Fatal("request kept")
	}
}

func TestRebootRefusalsAreReportedWithTheirCodes(t *testing.T) {
	setup := newRebootSetup(t)
	daemon := setup.start(t)
	tick(t, daemon)

	setup.disabled = []string{optout.Reboot}
	setup.command(t, daemon, "r1", `{}`)
	setup.disabled = nil
	setup.reboots.capable = ActionCapability{Reason: new("the root helper does not answer")}
	setup.command(t, daemon, "r2", `{}`)
	setup.reboots.capable = ActionCapability{Available: true}
	setup.command(t, daemon, "r3", `{"delay":60}`)
	setup.command(t, daemon, "r4", `{"not_after":"tonight"}`)

	if setup.reboots.count() != 0 {
		t.Fatalf("rebooted %d times", setup.reboots.count())
	}
	for id, want := range map[string]struct{ code, detail string }{
		"r1": {RebootCodeDisabled, "rebooting is turned off on this server; `serverchief enable reboot` turns it on"},
		"r2": {RebootCodeUnsupported, "the root helper does not answer"},
	} {
		results := setup.server.resultsOf(id)
		if len(results) != 1 || results[0].Status != CommandFailed || rebootResultData(t, results[0]).Code != want.code || *results[0].Detail != want.detail {
			t.Fatalf("%s results = %+v", id, results)
		}
		if data := rebootResultData(t, results[0]); data.BootTime == nil || !data.BootTime.Equal(*data.PreviousBootTime) {
			t.Fatalf("%s data = %+v", id, data)
		}
	}
	for _, id := range []string{"r3", "r4"} {
		if results := setup.server.resultsOf(id); len(results) != 1 || results[0].Status != CommandFailed || !strings.HasPrefix(*results[0].Detail, "invalid args:") || results[0].Data != nil {
			t.Fatalf("%s results = %+v", id, results)
		}
	}
}

func TestRebootIsNotStartedAfterItsDeadline(t *testing.T) {
	setup := newRebootSetup(t)
	daemon := setup.start(t)
	tick(t, daemon)

	past := setup.clock.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	setup.command(t, daemon, "r1", `{"not_after":"`+past+`"}`)
	results := setup.server.resultsOf("r1")
	if len(results) != 1 || results[0].Status != CommandFailed || rebootResultData(t, results[0]).Code != RebootCodeTooLate ||
		!strings.Contains(*results[0].Detail, "not_after "+past) || setup.reboots.count() != 0 {
		t.Fatalf("results = %+v, reboots %d", results, setup.reboots.count())
	}

	future := setup.clock.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	setup.command(t, daemon, "r2", `{"not_after":"`+future+`"}`)
	if setup.reboots.count() != 1 {
		t.Fatalf("reboots = %d before the deadline", setup.reboots.count())
	}
}

func TestRebootWaitingForThePackageManagerStopsAtItsDeadline(t *testing.T) {
	setup := newRebootSetup(t)
	cache := time.Now()
	setup.packages = &fakePackages{token: "dpkg@1", cache: &cache, lock: "/var/lib/dpkg/lock-frontend (fcntl, pid 42)"}
	daemon := setup.start(t)
	tick(t, daemon)

	deadline := setup.clock.Now().Add(2 * time.Minute).UTC().Format(time.RFC3339)
	setup.command(t, daemon, "r1", `{"not_after":"`+deadline+`"}`)
	setup.clock.advance(rebootLockRetry)
	tick(t, daemon)
	if len(setup.server.resultsOf("r1")) != 0 {
		t.Fatal("gave up before the deadline")
	}
	for range 4 {
		setup.clock.advance(rebootLockRetry)
		tick(t, daemon)
	}
	results := setup.server.resultsOf("r1")
	if len(results) != 1 || rebootResultData(t, results[0]).Code != RebootCodeTooLate || setup.reboots.count() != 0 || daemon.reboot.waiting != nil {
		t.Fatalf("results = %+v, reboots %d", results, setup.reboots.count())
	}
}

func TestRebootHelperFailureIsReportedWithItsOutput(t *testing.T) {
	setup := newRebootSetup(t)
	setup.reboots.rebootErr = errors.New("rebooting is turned off on this server")
	setup.reboots.output = "$ /sbin/shutdown -r +1 x\nFailed to talk to init daemon.\n"
	daemon := setup.start(t)
	tick(t, daemon)
	setup.command(t, daemon, "r1", `{}`)

	results := setup.server.resultsOf("r1")
	if len(results) != 1 || results[0].Status != CommandFailed || rebootResultData(t, results[0]).Code != RebootCodeFailed ||
		*results[0].Detail != "the reboot could not be scheduled: rebooting is turned off on this server" ||
		results[0].Output == nil || *results[0].Output != setup.reboots.output {
		t.Fatalf("results = %+v", results)
	}
	if setup.reboots.requestExists() || daemon.commands.get("r1").State != commandFinished {
		t.Fatal("request kept or command not finished")
	}
}

func TestRebootWaitsForThePackageManager(t *testing.T) {
	setup := newRebootSetup(t)
	cache := time.Now()
	setup.packages = &fakePackages{token: "dpkg@1", cache: &cache, lock: "/var/lib/dpkg/lock-frontend (fcntl, pid 42)"}
	daemon := setup.start(t)
	tick(t, daemon)

	setup.command(t, daemon, "r1", `{}`)
	if setup.reboots.count() != 0 || daemon.reboot.waiting == nil {
		t.Fatalf("rebooted %d times while apt runs", setup.reboots.count())
	}
	if at := daemon.wakeAt(); at.After(setup.clock.Now().Add(rebootLockRetry)) {
		t.Fatalf("wakes at %v", at)
	}
	// A second reboot meanwhile is refused.
	setup.command(t, daemon, "r2", `{}`)
	if results := setup.server.resultsOf("r2"); len(results) != 1 || rebootResultData(t, results[0]).Code != RebootCodeFailed || !strings.Contains(*results[0].Detail, "another reboot (r1)") {
		t.Fatalf("r2 results = %+v", results)
	}

	setup.clock.advance(rebootLockRetry)
	tick(t, daemon)
	if setup.reboots.count() != 0 {
		t.Fatal("rebooted while apt runs")
	}
	setup.packages.set(func(fake *fakePackages) { fake.lock = "" })
	setup.clock.advance(rebootLockRetry)
	tick(t, daemon)
	if setup.reboots.count() != 1 || daemon.commands.get("r1").State != commandHandedOff {
		t.Fatalf("reboots = %d after the lock went", setup.reboots.count())
	}
}

func TestRebootGivesUpOnABusyPackageManager(t *testing.T) {
	setup := newRebootSetup(t)
	cache := time.Now()
	setup.packages = &fakePackages{token: "dpkg@1", cache: &cache, lock: "/var/run/yum.pid (pid, pid 7)"}
	daemon := setup.start(t)
	tick(t, daemon)
	setup.command(t, daemon, "r1", `{}`)
	for range 30 {
		setup.clock.advance(rebootLockRetry)
		tick(t, daemon)
	}
	results := setup.server.resultsOf("r1")
	if len(results) != 1 || rebootResultData(t, results[0]).Code != RebootCodePackageManagerBusy ||
		!strings.Contains(*results[0].Detail, "/var/run/yum.pid") || setup.reboots.count() != 0 || daemon.reboot.waiting != nil {
		t.Fatalf("results = %+v, reboots %d", results, setup.reboots.count())
	}
}

func TestHandedOffRebootWithoutARequestIsReportedFailed(t *testing.T) {
	setup := newRebootSetup(t)
	journal, err := OpenCommandJournal(setup.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.put(commandRecord{ID: "r1", Kind: CommandReboot, State: commandHandedOff, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	setup.server.addCommand("r1", CommandReboot, `{}`)
	daemon := setup.start(t)
	tick(t, daemon)
	results := setup.server.resultsOf("r1")
	if len(results) != 1 || rebootResultData(t, results[0]).Code != RebootCodeFailed || *results[0].Detail != "the agent stopped before the reboot was scheduled" {
		t.Fatalf("results = %+v", results)
	}
}

func TestRebootRequestOutlivesALostJournal(t *testing.T) {
	setup := newRebootSetup(t)
	first := setup.start(t)
	tick(t, first)
	setup.command(t, first, "r1", `{}`)
	if err := os.Remove(setup.journalPath); err != nil {
		t.Fatal(err)
	}
	setup.reboots.rebooted(time.Now().UTC().Truncate(time.Second))
	second := setup.start(t)
	tick(t, second)
	if results := setup.server.resultsOf("r1"); len(results) != 1 || rebootResultData(t, results[0]).Code != RebootCodeRebooted {
		t.Fatalf("results = %+v", results)
	}
}

func TestRebootIfRequiredOnlyRebootsWhenTheHostNeedsIt(t *testing.T) {
	setup := newRebootSetup(t)
	daemon := setup.start(t)
	tick(t, daemon)

	yes, no := true, false
	setup.reboots.required = &no
	setup.command(t, daemon, "r1", `{"if_required":true}`)
	results := setup.server.resultsOf("r1")
	if len(results) != 1 || results[0].Status != CommandSucceeded || rebootResultData(t, results[0]).Code != RebootCodeNotRequired || setup.reboots.count() != 0 {
		t.Fatalf("not required: results = %+v, reboots %d", results, setup.reboots.count())
	}

	setup.reboots.required = nil
	setup.command(t, daemon, "r2", `{"if_required":true}`)
	results = setup.server.resultsOf("r2")
	if len(results) != 1 || results[0].Status != CommandFailed || rebootResultData(t, results[0]).Code != RebootCodeRequiredUnknown || setup.reboots.count() != 0 {
		t.Fatalf("unknown: results = %+v, reboots %d", results, setup.reboots.count())
	}

	setup.reboots.required = &yes
	setup.command(t, daemon, "r3", `{"if_required":true}`)
	if setup.reboots.count() != 1 || daemon.commands.get("r3").State != commandHandedOff {
		t.Fatalf("required: reboots = %d", setup.reboots.count())
	}
}

func TestRebootWithoutIfRequiredRebootsRegardless(t *testing.T) {
	for _, args := range []string{`{}`, `{"if_required":false}`, `{"if_required":null}`} {
		setup := newRebootSetup(t)
		no := false
		setup.reboots.required = &no
		daemon := setup.start(t)
		tick(t, daemon)
		setup.command(t, daemon, "r1", args)
		if setup.reboots.count() != 1 {
			t.Fatalf("%s: reboots = %d", args, setup.reboots.count())
		}
	}
}

func TestRebootIfRequiredMustBeABoolean(t *testing.T) {
	setup := newRebootSetup(t)
	daemon := setup.start(t)
	tick(t, daemon)
	setup.command(t, daemon, "r1", `{"if_required":"yes"}`)
	if results := setup.server.resultsOf("r1"); len(results) != 1 || !strings.HasPrefix(*results[0].Detail, "invalid args:") || setup.reboots.count() != 0 {
		t.Fatalf("results = %+v", results)
	}
}

func TestRebootSendsAHeartbeatOnceItIsScheduled(t *testing.T) {
	setup := newRebootSetup(t)
	daemon := setup.start(t)
	tick(t, daemon)

	before := len(setup.server.requestLog())
	setup.command(t, daemon, "r1", `{}`)
	if at := daemon.wakeAt(); at.After(setup.clock.Now()) {
		t.Fatalf("next heartbeat at %v", at)
	}
	tick(t, daemon)
	if got := requestsSince(setup.server, before); !slices.Contains(got, "/heartbeat") {
		t.Fatalf("requests = %q", got)
	}
}
