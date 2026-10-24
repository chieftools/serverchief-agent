package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"go.chief.tools/serverchief-agent/initsys"
)

// Capability is capabilities.agent_update of the heartbeat.
type Capability struct {
	Available bool
	// Reason says why updates are unavailable; "" when available.
	Reason string
}

func unavailable(reason string) Capability { return Capability{Reason: reason} }

// Probe decides whether this agent can update itself.
type Probe struct {
	// Version is the running build's version.
	Version string
	// GOOS defaults to runtime.GOOS.
	GOOS  string
	Tools Tools
	// Run defaults to ExecRunner.
	Run Runner
	// Init is the host's init system.
	Init initsys.Kind
	// PID is the daemon's process ID (os.Getpid()).
	PID int
}

// Detect checks, in order: a stable release build, the serverchief
// package installed at the running version, the Server Chief repository
// configuration, and a way to launch the helper that survives the
// package's restart of the daemon.
func (probe Probe) Detect(ctx context.Context) Capability {
	goos := probe.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	run := probe.Run
	if run == nil {
		run = ExecRunner
	}
	switch {
	case probe.Version == "dev" || probe.Version == "":
		return unavailable("dev build")
	case !ValidVersion(probe.Version):
		return unavailable("not a stable release build (" + probe.Version + ")")
	case goos != "linux":
		return unavailable("unsupported platform " + goos)
	}

	manager, err := DetectManager(probe.Tools, run)
	if manager == nil {
		return unavailable(err.Error())
	}
	if manager.DpkgQuery == "" && manager.RPM == "" {
		return unavailable("no package database")
	}
	installed, queryErr := manager.Installed(ctx)
	switch {
	case queryErr != nil:
		return unavailable("cannot query the package database: " + queryErr.Error())
	case installed.Version == "":
		return unavailable("installed from a tarball, not the serverchief package")
	case installed.Version != probe.Version:
		return unavailable("the installed serverchief package (" + installed.Version + ") is not the running version (" + probe.Version + ")")
	case !installed.Intact:
		return unavailable("the serverchief package is not fully installed")
	case errors.Is(err, errRepositoryMissing):
		return unavailable("repository not configured")
	case err != nil:
		return unavailable(err.Error())
	}
	if manager.Commands.Manager == ManagerApt && !probe.aptRecentEnough(ctx, manager, run) {
		// --allow-downgrades and deb822 source files arrived in apt 1.1.
		return unavailable("apt 1.1 or later is required")
	}
	return probe.launchable()
}

func (probe Probe) aptRecentEnough(ctx context.Context, manager *SystemManager, run Runner) bool {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	output, err := run(ctx, nil, []string{manager.DpkgQuery, "-W", "-f=${Version}", "apt"})
	if err != nil || output.ExitCode != 0 {
		return false
	}
	version := strings.TrimSpace(string(output.Stdout))
	if _, withoutEpoch, found := strings.Cut(version, ":"); found {
		version = withoutEpoch
	}
	parts := strings.FieldsFunc(version, func(r rune) bool { return r < '0' || r > '9' })
	if len(parts) < 2 {
		return false
	}
	major, _ := strconv.Atoi(parts[0])
	minor, _ := strconv.Atoi(parts[1])
	return major > 1 || major == 1 && minor >= 1
}

// launchable checks that the helper can be launched so that it outlives
// the daemon, and that the package's maintainer scripts restart this
// daemon (otherwise the new version never reports ready).
func (probe Probe) launchable() Capability {
	tools := probe.Tools
	switch probe.Init {
	case initsys.Systemd:
		if tools.Find("systemd-run") == "" {
			return unavailable("systemd-run not found")
		}
		if !probe.inServiceCgroup() {
			return unavailable("not running as serverchief.service")
		}
		return Capability{Available: true}
	case initsys.SysV, initsys.Upstart:
		script := ""
		for _, candidate := range []string{"/etc/init.d/serverchief", "/etc/rc.d/init.d/serverchief"} {
			if info, err := os.Stat(filepath.Join(tools.Root, candidate)); err == nil && info.Mode().Perm()&0o111 != 0 {
				script = candidate
				break
			}
		}
		if script == "" {
			return unavailable("the serverchief init script is missing")
		}
		data, err := os.ReadFile(filepath.Join(tools.Root, "/var/run/serverchief.pid"))
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(data))); err != nil || pid != probe.PID {
			return unavailable("not running under the serverchief init script")
		}
		return Capability{Available: true}
	default:
		return unavailable("no init system manages the agent (a container?)")
	}
}

// inServiceCgroup reads /proc/self/cgroup (v1 or v2) for the unit.
func (probe Probe) inServiceCgroup() bool {
	data, err := os.ReadFile(filepath.Join(probe.Tools.Root, "/proc/self/cgroup"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) == 3 && strings.HasSuffix(parts[2], "/serverchief.service") {
			return true
		}
	}
	return false
}
