package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeManager is a package manager whose database and repository are
// scripted. Install simulates the package's postinst restarting the
// daemon: the new version writes its readiness marker unless it is in
// neverReady.
type fakeManager struct {
	mu        sync.Mutex
	store     Store
	commandID string

	installed  Installed
	available  []string
	refreshErr error
	// installErrs are returned for a version once each, in order.
	installErrs map[string][]error
	// leaves is what an Install of a version leaves installed when it
	// fails (default: unchanged).
	leaves     map[string]Installed
	neverReady map[string]bool
	calls      []string
}

func newFakeManager(store Store) *fakeManager {
	return &fakeManager{
		store: store, commandID: "c1",
		installed:   Installed{Version: "1.2.3", Intact: true},
		available:   []string{"1.2.2", "1.2.3", "1.3.0"},
		installErrs: map[string][]error{}, leaves: map[string]Installed{}, neverReady: map[string]bool{},
	}
}

func (fake *fakeManager) record(call string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls = append(fake.calls, call)
}

func (fake *fakeManager) callLog() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return slices.Clone(fake.calls)
}

func (fake *fakeManager) Installed(context.Context) (Installed, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.installed, nil
}

func (fake *fakeManager) Refresh(context.Context) error {
	fake.record("refresh")
	return fake.refreshErr
}

func (fake *fakeManager) Available(context.Context) ([]string, error) {
	fake.record("available")
	return fake.available, nil
}

func (fake *fakeManager) Install(_ context.Context, version, installed string) error {
	fake.record("install " + version + " from " + installed)
	fake.mu.Lock()
	if queued := fake.installErrs[version]; len(queued) > 0 {
		err := queued[0]
		fake.installErrs[version] = queued[1:]
		if leaves, ok := fake.leaves[version]; ok {
			fake.installed = leaves
		}
		fake.mu.Unlock()
		return err
	}
	fake.installed = Installed{Version: version, Intact: true}
	ready := !fake.neverReady[version]
	fake.mu.Unlock()
	if ready {
		return fake.store.WriteReadiness(Readiness{CommandID: fake.commandID, Version: version, PID: os.Getpid(), WrittenAt: time.Now()})
	}
	return nil
}

// fakeClock advances only when the helper sleeps.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clock.mu.Lock()
	clock.now = clock.now.Add(d)
	clock.mu.Unlock()
	return nil
}

type helperTest struct {
	t       *testing.T
	store   Store
	manager *fakeManager
	clock   *fakeClock
	options HelperOptions
}

func newHelperTest(t *testing.T) *helperTest {
	t.Helper()
	store := Store{Directory: t.TempDir(), Owner: -1}
	if err := store.WriteRequest(Request{CommandID: "c1", PreviousVersion: "1.2.3", TargetVersion: "1.3.0", RequestedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	manager := newFakeManager(store)
	clock := &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	return &helperTest{t: t, store: store, manager: manager, clock: clock, options: HelperOptions{
		Store: store, Manager: manager, Now: clock.Now, Sleep: clock.Sleep,
		Alive: func(pid int) bool { return pid == os.Getpid() },
	}}
}

func (test *helperTest) run() error {
	test.t.Helper()
	return RunHelper(context.Background(), test.options)
}

// result runs the helper and returns its result; the request, state and
// readiness marker must be gone.
func (test *helperTest) result() Result {
	test.t.Helper()
	if err := test.run(); err != nil {
		test.t.Fatalf("RunHelper() = %v", err)
	}
	result, err := test.store.ReadResult()
	if err != nil || result == nil {
		test.t.Fatalf("result = %v, %v", result, err)
	}
	for _, name := range []string{RequestFile, StateFile, ReadyFile} {
		if _, err := os.Stat(filepath.Join(test.store.Directory, name)); err == nil {
			test.t.Fatalf("%s left behind", name)
		}
	}
	if result.CommandID != "c1" || result.PreviousVersion != "1.2.3" || result.TargetVersion != "1.3.0" || result.FinishedAt.IsZero() {
		test.t.Fatalf("result = %+v", result)
	}
	return *result
}

func (test *helperTest) writeState(phase string) {
	test.t.Helper()
	if err := test.store.writeState(state{CommandID: "c1", Phase: phase, PreviousVersion: "1.2.3", TargetVersion: "1.3.0", WaitStartedAt: test.clock.Now(), Starts: 1}); err != nil {
		test.t.Fatal(err)
	}
}

func (test *helperTest) wantCalls(want ...string) {
	test.t.Helper()
	if got := test.manager.callLog(); !slices.Equal(got, want) {
		test.t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestHelperUpdates(t *testing.T) {
	test := newHelperTest(t)
	result := test.result()
	if result.Code != CodeUpdated || result.InstalledVersion != "1.3.0" || result.Detail != "" || !result.Succeeded() {
		t.Fatalf("result = %+v", result)
	}
	test.wantCalls("refresh", "available", "install 1.3.0 from 1.2.3")
}

func TestHelperIgnoresAStaleReadinessMarker(t *testing.T) {
	test := newHelperTest(t)
	// A marker of the target version from a daemon that no longer runs.
	if err := test.store.WriteReadiness(Readiness{CommandID: "c1", Version: "1.3.0", PID: 999999}); err != nil {
		t.Fatal(err)
	}
	test.manager.neverReady["1.3.0"] = true
	if result := test.result(); result.Code != CodeReadinessFailedRolledBack {
		t.Fatalf("result = %+v", result)
	}
}

func TestHelperInstallFailureKeepsThePreviousVersion(t *testing.T) {
	test := newHelperTest(t)
	test.manager.installErrs["1.3.0"] = []error{errors.New("apt-get exited with status 100: E: Sub-process /usr/bin/dpkg returned an error code (1)")}
	result := test.result()
	if result.Code != CodeInstallFailed || result.InstalledVersion != "1.2.3" || !strings.Contains(result.Detail, "status 100") {
		t.Fatalf("result = %+v", result)
	}
	test.wantCalls("refresh", "available", "install 1.3.0 from 1.2.3")
}

func TestHelperRollsBackAHalfInstalledTarget(t *testing.T) {
	test := newHelperTest(t)
	test.manager.installErrs["1.3.0"] = []error{errors.New("dpkg: error processing package serverchief (--configure)")}
	test.manager.leaves["1.3.0"] = Installed{Version: "1.3.0", Intact: false}
	result := test.result()
	if result.Code != CodeReadinessFailedRolledBack || result.InstalledVersion != "1.2.3" || !strings.Contains(result.Detail, "--configure") {
		t.Fatalf("result = %+v", result)
	}
	test.wantCalls("refresh", "available", "install 1.3.0 from 1.2.3", "install 1.2.3 from 1.3.0")
}

func TestHelperRollsBackWhenTheTargetNeverGetsReady(t *testing.T) {
	test := newHelperTest(t)
	test.manager.neverReady["1.3.0"] = true
	result := test.result()
	if result.Code != CodeReadinessFailedRolledBack || result.InstalledVersion != "1.2.3" ||
		result.Detail != "rolled back to 1.2.3; cause: agent 1.3.0 did not report ready within 2m0s" {
		t.Fatalf("result = %+v", result)
	}
	test.wantCalls("refresh", "available", "install 1.3.0 from 1.2.3", "install 1.2.3 from 1.3.0")
	if waited := test.clock.Now().Sub(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)); waited < 2*time.Minute || waited > 2*time.Minute+5*time.Second {
		t.Fatalf("waited %s for readiness", waited)
	}
}

func TestHelperReportsAFailedRollback(t *testing.T) {
	test := newHelperTest(t)
	test.manager.neverReady["1.3.0"] = true
	test.manager.installErrs["1.2.3"] = []error{errors.New("E: Version '1.2.3' for 'serverchief' was not found")}
	result := test.result()
	if result.Code != CodeRollbackFailed || result.InstalledVersion != "1.3.0" ||
		!strings.Contains(result.Detail, "rollback to 1.2.3 failed: E: Version") || !strings.Contains(result.Detail, "cause: agent 1.3.0 did not report ready") {
		t.Fatalf("result = %+v", result)
	}
}

func TestHelperReportsARollbackThatNeverGetsReady(t *testing.T) {
	test := newHelperTest(t)
	test.manager.neverReady["1.3.0"] = true
	test.manager.neverReady["1.2.3"] = true
	result := test.result()
	if result.Code != CodeRollbackFailed || result.InstalledVersion != "1.2.3" || !strings.HasPrefix(result.Detail, "rolled back to 1.2.3, but agent 1.2.3 did not report ready") {
		t.Fatalf("result = %+v", result)
	}
}

func TestHelperPreflight(t *testing.T) {
	for _, test := range []struct {
		name      string
		available []string
		refresh   error
		code      string
		detail    string
	}{
		{"target missing", []string{"1.2.3"}, nil, CodeTargetUnavailable, "the Server Chief repository does not offer 1.3.0"},
		{"target missing after a failed refresh", []string{"1.2.3"}, errors.New("apt-get: W: Failed to fetch"), CodeTargetUnavailable, "the Server Chief repository does not offer 1.3.0 (refresh failed: apt-get: W: Failed to fetch)"},
		{"installed version missing", []string{"1.3.0"}, nil, CodeRollbackUnavailable, "the Server Chief repository no longer offers the installed version 1.2.3, which a rollback needs"},
		{"empty repository", nil, nil, CodeTargetUnavailable, "the Server Chief repository does not offer 1.3.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			helper := newHelperTest(t)
			helper.manager.available, helper.manager.refreshErr = test.available, test.refresh
			result := helper.result()
			if result.Code != test.code || result.Detail != test.detail || result.InstalledVersion != "1.2.3" {
				t.Fatalf("result = %+v", result)
			}
			helper.wantCalls("refresh", "available")
		})
	}
}

func TestHelperRefusesAChangedInstallation(t *testing.T) {
	test := newHelperTest(t)
	test.manager.installed = Installed{Version: "1.2.4", Intact: true}
	if result := test.result(); result.Code != CodeUnsupportedInstall || result.InstalledVersion != "1.2.4" {
		t.Fatalf("result = %+v", result)
	}
	test.wantCalls()
}

func TestHelperWaitsForThePackageManager(t *testing.T) {
	test := newHelperTest(t)
	checks := 0
	test.options.Locks = func() (string, bool) {
		checks++
		return "/var/lib/dpkg/lock-frontend (fcntl, pid 42)", checks <= 3
	}
	if result := test.result(); result.Code != CodeUpdated {
		t.Fatalf("result = %+v", result)
	}
	// Three held checks, 30 s apart, then the update went ahead.
	if waited := test.clock.Now().Sub(time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)); waited != 90*time.Second {
		t.Fatalf("waited %s", waited)
	}
}

func TestHelperGivesUpOnABusyPackageManager(t *testing.T) {
	test := newHelperTest(t)
	checks := 0
	test.options.Locks = func() (string, bool) { checks++; return "/var/lib/dpkg/lock (fcntl, pid 42)", true }
	result := test.result()
	if result.Code != CodePackageManagerBusy || result.InstalledVersion != "1.2.3" ||
		result.Detail != "/var/lib/dpkg/lock (fcntl, pid 42) was still held after waiting 1h0m0s" {
		t.Fatalf("result = %+v", result)
	}
	// One check every 30 s for an hour, and nothing ran.
	if checks != 121 {
		t.Fatalf("%d lock checks", checks)
	}
	test.wantCalls()
}

func TestHelperBusyTimeoutInRealTime(t *testing.T) {
	test := newHelperTest(t)
	test.options.Now, test.options.Sleep = nil, nil
	test.options.LockWait, test.options.LockPoll = 300*time.Millisecond, 20*time.Millisecond
	checks := 0
	test.options.Locks = func() (string, bool) { checks++; return "/var/run/yum.pid (pid, pid 7)", true }
	started := time.Now()
	result := test.result()
	if elapsed := time.Since(started); result.Code != CodePackageManagerBusy || elapsed < 300*time.Millisecond || elapsed > 5*time.Second || checks < 5 {
		t.Fatalf("result = %+v after %s and %d checks", result, elapsed, checks)
	}
}

func TestHelperWaitsAgainWhenTheLockIsTakenUnderIt(t *testing.T) {
	test := newHelperTest(t)
	test.manager.installErrs["1.3.0"] = []error{&BusyError{Lock: "E: Could not get lock /var/lib/dpkg/lock-frontend"}}
	if result := test.result(); result.Code != CodeUpdated {
		t.Fatalf("result = %+v", result)
	}
	test.wantCalls("refresh", "available", "install 1.3.0 from 1.2.3", "refresh", "available", "install 1.3.0 from 1.2.3")
}

func TestHelperResumesFromEveryPhase(t *testing.T) {
	ready := func(version string) func(*helperTest) {
		return func(test *helperTest) {
			if err := test.store.WriteReadiness(Readiness{CommandID: "c1", Version: version, PID: os.Getpid()}); err != nil {
				test.t.Fatal(err)
			}
		}
	}
	installed := func(version string, intact bool) func(*helperTest) {
		return func(test *helperTest) { test.manager.installed = Installed{Version: version, Intact: intact} }
	}
	for _, test := range []struct {
		name      string
		phase     string
		arrange   []func(*helperTest)
		code      string
		installed string
		calls     []string
	}{
		{"waiting", phaseWaiting, nil, CodeUpdated, "1.3.0", []string{"refresh", "available", "install 1.3.0 from 1.2.3"}},
		{"preflight", phasePreflight, nil, CodeUpdated, "1.3.0", []string{"refresh", "available", "install 1.3.0 from 1.2.3"}},
		{"installing, not yet started", phaseInstalling, nil, CodeUpdated, "1.3.0", []string{"install 1.3.0 from 1.2.3"}},
		{"installing, finished", phaseInstalling, []func(*helperTest){installed("1.3.0", true), ready("1.3.0")}, CodeUpdated, "1.3.0", nil},
		{"installing, interrupted mid-dpkg", phaseInstalling, []func(*helperTest){installed("1.3.0", false)}, CodeReadinessFailedRolledBack, "1.2.3", []string{"install 1.2.3 from 1.3.0"}},
		{"verifying, ready", phaseVerifying, []func(*helperTest){installed("1.3.0", true), ready("1.3.0")}, CodeUpdated, "1.3.0", nil},
		{"verifying, never ready", phaseVerifying, []func(*helperTest){installed("1.3.0", true)}, CodeReadinessFailedRolledBack, "1.2.3", []string{"install 1.2.3 from 1.3.0"}},
		{"rolling back, not yet started", phaseRollingBack, []func(*helperTest){installed("1.3.0", true)}, CodeReadinessFailedRolledBack, "1.2.3", []string{"install 1.2.3 from 1.3.0"}},
		{"rolling back, finished", phaseRollingBack, []func(*helperTest){ready("1.2.3")}, CodeReadinessFailedRolledBack, "1.2.3", nil},
		{"verifying the rollback", phaseVerifyingRollback, []func(*helperTest){ready("1.2.3")}, CodeReadinessFailedRolledBack, "1.2.3", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			helper := newHelperTest(t)
			helper.writeState(test.phase)
			for _, arrange := range test.arrange {
				arrange(helper)
			}
			result := helper.result()
			if result.Code != test.code || result.InstalledVersion != test.installed {
				t.Fatalf("result = %+v", result)
			}
			helper.wantCalls(test.calls...)
		})
	}
}

func TestHelperSurvivesACancelledRunAndResumes(t *testing.T) {
	test := newHelperTest(t)
	test.manager.neverReady["1.3.0"] = true
	ctx, cancel := context.WithCancel(context.Background())
	// SIGTERM while waiting for the new daemon: the state stays.
	test.options.Sleep = func(context.Context, time.Duration) error { cancel(); return context.Canceled }
	if err := RunHelper(ctx, test.options); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunHelper() = %v", err)
	}
	current, _ := test.store.readState()
	if current == nil || current.Phase != phaseVerifying || current.Starts != 1 {
		t.Fatalf("state = %+v", current)
	}
	if result, _ := test.store.ReadResult(); result != nil {
		t.Fatalf("result after a cancelled run = %+v", result)
	}

	// The daemon came up after all; the next run only confirms it.
	test.options.Sleep = test.clock.Sleep
	if err := test.store.WriteReadiness(Readiness{CommandID: "c1", Version: "1.3.0", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if result := test.result(); result.Code != CodeUpdated {
		t.Fatalf("result = %+v", result)
	}
	test.wantCalls("refresh", "available", "install 1.3.0 from 1.2.3")
}

func TestHelperIsIdempotent(t *testing.T) {
	test := newHelperTest(t)
	first := test.result()
	// A second run (a duplicate launch) finds no request.
	if err := test.run(); err != nil {
		t.Fatal(err)
	}
	// A run that finds the request again next to its result only cleans up.
	if err := test.store.WriteRequest(Request{CommandID: "c1", PreviousVersion: "1.2.3", TargetVersion: "1.3.0"}); err != nil {
		t.Fatal(err)
	}
	second := test.result()
	if first != second {
		t.Fatalf("results differ: %+v, %+v", first, second)
	}
	test.wantCalls("refresh", "available", "install 1.3.0 from 1.2.3")
}

func TestHelperGivesUpAfterRepeatedCrashes(t *testing.T) {
	test := newHelperTest(t)
	test.writeState(phaseInstalling)
	current, _ := test.store.readState()
	current.Starts = maximumHelperStarts
	if err := test.store.writeState(*current); err != nil {
		t.Fatal(err)
	}
	result := test.result()
	if result.Code != CodeInstallFailed || result.Detail != "the update helper stopped 5 times in phase installing" {
		t.Fatalf("result = %+v", result)
	}
	test.wantCalls()
}

func TestHelperDowngradesWhenTheTargetIsOlder(t *testing.T) {
	test := newHelperTest(t)
	if err := test.store.WriteRequest(Request{CommandID: "c1", PreviousVersion: "1.2.3", TargetVersion: "1.2.2"}); err != nil {
		t.Fatal(err)
	}
	if err := test.run(); err != nil {
		t.Fatal(err)
	}
	result, _ := test.store.ReadResult()
	if result == nil || result.Code != CodeUpdated || result.InstalledVersion != "1.2.2" {
		t.Fatalf("result = %+v", result)
	}
}

func TestHelperWithoutRequestDoesNothing(t *testing.T) {
	test := newHelperTest(t)
	if err := test.store.RemoveRequest(); err != nil {
		t.Fatal(err)
	}
	if err := test.run(); err != nil {
		t.Fatal(err)
	}
	test.wantCalls()
	if result, _ := test.store.ReadResult(); result != nil {
		t.Fatalf("result = %+v", result)
	}
}
