// Package cmd implements the serverchief command line.
package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"go.chief.tools/serverchief-agent/agent"
)

// NewRootCommand builds the CLI for a build version.
func NewRootCommand(version string) *cobra.Command {
	if version == "" {
		version = "dev"
	}

	root := &cobra.Command{
		Use:           "serverchief",
		Short:         "Server Chief inventory agent",
		Long:          "Collect inventory facts about this Linux server and report changes to Server Chief.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetVersionTemplate("Server Chief agent {{.Version}}\n")

	configPath := agent.DefaultPaths().Config
	root.PersistentFlags().StringVar(&configPath, "config", configPath, "Configuration file")

	root.AddCommand(newVersionCommand(version))
	root.AddCommand(newCollectCommand())
	root.AddCommand(newSampleCommand())
	root.AddCommand(newRunCommand(version, &configPath))
	root.AddCommand(newSetupCommand(version))
	root.AddCommand(newInternalUpdateCommand())
	root.AddCommand(newInternalHelperCommand())
	root.AddCommand(newInternalUnprivilegeCommand())
	return root
}

func newVersionCommand(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the build version",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(command.OutOrStdout(), "Server Chief agent %s (%s, %s/%s, protocol %s)\n",
				version, runtime.Version(), runtime.GOOS, runtime.GOARCH, agent.ProtocolRevision)
			return err
		},
	}
}
