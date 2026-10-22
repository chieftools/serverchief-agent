package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"go.chief.tools/serverchief-agent/agent"
)

func newSetupCommand(version string) *cobra.Command {
	var endpoint string
	command := &cobra.Command{
		Use:   "setup <token>",
		Short: "Connect this server to Server Chief",
		Long: `Exchange a one-time setup token (scenr_…) from the Server Chief dashboard for
this server's identity, then enable and start the agent service.`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			identity, err := agent.Enroll(command.Context(), args[0], agent.EnrollOptions{
				Paths:    agent.DefaultPaths(),
				Endpoint: endpoint,
				Version:  version,
			})
			if errors.Is(err, agent.ErrNotImplemented) {
				fmt.Fprintln(command.ErrOrStderr(), "Setup is not implemented yet: enrollment needs the Server Chief agent API, which does not exist yet.")
				fmt.Fprintln(command.ErrOrStderr(), "Use `serverchief collect --pretty` to inspect what this server would report.")
				return err
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Server %s is set up and serverchief is running.\n", identity.ServerID)
			return err
		},
	}
	command.Flags().StringVar(&endpoint, "endpoint", agent.DefaultEndpoint, "Server Chief agent API endpoint")
	_ = command.Flags().MarkHidden("endpoint")
	return command
}
