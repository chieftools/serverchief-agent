package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.chief.tools/serverchief-agent/agent"
	"go.chief.tools/serverchief-agent/collector"
	"go.chief.tools/serverchief-agent/execlog"
	"go.chief.tools/serverchief-agent/helper"
	"go.chief.tools/serverchief-agent/initsys"
	"go.chief.tools/serverchief-agent/optout"
	"go.chief.tools/serverchief-agent/updater"
)

// rebootMessage is the wall message logged-in users see.
const rebootMessage = "Server Chief: rebooting in 1 minute, requested from the panel"

// applicationContainers have no init system of their own to reboot.
// System containers (lxc, nspawn, openvz) and virtual machines can reboot.
var applicationContainers = map[string]bool{
	"docker":     true,
	"podman":     true,
	"containerd": true,
	"kubernetes": true,
	"wsl":        true,
}

// rebootCommand is the argv that schedules a reboot in one minute:
// shutdown -r +1, or systemctl reboot on a systemd host without shutdown.
func rebootCommand(tools updater.Tools, init initsys.Kind) ([]string, error) {
	if shutdown := tools.Find("shutdown"); shutdown != "" {
		return []string{shutdown, "-r", "+1", rebootMessage}, nil
	}
	if systemctl := tools.Find("systemctl"); systemctl != "" && init == initsys.Systemd {
		return []string{systemctl, "reboot"}, nil
	}
	return nil, errors.New("shutdown is not installed")
}

// rebootCapability tells whether this server can reboot, regardless of the
// operator's opt-out.
func rebootCapability(container string, helperAnswers bool, commandErr error) agent.ActionCapability {
	reason := ""
	switch {
	case applicationContainers[container]:
		reason = fmt.Sprintf("running in a %s container, which has no init system to reboot", container)
	case !helperAnswers:
		reason = "the root helper does not answer"
	case commandErr != nil:
		reason = commandErr.Error()
	default:
		return agent.ActionCapability{Available: true}
	}
	return agent.ActionCapability{Reason: &reason}
}

// newReboots wires the daemon's reboots to the host and the root helper.
func newReboots(requestPath string, privilege *agent.PrivilegeProbe, host *collector.Host) *agent.Reboots {
	return &agent.Reboots{
		RequestPath: requestPath,
		Detect: func(ctx context.Context) agent.ActionCapability {
			_, err := rebootCommand(updater.Tools{}, initsys.DetectSystem())
			return rebootCapability(host.Container(), privilege.HelperAnswers(ctx), err)
		},
		Reboot: func(ctx context.Context) (string, error) {
			result, err := privilege.Runner.Do(ctx, helper.ActionReboot, nil)
			output := string(result.Stdout)
			if err == nil && result.ExitCode != 0 {
				err = fmt.Errorf("the reboot command exited with status %d", result.ExitCode)
			}
			return output, err
		},
		BootID:   host.BootID,
		BootTime: host.BootTime,
		Required: host.RebootRequired,
	}
}

// rebootAction is the root helper's reboot action. It reads the opt-out at
// optOutPath itself, as root (owner 0), and refuses when reboot is turned
// off, whatever the daemon decided.
func rebootAction(optOutPath string, owner int) helper.Handler {
	return helper.Handler{
		Timeout: time.Minute,
		Run: func(ctx context.Context, _ map[string]string) helper.Result {
			if state := optout.Load(optOutPath, owner); state.IsDisabled(optout.Reboot) {
				reason := "rebooting is turned off on this server"
				if state.Err != nil {
					reason += ": " + state.Err.Error()
				}
				return helper.Result{Error: reason}
			}
			argv, err := rebootCommand(updater.Tools{}, initsys.DetectSystem())
			if err != nil {
				return helper.Result{Error: err.Error()}
			}
			var log execlog.Log
			code, err := execlog.Run(ctx, &log, argv)
			text, _ := log.Text()
			if err != nil {
				return helper.Result{Stdout: []byte(text), Error: err.Error()}
			}
			return helper.Result{Stdout: []byte(text), ExitCode: code}
		},
	}
}
