package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// ErrNotImplemented is returned by features that wait on the backend.
var ErrNotImplemented = errors.New("not implemented yet")

// EnrollOptions configures Enroll. Function fields exist for tests.
type EnrollOptions struct {
	Paths        Paths
	Endpoint     string
	Version      string
	Platform     string
	Architecture string
	RootCheck    func() bool
}

// Enroll will exchange a one-time setup token for a server identity. For
// now it validates its preconditions and stops.
//
// TODO(enroll): port backupchief-agent's agent.Enroll:
//  1. Journal a pendingEnrollment in Paths.Pending (token hash, ULID
//     attempt ID, freshly generated credential) so a retried setup reuses
//     both and the backend can deduplicate.
//  2. POST /enroll with EnrollmentRequest and the token as bearer; the
//     backend stores only SHA-256(credential).
//  3. On re-enrollment, stop the service first and require the response
//     to advance Identity.Generation by exactly one.
//  4. Write Identity to Paths.Identity (0600), reset the inventory state
//     so every dataset is sent in full, remove the journal, then enable
//     and start the service through initsys.
func Enroll(_ context.Context, token string, options EnrollOptions) (Identity, error) {
	if options.RootCheck == nil {
		options.RootCheck = func() bool { return os.Geteuid() == 0 }
	}
	if !options.RootCheck() {
		return Identity{}, fmt.Errorf("setup must run as root")
	}
	if options.Platform == "" {
		options.Platform = runtime.GOOS
	}
	if options.Architecture == "" {
		options.Architecture = runtime.GOARCH
	}
	if options.Platform != "linux" || options.Architecture != "amd64" && options.Architecture != "arm64" {
		return Identity{}, fmt.Errorf("setup requires Linux on amd64 or arm64, not %s/%s", options.Platform, options.Architecture)
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, EnrollmentTokenPrefix) || len(token) <= len(EnrollmentTokenPrefix) {
		return Identity{}, fmt.Errorf("setup token must start with %s", EnrollmentTokenPrefix)
	}
	if options.Endpoint == "" {
		options.Endpoint = DefaultEndpoint
	}
	if !validEndpoint(options.Endpoint) {
		return Identity{}, fmt.Errorf("endpoint must be an https:// URL")
	}
	return Identity{}, fmt.Errorf("setup: %w", ErrNotImplemented)
}
