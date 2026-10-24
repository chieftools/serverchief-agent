package updater

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"go.chief.tools/serverchief-agent/inventory"
)

// DefaultDirectory holds the request, state, readiness and result files.
const DefaultDirectory = "/var/lib/serverchief"

// File names inside the state directory. Every file is mode 0600 and
// replaced atomically.
const (
	// RequestFile is written by the daemon before it launches the helper.
	RequestFile = "update-request.json"
	// StateFile is the helper's progress, so a crash resumes where it was.
	StateFile = "update-state.json"
	// ReadyFile is written by a daemon that started while a request was
	// pending, once it sent a successful heartbeat.
	ReadyFile = "update-ready.json"
	// ResultFile is the helper's outcome, reported by the daemon.
	ResultFile = "update-result.json"
	// LockFile is locked by the running helper for its whole lifetime.
	LockFile = "update.lock"
)

// maximumFileBytes caps every file the updater reads.
const maximumFileBytes = 16 << 10

// commandIDPattern accepts the backend's ULIDs and the IDs tests use. The
// ID never reaches a package manager.
var commandIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Request is the daemon's hand-over to the helper.
type Request struct {
	CommandID       string    `json:"command_id"`
	PreviousVersion string    `json:"previous_version"`
	TargetVersion   string    `json:"target_version"`
	RequestedAt     time.Time `json:"requested_at"`
}

// Validate checks every field; the helper trusts nothing else.
func (request Request) Validate() error {
	switch {
	case !commandIDPattern.MatchString(request.CommandID):
		return errors.New("update request: invalid command id")
	case !ValidVersion(request.PreviousVersion):
		return errors.New("update request: invalid previous version")
	case !ValidVersion(request.TargetVersion):
		return errors.New("update request: invalid target version")
	}
	return nil
}

// Readiness is written by the daemon after its startup heartbeat.
type Readiness struct {
	CommandID string    `json:"command_id"`
	Version   string    `json:"version"`
	PID       int       `json:"pid"`
	WrittenAt time.Time `json:"written_at"`
}

// Result codes, reported in the command result's data.code.
const (
	CodeUpdated                   = "updated"
	CodeInstallFailed             = "install_failed"
	CodeReadinessFailedRolledBack = "readiness_failed_rolled_back"
	CodeRollbackFailed            = "rollback_failed"
	CodeTargetUnavailable         = "target_unavailable"
	CodeRollbackUnavailable       = "rollback_unavailable"
	CodePackageManagerBusy        = "package_manager_busy"
	CodeUnsupportedInstall        = "unsupported_install"
)

// Result is the helper's outcome.
type Result struct {
	CommandID        string    `json:"command_id"`
	Code             string    `json:"code"`
	Detail           string    `json:"detail,omitempty"`
	PreviousVersion  string    `json:"previous_version"`
	TargetVersion    string    `json:"target_version"`
	InstalledVersion string    `json:"installed_version"`
	FinishedAt       time.Time `json:"finished_at"`
}

// Succeeded reports whether the update installed the target version.
func (result Result) Succeeded() bool { return result.Code == CodeUpdated }

// Helper phases, persisted in StateFile.
const (
	phaseWaiting            = "waiting_for_package_manager"
	phasePreflight          = "preflight"
	phaseInstalling         = "installing"
	phaseVerifying          = "verifying"
	phaseRollingBack        = "rolling_back"
	phaseVerifyingRollback  = "verifying_rollback"
	maximumHelperStarts     = 5
	maximumDiagnosticLength = 900
)

// state is the helper's progress for one request.
type state struct {
	CommandID       string `json:"command_id"`
	Phase           string `json:"phase"`
	PreviousVersion string `json:"previous_version"`
	TargetVersion   string `json:"target_version"`
	// WaitStartedAt bounds the package-manager wait across restarts.
	WaitStartedAt time.Time `json:"wait_started_at"`
	// Starts counts helper runs for this request; a helper that keeps
	// crashing gives up.
	Starts int `json:"starts"`
	// Cause is why a rollback started.
	Cause     string    `json:"cause,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store reads and writes the updater's files in one directory.
type Store struct {
	Directory string
	// Owner is the UID every file must belong to; -1 skips the check
	// (tests). The packaged agent runs as root.
	Owner int
}

// NewStore returns the store of the packaged agent.
func NewStore() Store { return Store{Directory: DefaultDirectory, Owner: 0} }

func (store Store) path(name string) string { return filepath.Join(store.Directory, name) }

// LockPath is the helper's lock file.
func (store Store) LockPath() string { return store.path(LockFile) }

// ReadRequest returns the pending request, or nil.
func (store Store) ReadRequest() (*Request, error) {
	var request Request
	found, err := store.read(RequestFile, &request)
	if err != nil || !found {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return &request, nil
}

// WriteRequest persists a validated request.
func (store Store) WriteRequest(request Request) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return store.write(RequestFile, request)
}

// RemoveRequest deletes the request.
func (store Store) RemoveRequest() error { return store.remove(RequestFile) }

// ReadResult returns the helper's result, or nil.
func (store Store) ReadResult() (*Result, error) {
	var result Result
	found, err := store.read(ResultFile, &result)
	if err != nil || !found {
		return nil, err
	}
	if !commandIDPattern.MatchString(result.CommandID) || result.Code == "" {
		return nil, errors.New("update result: invalid command id or code")
	}
	return &result, nil
}

// RemoveResult deletes the result when it belongs to commandID.
func (store Store) RemoveResult(commandID string) error {
	result, err := store.ReadResult()
	if err != nil {
		// An unreadable result is not reportable either.
		return store.remove(ResultFile)
	}
	if result == nil || result.CommandID != commandID {
		return nil
	}
	return store.remove(ResultFile)
}

func (store Store) writeResult(result Result) error { return store.write(ResultFile, result) }

// ReadReadiness returns the readiness marker, or nil.
func (store Store) ReadReadiness() (*Readiness, error) {
	var readiness Readiness
	found, err := store.read(ReadyFile, &readiness)
	if err != nil || !found {
		return nil, err
	}
	return &readiness, nil
}

// WriteReadiness records that this daemon is up and talking to the backend.
func (store Store) WriteReadiness(readiness Readiness) error {
	if !commandIDPattern.MatchString(readiness.CommandID) {
		return errors.New("update readiness: invalid command id")
	}
	return store.write(ReadyFile, readiness)
}

func (store Store) removeReadiness() error { return store.remove(ReadyFile) }

func (store Store) readState() (*state, error) {
	var current state
	found, err := store.read(StateFile, &current)
	if err != nil || !found {
		return nil, err
	}
	return &current, nil
}

func (store Store) writeState(current state) error { return store.write(StateFile, current) }

func (store Store) removeState() error { return store.remove(StateFile) }

// read decodes name strictly. It refuses anything but a small regular
// file that only its owner can access.
func (store Store) read(name string, target any) (bool, error) {
	path := store.path(name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maximumFileBytes {
		return false, fmt.Errorf("%s is not a private regular file", path)
	}
	if store.Owner >= 0 {
		if uid, ok := fileOwner(info); ok && uid != store.Owner {
			return false, fmt.Errorf("%s is owned by uid %d", path, uid)
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximumFileBytes+1))
	if err != nil {
		return false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return false, fmt.Errorf("decode %s: %w", path, err)
	}
	if decoder.More() {
		return false, fmt.Errorf("decode %s: trailing data", path)
	}
	return true, nil
}

func (store Store) write(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return inventory.WriteFileAtomic(store.path(name), append(data, '\n'), 0o600)
}

func (store Store) remove(name string) error {
	err := os.Remove(store.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// truncate keeps diagnostics within the result's detail limit.
func truncate(message string) string {
	runes := []rune(message)
	if len(runes) <= maximumDiagnosticLength {
		return message
	}
	return string(runes[:maximumDiagnosticLength-1]) + "…"
}
