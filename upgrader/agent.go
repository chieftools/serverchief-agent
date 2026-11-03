package upgrader

import (
	"context"
	"fmt"
	"time"

	"go.chief.tools/serverchief-agent/execlog"
	"go.chief.tools/serverchief-agent/updater"
)

// AgentUpdateOptions configures UpdateAgent.
type AgentUpdateOptions struct {
	Store updater.Store
	// Manager drives the package manager. Nil detects it, with its
	// commands added to the output.
	Manager updater.PackageManager
	// DetectError is why Manager could not be detected; it makes the
	// update unsupported.
	DetectError error
	// Locks reports a held package-manager lock.
	Locks func() (string, bool)
	// Helper overrides updater.HelperOptions for tests.
	Helper func(options *updater.HelperOptions)
	Now    func() time.Time
}

// LoggedRunner runs commands with updater.ExecRunner and adds them, with
// their output, to output.
func LoggedRunner(output *execlog.Log) updater.Runner {
	return func(ctx context.Context, env []string, argv []string) (updater.Output, error) {
		output.Command(argv)
		result, err := updater.ExecRunner(ctx, env, argv)
		_, _ = output.Write(result.Stdout)
		_, _ = output.Write(result.Stderr)
		if err != nil {
			_, _ = output.Write([]byte(err.Error() + "\n"))
		}
		return result, err
	}
}

// UpdateAgent updates the agent to request.AgentVersion before the package
// upgrade, exactly like update_agent: from the Server Chief repository
// only, waiting for the new daemon to report ready and rolling back when
// it does not. It runs the agent update helper in-process under the
// updater's lock, with updater.NestedCommandID as the update's ID, so the
// restarted daemon writes the readiness marker for it but leaves its
// result alone. It returns nil when the installed agent is not older.
func UpdateAgent(ctx context.Context, request Request, output *execlog.Log, options AgentUpdateOptions) *AgentUpdate {
	target := request.AgentVersion
	now := options.Now
	if now == nil {
		now = time.Now
	}
	note := func(format string, args ...any) {
		_, _ = output.Write([]byte("# " + fmt.Sprintf(format, args...) + "\n"))
	}
	manager := options.Manager
	if manager == nil {
		note("the agent cannot update itself: %v", options.DetectError)
		return &AgentUpdate{Code: updater.CodeUnsupportedInstall}
	}
	installed, err := manager.Installed(context.WithoutCancel(ctx))
	if err != nil || !installed.Intact || !updater.ValidVersion(installed.Version) {
		note("the installed serverchief package cannot be updated (version %q, intact %t, error %v)", installed.Version, installed.Intact, err)
		return &AgentUpdate{Code: updater.CodeUnsupportedInstall, InstalledVersion: installed.Version}
	}
	if options.DetectError != nil {
		note("the agent cannot update itself: %v", options.DetectError)
		return &AgentUpdate{Code: updater.CodeUnsupportedInstall, PreviousVersion: installed.Version, InstalledVersion: installed.Version}
	}
	if updater.CompareVersions(target, installed.Version) <= 0 {
		note("the agent already runs %s, not older than %s", installed.Version, target)
		return nil
	}
	busy := &AgentUpdate{Code: updater.CodePackageManagerBusy, PreviousVersion: installed.Version, InstalledVersion: installed.Version}

	store := options.Store
	release, acquired, err := updater.AcquireLock(store.LockPath())
	if err != nil {
		note("take the agent update lock: %v", err)
		return &AgentUpdate{Code: updater.CodeInstallFailed, PreviousVersion: installed.Version, InstalledVersion: installed.Version}
	}
	if !acquired {
		note("another agent update is running")
		return busy
	}
	defer release()

	nested := updater.NestedCommandID(request.CommandID)
	pending, err := store.ReadRequest()
	switch {
	case err != nil:
		note("an agent update request is pending but unreadable: %v", err)
		return busy
	case pending != nil && pending.CommandID != nested:
		note("another agent update (%s) is pending", pending.CommandID)
		return busy
	case pending == nil:
		if result, _ := store.ReadResult(); result == nil || result.CommandID != nested {
			update := updater.Request{CommandID: nested, PreviousVersion: installed.Version, TargetVersion: target, RequestedAt: now().UTC().Truncate(time.Second)}
			if err := store.WriteRequest(update); err != nil {
				note("write the agent update request: %v", err)
				return &AgentUpdate{Code: updater.CodeInstallFailed, PreviousVersion: installed.Version, InstalledVersion: installed.Version}
			}
		}
	}
	note("updating the Server Chief agent from %s to %s first", installed.Version, target)
	helperOptions := updater.HelperOptions{Store: store, Manager: manager, Locks: options.Locks}
	if options.Helper != nil {
		options.Helper(&helperOptions)
	}
	if err := updater.RunHelper(ctx, helperOptions); err != nil {
		note("the agent update stopped: %v", err)
	}
	result, err := store.ReadResult()
	if err != nil || result == nil || result.CommandID != nested {
		note("the agent update left no result")
		return &AgentUpdate{Code: updater.CodeInstallFailed, PreviousVersion: installed.Version, InstalledVersion: installed.Version}
	}
	_ = store.RemoveResult(nested)
	if result.Detail != "" {
		note("agent update %s: %s", result.Code, result.Detail)
	} else {
		note("agent update %s: %s is installed", result.Code, result.InstalledVersion)
	}
	return &AgentUpdate{Code: result.Code, PreviousVersion: result.PreviousVersion, InstalledVersion: result.InstalledVersion}
}
