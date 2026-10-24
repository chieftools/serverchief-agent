//go:build serverchief_e2e_never_ready

package updater

// A test build whose daemon never reports ready, so every update to it
// rolls back.
func init() { e2eNeverReady = true }
