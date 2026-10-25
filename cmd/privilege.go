package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"go.chief.tools/serverchief-agent/agent"
	"go.chief.tools/serverchief-agent/helper"
	"go.chief.tools/serverchief-agent/initsys"
)

// ambientCapabilitiesSince is the first systemd that grants ambient
// capabilities, which the unprivileged unit relies on.
const ambientCapabilitiesSince = 229

// newPrivilegeProbe tells how the daemon runs: as the serverchief user with
// the root helper, or as root running the helper's actions in-process.
func newPrivilegeProbe(ctx context.Context) *agent.PrivilegeProbe {
	actions := helperActions()
	if os.Geteuid() != 0 {
		return &agent.PrivilegeProbe{
			Unprivileged: true,
			Runner:       helper.Client{Dir: helper.Directory},
			Actions:      actions.Names(),
		}
	}
	return &agent.PrivilegeProbe{
		Runner:     helper.Local{Actions: actions},
		Actions:    actions.Names(),
		RootReason: rootReason(ctx),
	}
}

// rootReason explains why the daemon runs as root.
func rootReason(ctx context.Context) string {
	switch initsys.DetectSystem() {
	case initsys.Systemd:
		if version := systemdVersion(ctx); version > 0 && version < ambientCapabilitiesSince {
			return fmt.Sprintf("systemd %d cannot grant ambient capabilities (%d or later is required)", version, ambientCapabilitiesSince)
		}
		return "the serverchief service is configured to run as root"
	case initsys.SysV, initsys.Upstart:
		return "SysV init cannot grant ambient capabilities"
	default:
		return "started as root outside the serverchief service"
	}
}

func systemdVersion(ctx context.Context) int {
	versionContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(versionContext, "systemctl", "--version").Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 || fields[0] != "systemd" {
		return 0
	}
	version, _ := strconv.Atoi(fields[1])
	return version
}
