package agent

import (
	"context"
	"errors"
	"log"
	"time"

	"go.chief.tools/serverchief-agent/collector"
)

// PackageSystem is the host's package manager as the daemon's metadata
// refresh and package activity trigger see it (collector.PackageSystem).
type PackageSystem interface {
	// DatabaseToken stats the package database; "" when there is none.
	DatabaseToken() string
	// BusyLock inspects, never takes, the package-manager locks. heldFor
	// is how long some lock has been held continuously.
	BusyLock() (lock string, heldFor time.Duration, held bool)
	// CacheUpdatedAt is the updates dataset's cache_updated_at.
	CacheUpdatedAt() *time.Time
	// RefreshMetadata runs apt-get update, yum makecache or dnf makecache
	// until ctx ends. A lock held by someone else returns an error
	// matching collector.ErrPackageManagerBusy.
	RefreshMetadata(ctx context.Context) error
}

// PackageTimings tune the metadata refresh and the package activity
// trigger. Zero fields take the protocol's values.
type PackageTimings struct {
	// Poll is how often the package database is stat'ed (30 s).
	Poll time.Duration
	// Idle is how long the database must stay unchanged, with no lock
	// held, before packages and updates are collected (120 s).
	Idle time.Duration
	// LockPatience is how long a held lock defers that collection (2 h).
	LockPatience time.Duration
	// LockRetry is when a refresh or collection skipped because of a lock
	// is tried again, plus up to half of it as jitter (10-15 min).
	LockRetry time.Duration
	// RefreshTimeout bounds one metadata refresh (10 min).
	RefreshTimeout time.Duration
	// RefreshStartDelay spreads the first refresh after a start (up to
	// 5 min), so a fleet restarted at once does not hit the mirrors at once.
	RefreshStartDelay time.Duration
	// RefreshInterval overrides the configured interval, bypassing its
	// minimum. Development only.
	RefreshInterval time.Duration
}

const (
	defaultPackagePoll       = 30 * time.Second
	defaultPackageIdle       = 2 * time.Minute
	defaultLockPatience      = 2 * time.Hour
	defaultLockRetry         = 10 * time.Minute
	defaultRefreshTimeout    = 10 * time.Minute
	defaultRefreshStartDelay = 5 * time.Minute
	// maximumRefreshBackoff caps the wait after repeated refresh failures
	// (EOL mirrors fail forever).
	maximumRefreshBackoff = 24 * time.Hour
	// refreshShutdownWait is how long a shutdown waits for a running
	// refresh, which got SIGTERM, to exit.
	refreshShutdownWait = 15 * time.Second
)

func (timings PackageTimings) withDefaults() PackageTimings {
	defaults := func(value *time.Duration, fallback time.Duration) {
		if *value <= 0 {
			*value = fallback
		}
	}
	defaults(&timings.Poll, defaultPackagePoll)
	defaults(&timings.Idle, defaultPackageIdle)
	defaults(&timings.LockPatience, defaultLockPatience)
	defaults(&timings.LockRetry, defaultLockRetry)
	defaults(&timings.RefreshTimeout, defaultRefreshTimeout)
	defaults(&timings.RefreshStartDelay, defaultRefreshStartDelay)
	return timings
}

// packageWork is the daemon's metadata refresh and package activity
// state.
type packageWork struct {
	timings PackageTimings

	pollAt    time.Time
	token     string
	seen      bool
	pending   bool
	changedAt time.Time
	// waiting is the lock last logged as deferring the collection.
	waiting string

	// refreshAt is zero while the refresh is disabled or running.
	refreshAt time.Time
	running   bool
	startedAt time.Time
	done      chan error
	failures  int
}

const (
	datasetPackages = "packages"
	datasetUpdates  = "updates"
)

// packagesActive reports whether the activity trigger has a dataset to
// collect.
func (daemon *daemon) packagesActive() bool {
	if daemon.options.Packages == nil {
		return false
	}
	_, packages := daemon.next[datasetPackages]
	_, updates := daemon.next[datasetUpdates]
	return packages || updates
}

// refreshSettings returns whether the metadata refresh runs, and its
// interval. It only runs while the updates dataset is enabled: nothing
// else reads the refreshed metadata.
func (daemon *daemon) refreshSettings() (bool, time.Duration) {
	if daemon.options.Packages == nil {
		return false, 0
	}
	if _, updates := daemon.next[datasetUpdates]; !updates {
		return false, 0
	}
	enabled, interval := daemon.settings.MetadataRefresh()
	if override := daemon.packages.timings.RefreshInterval; override > 0 {
		interval = override
	}
	return enabled, interval
}

// scheduleRefresh applies the refresh settings: a newly enabled refresh
// starts after a short random delay and a shorter interval takes effect
// at once.
func (daemon *daemon) scheduleRefresh(now time.Time) {
	work := &daemon.packages
	enabled, interval := daemon.refreshSettings()
	switch {
	case work.running:
	case !enabled:
		work.refreshAt = time.Time{}
	case work.refreshAt.IsZero():
		work.refreshAt = now.Add(daemon.options.Jitter(work.timings.RefreshStartDelay))
	case work.refreshAt.After(now.Add(interval)):
		work.refreshAt = now.Add(interval)
	}
}

// lockRetry is when to try again after a held lock: 10-15 minutes.
func (daemon *daemon) lockRetry() time.Duration {
	retry := daemon.packages.timings.LockRetry
	return retry + daemon.options.Jitter(retry/2)
}

// packagesWakeAt returns the earliest package work, or zero.
func (daemon *daemon) packagesWakeAt() time.Time {
	var wakeAt time.Time
	if daemon.packagesActive() {
		wakeAt = daemon.packages.pollAt
	}
	if at := daemon.packages.refreshAt; !at.IsZero() && (wakeAt.IsZero() || at.Before(wakeAt)) {
		wakeAt = at
	}
	return wakeAt
}

// tickPackages polls the package database and starts a due refresh.
func (daemon *daemon) tickPackages(ctx context.Context, now time.Time) {
	work := &daemon.packages
	if daemon.packagesActive() && !now.Before(work.pollAt) {
		daemon.pollPackageActivity(now)
	}
	if !work.refreshAt.IsZero() && !now.Before(work.refreshAt) {
		daemon.startRefresh(ctx, now)
	}
}

// pollPackageActivity stats the package database. After a change it waits
// until the database has been idle for timings.Idle and no lock is held
// (or a lock has outlasted timings.LockPatience), then makes packages and
// updates due. A long upgrade keeps moving the database and holding the
// lock, so it is collected once, after it finished.
func (daemon *daemon) pollPackageActivity(now time.Time) {
	work := &daemon.packages
	work.pollAt = now.Add(work.timings.Poll)
	// Every poll observes the locks, so their held time is accurate for
	// the patience check here and in the updates collector.
	lock, heldFor, held := daemon.options.Packages.BusyLock()
	token := daemon.options.Packages.DatabaseToken()
	switch {
	case !work.seen:
		work.seen, work.token = true, token
	case token != work.token:
		work.token = token
		if !work.pending {
			log.Printf("[packages] package database changed; collecting packages and updates once it has been idle for %s", work.timings.Idle)
		}
		work.pending, work.changedAt, work.waiting = true, now, ""
	}
	if !work.pending || now.Sub(work.changedAt) < work.timings.Idle {
		return
	}
	if held && heldFor < work.timings.LockPatience {
		if work.waiting != lock {
			log.Printf("[packages] package database is idle, but %s is held; waiting for it", lock)
			work.waiting = lock
		}
		return
	}
	if held {
		log.Printf("[packages] %s has been held for %s; collecting packages and updates anyway", lock, heldFor.Round(time.Second))
	} else {
		log.Printf("[packages] package activity finished; collecting packages and updates now")
	}
	work.pending, work.waiting = false, ""
	for _, name := range []string{datasetPackages, datasetUpdates} {
		if _, enabled := daemon.next[name]; enabled {
			daemon.next[name] = now
		}
	}
}

// startRefresh runs a due metadata refresh in the background, unless the
// cache is fresh enough or a package manager holds a lock.
func (daemon *daemon) startRefresh(ctx context.Context, now time.Time) {
	work := &daemon.packages
	enabled, interval := daemon.refreshSettings()
	if !enabled {
		work.refreshAt = time.Time{}
		return
	}
	period := normalJitter(interval, JitterFraction, daemon.options.Jitter)
	if cache := daemon.options.Packages.CacheUpdatedAt(); cache != nil && now.Sub(*cache) < period {
		// A cache from the future (a clock that jumped back) counts as new.
		from := *cache
		if from.After(now) {
			from = now
		}
		work.refreshAt = later(from.Add(period), now.Add(time.Minute))
		log.Printf("[packages] metadata refresh skipped: the cache is from %s, younger than %s; next check in %s",
			cache.UTC().Format(time.RFC3339), interval, work.refreshAt.Sub(now).Round(time.Second))
		return
	}
	if lock, _, held := daemon.options.Packages.BusyLock(); held {
		work.refreshAt = now.Add(daemon.lockRetry())
		log.Printf("[packages] metadata refresh skipped: %s is held; retrying in %s", lock, work.refreshAt.Sub(now).Round(time.Second))
		return
	}

	work.running, work.refreshAt, work.startedAt = true, time.Time{}, now
	timeout := work.timings.RefreshTimeout
	packages := daemon.options.Packages
	done := work.done
	go func() {
		refreshContext, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		done <- packages.RefreshMetadata(refreshContext)
	}()
	log.Printf("[packages] refreshing package metadata (nice 19, idle IO, timeout %s)", timeout)
}

// finishRefresh schedules the next refresh and, after a success, collects
// updates now. A failure is only logged: updates still reports from the
// existing cache, so it is never a dataset error.
func (daemon *daemon) finishRefresh(err error, now time.Time) {
	work := &daemon.packages
	work.running = false
	enabled, interval := daemon.refreshSettings()
	switch {
	case err == nil:
		work.failures = 0
		work.refreshAt = now.Add(normalJitter(interval, JitterFraction, daemon.options.Jitter))
		log.Printf("[packages] package metadata refreshed in %s; collecting updates now", now.Sub(work.startedAt).Round(time.Second))
		if record := daemon.options.RecordRefresh; record != nil {
			if err := record(now); err != nil {
				log.Printf("[packages] record the refresh: %v", err)
			}
		}
		if _, updates := daemon.next[datasetUpdates]; updates {
			daemon.next[datasetUpdates] = now
			// The refresh moved the cache: do not collect a second time when
			// the change poll notices.
			if instance, ok := daemon.options.Registry.Get(datasetUpdates); ok {
				if detector, detects := instance.(collector.ChangeDetector); detects {
					daemon.changeTokens[datasetUpdates] = detector.ChangeToken()
				}
			}
		}
	case errors.Is(err, collector.ErrPackageManagerBusy):
		work.refreshAt = now.Add(daemon.lockRetry())
		log.Printf("[packages] metadata refresh skipped: %v; retrying in %s", err, work.refreshAt.Sub(now).Round(time.Second))
	default:
		work.failures++
		backoff := interval
		for range work.failures - 1 {
			if backoff >= maximumRefreshBackoff {
				break
			}
			backoff *= 2
		}
		backoff = max(min(backoff, maximumRefreshBackoff), interval)
		work.refreshAt = now.Add(normalJitter(backoff, JitterFraction, daemon.options.Jitter))
		log.Printf("[packages] metadata refresh failed (%d in a row): %v; retrying in %s", work.failures, err, work.refreshAt.Sub(now).Round(time.Minute))
	}
	if !enabled {
		work.refreshAt = time.Time{}
	}
}

// awaitRefresh lets a refresh that is being cancelled exit before the
// daemon returns.
func (daemon *daemon) awaitRefresh() {
	if !daemon.packages.running {
		return
	}
	timer := time.NewTimer(refreshShutdownWait)
	defer timer.Stop()
	select {
	case <-daemon.packages.done:
		daemon.packages.running = false
	case <-timer.C:
	}
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
