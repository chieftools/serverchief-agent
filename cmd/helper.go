package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/spf13/cobra"

	"go.chief.tools/serverchief-agent/collector"
	"go.chief.tools/serverchief-agent/helper"
	"go.chief.tools/serverchief-agent/initsys"
	"go.chief.tools/serverchief-agent/logging"
	"go.chief.tools/serverchief-agent/optout"
	"go.chief.tools/serverchief-agent/updater"
)

// newInternalHelperCommand is the root helper the serverchief-helper path
// unit starts when the unprivileged daemon drops a request. It runs every
// pending request and exits.
func newInternalHelperCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "internal-helper",
		Short:  "Run the agent's pending root helper requests (run by systemd)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if os.Geteuid() != 0 {
				return errors.New("internal-helper must run as root")
			}
			if _, err := logging.Setup(""); err != nil {
				return err
			}
			return helper.Serve(command.Context(), helper.Directory, helperActions())
		},
	}
}

// helperActions are the only things the root helper does for the daemon,
// also run in-process by a daemon that runs as root. Every action takes
// no arguments: what it runs is decided here.
func helperActions() helper.Actions {
	return helper.Actions{
		helper.ActionPing: {
			Timeout: 5 * time.Second,
			Run: func(context.Context, map[string]string) helper.Result {
				return helper.Result{Stdout: []byte("pong")}
			},
		},
		helper.ActionRefreshMetadata: {
			Timeout: 15 * time.Minute,
			Run: func(ctx context.Context, _ map[string]string) helper.Result {
				host := collector.NewHost("/")
				argv, err := collector.NewPackageSystem(host).RefreshCommand()
				if err != nil {
					return helper.Result{Error: err.Error()}
				}
				result, err := host.Command(ctx, argv[0], argv[1:]...)
				if err != nil {
					return helper.Result{Error: err.Error()}
				}
				return helper.Result{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}
			},
		},
		helper.ActionPleskSites: {
			Timeout: 5 * time.Minute,
			Run: func(ctx context.Context, _ map[string]string) helper.Result {
				data, err := json.Marshal(collector.NewHost("/").FetchPleskSites(ctx))
				if err != nil {
					return helper.Result{Error: err.Error()}
				}
				return helper.Result{Data: data}
			},
		},
		helper.ActionApacheVHosts: {
			Timeout: 2 * time.Minute,
			Run: func(ctx context.Context, _ map[string]string) helper.Result {
				output, err := collector.NewHost("/").DumpApacheVHosts(ctx)
				if err != nil {
					return helper.Result{Error: err.Error()}
				}
				return helper.Result{Stdout: output}
			},
		},
		helper.ActionStartAgentUpdate: {
			Timeout: time.Minute,
			Run: func(ctx context.Context, _ map[string]string) helper.Result {
				// The update helper validates the request the daemon wrote
				// to its state directory and trusts nothing else.
				if err := (updater.Launcher{Init: initsys.DetectSystem()}).Launch(ctx); err != nil {
					return helper.Result{Error: err.Error()}
				}
				return helper.Result{}
			},
		},
		helper.ActionReboot:            rebootAction(optout.Path, 0),
		helper.ActionDisableCapability: disableCapabilityAction(optout.Path, 0),
		helper.ActionStartPackageUpgrade: startUpgradeAction(optout.Path, 0, func(ctx context.Context) error {
			return upgradeLauncher(initsys.DetectSystem()).Launch(ctx)
		}),
	}
}
