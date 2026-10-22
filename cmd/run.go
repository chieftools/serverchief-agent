package cmd

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"go.chief.tools/serverchief-agent/agent"
	"go.chief.tools/serverchief-agent/collector"
	"go.chief.tools/serverchief-agent/initsys"
	"go.chief.tools/serverchief-agent/inventory"
	"go.chief.tools/serverchief-agent/logging"
	"go.chief.tools/serverchief-agent/systemdnotify"
)

func newRunCommand(version string, configPath *string) *cobra.Command {
	var (
		logFile  string
		stateDir string
		root     string
	)
	command := &cobra.Command{
		Use:   "run",
		Short: "Run the agent daemon",
		Long: `Collect inventory on a schedule and deliver changed datasets.

SIGINT and SIGTERM stop the daemon gracefully. SIGHUP reloads the
configuration, reopens the log file and collects every dataset again.`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			logWriter, err := logging.Setup(logFile)
			if err != nil {
				return err
			}
			if logWriter != nil {
				defer logWriter.Close()
			}

			paths := agent.DefaultPaths()
			paths.Config = *configPath
			if stateDir != "" {
				paths = paths.WithStateDirectory(stateDir)
			}

			config, err := agent.LoadConfig(paths.Config)
			if err != nil {
				return err
			}
			store, err := inventory.OpenStateStore(paths.State)
			if err != nil {
				return err
			}

			log.Printf("Server Chief agent %s starting (init: %s, state: %s)", version, initsys.DetectSystem(), paths.State)
			if _, err := os.Stat(paths.Identity); errors.Is(err, os.ErrNotExist) {
				log.Printf("[agent] not set up yet (%s missing); collecting locally only", paths.Identity)
			}

			ctx := command.Context()
			reload := make(chan agent.Config)
			stopReload := watchReload(ctx, *configPath, logWriter, reload)
			defer stopReload()

			err = agent.Run(ctx, agent.RunOptions{
				Registry: collector.Default(collector.NewHost(root)),
				Store:    store,
				Client:   agent.LogClient{},
				Config:   config,
				Version:  version,
				Reload:   reload,
				Ready: func() {
					_ = systemdnotify.NotifyReady("collecting inventory")
				},
			})
			_ = systemdnotify.NotifyStopping("shutting down")
			log.Printf("Server Chief agent stopped")
			return err
		},
	}
	command.Flags().StringVar(&logFile, "log-file", "", "Log to this file with size-capped rotation instead of stderr (for SysV hosts)")
	command.Flags().StringVar(&stateDir, "state-dir", "", "Directory for state.json (default /var/lib/serverchief)")
	command.Flags().StringVar(&root, "root", "/", "Read files from this directory instead of /")
	_ = command.Flags().MarkHidden("root")
	return command
}

// watchReload turns SIGHUP into configuration reloads. An invalid file is
// logged and the running configuration is kept.
func watchReload(ctx context.Context, configPath string, logWriter *logging.RotatingFile, reload chan<- agent.Config) func() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-signals:
				_ = systemdnotify.NotifyReloading("reloading configuration")
				if logWriter != nil {
					if err := logWriter.Reopen(); err != nil {
						log.Printf("[config] reopen log file: %v", err)
					}
				}
				config, err := agent.LoadConfig(configPath)
				if err != nil {
					log.Printf("[config] reload failed, keeping the running configuration: %v", err)
				} else {
					select {
					case reload <- config:
					case <-ctx.Done():
						return
					}
				}
				_ = systemdnotify.NotifyReady("collecting inventory")
			}
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}
