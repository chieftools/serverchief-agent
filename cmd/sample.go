package cmd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"go.chief.tools/serverchief-agent/collector"
	"go.chief.tools/serverchief-agent/metrics"
)

func newSampleCommand() *cobra.Command {
	var (
		pretty bool
		root   string
		over   time.Duration
	)
	command := &cobra.Command{
		Use:   "sample",
		Short: "Take one metrics sample locally and print it as JSON",
		Long: `Read the host's usage twice, the given time apart, and print the sample the
agent would send to the metrics gateway, with rates over that time. Nothing
is sent and no setup is needed.`,
		Example: `  serverchief sample --pretty
  serverchief sample --over 10s`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if over <= 0 {
				return fmt.Errorf("--over must be positive")
			}
			host := collector.NewHost(root)
			first := host.ReadUsage()
			select {
			case <-command.Context().Done():
				return command.Context().Err()
			case <-time.After(over):
			}
			second := host.ReadUsage()
			sample := metrics.Sample{At: second.At.Unix(), Points: metrics.Derive(&first, second)}

			var output []byte
			var err error
			if pretty {
				output, err = json.MarshalIndent(sample, "", "  ")
			} else {
				output, err = json.Marshal(sample)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), string(output))
			return err
		},
	}
	command.Flags().BoolVar(&pretty, "pretty", false, "Indent the JSON output")
	command.Flags().DurationVar(&over, "over", time.Second, "Time between the two readings rates are computed over")
	command.Flags().StringVar(&root, "root", "/", "Read files from this directory instead of /")
	_ = command.Flags().MarkHidden("root")
	return command
}
