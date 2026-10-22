package cmd

import (
	"os"

	"github.com/spf13/cobra"

	"go.chief.tools/serverchief-agent/agent"
)

func newSetupCommand(version string) *cobra.Command {
	var endpoint, caFile string
	command := &cobra.Command{
		Use:   "setup [--endpoint URL] [--ca-file PATH] <token>",
		Short: "Connect this server to Server Chief",
		Long: `Exchange a one-time setup token (scenr_…) from the Server Chief dashboard for
this server's identity, then enable and start the agent service.

Running setup again with a new token re-enrolls the server: the previous
credential is revoked and the service is restarted with the new one.

--endpoint and --ca-file default to $` + agent.EndpointEnv + ` and $` + agent.CAFileEnv + `.
--ca-file adds a PEM CA (such as Laravel Valet's) to the trusted roots; it is
remembered in identity.json, so the daemon trusts it too.`,
		Example: `  sudo serverchief setup scenr_…
  sudo serverchief setup --endpoint https://server.chief.test/agent/v1 \
      --ca-file /path/to/LaravelValetCASelfSigned.pem scenr_…`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			result, err := agent.Enroll(command.Context(), args[0], agent.EnrollOptions{
				Paths:    agent.DefaultPaths(),
				Endpoint: endpoint,
				CAFile:   caFile,
				Version:  version,
			})
			if result.Identity.ServerID != "" {
				agent.PrintEnrollResult(command.OutOrStdout(), result)
			}
			return err
		},
	}
	command.Flags().StringVar(&endpoint, "endpoint", envOr(agent.EndpointEnv, agent.DefaultEndpoint), "Server Chief agent API endpoint")
	command.Flags().StringVar(&caFile, "ca-file", os.Getenv(agent.CAFileEnv), "Extra PEM CA certificate to trust, for a local development backend")
	return command
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
