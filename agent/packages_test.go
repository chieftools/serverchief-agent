package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.chief.tools/serverchief-agent/collector"
)

// fakePackages is a PackageSystem with fake data.
type fakePackages struct {
	mu        sync.Mutex
	token     string
	lock      string
	heldFor   time.Duration
	cache     *time.Time
	result    error
	refreshes int
	// onRefresh runs inside RefreshMetadata.
	onRefresh func()
}

func (fake *fakePackages) DatabaseToken() string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.token
}

func (fake *fakePackages) BusyLock() (string, time.Duration, bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.lock, fake.heldFor, fake.lock != ""
}

func (fake *fakePackages) CacheUpdatedAt() *time.Time {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.cache
}

func (fake *fakePackages) RefreshMetadata(context.Context) error {
	fake.mu.Lock()
	fake.refreshes++
	onRefresh, result := fake.onRefresh, fake.result
	fake.mu.Unlock()
	if onRefresh != nil {
		onRefresh()
	}
	return result
}

func (fake *fakePackages) set(change func(*fakePackages)) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	change(fake)
}

func (fake *fakePackages) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.refreshes
}

// packagesDaemon returns a daemon with packages and updates enabled and a
// fake package system whose cache is a day old.
func packagesDaemon(t *testing.T, server *protocolServer, now time.Time, change func(*RunOptions)) (*daemon, *fakePackages, *fakeCollector, *fakeCollector) {
	t.Helper()
	packages := &fakeCollector{name: "packages", data: map[string]string{"bash": "5.2"}}
	updates := &fakeCollector{name: "updates", data: map[string]int{"pending": 1}}
	setup := newTestSetup(t, packages, updates)
	options := setup.options(t, server.client(), "packages", "updates")
	cache := now.Add(-24 * time.Hour)
	system := &fakePackages{token: "status@1", cache: &cache}
	options.Packages = system
	options.Now = func() time.Time { return now }
	if change != nil {
		change(&options)
	}
	return newDaemon(options), system, packages, updates
}

// runRefresh starts a due refresh and finishes it once it returned.
func runRefresh(t *testing.T, daemon *daemon, now time.Time) {
	t.Helper()
	daemon.packages.refreshAt = now
	daemon.startRefresh(context.Background(), now)
	if !daemon.packages.running {
		t.Fatal("refresh did not start")
	}
	select {
	case err := <-daemon.packages.done:
		daemon.finishRefresh(err, now)
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not finish")
	}
}

func TestRefreshScheduledAfterStartDelay(t *testing.T) {
	server := newProtocolServer(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	daemon, _, _, _ := packagesDaemon(t, server, now, func(options *RunOptions) {
		options.Jitter = func(maximum time.Duration) time.Duration { return maximum }
	})
	if want := now.Add(5 * time.Minute); !daemon.packages.refreshAt.Equal(want) {
		t.Fatalf("first refresh at %s, want %s", daemon.packages.refreshAt, want)
	}
	if !daemon.wakeAt().Equal(now) {
		t.Fatalf("wakeAt = %s (the first activity poll is due)", daemon.wakeAt())
	}
}

func TestRefreshSkippedWhileCacheIsFresh(t *testing.T) {
	server := newProtocolServer(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	daemon, system, _, _ := packagesDaemon(t, server, now, nil)
	cache := now.Add(-time.Hour) // apt's daily timer just ran
	system.set(func(fake *fakePackages) { fake.cache = &cache })

	daemon.packages.refreshAt = now
	daemon.startRefresh(context.Background(), now)
	if daemon.packages.running || system.count() != 0 {
		t.Fatal("refreshed a fresh cache")
	}
	// Jitter 0 gives the low end of interval ± 10%: 5h24m after the cache.
	if want := cache.Add(5*time.Hour + 24*time.Minute); !daemon.packages.refreshAt.Equal(want) {
		t.Fatalf("next check at %s, want %s", daemon.packages.refreshAt, want)
	}
}

func TestRefreshSkippedWhileLocked(t *testing.T) {
	server := newProtocolServer(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	daemon, system, _, _ := packagesDaemon(t, server, now, nil)
	system.set(func(fake *fakePackages) { fake.lock = "/var/lib/dpkg/lock-frontend (fcntl, pid 4242)" })

	daemon.packages.refreshAt = now
	daemon.startRefresh(context.Background(), now)
	if daemon.packages.running || system.count() != 0 {
		t.Fatal("refreshed while a lock is held")
	}
	if want := now.Add(10 * time.Minute); !daemon.packages.refreshAt.Equal(want) {
		t.Fatalf("retry at %s, want %s", daemon.packages.refreshAt, want)
	}

	// The lock is gone at the retry.
	system.set(func(fake *fakePackages) { fake.lock = "" })
	runRefresh(t, daemon, now.Add(10*time.Minute))
	if system.count() != 1 {
		t.Fatalf("%d refreshes after the lock was released", system.count())
	}
}

func TestRefreshSuccessCollectsUpdatesNow(t *testing.T) {
	server := newProtocolServer(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	daemon, system, _, updates := packagesDaemon(t, server, now, nil)
	ctx := context.Background()
	if err := daemon.deliver(ctx, []string{"packages", "updates"}); err != nil {
		t.Fatal(err)
	}
	daemon.next["updates"] = now.Add(6 * time.Hour)

	var recorded []time.Time
	daemon.options.RecordRefresh = func(at time.Time) error {
		recorded = append(recorded, at)
		return nil
	}
	fresh := now
	system.set(func(fake *fakePackages) {
		fake.onRefresh = func() {
			system.set(func(fake *fakePackages) { fake.cache = &fresh })
			updates.set(map[string]int{"pending": 3}, nil)
		}
	})
	runRefresh(t, daemon, now)
	if !daemon.next["updates"].Equal(now) {
		t.Fatalf("updates due at %s after a refresh", daemon.next["updates"])
	}
	if len(recorded) != 1 || !recorded[0].Equal(now) {
		t.Fatalf("recorded refreshes = %v", recorded)
	}
	if want := now.Add(5*time.Hour + 24*time.Minute); !daemon.packages.refreshAt.Equal(want) {
		t.Fatalf("next refresh at %s, want %s", daemon.packages.refreshAt, want)
	}
	if err := daemon.deliver(ctx, daemon.due(now)); err != nil {
		t.Fatal(err)
	}
	sent := server.sent()
	if len(sent) != 2 || !reflect.DeepEqual(envelopeNames(sent[1]), []string{"updates"}) {
		t.Fatalf("envelopes = %d, last %v", len(sent), envelopeNames(sent[len(sent)-1]))
	}
}

func TestRefreshFailureBacksOffAndIsNoDatasetError(t *testing.T) {
	server := newProtocolServer(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	daemon, system, _, _ := packagesDaemon(t, server, now, nil)
	system.set(func(fake *fakePackages) {
		fake.result = errors.New("apt-get update exited with status 100: E: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/xenial/InRelease")
	})
	daemon.options.RecordRefresh = func(time.Time) error {
		t.Fatal("a failed or skipped refresh was recorded")
		return nil
	}
	// Jitter 0: 90% of 6h, 12h, 24h, then capped at 24h.
	for attempt, want := range []time.Duration{324 * time.Minute, 648 * time.Minute, 1296 * time.Minute, 1296 * time.Minute} {
		runRefresh(t, daemon, now)
		if got := daemon.packages.refreshAt.Sub(now); got != want {
			t.Fatalf("failure %d: retry in %s, want %s", attempt+1, got, want)
		}
	}
	if daemon.packages.failures != 4 {
		t.Fatalf("failures = %d", daemon.packages.failures)
	}
	if err := daemon.deliver(context.Background(), []string{"packages", "updates"}); err != nil {
		t.Fatal(err)
	}
	for _, envelope := range server.sent() {
		if len(envelope.Errors) != 0 {
			t.Fatalf("refresh failure reported as a dataset error: %v", envelope.Errors)
		}
	}

	// A lock taken between the check and the run retries soon, without
	// counting as a failure; a success resets the backoff.
	system.set(func(fake *fakePackages) {
		fake.result = &collector.BusyError{Lock: "E: Could not get lock /var/lib/apt/lists/lock"}
	})
	runRefresh(t, daemon, now)
	if got := daemon.packages.refreshAt.Sub(now); got != 10*time.Minute || daemon.packages.failures != 4 {
		t.Fatalf("lock race: retry in %s, failures %d", got, daemon.packages.failures)
	}
	system.set(func(fake *fakePackages) { fake.result = nil })
	daemon.options.RecordRefresh = nil
	runRefresh(t, daemon, now)
	if daemon.packages.failures != 0 {
		t.Fatal("success did not reset the backoff")
	}
}

func TestRefreshFollowsConfiguration(t *testing.T) {
	server := newProtocolServer(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	daemon, _, _, _ := packagesDaemon(t, server, now, func(options *RunOptions) {
		options.ServerConfig.PackageMetadataRefresh = &PackageMetadataRefresh{Enabled: false, Interval: 21600}
	})
	if !daemon.packages.refreshAt.IsZero() {
		t.Fatal("disabled refresh is scheduled")
	}

	// The backend enables it with a shorter interval.
	config := daemon.settings.server
	config.PackageMetadataRefresh = &PackageMetadataRefresh{Enabled: true, Interval: 7200}
	daemon.applyConfig(config)
	if !daemon.packages.refreshAt.Equal(now) {
		t.Fatalf("enabled refresh at %s", daemon.packages.refreshAt)
	}
	if _, interval := daemon.refreshSettings(); interval != 2*time.Hour {
		t.Fatalf("interval = %s", interval)
	}

	// Without the updates dataset nothing reads the metadata.
	config.Datasets = []string{"packages"}
	daemon.applyConfig(config)
	if enabled, _ := daemon.refreshSettings(); enabled || !daemon.packages.refreshAt.IsZero() {
		t.Fatal("refresh scheduled without the updates dataset")
	}

	// Without a package system there is no refresh at all.
	daemon.options.Packages = nil
	if enabled, _ := daemon.refreshSettings(); enabled {
		t.Fatal("refresh without a package system")
	}
}

func TestMetadataRefreshSettings(t *testing.T) {
	enabled, disabled := true, false
	for _, test := range []struct {
		name     string
		server   *PackageMetadataRefresh
		local    *LocalMetadataRefresh
		enabled  bool
		interval time.Duration
	}{
		{"absent", nil, nil, true, 6 * time.Hour},
		{"backend disabled", &PackageMetadataRefresh{Enabled: false, Interval: 21600}, nil, false, 6 * time.Hour},
		{"backend interval", &PackageMetadataRefresh{Enabled: true, Interval: 43200}, nil, true, 12 * time.Hour},
		{"backend interval clamped", &PackageMetadataRefresh{Enabled: true, Interval: 60}, nil, true, time.Hour},
		{"backend zero interval", &PackageMetadataRefresh{Enabled: true}, nil, true, 6 * time.Hour},
		{"local disables", &PackageMetadataRefresh{Enabled: true, Interval: 21600}, &LocalMetadataRefresh{Enabled: &disabled}, false, 6 * time.Hour},
		{"local enables and sets interval", &PackageMetadataRefresh{Enabled: false}, &LocalMetadataRefresh{Enabled: &enabled, Interval: Duration(3 * time.Hour)}, true, 3 * time.Hour},
		{"local interval only", nil, &LocalMetadataRefresh{Interval: Duration(2 * time.Hour)}, true, 2 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := settings{server: ServerConfig{PackageMetadataRefresh: test.server}, local: Config{PackageMetadataRefresh: test.local}}
			if gotEnabled, gotInterval := settings.MetadataRefresh(); gotEnabled != test.enabled || gotInterval != test.interval {
				t.Fatalf("MetadataRefresh() = %v %s", gotEnabled, gotInterval)
			}
		})
	}
}

func TestMetadataRefreshConfigDecoding(t *testing.T) {
	var config ServerConfig
	if err := json.Unmarshal([]byte(`{"etag":"\"c2\"","datasets":["updates"],"package_metadata_refresh":{"enabled":false,"interval":43200}}`), &config); err != nil {
		t.Fatal(err)
	}
	if config.PackageMetadataRefresh == nil || config.PackageMetadataRefresh.Enabled || config.PackageMetadataRefresh.Interval != 43200 {
		t.Fatalf("decoded %+v", config.PackageMetadataRefresh)
	}
	var absent ServerConfig
	if err := json.Unmarshal([]byte(`{"etag":"\"c2\""}`), &absent); err != nil || absent.PackageMetadataRefresh != nil {
		t.Fatalf("absent: %+v %v", absent.PackageMetadataRefresh, err)
	}

	local := Config{}
	if err := json.Unmarshal([]byte(`{"package_metadata_refresh":{"enabled":false,"interval":"12h"}}`), &local); err != nil {
		t.Fatal(err)
	}
	if local.PackageMetadataRefresh == nil || *local.PackageMetadataRefresh.Enabled || time.Duration(local.PackageMetadataRefresh.Interval) != 12*time.Hour {
		t.Fatalf("local %+v", local.PackageMetadataRefresh)
	}
	if err := local.Validate(); err != nil {
		t.Fatal(err)
	}
	local.PackageMetadataRefresh.Interval = Duration(10 * time.Minute)
	if err := local.Validate(); err == nil || !strings.Contains(err.Error(), "package_metadata_refresh.interval") {
		t.Fatalf("validate = %v", err)
	}
}

// activityClock steps a daemon through package activity polls.
type activityClock struct {
	t      *testing.T
	daemon *daemon
	now    time.Time
}

// poll advances to the next poll and reports whether packages and
// updates became due.
func (clock *activityClock) poll() bool {
	clock.t.Helper()
	clock.now = clock.now.Add(30 * time.Second)
	clock.daemon.next["packages"] = clock.now.Add(time.Hour)
	clock.daemon.next["updates"] = clock.now.Add(time.Hour)
	clock.daemon.pollPackageActivity(clock.now)
	packages, updates := !clock.daemon.next["packages"].After(clock.now), !clock.daemon.next["updates"].After(clock.now)
	if packages != updates {
		clock.t.Fatalf("packages due %v, updates due %v", packages, updates)
	}
	return packages
}

func newActivityClock(t *testing.T) (*activityClock, *fakePackages) {
	t.Helper()
	server := newProtocolServer(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	daemon, system, _, _ := packagesDaemon(t, server, now, nil)
	clock := &activityClock{t: t, daemon: daemon, now: now}
	if clock.poll() {
		t.Fatal("the first observation triggered a collection")
	}
	return clock, system
}

func TestPackageActivityWaitsForIdle(t *testing.T) {
	clock, system := newActivityClock(t)
	if clock.poll() || clock.poll() {
		t.Fatal("unchanged database triggered a collection")
	}
	system.set(func(fake *fakePackages) { fake.token = "status@2" })
	// The first poll sees the change; the next three are within the idle
	// period, the fifth is 120 s after it.
	for poll := 1; poll <= 4; poll++ {
		if clock.poll() {
			t.Fatalf("collected at poll %d after the change", poll)
		}
	}
	if !clock.poll() {
		t.Fatal("not collected after 120s idle")
	}
	if clock.poll() || clock.poll() {
		t.Fatal("collected twice for one change")
	}
}

// TestPackageActivityDebouncesLongUpgrade models apt upgrade: the database
// changes every minute for 20 minutes while dpkg holds its lock, which it
// keeps for a minute after the last write. One collection follows.
func TestPackageActivityDebouncesLongUpgrade(t *testing.T) {
	clock, system := newActivityClock(t)
	system.set(func(fake *fakePackages) { fake.lock = "/var/lib/dpkg/lock-frontend (fcntl, pid 4242)" })
	collections := 0
	for poll := 1; poll <= 40; poll++ {
		if poll%2 == 0 {
			system.set(func(fake *fakePackages) { fake.token = "status@" + strconv.Itoa(poll) })
		}
		if clock.poll() {
			collections++
		}
	}
	if collections != 0 {
		t.Fatalf("collected %d times during the upgrade", collections)
	}
	// The database settles; the lock is still held for two more polls.
	for range 6 {
		if clock.poll() {
			t.Fatal("collected while the lock is held")
		}
	}
	system.set(func(fake *fakePackages) { fake.lock = "" })
	if !clock.poll() {
		t.Fatal("not collected once idle and unlocked")
	}
	for range 10 {
		if clock.poll() {
			t.Fatal("collected again without a change")
		}
	}
}

func TestPackageActivityLockPatience(t *testing.T) {
	clock, system := newActivityClock(t)
	system.set(func(fake *fakePackages) {
		fake.token = "status@2"
		fake.lock = "/var/lib/dpkg/lock (fcntl, pid 99)"
		fake.heldFor = time.Hour
	})
	for range 10 {
		if clock.poll() {
			t.Fatal("collected while a lock held for an hour")
		}
	}
	// A change resets the idle period even after a long wait.
	system.set(func(fake *fakePackages) { fake.token = "status@3"; fake.heldFor = 2*time.Hour + time.Second })
	for range 4 {
		if clock.poll() {
			t.Fatal("collected within the idle period")
		}
	}
	// Held for over 2 hours: collected once the database is idle.
	if !clock.poll() {
		t.Fatal("not collected after the lock outlasted its patience")
	}
}

func TestBusyCollectorKeepsPreviousDataset(t *testing.T) {
	server := newProtocolServer(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	daemon, _, _, updates := packagesDaemon(t, server, now, nil)
	ctx := context.Background()
	updates.set(nil, errors.New("dnf -C check-update exited with status 1: no cache"))
	if err := daemon.deliver(ctx, []string{"packages", "updates"}); err != nil {
		t.Fatal(err)
	}
	if reported := daemon.options.Store.ReportedErrors(); reported["updates"] == "" {
		t.Fatalf("reported = %v", reported)
	}

	updates.set(nil, &collector.BusyError{Lock: "/var/run/yum.pid (pid, pid 22)"})
	if err := daemon.deliver(ctx, []string{"updates"}); err != nil {
		t.Fatal(err)
	}
	if sent := server.sent(); len(sent) != 1 {
		t.Fatalf("a busy package manager sent %d more envelopes", len(sent)-1)
	}
	if reported := daemon.options.Store.ReportedErrors(); !strings.Contains(reported["updates"], "no cache") {
		t.Fatalf("busy collection cleared the reported error: %v", reported)
	}
	if want := now.Add(10 * time.Minute); !daemon.next["updates"].Equal(want) {
		t.Fatalf("retry at %s, want %s", daemon.next["updates"], want)
	}
}

// TestRunRefreshesAndPushesUpdates drives the refresh through Run's loop.
func TestRunRefreshesAndPushesUpdates(t *testing.T) {
	server := newProtocolServer(t)
	packages := &fakeCollector{name: "packages", data: map[string]string{"bash": "5.2"}}
	updates := &fakeCollector{name: "updates", data: map[string]int{"pending": 1}}
	setup := newTestSetup(t, packages, updates)
	options := setup.options(t, server.client(), "packages", "updates")
	server.config = options.ServerConfig
	old := time.Now().Add(-24 * time.Hour)
	system := &fakePackages{token: "status@1", cache: &old}
	refreshed := make(chan struct{})
	system.onRefresh = func() {
		// The start round has been pushed; the refresh finds a new update.
		for wait := time.Now().Add(5 * time.Second); len(server.sent()) == 0 && time.Now().Before(wait); {
			time.Sleep(5 * time.Millisecond)
		}
		updates.set(map[string]int{"pending": 2}, nil)
		close(refreshed)
	}
	options.Packages = system
	options.PackageTimings = PackageTimings{RefreshStartDelay: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- Run(ctx, options) }()

	select {
	case <-refreshed:
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		sent := server.sent()
		if len(sent) >= 2 && reflect.DeepEqual(envelopeNames(sent[len(sent)-1]), []string{"updates"}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("updates not pushed after the refresh: %d envelopes", len(sent))
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	if system.count() != 1 {
		t.Fatalf("%d refreshes", system.count())
	}
}

func TestOwnRefreshDoesNotTriggerCollections(t *testing.T) {
	server := newProtocolServer(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	updates := &changingCollector{fakeCollector: fakeCollector{name: "updates", data: 1}, token: "cache@1"}
	repositories := &changingCollector{fakeCollector: fakeCollector{name: "repositories", data: 1}, token: "lists@1"}
	setup := newTestSetup(t, &fakeCollector{name: "packages", data: 1}, updates, repositories)
	options := setup.options(t, server.client(), "packages", "updates", "repositories")
	options.Now = func() time.Time { return now }
	system := &fakePackages{token: "status@1"}
	release := make(chan struct{})
	system.onRefresh = func() {
		// The refresh moves the cache and holds the lists lock, but leaves
		// the package database alone.
		updates.mu.Lock()
		updates.token = "cache@2"
		updates.mu.Unlock()
		repositories.mu.Lock()
		repositories.token = "lists@2"
		repositories.mu.Unlock()
		system.set(func(fake *fakePackages) { fake.lock = "/var/lib/apt/lists/lock (fcntl, pid 7)" })
		<-release
		system.set(func(fake *fakePackages) { fake.lock = "" })
	}
	options.Packages = system
	daemon := newDaemon(options)
	daemon.pollChanges(now)
	daemon.pollPackageActivity(now)
	daemon.next["packages"], daemon.next["updates"], daemon.next["repositories"] = now.Add(time.Hour), now.Add(time.Hour), now.Add(time.Hour)

	daemon.packages.refreshAt = now
	daemon.startRefresh(context.Background(), now)
	for _, _, held := system.BusyLock(); !held; _, _, held = system.BusyLock() {
		time.Sleep(time.Millisecond) // the refresh is running
	}
	for range 5 {
		now = now.Add(30 * time.Second)
		daemon.pollChanges(now)
		daemon.pollPackageActivity(now)
	}
	for _, name := range []string{"packages", "updates", "repositories"} {
		if daemon.next[name].Before(now.Add(time.Minute)) {
			t.Fatalf("the agent's own refresh triggered a %s collection", name)
		}
	}
	close(release)
	daemon.finishRefresh(<-daemon.packages.done, now)
	if !daemon.next["updates"].Equal(now) || !daemon.next["repositories"].Equal(now) || !daemon.next["packages"].After(now) {
		t.Fatal("a successful refresh must collect updates and repositories only")
	}
	daemon.next["updates"], daemon.next["repositories"] = now.Add(time.Hour), now.Add(time.Hour)
	daemon.pollChanges(now.Add(time.Minute))
	if !daemon.next["updates"].After(now.Add(time.Minute)) || !daemon.next["repositories"].After(now.Add(time.Minute)) {
		t.Fatal("the change poll collected a second time")
	}
}

// TestPackageActivityCollectsRepositories: the repositories' package
// counts follow package activity, even with only repositories enabled.
func TestPackageActivityCollectsRepositories(t *testing.T) {
	server := newProtocolServer(t)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	setup := newTestSetup(t, &fakeCollector{name: "repositories", data: 1})
	options := setup.options(t, server.client(), "repositories")
	system := &fakePackages{token: "status@1"}
	options.Packages = system
	options.Now = func() time.Time { return now }
	daemon := newDaemon(options)
	if !daemon.packagesActive() {
		t.Fatal("the activity trigger ignores repositories")
	}
	daemon.pollPackageActivity(now)
	daemon.next["repositories"] = now.Add(time.Hour)
	system.set(func(fake *fakePackages) { fake.token = "status@2" })
	daemon.pollPackageActivity(now)
	later := now.Add(3 * time.Minute)
	daemon.pollPackageActivity(later)
	if !daemon.next["repositories"].Equal(later) {
		t.Fatalf("repositories due at %s after package activity", daemon.next["repositories"])
	}
}
