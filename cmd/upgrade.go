package cmd

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/spf13/cobra"

	"go.chief.tools/serverchief-agent/agent"
	"go.chief.tools/serverchief-agent/collector"
	"go.chief.tools/serverchief-agent/execlog"
	"go.chief.tools/serverchief-agent/helper"
	"go.chief.tools/serverchief-agent/initsys"
	"go.chief.tools/serverchief-agent/logging"
	"go.chief.tools/serverchief-agent/optout"
	"go.chief.tools/serverchief-agent/pkglock"
	"go.chief.tools/serverchief-agent/updater"
	"go.chief.tools/serverchief-agent/upgrader"
)

// Names of the package upgrade helper.
const (
	upgradeUnit        = "serverchief-upgrade"
	upgradeSubcommand  = "internal-upgrade"
	upgradeDescription = "Server Chief package upgrade"
	upgradeLogPath     = "/var/log/serverchief/upgrade.log"
)

// upgradeLauncher starts `serverchief internal-upgrade` detached, like the
// agent update helper: a transient systemd unit, or a double-forked setsid
// child under SysV.
func upgradeLauncher(init initsys.Kind) updater.Launcher {
	return updater.Launcher{Init: init, Unit: upgradeUnit, Subcommand: upgradeSubcommand, Description: upgradeDescription, LogPath: upgradeLogPath}
}

// upgradeCapability tells whether package updates can be installed,
// regardless of the operator's opt-out.
func upgradeCapability(manager string, helperAnswers bool, init initsys.Kind, systemdRun string) upgrader.Capability {
	reason := ""
	switch {
	case manager == "":
		reason = "no supported package manager (apt, dnf or yum) found"
	case !helperAnswers:
		reason = "the root helper does not answer"
	case init == initsys.Systemd && systemdRun == "":
		reason = "systemd-run is not installed"
	default:
		return upgrader.Capability{Available: true}
	}
	return upgrader.Capability{Reason: reason}
}

// newUpgrades wires the daemon's package upgrades to the host and the root
// helper: an unprivileged daemon starts the helper through the root
// helper's start-package-upgrade action, a root daemon directly.
func newUpgrades(directory string, privilege *agent.PrivilegeProbe, host *collector.Host) *upgrader.Controller {
	controller := upgrader.NewController(directory, os.Geteuid())
	init := initsys.DetectSystem()
	controller.Detect = func(ctx context.Context) upgrader.Capability {
		return upgradeCapability(collector.NewPackageSystem(host).Manager(), privilege.HelperAnswers(ctx), init, updater.Tools{}.Find("systemd-run"))
	}
	if privilege.Unprivileged {
		controller.Launch = func(ctx context.Context) error {
			result, err := privilege.Runner.Do(ctx, helper.ActionStartPackageUpgrade, nil)
			if err == nil && result.Error != "" {
				err = errors.New(result.Error)
			}
			return err
		}
	} else {
		controller.Launch = upgradeLauncher(init).Launch
	}
	return controller
}

// startUpgradeAction is the root helper's start-package-upgrade action. It
// reads the opt-out at optOutPath itself, as root (owner 0), and refuses
// when upgrade_packages is turned off, whatever the daemon decided. The
// helper validates the request the daemon wrote and trusts nothing else.
func startUpgradeAction(optOutPath string, owner int, launch func(ctx context.Context) error) helper.Handler {
	return helper.Handler{
		Timeout: time.Minute,
		Run: func(ctx context.Context, _ map[string]string) helper.Result {
			if state := optout.Load(optOutPath, owner); state.IsDisabled(optout.UpgradePackages) {
				reason := "installing package updates is turned off on this server"
				if state.Err != nil {
					reason += ": " + state.Err.Error()
				}
				return helper.Result{Error: reason}
			}
			if err := launch(ctx); err != nil {
				return helper.Result{Error: err.Error()}
			}
			return helper.Result{}
		},
	}
}

// newInternalUpgradeCommand is the detached package upgrade helper the
// daemon launches for upgrade_packages. Without a pending
// /var/lib/serverchief/upgrade-request.json it does nothing.
func newInternalUpgradeCommand() *cobra.Command {
	var detach bool
	command := &cobra.Command{
		Use:    upgradeSubcommand,
		Short:  "Install requested package updates (run by the agent)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if os.Geteuid() != 0 {
				return errors.New("internal-upgrade must run as root")
			}
			if detach {
				// Second fork: start the helper in its own session and exit.
				_, err := updater.SpawnDetached(updater.Binary, []string{upgradeSubcommand}, upgradeLogPath)
				return err
			}
			if _, err := logging.Setup(""); err != nil {
				return err
			}
			store := updater.NewStore()
			files := upgrader.Files{Store: store}
			release, acquired, err := updater.AcquireLock(files.LockPath())
			if err != nil {
				return err
			}
			if !acquired {
				log.Printf("[upgrade] another package upgrade helper is running")
				return nil
			}
			defer release()
			inspector := pkglock.New()
			locks := func() (string, bool) {
				lock, held := inspector.Busy()
				return lock.String(), held
			}
			init := initsys.DetectSystem()
			return upgrader.RunHelper(command.Context(), upgrader.Options{
				Files:    files,
				Packages: collector.NewPackageSystem(collector.NewHost("/")),
				Locks:    locks,
				Systemd:  init == initsys.Systemd,
				AgentUpdate: func(ctx context.Context, request upgrader.Request, output *execlog.Log) *upgrader.AgentUpdate {
					manager, detectErr := updater.DetectManager(updater.Tools{}, upgrader.LoggedRunner(output))
					options := upgrader.AgentUpdateOptions{Store: store, DetectError: detectErr, Locks: locks}
					if manager != nil {
						options.Manager = manager
					}
					return upgrader.UpdateAgent(ctx, request, output, options)
				},
			})
		},
	}
	command.Flags().BoolVar(&detach, "detach", false, "Start the helper detached and exit")
	_ = command.Flags().MarkHidden("detach")
	return command
}
