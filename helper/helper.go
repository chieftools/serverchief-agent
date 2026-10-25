// Package helper runs the few things the agent needs root for on behalf of
// the unprivileged daemon.
//
// The daemon runs as the serverchief user with only CAP_DAC_READ_SEARCH, so
// it can read the host but write nothing outside its own directories. For
// the fixed list of actions that need root, it drops a request file in
// Directory. A systemd path unit starts `serverchief internal-helper` as
// root, which validates each request against the actions it knows, runs it
// and writes a result file next to it. Requests name an action and a few
// validated arguments, never a command line or a query.
//
// Where the daemon itself runs as root (systemd older than 229 cannot grant
// ambient capabilities, and SysV), Local runs the same actions in-process.
package helper

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Directory holds the request and result files. It belongs to the
// serverchief user.
const Directory = "/var/lib/serverchief/helper"

// Action names.
const (
	// ActionPing answers at once; the daemon uses it to see that the
	// helper is installed and running.
	ActionPing = "ping"
	// ActionRefreshMetadata refreshes the package manager's metadata cache
	// (apt-get update, yum/dnf makecache). It never installs anything.
	ActionRefreshMetadata = "refresh-metadata"
	// ActionPleskSites reads the Plesk sites from the psa database, or
	// their names from `plesk bin site --list` when that fails. The query
	// is built by the helper, never by the daemon.
	ActionPleskSites = "plesk-sites"
	// ActionStartAgentUpdate starts the update helper for the update
	// request the daemon wrote to its state directory.
	ActionStartAgentUpdate = "start-agent-update"
)

const (
	maxRequestBytes = 64 << 10
	// MaxOutputBytes caps the stdout and stderr of an action.
	MaxOutputBytes = 32 << 20
)

var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Request asks the helper to run one action.
type Request struct {
	ID        string            `json:"id"`
	Action    string            `json:"action"`
	Args      map[string]string `json:"args,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// Result is the outcome of a request. Error is set when the action could
// not run at all; a command that ran and failed reports its ExitCode.
type Result struct {
	ID       string          `json:"id"`
	ExitCode int             `json:"exit_code"`
	Stdout   []byte          `json:"stdout,omitempty"`
	Stderr   []byte          `json:"stderr,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// Handler runs one action as root. It must only act on validated input.
type Handler struct {
	// Timeout bounds one run of the action.
	Timeout time.Duration
	// Validate checks the arguments; nil accepts none.
	Validate func(args map[string]string) error
	Run      func(ctx context.Context, args map[string]string) Result
}

// Actions maps action names to their handlers.
type Actions map[string]Handler

// Names returns the action names, sorted.
func (actions Actions) Names() []string {
	names := make([]string, 0, len(actions))
	for name := range actions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Runner runs actions: Client through the root helper, Local in-process.
type Runner interface {
	Do(ctx context.Context, action string, args map[string]string) (Result, error)
}

// ErrUnknownAction is returned for an action the helper does not offer.
var ErrUnknownAction = errors.New("unknown helper action")

// run validates and runs one request with its handler's timeout.
func (actions Actions) run(ctx context.Context, request Request) Result {
	result := Result{ID: request.ID}
	handler, ok := actions[request.Action]
	if !ok {
		result.Error = fmt.Sprintf("%s %q", ErrUnknownAction, request.Action)
		return result
	}
	validate := handler.Validate
	if validate == nil {
		validate = noArgs
	}
	if err := validate(request.Args); err != nil {
		result.Error = "invalid arguments: " + err.Error()
		return result
	}
	timeout := handler.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	actionContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	outcome := handler.Run(actionContext, request.Args)
	outcome.ID = request.ID
	outcome.Stdout = capBytes(outcome.Stdout)
	outcome.Stderr = capBytes(outcome.Stderr)
	return outcome
}

func noArgs(args map[string]string) error {
	if len(args) > 0 {
		return errors.New("this action takes no arguments")
	}
	return nil
}

func capBytes(data []byte) []byte {
	if len(data) > MaxOutputBytes {
		return data[:MaxOutputBytes]
	}
	return data
}

// Local runs the actions in-process, for a daemon that already runs as
// root.
type Local struct {
	Actions Actions
}

// Do runs the action directly.
func (local Local) Do(ctx context.Context, action string, args map[string]string) (Result, error) {
	result := local.Actions.run(ctx, Request{ID: newID(), Action: action, Args: args})
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	return result, nil
}

func newID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buffer)
}
