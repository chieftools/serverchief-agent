package updater

import "time"

// Test-only build hooks for end-to-end runs in throwaway containers. They
// are off unless a build tag in a testbuild_*.go file turns them on;
// release builds never set those tags.
var (
	// e2eNeverReady (tag serverchief_e2e_never_ready) keeps the daemon
	// from writing the readiness marker, to exercise the rollback.
	e2eNeverReady = false
	// e2eLockWait and e2eLockPoll (tag serverchief_e2e_short_lock_wait)
	// shorten the package-manager wait.
	e2eLockWait time.Duration
	e2eLockPoll time.Duration
)

func lockWaitDefault() time.Duration {
	if e2eLockWait > 0 {
		return e2eLockWait
	}
	return DefaultLockWait
}

func lockPollDefault() time.Duration {
	if e2eLockPoll > 0 {
		return e2eLockPoll
	}
	return DefaultLockPoll
}
