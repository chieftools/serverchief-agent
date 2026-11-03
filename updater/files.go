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
	"syscall"
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

// ValidCommandID reports whether id is a command ID the helpers accept.
func ValidCommandID(id string) bool { return commandIDPattern.MatchString(id) }

// nestedSuffix marks the agent update an upgrade_packages run carries out
// itself (NestedCommandID). Its request, readiness marker and result use
// the updater's files like any update, but no update_agent command has the
// ID: the daemon writes the readiness marker for it and leaves its result
// to the package upgrade helper.
const nestedSuffix = "-agent"

// NestedCommandID is the update ID of the agent update an upgrade_packages
// command runs first.
func NestedCommandID(commandID string) string { return commandID + nestedSuffix }

// IsNestedCommandID reports whether id is a NestedCommandID.
func IsNestedCommandID(id string) bool {
	return len(id) > len(nestedSuffix) && id[len(id)-len(nestedSuffix):] == nestedSuffix
}

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
	// Owner is the UID every file must belong to, the owner of Directory:
	// the serverchief user, or root where the daemon runs as root. Files
	// root writes are handed to it. -1 skips the check (tests).
	Owner int
}

// NewStore returns the store of the packaged agent, for the root update
// helper.
func NewStore() Store {
	owner := 0
	if info, err := os.Lstat(DefaultDirectory); err == nil && info.IsDir() {
		if uid, ok := fileOwner(info); ok {
			owner = uid
		}
	}
	return Store{Directory: DefaultDirectory, Owner: owner}
}

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

// ReadJSON decodes name like the updater's own files, for the package
// upgrade helper's files in the same directory.
func (store Store) ReadJSON(name string, target any) (bool, error) { return store.read(name, target) }

// WriteJSON writes name like the updater's own files.
func (store Store) WriteJSON(name string, value any) error { return store.write(name, value) }

// Remove deletes name; a missing file is not an error.
func (store Store) Remove(name string) error { return store.remove(name) }

// ReadText returns the text file name, at most limit bytes, with the same
// checks as the JSON files. found is false when it does not exist.
func (store Store) ReadText(name string, limit int64) (string, bool, error) {
	data, found, err := store.readBytes(name, limit)
	return string(data), found, err
}

// WriteText writes the text file name like the JSON files.
func (store Store) WriteText(name, text string) error {
	if err := inventory.WriteFileAtomic(store.path(name), []byte(text), 0o600); err != nil {
		return err
	}
	if os.Geteuid() == 0 && store.Owner > 0 {
		return os.Lchown(store.path(name), store.Owner, -1)
	}
	return nil
}

// readBytes reads name. It refuses anything but a regular file of at most
// limit bytes that only its owner can access.
func (store Store) readBytes(name string, limit int64) ([]byte, bool, error) {
	path := store.path(name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > limit {
		return nil, false, fmt.Errorf("%s is not a private regular file", path)
	}
	if store.Owner >= 0 {
		if uid, ok := fileOwner(info); ok && uid != store.Owner {
			return nil, false, fmt.Errorf("%s is owned by uid %d", path, uid)
		}
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	// The file seen by Lstat must be the file opened
	if opened, err := file.Stat(); err != nil || !os.SameFile(info, opened) {
		return nil, false, fmt.Errorf("%s changed while it was read", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return nil, false, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return data, true, nil
}

// read decodes name strictly. It refuses anything but a small regular
// file that only its owner can access.
func (store Store) read(name string, target any) (bool, error) {
	path := store.path(name)
	data, found, err := store.readBytes(name, maximumFileBytes)
	if err != nil || !found {
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
	if err := inventory.WriteFileAtomic(store.path(name), append(data, '\n'), 0o600); err != nil {
		return err
	}
	// Root hands its files to the unprivileged daemon that reads them;
	// Lchown never follows a link that replaced the file meanwhile.
	if os.Geteuid() == 0 && store.Owner > 0 {
		return os.Lchown(store.path(name), store.Owner, -1)
	}
	return nil
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
