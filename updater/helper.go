package updater

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"
)

// Timings of the helper. Zero fields take these defaults.
const (
	// DefaultLockWait is how long the helper waits for a package manager
	// that holds its lock before it reports package_manager_busy.
	DefaultLockWait = time.Hour
	// DefaultLockPoll is how often the locks are checked while waiting.
	DefaultLockPoll = 30 * time.Second
	// DefaultReadyTimeout is how long the helper waits for the daemon's
	// readiness marker after an install.
	DefaultReadyTimeout = 2 * time.Minute
	// DefaultReadyPoll is how often the marker is checked.
	DefaultReadyPoll = time.Second
)

// HelperOptions configures RunHelper.
type HelperOptions struct {
	Store Store
	// Manager drives the package manager. Nil detects it.
	Manager PackageManager
	// Locks reports a held package-manager lock (pkglock.Inspector.Busy).
	Locks func() (string, bool)

	LockWait     time.Duration
	LockPoll     time.Duration
	ReadyTimeout time.Duration
	ReadyPoll    time.Duration

	Now func() time.Time
	// Sleep waits for d or until ctx ends.
	Sleep func(ctx context.Context, d time.Duration) error
	// Alive reports whether the daemon that wrote a readiness marker
	// still runs. A marker from a daemon the install stopped is stale.
	Alive func(pid int) bool
}

func (options *HelperOptions) defaults() {
	defaults := func(value *time.Duration, fallback time.Duration) {
		if *value <= 0 {
			*value = fallback
		}
	}
	defaults(&options.LockWait, lockWaitDefault())
	defaults(&options.LockPoll, lockPollDefault())
	defaults(&options.ReadyTimeout, DefaultReadyTimeout)
	defaults(&options.ReadyPoll, DefaultReadyPoll)
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Sleep == nil {
		options.Sleep = sleep
	}
	if options.Locks == nil {
		options.Locks = func() (string, bool) { return "", false }
	}
	if options.Alive == nil {
		options.Alive = processAlive
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// helper runs one request through the phases.
type helper struct {
	options HelperOptions
	request Request
	state   state
	manager PackageManager
}

// RunHelper carries out the pending request, resuming from the persisted
// state. It is idempotent: without a request, or with a result already
// written for it, it only cleans up. Cancelling ctx (SIGTERM) stops a
// wait, never a running package manager, and leaves the state for the
// next run. The caller holds the helper lock.
func RunHelper(ctx context.Context, options HelperOptions) error {
	options.defaults()
	store := options.Store
	request, err := store.ReadRequest()
	if err != nil {
		return fmt.Errorf("read the update request: %w", err)
	}
	if request == nil {
		log.Printf("[update] no pending update request")
		return nil
	}
	if result, err := store.ReadResult(); err == nil && result != nil && result.CommandID == request.CommandID {
		log.Printf("[update] %s already finished with %s; cleaning up", request.CommandID, result.Code)
		return cleanup(store)
	}

	run := &helper{options: options, request: *request, manager: options.Manager}
	current, err := store.readState()
	if err != nil {
		log.Printf("[update] %v; starting over", err)
	}
	if current != nil && current.CommandID == request.CommandID {
		run.state = *current
		log.Printf("[update] resuming %s (%s → %s) in phase %s", request.CommandID, request.PreviousVersion, request.TargetVersion, current.Phase)
	} else {
		run.state = state{
			CommandID: request.CommandID, Phase: phaseWaiting,
			PreviousVersion: request.PreviousVersion, TargetVersion: request.TargetVersion,
			WaitStartedAt: options.Now().UTC(),
		}
		log.Printf("[update] starting %s: %s → %s", request.CommandID, request.PreviousVersion, request.TargetVersion)
	}
	run.state.Starts++
	if err := run.save(); err != nil {
		return err
	}
	if run.manager == nil {
		manager, err := DetectManager(Tools{}, nil)
		if err != nil {
			return run.finish(CodeUnsupportedInstall, err.Error(), run.request.PreviousVersion)
		}
		run.manager = manager
	}
	if run.state.Starts > maximumHelperStarts {
		return run.giveUp(ctx)
	}
	return run.loop(ctx)
}

func (run *helper) save() error {
	run.state.UpdatedAt = run.options.Now().UTC()
	if err := run.options.Store.writeState(run.state); err != nil {
		return fmt.Errorf("save the update state: %w", err)
	}
	return nil
}

func (run *helper) enter(phase, cause string) error {
	log.Printf("[update] %s: %s → %s", run.request.CommandID, run.state.Phase, phase)
	run.state.Phase = phase
	if cause != "" {
		run.state.Cause = cause
	}
	return run.save()
}

// installed queries the package database, never with ctx's cancellation:
// the answer decides what happens next.
func (run *helper) installed(ctx context.Context) (Installed, error) {
	return run.manager.Installed(context.WithoutCancel(ctx))
}

func (run *helper) loop(ctx context.Context) error {
	previous, target := run.request.PreviousVersion, run.request.TargetVersion
	for {
		if err := ctx.Err(); err != nil {
			log.Printf("[update] stopping in phase %s; the next run resumes from here", run.state.Phase)
			return err
		}
		switch run.state.Phase {
		case phaseWaiting:
			if done, err := run.waitForPackageManager(ctx); done || err != nil {
				return err
			}
			if err := run.enter(phasePreflight, ""); err != nil {
				return err
			}
		case phasePreflight:
			next, err := run.preflight(ctx)
			if err != nil || next == "" {
				return err
			}
			if next == phaseInstalling {
				// A marker from before this install proves nothing.
				_ = run.options.Store.removeReadiness()
			}
			if err := run.enter(next, ""); err != nil {
				return err
			}
		case phaseInstalling:
			next, cause, err := run.install(ctx, target, previous)
			if err != nil || next == "" {
				return err
			}
			if err := run.enter(next, cause); err != nil {
				return err
			}
		case phaseVerifying:
			if err := run.waitReady(ctx, target); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				_ = run.options.Store.removeReadiness()
				if err := run.enter(phaseRollingBack, err.Error()); err != nil {
					return err
				}
				continue
			}
			return run.finish(CodeUpdated, "", target)
		case phaseRollingBack:
			done, err := run.rollback(ctx)
			if done || err != nil {
				return err
			}
			if err := run.enter(phaseVerifyingRollback, ""); err != nil {
				return err
			}
		case phaseVerifyingRollback:
			if err := run.waitReady(ctx, previous); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return run.finish(CodeRollbackFailed, run.cause("rolled back to "+previous+", but "+err.Error()), run.installedVersion(ctx))
			}
			return run.finish(CodeReadinessFailedRolledBack, run.cause("rolled back to "+previous), previous)
		default:
			log.Printf("[update] unknown phase %q; starting over", run.state.Phase)
			run.state.Phase = phaseWaiting
		}
	}
}

// waitForPackageManager waits until no package-manager lock is held. It
// finishes with package_manager_busy (done) once the wait, counted from
// the first attempt, exceeds LockWait.
func (run *helper) waitForPackageManager(ctx context.Context) (bool, error) {
	options := run.options
	deadline := run.state.WaitStartedAt.Add(options.LockWait)
	logged := ""
	for {
		lock, held := options.Locks()
		if !held {
			return false, nil
		}
		if !options.Now().Before(deadline) {
			detail := fmt.Sprintf("%s was still held after waiting %s", lock, options.LockWait)
			return true, run.finish(CodePackageManagerBusy, detail, run.request.PreviousVersion)
		}
		if lock != logged {
			log.Printf("[update] waiting for the package manager: %s is held (giving up at %s)", lock, deadline.UTC().Format(time.RFC3339))
			logged = lock
		}
		if err := options.Sleep(ctx, min(options.LockPoll, max(deadline.Sub(options.Now()), time.Millisecond))); err != nil {
			return false, err
		}
	}
}

// preflight refreshes the repository and checks that it offers both the
// target and the installed version (for a rollback). It returns the next
// phase, or "" after finishing.
func (run *helper) preflight(ctx context.Context) (string, error) {
	previous, target := run.request.PreviousVersion, run.request.TargetVersion
	installed, err := run.installed(ctx)
	if err != nil {
		return "", run.finish(CodeInstallFailed, "query the installed package: "+err.Error(), previous)
	}
	switch {
	case installed.Version == target && installed.Intact:
		// Already there (installed by someone else); confirm it runs.
		return phaseVerifying, nil
	case installed.Version != previous || !installed.Intact:
		detail := fmt.Sprintf("the installed serverchief package is %q, not the running version %s", installed.Version, previous)
		return "", run.finish(CodeUnsupportedInstall, detail, installed.Version)
	}

	packageContext := context.WithoutCancel(ctx)
	refreshErr := run.manager.Refresh(packageContext)
	if errors.Is(refreshErr, ErrBusy) {
		return phaseWaiting, nil
	}
	if refreshErr != nil {
		// The lists from an earlier refresh may still have the versions.
		log.Printf("[update] refreshing the Server Chief repository failed, checking its existing metadata: %v", refreshErr)
	} else {
		log.Printf("[update] refreshed the Server Chief repository's metadata")
	}
	versions, err := run.manager.Available(packageContext)
	if errors.Is(err, ErrBusy) {
		return phaseWaiting, nil
	}
	if err != nil {
		return "", run.finish(CodeTargetUnavailable, "query the Server Chief repository: "+err.Error(), previous)
	}
	suffix := ""
	if refreshErr != nil {
		suffix = " (refresh failed: " + refreshErr.Error() + ")"
	}
	if !slices.Contains(versions, target) {
		return "", run.finish(CodeTargetUnavailable, "the Server Chief repository does not offer "+target+suffix, previous)
	}
	if !slices.Contains(versions, previous) {
		return "", run.finish(CodeRollbackUnavailable, "the Server Chief repository no longer offers the installed version "+previous+", which a rollback needs"+suffix, previous)
	}
	log.Printf("[update] preflight passed: the repository offers %s and %s", target, previous)
	return phaseInstalling, nil
}

// install installs the target. A resumed run first looks at what the
// interrupted one left behind. It returns the next phase and the cause
// of a rollback, or "" after finishing.
func (run *helper) install(ctx context.Context, target, previous string) (string, string, error) {
	before, err := run.installed(ctx)
	if err != nil {
		return phaseRollingBack, "query the installed package: " + err.Error(), nil
	}
	switch {
	case before.Version == target && before.Intact:
		return phaseVerifying, "", nil
	case before.Version != previous || !before.Intact:
		return phaseRollingBack, fmt.Sprintf("an interrupted install left serverchief %q (intact: %t)", before.Version, before.Intact), nil
	}

	log.Printf("[update] installing serverchief %s", target)
	installErr := run.manager.Install(context.WithoutCancel(ctx), target, previous)
	after, err := run.installed(ctx)
	if err != nil {
		return phaseRollingBack, "query the installed package: " + err.Error(), nil
	}
	switch {
	case installErr == nil && after.Version == target && after.Intact:
		log.Printf("[update] serverchief %s installed; waiting up to %s for the agent to report ready", target, run.options.ReadyTimeout)
		return phaseVerifying, "", nil
	case installErr != nil && after.Version == previous && after.Intact:
		if errors.Is(installErr, ErrBusy) {
			// Someone took the lock between the check and the install.
			return phaseWaiting, "", nil
		}
		return "", "", run.finish(CodeInstallFailed, installErr.Error(), previous)
	case installErr != nil:
		return phaseRollingBack, installErr.Error(), nil
	default:
		return phaseRollingBack, fmt.Sprintf("the package manager succeeded but serverchief %q is installed (intact: %t)", after.Version, after.Intact), nil
	}
}

// rollback installs the previous version again. done means it finished.
func (run *helper) rollback(ctx context.Context) (bool, error) {
	previous := run.request.PreviousVersion
	installed, err := run.installed(ctx)
	if err == nil && installed.Version == previous && installed.Intact {
		// Rolled back already (a resumed run): its marker counts.
		return false, nil
	}
	_ = run.options.Store.removeReadiness()
	log.Printf("[update] rolling back to serverchief %s: %s", previous, run.state.Cause)
	deadline := run.options.Now().Add(run.options.LockWait)
	for {
		err = run.manager.Install(context.WithoutCancel(ctx), previous, installed.Version)
		if !errors.Is(err, ErrBusy) || !run.options.Now().Before(deadline) {
			break
		}
		log.Printf("[update] rollback: %v; retrying", err)
		if err := run.options.Sleep(ctx, run.options.LockPoll); err != nil {
			return false, err
		}
	}
	after, queryErr := run.installed(ctx)
	if err == nil && queryErr == nil && after.Version == previous && after.Intact {
		return false, nil
	}
	if err == nil {
		err = fmt.Errorf("serverchief %q is installed (intact: %t)", after.Version, after.Intact)
	}
	return true, run.finish(CodeRollbackFailed, run.cause("rollback to "+previous+" failed: "+err.Error()), after.Version)
}

// waitReady waits for a readiness marker of version for this command.
func (run *helper) waitReady(ctx context.Context, version string) error {
	options := run.options
	deadline := options.Now().Add(options.ReadyTimeout)
	for {
		readiness, err := options.Store.ReadReadiness()
		if err == nil && readiness != nil && readiness.CommandID == run.request.CommandID && readiness.Version == version && options.Alive(readiness.PID) {
			log.Printf("[update] agent %s is ready (pid %d)", version, readiness.PID)
			return nil
		}
		if !options.Now().Before(deadline) {
			return fmt.Errorf("agent %s did not report ready within %s", version, options.ReadyTimeout)
		}
		if err := options.Sleep(ctx, options.ReadyPoll); err != nil {
			return err
		}
	}
}

// giveUp ends a request whose helper keeps crashing, with what is
// installed now.
func (run *helper) giveUp(ctx context.Context) error {
	installed, _ := run.installed(ctx)
	detail := fmt.Sprintf("the update helper stopped %d times in phase %s", run.state.Starts-1, run.state.Phase)
	switch {
	case installed.Version == run.request.PreviousVersion && installed.Intact && slices.Contains([]string{phaseWaiting, phasePreflight, phaseInstalling}, run.state.Phase):
		return run.finish(CodeInstallFailed, detail, installed.Version)
	default:
		return run.finish(CodeRollbackFailed, run.cause(detail), installed.Version)
	}
}

func (run *helper) installedVersion(ctx context.Context) string {
	installed, _ := run.installed(ctx)
	return installed.Version
}

func (run *helper) cause(message string) string {
	if run.state.Cause == "" {
		return message
	}
	return message + "; cause: " + run.state.Cause
}

// finish writes the result, then removes the request, state and marker.
// The daemon reads the request before the result, so it never sees
// neither.
func (run *helper) finish(code, detail, installed string) error {
	result := Result{
		CommandID: run.request.CommandID, Code: code, Detail: truncate(detail),
		PreviousVersion: run.request.PreviousVersion, TargetVersion: run.request.TargetVersion,
		InstalledVersion: installed, FinishedAt: run.options.Now().UTC().Truncate(time.Second),
	}
	if err := run.options.Store.writeResult(result); err != nil {
		return fmt.Errorf("write the update result: %w", err)
	}
	if detail != "" {
		log.Printf("[update] %s finished: %s (installed %s): %s", run.request.CommandID, code, installed, detail)
	} else {
		log.Printf("[update] %s finished: %s (installed %s)", run.request.CommandID, code, installed)
	}
	return cleanup(run.options.Store)
}

func cleanup(store Store) error {
	if err := store.RemoveRequest(); err != nil {
		return err
	}
	_ = store.removeState()
	_ = store.removeReadiness()
	return nil
}
