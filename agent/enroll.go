package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.chief.tools/serverchief-agent/collector"
	"go.chief.tools/serverchief-agent/initsys"
	"go.chief.tools/serverchief-agent/inventory"
)

// ErrNotEnrolled means identity.json does not exist.
var ErrNotEnrolled = errors.New("not enrolled, run `serverchief setup <token>`")

// ManualStartInstructions explains how to run the agent without a managed
// service.
const ManualStartInstructions = `Start the agent yourself and keep it running, for example:
  serverchief run --log-file /var/log/serverchief/agent.log
Install the serverchief package to get a systemd unit or SysV init script.`

// ServiceLookup finds the manager of the agent service. It returns a nil
// manager and the reason when the host has no supported init system or the
// service is not installed.
type ServiceLookup func() (initsys.Manager, string)

// EnrollOptions configures Enroll. Every host-specific input can be set,
// so enrollment is testable without root.
type EnrollOptions struct {
	Paths    Paths
	Endpoint string
	// CAFile is an extra PEM CA trusted for Endpoint; it is stored in the
	// identity so `serverchief run` trusts it too.
	CAFile  string
	Version string

	// Reported to the backend; detected when empty.
	Hostname   string
	Platform   string
	InitSystem string

	// Checked before enrolling; runtime values when empty.
	GOOS         string
	Architecture string
	RootCheck    func() bool

	HTTPClient *http.Client
	Service    ServiceLookup
	Now        func() time.Time
}

// EnrollResult describes a completed setup.
type EnrollResult struct {
	Identity   Identity
	Reenrolled bool
	// Service is the init system that runs the agent, or "" when the
	// service could not be managed. ManualStart then says why.
	Service     initsys.Kind
	ManualStart string
	// ServiceErr is set when the service exists but failed to start.
	ServiceErr error
}

// Enroll exchanges a one-time setup token for a server identity, writes
// identity.json and starts the agent service:
//
//  1. Journal the attempt ID and a fresh credential in Paths.Pending
//     (0600) before calling the backend, so a retried setup reuses both and
//     the backend can replay its answer.
//  2. POST /enroll with the token in the body. The backend stores only
//     SHA-256(credential).
//  3. Write Paths.Identity (0600), cache the configuration from the
//     response, reset the delivery state so every dataset is sent in full,
//     and remove the journal.
//  4. Enable and start (or restart) the service through initsys.
func Enroll(ctx context.Context, token string, options EnrollOptions) (EnrollResult, error) {
	if options.RootCheck == nil {
		options.RootCheck = func() bool { return os.Geteuid() == 0 }
	}
	if !options.RootCheck() {
		return EnrollResult{}, fmt.Errorf("setup must run as root")
	}
	if options.GOOS == "" {
		options.GOOS = runtime.GOOS
	}
	if options.Architecture == "" {
		options.Architecture = runtime.GOARCH
	}
	if options.GOOS != "linux" || options.Architecture != "amd64" && options.Architecture != "arm64" {
		return EnrollResult{}, fmt.Errorf("setup requires Linux on amd64 or arm64, not %s/%s", options.GOOS, options.Architecture)
	}
	token = strings.TrimSpace(token)
	if err := ValidateEnrollmentToken(token); err != nil {
		return EnrollResult{}, err
	}
	if options.Endpoint == "" {
		options.Endpoint = DefaultEndpoint
	}
	options.Endpoint = strings.TrimRight(options.Endpoint, "/")
	if !validEndpoint(options.Endpoint) {
		return EnrollResult{}, fmt.Errorf("endpoint must be an https:// URL")
	}
	if options.CAFile != "" {
		absolute, err := filepath.Abs(options.CAFile)
		if err != nil {
			return EnrollResult{}, fmt.Errorf("CA file: %w", err)
		}
		options.CAFile = absolute
	}
	if options.HTTPClient == nil {
		client, err := NewHTTPClient(options.CAFile)
		if err != nil {
			return EnrollResult{}, err
		}
		options.HTTPClient = client
	} else if options.CAFile != "" {
		if _, err := TLSConfig(options.CAFile); err != nil {
			return EnrollResult{}, err
		}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Service == nil {
		options.Service = DetectService(os.DirFS("/"), nil)
	}
	fillHostDetails(&options)

	tokenHash := digest(token)
	previous, err := LoadIdentity(options.Paths.Identity)
	reenrolling := err == nil
	if err != nil && !errors.Is(err, ErrNotEnrolled) {
		return EnrollResult{}, fmt.Errorf("load the installed identity: %w", err)
	}
	if reenrolling && previous.SetupTokenHash == tokenHash && previous.Endpoint == options.Endpoint {
		// This token already completed; only make sure the service runs.
		if err := removeIfExists(options.Paths.Pending); err != nil {
			return EnrollResult{}, err
		}
		result := EnrollResult{Identity: previous}
		startService(ctx, options.Service, true, &result)
		return result, result.serviceError()
	}

	pending, err := loadPending(options.Paths.Pending)
	if err != nil {
		log.Printf("[setup] ignoring unreadable %s: %v", options.Paths.Pending, err)
	}
	if err != nil || pending.TokenHash != tokenHash || pending.Endpoint != options.Endpoint {
		pending, err = newPending(options.Endpoint, tokenHash, options.Now())
		if err != nil {
			return EnrollResult{}, err
		}
		if reenrolling {
			pending.PreviousServerID = previous.ServerID
			pending.PreviousGeneration = previous.Generation
		}
		if err := writeJSON(options.Paths.Pending, pending); err != nil {
			return EnrollResult{}, fmt.Errorf("save pending setup: %w", err)
		}
	}

	request := EnrollmentRequest{
		ProtocolRevision: ProtocolRevision,
		Token:            token,
		AttemptID:        pending.AttemptID,
		Credential:       pending.Credential,
		AgentVersion:     normalizeVersion(options.Version),
		Hostname:         options.Hostname,
		Platform:         options.Platform,
		Architecture:     options.Architecture,
		InitSystem:       options.InitSystem,
	}
	if pending.PreviousServerID != "" {
		request.PreviousServerID = &pending.PreviousServerID
		request.PreviousGeneration = &pending.PreviousGeneration
	}
	client := NewClient(options.Endpoint, "", options.Version, options.HTTPClient)
	response, err := client.Enroll(ctx, request)
	if err != nil {
		return EnrollResult{}, enrollmentError(err)
	}
	if response.Endpoint != "" && strings.TrimRight(response.Endpoint, "/") != options.Endpoint {
		log.Printf("[setup] the server calls its endpoint %s; keeping %s, which this setup used", response.Endpoint, options.Endpoint)
	}

	manager, _ := options.Service()
	if reenrolling && manager != nil {
		// The running agent still holds the old credential and state.
		if err := manager.Stop(ctx); err != nil {
			log.Printf("[setup] stopping the running agent: %v", err)
		}
	}

	identity := Identity{
		ProtocolRevision: ProtocolRevision,
		Endpoint:         options.Endpoint,
		CAFile:           options.CAFile,
		ServerID:         response.ServerID,
		Generation:       response.Generation,
		Credential:       pending.Credential,
		Hostname:         options.Hostname,
		SetupTokenHash:   tokenHash,
		EnrolledAt:       options.Now().UTC().Truncate(time.Second),
	}
	if err := writeJSON(options.Paths.Identity, identity); err != nil {
		return EnrollResult{}, fmt.Errorf("save identity: %w", err)
	}
	if err := shareWithAgentGroup(options.Paths.Identity); err != nil {
		return EnrollResult{}, fmt.Errorf("save identity: %w", err)
	}
	if err := resetDeliveryState(options.Paths, response.ServerConfig()); err != nil {
		return EnrollResult{}, err
	}
	if err := removeIfExists(options.Paths.Pending); err != nil {
		return EnrollResult{}, err
	}

	result := EnrollResult{Identity: identity, Reenrolled: reenrolling}
	startService(ctx, options.Service, reenrolling, &result)
	return result, result.serviceError()
}

// serviceError reports a service that exists but failed to start. A host
// without a manageable service is not an error.
func (result *EnrollResult) serviceError() error {
	if result.ServiceErr != nil {
		return fmt.Errorf("server %s is enrolled, but the service did not start: %w", result.Identity.ServerID, result.ServiceErr)
	}
	return nil
}

func startService(ctx context.Context, lookup ServiceLookup, restart bool, result *EnrollResult) {
	manager, reason := lookup()
	if manager == nil {
		result.ManualStart = reason
		return
	}
	if err := manager.Enable(ctx); err != nil {
		result.ServiceErr = fmt.Errorf("enable: %w", err)
		return
	}
	start := manager.Start
	if restart {
		start = manager.Restart
	}
	if err := start(ctx); err != nil {
		result.ServiceErr = fmt.Errorf("start: %w", err)
		return
	}
	result.Service = manager.Kind()
}

// DetectService returns a ServiceLookup for the host rooted at fsys:
// systemd when it runs and serverchief.service is installed, SysV (also on
// Upstart) when /etc/init.d/serverchief exists.
func DetectService(fsys fs.FS, runner initsys.Runner) ServiceLookup {
	return func() (initsys.Manager, string) {
		kind := initsys.Detect(fsys)
		switch kind {
		case initsys.Systemd:
			for _, unit := range []string{"etc/systemd/system", "lib/systemd/system", "usr/lib/systemd/system"} {
				if _, err := fs.Stat(fsys, unit+"/"+initsys.DefaultService+".service"); err == nil {
					manager, err := initsys.New(kind, fsys, initsys.DefaultService, runner)
					if err != nil {
						return nil, err.Error()
					}
					return manager, ""
				}
			}
			return nil, "systemd is running, but serverchief.service is not installed"
		case initsys.SysV, initsys.Upstart:
			for _, script := range []string{"etc/init.d/", "etc/rc.d/init.d/"} {
				if _, err := fs.Stat(fsys, script+initsys.DefaultService); err == nil {
					manager, err := initsys.New(kind, fsys, initsys.DefaultService, runner)
					if err != nil {
						return nil, err.Error()
					}
					return manager, ""
				}
			}
			return nil, "/etc/init.d/serverchief is not installed"
		default:
			return nil, "no supported init system (systemd or SysV) was found"
		}
	}
}

// ValidateEnrollmentToken checks the scenr_ prefix and the base62 body.
// The protocol forbids rejecting a token on its exact length (the backend
// looks tokens up by hash), so only absurd lengths are refused.
func ValidateEnrollmentToken(token string) error {
	body, ok := strings.CutPrefix(token, EnrollmentTokenPrefix)
	if !ok || body == "" || len(body) > 256 {
		return fmt.Errorf("setup token must start with %s followed by letters and digits", EnrollmentTokenPrefix)
	}
	for _, character := range body {
		if (character < '0' || character > '9') && (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') {
			return fmt.Errorf("setup token contains %q; copy it again from the dashboard", character)
		}
	}
	return nil
}

func enrollmentError(err error) error {
	var problem *ProblemError
	if !errors.As(err, &problem) {
		return fmt.Errorf("enrollment failed: %w", err)
	}
	switch {
	case problem.Status == http.StatusUnauthorized:
		return fmt.Errorf("enrollment failed: %w; create a new token in the Server Chief dashboard", err)
	case problem.Type == ProblemEnrollmentConflict || problem.Status == http.StatusConflict:
		return fmt.Errorf("enrollment failed: %w; this token was already used by a different setup attempt, so create a new token", err)
	default:
		return fmt.Errorf("enrollment failed: %w", err)
	}
}

// LoadIdentity reads identity.json, returning ErrNotEnrolled when it does
// not exist.
func LoadIdentity(path string) (Identity, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Identity{}, ErrNotEnrolled
	}
	if err != nil {
		return Identity{}, fmt.Errorf("read %s: %w", path, err)
	}
	var identity Identity
	if err := json.Unmarshal(data, &identity); err != nil {
		return Identity{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if !validEndpoint(identity.Endpoint) || identity.ServerID == "" || identity.Generation == 0 || !strings.HasPrefix(identity.Credential, CredentialPrefix) {
		return Identity{}, fmt.Errorf("%s is incomplete; run `serverchief setup <token>` again", path)
	}
	return identity, nil
}

// resetDeliveryState caches the enrollment's configuration and clears the
// sequence number and acknowledged hashes, as the backend does for a new
// generation, so the new identity starts at seq 1 with full payloads.
func resetDeliveryState(paths Paths, config ServerConfig) error {
	if paths.ServerConfig != "" {
		if err := SaveServerConfig(paths.ServerConfig, config); err != nil {
			return fmt.Errorf("cache server configuration: %w", err)
		}
	}
	if paths.State != "" {
		store, err := inventory.OpenStateStore(paths.State)
		if err != nil {
			return fmt.Errorf("open delivery state: %w", err)
		}
		if err := store.Clear(); err != nil {
			return fmt.Errorf("reset delivery state: %w", err)
		}
	}
	if paths.Payloads != "" {
		if err := inventory.NewPayloadStore(paths.Payloads).Clear(); err != nil {
			return err
		}
	}
	return nil
}

func fillHostDetails(options *EnrollOptions) {
	if options.Hostname == "" {
		options.Hostname, _ = os.Hostname()
	}
	if options.Platform == "" {
		release := collector.NewHost("/").OSRelease()
		version := release.VersionFull
		if version == "" {
			version = release.Version
		}
		options.Platform = strings.TrimSpace(release.ID + " " + version)
		if options.Platform == "" {
			options.Platform = options.GOOS
		}
	}
	if options.InitSystem == "" {
		options.InitSystem = string(initsys.DetectSystem())
	}
}

func loadPending(path string) (pendingEnrollment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pendingEnrollment{}, nil
		}
		return pendingEnrollment{}, err
	}
	var pending pendingEnrollment
	if err := json.Unmarshal(data, &pending); err != nil {
		return pendingEnrollment{}, err
	}
	if pending.AttemptID == "" || !strings.HasPrefix(pending.Credential, CredentialPrefix) {
		return pendingEnrollment{}, errors.New("incomplete journal")
	}
	return pending, nil
}

func newPending(endpoint, tokenHash string, now time.Time) (pendingEnrollment, error) {
	attemptID, err := newULID(now)
	if err != nil {
		return pendingEnrollment{}, err
	}
	credential, err := newCredential()
	if err != nil {
		return pendingEnrollment{}, err
	}
	return pendingEnrollment{
		Endpoint:   endpoint,
		TokenHash:  tokenHash,
		AttemptID:  attemptID,
		Credential: credential,
		CreatedAt:  now.UTC().Truncate(time.Second),
	}, nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return inventory.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// shareWithAgentGroup lets the unprivileged daemon read a file setup wrote
// as root: root:serverchief, mode 0640. Without the serverchief group (a
// daemon that runs as root, or setup outside the package) it stays 0600.
func shareWithAgentGroup(path string) error {
	group, err := user.LookupGroup(AgentGroup)
	if err != nil || os.Geteuid() != 0 {
		return nil
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil {
		return nil
	}
	if err := os.Lchown(path, 0, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o640)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// newCredential returns scagt_ plus 32 random bytes as unpadded base64url
// (43 characters).
func newCredential() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate credential: %w", err)
	}
	return CredentialPrefix + base64.RawURLEncoding.EncodeToString(value), nil
}

// newULID returns a Crockford base32 ULID with the timestamp now.
func newULID(now time.Time) (string, error) {
	value := make([]byte, 16)
	milliseconds := uint64(now.UTC().UnixMilli())
	for index := range 6 {
		value[index] = byte(milliseconds >> (8 * (5 - index)))
	}
	if _, err := rand.Read(value[6:]); err != nil {
		return "", fmt.Errorf("generate enrollment attempt: %w", err)
	}
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	number := new(big.Int).SetBytes(value)
	base := big.NewInt(32)
	encoded := make([]byte, 26)
	for index := len(encoded) - 1; index >= 0; index-- {
		remainder := new(big.Int)
		number.QuoRem(number, base, remainder)
		encoded[index] = alphabet[remainder.Int64()]
	}
	return string(encoded), nil
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// PrintEnrollResult writes the human summary of a setup.
func PrintEnrollResult(writer io.Writer, result EnrollResult) {
	action := "enrolled"
	if result.Reenrolled {
		action = "re-enrolled"
	}
	fmt.Fprintf(writer, "This server is %s in Server Chief as %s (generation %d).\n", action, result.Identity.ServerID, result.Identity.Generation)
	if result.Service != "" {
		fmt.Fprintf(writer, "The serverchief service is enabled and running (%s).\n", result.Service)
		return
	}
	if result.ManualStart != "" {
		fmt.Fprintf(writer, "The service was not started: %s.\n%s\n", result.ManualStart, ManualStartInstructions)
	}
}
