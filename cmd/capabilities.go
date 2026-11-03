package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"go.chief.tools/serverchief-agent/initsys"
	"go.chief.tools/serverchief-agent/optout"
)

// optOutCommands are the opt-out's file and the owner it must have (root),
// the effective user and how a running daemon is asked to reload; tests
// replace them.
type optOutCommands struct {
	path   string
	owner  int
	euid   func() int
	reload func(ctx context.Context) error
}

func defaultOptOutCommands() optOutCommands {
	return optOutCommands{path: optout.Path, owner: 0, euid: os.Geteuid, reload: reloadDaemon}
}

// capabilityDescriptions say what each capability lets Server Chief do.
var capabilityDescriptions = map[string]string{
	optout.Reboot:          "reboot this server",
	optout.UpgradePackages: "install package updates",
}

func newDisableCommand(commands optOutCommands) *cobra.Command {
	return &cobra.Command{
		Use:   "disable <capability>...",
		Short: "Turn off remote actions on this server",
		Long: `Turn off remote actions Server Chief may otherwise ask this agent for.
The capabilities are ` + strings.Join(optout.Known, " and ") + `.

The setting is stored on this server in ` + optout.Path + `,
which neither Server Chief nor the agent can change. The agent stops offering
the capability and refuses commands for it; the dashboard shows it as turned
off on the server. ` + "`serverchief enable`" + ` turns it on again. Requires root.`,
		Example: "  serverchief disable reboot upgrade_packages",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, names []string) error {
			return changeOptOut(command, commands, names, true)
		},
	}
}

func newEnableCommand(commands optOutCommands) *cobra.Command {
	return &cobra.Command{
		Use:     "enable <capability>...",
		Short:   "Turn remote actions on again on this server",
		Long:    "Turn on remote actions that `serverchief disable` turned off. Requires root.",
		Example: "  serverchief enable reboot",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, names []string) error {
			return changeOptOut(command, commands, names, false)
		},
	}
}

func newCapabilitiesCommand(commands optOutCommands) *cobra.Command {
	return &cobra.Command{
		Use:   "capabilities",
		Short: "List the remote actions and whether they are turned off",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return printCapabilities(command.OutOrStdout(), optout.Load(commands.path, commands.owner))
		},
	}
}

func changeOptOut(command *cobra.Command, commands optOutCommands, names []string, disable bool) error {
	if commands.euid() != 0 {
		return errors.New("changing the remote actions requires root; run it with sudo")
	}
	if err := optout.Validate(names); err != nil {
		return err
	}
	out := command.OutOrStdout()
	state := optout.Load(commands.path, commands.owner)
	disabled := state.Disabled
	if state.Err != nil {
		_, _ = fmt.Fprintf(out, "Warning: %v; starting from an empty list.\n", state.Err)
		disabled = nil
	}
	if disable {
		disabled = append(disabled, names...)
	} else {
		disabled = slices.DeleteFunc(slices.Clone(disabled), func(name string) bool { return slices.Contains(names, name) })
	}
	if err := optout.Save(commands.path, disabled); err != nil {
		return err
	}
	verb := "Turned on"
	if disable {
		verb = "Turned off"
	}
	_, _ = fmt.Fprintf(out, "%s: %s.\n", verb, strings.Join(names, ", "))

	ctx, cancel := context.WithTimeout(command.Context(), 30*time.Second)
	defer cancel()
	if err := commands.reload(ctx); err != nil {
		_, _ = fmt.Fprintf(out, "The agent picks up the change with its next heartbeat, within 5 minutes (%v).\n", err)
	} else {
		_, _ = fmt.Fprintln(out, "The agent reports the change to Server Chief now.")
	}
	return printCapabilities(out, optout.Load(commands.path, commands.owner))
}

func printCapabilities(out io.Writer, state optout.State) error {
	if state.Err != nil {
		_, _ = fmt.Fprintf(out, "Warning: %v; every remote action counts as turned off.\n", state.Err)
	}
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, name := range optout.Known {
		status := "on"
		if state.IsDisabled(name) {
			status = "off"
		}
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\n", name, status, capabilityDescriptions[name])
	}
	return table.Flush()
}

// reloadDaemon asks a running agent to reload (SIGHUP), which makes it
// heartbeat right away. It fails when the agent is not running.
func reloadDaemon(ctx context.Context) error {
	switch initsys.DetectSystem() {
	case initsys.Systemd:
		if err := exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", initsys.DefaultService).Run(); err != nil {
			return errors.New("the agent is not running")
		}
		return initsys.ExecRunner{}.Run(ctx, "systemctl", "reload", initsys.DefaultService)
	case initsys.SysV, initsys.Upstart:
		script := "/etc/init.d/" + initsys.DefaultService
		if _, err := os.Stat(script); err != nil {
			return errors.New("the agent's init script is not installed")
		}
		return initsys.ExecRunner{}.Run(ctx, script, "reload")
	default:
		return errors.New("no supported init system runs the agent")
	}
}
