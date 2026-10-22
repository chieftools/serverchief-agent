package cmd

import (
	"context"
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
		logFile      string
		stateDir     string
		identityPath string
		root         string
	)
	command := &cobra.Command{
		Use:   "run",
		Short: "Run the agent daemon",
		Long: `Heartbeat, collect inventory on the schedule the backend configures and
deliver changed datasets. Requires a prior ` + "`serverchief setup <token>`" + `.

SIGINT and SIGTERM stop the daemon gracefully. SIGHUP reloads the local
configuration, reopens the log file and collects every dataset again. When
the backend revokes the credential the daemon exits with status 2, which
the systemd unit does not restart.`,
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
			if identityPath != "" {
				paths.Identity = identityPath
			}

			identity, err := agent.LoadIdentity(paths.Identity)
			if err != nil {
				return err
			}
			config, err := agent.LoadConfig(paths.Config)
			if err != nil {
				return err
			}
			store, err := inventory.OpenStateStore(paths.State)
			if err != nil {
				return err
			}
			commands, err := agent.OpenCommandJournal(paths.Commands)
			if err != nil {
				return err
			}
			serverConfig, _, err := agent.LoadServerConfig(paths.ServerConfig)
			if err != nil {
				log.Printf("[config] %v; fetching it again", err)
			}
			tlsConfig, err := agent.TLSConfig(identity.CAFile)
			if err != nil {
				return err
			}
			httpClient, err := agent.NewHTTPClient(identity.CAFile)
			if err != nil {
				return err
			}

			log.Printf("Server Chief agent %s starting (server %s, generation %d, endpoint %s, init %s, state %s)",
				version, identity.ServerID, identity.Generation, identity.Endpoint, initsys.DetectSystem(), paths.State)

			ctx := command.Context()
			reload := make(chan agent.Config)
			stopReload := watchReload(ctx, *configPath, logWriter, reload)
			defer stopReload()

			err = agent.Run(ctx, agent.RunOptions{
				Registry:     collector.Default(collector.NewHost(root)),
				Store:        store,
				Payloads:     inventory.NewPayloadStore(paths.Payloads),
				Commands:     commands,
				Client:       agent.NewClient(identity.Endpoint, identity.Credential, version, httpClient),
				Identity:     identity,
				Config:       config,
				ServerConfig: serverConfig,
				SaveServerConfig: func(serverConfig agent.ServerConfig) error {
					return agent.SaveServerConfig(paths.ServerConfig, serverConfig)
				},
				Realtime: agent.PusherConnector(version, identity.Credential, tlsConfig),
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
	command.Flags().StringVar(&stateDir, "state-dir", "", "Directory for state.json, the command journal and the cached server config (default /var/lib/serverchief)")
	command.Flags().StringVar(&identityPath, "identity", "", "Identity file written by setup (default /etc/serverchief/identity.json)")
	_ = command.Flags().MarkHidden("identity")
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
