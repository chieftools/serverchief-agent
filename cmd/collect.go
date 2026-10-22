package cmd

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.chief.tools/serverchief-agent/agent"
	"go.chief.tools/serverchief-agent/collector"
	"go.chief.tools/serverchief-agent/inventory"
)

func newCollectCommand() *cobra.Command {
	var (
		datasets []string
		pretty   bool
		root     string
		timeout  time.Duration
	)
	command := &cobra.Command{
		Use:   "collect",
		Short: "Collect inventory locally and print it as JSON",
		Long: `Run the collectors once and print the inventory envelope the agent would
send, including each dataset's hash. Nothing is sent and no setup is needed.

Available datasets: ` + strings.Join(collector.Default(collector.NewHost("/")).Names(), ", ") + `.`,
		Example: `  serverchief collect --pretty
  serverchief collect --dataset system --dataset network
  serverchief collect --root /mnt/image --dataset system`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			registry := collector.Default(collector.NewHost(root))
			selected, err := registry.Select(datasets)
			if err != nil {
				return err
			}

			collected, failures := agent.CollectDatasets(command.Context(), selected, timeout)
			envelope := inventory.NewEnvelope(0)
			for _, dataset := range collected {
				envelope.AddFull(dataset)
			}
			for name, failure := range failures {
				envelope.AddError(name, failure)
			}

			var output []byte
			if pretty {
				output, err = json.MarshalIndent(envelope, "", "  ")
			} else {
				output, err = json.Marshal(envelope)
			}
			if err != nil {
				return fmt.Errorf("encode envelope: %w", err)
			}
			if _, err := fmt.Fprintln(command.OutOrStdout(), string(output)); err != nil {
				return err
			}
			if len(failures) > 0 {
				names := make([]string, 0, len(failures))
				for name := range failures {
					names = append(names, name)
				}
				slices.Sort(names)
				return fmt.Errorf("collection failed for %s", strings.Join(names, ", "))
			}
			return nil
		},
	}
	command.Flags().StringSliceVarP(&datasets, "dataset", "d", nil, "Dataset to collect (repeatable or comma separated; default: all)")
	command.Flags().BoolVar(&pretty, "pretty", false, "Indent the JSON output")
	command.Flags().StringVar(&root, "root", "/", "Read files from this directory instead of / (live-only sources are skipped)")
	command.Flags().DurationVar(&timeout, "timeout", time.Minute, "Timeout per collector")
	return command
}
