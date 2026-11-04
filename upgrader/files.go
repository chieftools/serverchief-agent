// Package upgrader installs package updates for the upgrade_packages
// command: the request the daemon hands over, the detached helper
// (`serverchief internal-upgrade`) that runs as root, and its result.
//
// Nothing from a command reaches a package manager except the scope, a
// boolean and a version that passed updater.ValidVersion. Package names
// come from the host's own pending updates, never from the command.
package upgrader

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"go.chief.tools/serverchief-agent/execlog"
	"go.chief.tools/serverchief-agent/updater"
)

// File names inside the state directory, next to the agent update's.
const (
	// RequestFile is written by the daemon before it launches the helper.
	RequestFile = "upgrade-request.json"
	// StateFile is the helper's progress, so a crash resumes where it was.
	StateFile = "upgrade-state.json"
	// ResultFile is the helper's outcome, reported by the daemon.
	ResultFile = "upgrade-result.json"
	// OutputFile is the helper's execution log. The helper appends to it
	// as it runs, so the daemon streams it (protocol: Live output), and
	// the result carries its end.
	OutputFile = "upgrade-output.log"
	// StreamFile is how much of OutputFile the daemon has sent, so a
	// restarted daemon continues instead of sending it again.
	StreamFile = "upgrade-stream.json"
	// LockFile is locked by the running helper for its whole lifetime.
	LockFile = "upgrade.lock"
)

// outputTailBytes is how much of the end of the output file the result
// and a resumed helper read; execlog keeps at most half of it.
const outputTailBytes = 2 * execlog.MaxBytes

// Scopes of upgrade_packages.
const (
	ScopeSecurity = "security"
	ScopeAll      = "all"
)

// ValidScope reports whether scope is one of the scopes.
func ValidScope(scope string) bool { return scope == ScopeSecurity || scope == ScopeAll }

// Result codes, reported in the command result's data.code.
const (
	CodeUpgraded            = "upgraded"
	CodeNothingToUpgrade    = "nothing_to_upgrade"
	CodeUpgradeFailed       = "upgrade_failed"
	CodeDpkgInterrupted     = "dpkg_interrupted"
	CodeSecurityUnavailable = "security_unavailable"
	CodePackageManagerBusy  = "package_manager_busy"
	CodeDisabled            = "disabled"
	CodeUnsupported         = "unsupported"
	CodeTooLate             = "too_late"
)

// Command result statuses (the protocol's).
const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// MaximumServices caps data.services_restarted.
const MaximumServices = 50

// Request is the daemon's hand-over to the helper.
type Request struct {
	CommandID       string `json:"command_id"`
	Scope           string `json:"scope"`
	RestartServices bool   `json:"restart_services"`
	// AgentVersion is the agent version to update to first, "" for none.
	AgentVersion string    `json:"agent_version,omitempty"`
	RequestedAt  time.Time `json:"requested_at"`
}

// Validate checks every field; the helper trusts nothing else.
func (request Request) Validate() error {
	switch {
	case !updater.ValidCommandID(request.CommandID) || updater.IsNestedCommandID(request.CommandID):
		return errors.New("upgrade request: invalid command id")
	case !ValidScope(request.Scope):
		return errors.New("upgrade request: invalid scope")
	case request.AgentVersion != "" && !updater.ValidVersion(request.AgentVersion):
		return errors.New("upgrade request: invalid agent version")
	}
	return nil
}

// Data is the data of an upgrade_packages result
// (fixtures/upgrade-packages-result-request.json).
type Data struct {
	Code  string `json:"code"`
	Scope string `json:"scope"`
	// Upgraded and HeldBack count packages; nil when unknown.
	Upgraded *int `json:"upgraded"`
	HeldBack *int `json:"held_back"`
	// RebootRequired is nil when the host cannot tell.
	RebootRequired *bool `json:"reboot_required"`
	// ServicesRestarted is nil when services were not restarted or it
	// cannot be told.
	ServicesRestarted []string `json:"services_restarted"`
	// AgentUpdate is the agent update run first, nil when there was none.
	AgentUpdate *AgentUpdate `json:"agent_update"`
}

// AgentUpdate is the outcome of the agent update an upgrade runs first,
// with the update_agent codes.
type AgentUpdate struct {
	Code             string `json:"code"`
	PreviousVersion  string `json:"previous_version"`
	InstalledVersion string `json:"installed_version"`
}

// Result is the helper's outcome. The output is in OutputFile.
type Result struct {
	CommandID  string    `json:"command_id"`
	Status     string    `json:"status"`
	Detail     string    `json:"detail,omitempty"`
	Data       Data      `json:"data"`
	FinishedAt time.Time `json:"finished_at"`
}

// Helper phases, persisted in StateFile.
const (
	phaseWaiting   = "waiting_for_package_manager"
	phaseAudit     = "audit"
	phaseRefresh   = "refresh"
	phaseAgent     = "agent_update"
	phaseUpgrade   = "upgrading"
	phaseServices  = "services"
	phaseFinishing = "finishing"
	// maximumHelperStarts ends a request whose helper keeps crashing.
	maximumHelperStarts = 5
	// maximumDetailRunes keeps the detail within the result's limit.
	maximumDetailRunes = 900
)

// state is the helper's progress for one request.
type state struct {
	CommandID string `json:"command_id"`
	Phase     string `json:"phase"`
	Starts    int    `json:"starts"`
	// WaitStartedAt bounds the package-manager wait across restarts.
	WaitStartedAt time.Time `json:"wait_started_at"`
	// AgentDone is set once the agent update ran or was not needed.
	AgentDone   bool         `json:"agent_done,omitempty"`
	AgentUpdate *AgentUpdate `json:"agent_update,omitempty"`
	// Planned are the pending updates of the scope (name.arch) before the
	// upgrade, kept so a resumed run counts against the same list.
	Planned []string `json:"planned,omitempty"`
	// Ran is set once the upgrade command finished; ExitCode is its exit
	// status, Failure why it could not run.
	Ran      bool   `json:"ran,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Failure  string `json:"failure,omitempty"`
	// Services are the services restarted, Restarted whether restarting
	// was attempted and could be told.
	Services  []string  `json:"services,omitempty"`
	Restarted bool      `json:"restarted,omitempty"`
	Notes     []string  `json:"notes,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Files reads and writes the helper's files in the updater's directory.
type Files struct {
	Store updater.Store
}

// LockPath is the helper's lock file.
func (files Files) LockPath() string {
	return filepath.Join(files.Store.Directory, LockFile)
}

// ReadRequest returns the pending request, or nil.
func (files Files) ReadRequest() (*Request, error) {
	var request Request
	found, err := files.Store.ReadJSON(RequestFile, &request)
	if err != nil || !found {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return &request, nil
}

// WriteRequest persists a validated request.
func (files Files) WriteRequest(request Request) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return files.Store.WriteJSON(RequestFile, request)
}

// RemoveRequest deletes the request.
func (files Files) RemoveRequest() error { return files.Store.Remove(RequestFile) }

// ReadResult returns the helper's result with the end of its output, at
// most execlog.MaxBytes, and whether the output's beginning was cut off;
// nil without a result.
func (files Files) ReadResult() (*Result, string, bool, error) {
	var result Result
	found, err := files.Store.ReadJSON(ResultFile, &result)
	if err != nil || !found {
		return nil, "", false, err
	}
	if !updater.ValidCommandID(result.CommandID) || result.Data.Code == "" {
		return nil, "", false, errors.New("upgrade result: invalid command id or code")
	}
	output, truncated, err := files.readOutputTail()
	if err != nil {
		output, truncated = "(the output could not be read: "+err.Error()+")\n", false
	}
	return &result, output, truncated, nil
}

// ReadOutput returns at most limit bytes of the output file from offset,
// and the file's size; found is false when there is no output file.
func (files Files) ReadOutput(offset int64, limit int) ([]byte, int64, bool, error) {
	return files.Store.ReadRange(OutputFile, offset, limit)
}

// StreamPosition is how much of a command's output the daemon has sent.
type StreamPosition struct {
	CommandID string `json:"command_id"`
	Offset    int64  `json:"offset"`
}

// ReadStream returns the persisted stream position, or nil.
func (files Files) ReadStream() (*StreamPosition, error) {
	var position StreamPosition
	found, err := files.Store.ReadJSON(StreamFile, &position)
	if err != nil || !found {
		return nil, err
	}
	return &position, nil
}

// WriteStream persists the stream position.
func (files Files) WriteStream(position StreamPosition) error {
	return files.Store.WriteJSON(StreamFile, position)
}

// RemoveResult deletes the result, its output and the stream position.
func (files Files) RemoveResult() error {
	for _, name := range []string{ResultFile, OutputFile, StreamFile} {
		if err := files.Store.Remove(name); err != nil {
			return err
		}
	}
	return nil
}

func (files Files) readState() (*state, error) {
	var current state
	found, err := files.Store.ReadJSON(StateFile, &current)
	if err != nil || !found {
		return nil, err
	}
	return &current, nil
}

func (files Files) writeState(current state) error { return files.Store.WriteJSON(StateFile, current) }

// readOutputTail returns the end of the output file as execlog would keep
// it, and whether its beginning was cut off. A missing file is no output.
func (files Files) readOutputTail() (string, bool, error) {
	data, size, found, err := files.Store.ReadRange(OutputFile, 0, 0)
	if err != nil || !found {
		return "", false, err
	}
	start := max(size-outputTailBytes, 0)
	data, _, _, err = files.Store.ReadRange(OutputFile, start, outputTailBytes)
	if err != nil {
		return "", false, err
	}
	text, truncated := execlog.Cap(string(data))
	return text, truncated || start > 0, nil
}

// openOutput opens the output file for appending; fresh empties it for a
// new request.
func (files Files) openOutput(fresh bool) (*os.File, error) {
	return files.Store.OpenAppend(OutputFile, fresh)
}

func (files Files) writeResult(result Result) error { return files.Store.WriteJSON(ResultFile, result) }

// cleanup removes the request and state once the result is written.
func (files Files) cleanup() error {
	if err := files.RemoveRequest(); err != nil {
		return err
	}
	return files.Store.Remove(StateFile)
}
