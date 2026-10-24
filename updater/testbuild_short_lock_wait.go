//go:build serverchief_e2e_short_lock_wait

package updater

import "time"

// A test build whose helper gives up on a held package-manager lock after
// 30 seconds instead of an hour.
func init() {
	e2eLockWait = 30 * time.Second
	e2eLockPoll = 5 * time.Second
}
