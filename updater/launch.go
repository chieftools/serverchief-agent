package updater

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.chief.tools/serverchief-agent/initsys"
)

// Binary is the packaged agent, which runs the helper. After an upgrade
// it is the new version, which reads the same files.
const Binary = "/usr/bin/serverchief"

// HelperUnit is the transient unit the helper runs in under systemd.
const HelperUnit = "serverchief-update"

// DefaultLogPath receives the helper's output on hosts without journald.
const DefaultLogPath = "/var/log/serverchief/update.log"

// DetachFlag makes `internal-update` start the real helper in a new
// session and exit: the second fork of the SysV double fork.
const DetachFlag = "--detach"

// Launcher starts the helper so that it survives the daemon's restart.
type Launcher struct {
	Init   initsys.Kind
	Binary string
	Tools  Tools
	Run    Runner
	// LogPath receives the helper's output under SysV.
	LogPath string
	// Unit, Subcommand and Description name the helper; empty fields are
	// the agent update helper's (HelperUnit, internal-update). The package
	// upgrade helper launches the same way under its own names.
	Unit        string
	Subcommand  string
	Description string
}

func (launcher Launcher) unit() string {
	if launcher.Unit != "" {
		return launcher.Unit
	}
	return HelperUnit
}

func (launcher Launcher) subcommand() string {
	if launcher.Subcommand != "" {
		return launcher.Subcommand
	}
	return "internal-update"
}

func (launcher Launcher) description() string {
	if launcher.Description != "" {
		return launcher.Description
	}
	return "Server Chief agent update"
}

// Launch starts the helper. Under systemd it is a transient service
// (`systemd-run --unit=serverchief-update`), outside the daemon's unit and
// sandbox, so the package's restart of serverchief.service does not touch
// it. Elsewhere it is a double-forked setsid child with its output in
// LogPath.
func (launcher Launcher) Launch(ctx context.Context) error {
	binary := launcher.Binary
	if binary == "" {
		binary = Binary
	}
	if launcher.Init == initsys.Systemd {
		return launcher.systemdRun(ctx, binary)
	}
	logPath := launcher.LogPath
	if logPath == "" {
		logPath = DefaultLogPath
	}
	intermediate, err := SpawnDetached(binary, []string{launcher.subcommand(), DetachFlag}, logPath)
	if err != nil {
		return fmt.Errorf("start the %s helper: %w", launcher.subcommand(), err)
	}
	// The intermediate process starts the helper and exits at once;
	// reaping it leaves the helper orphaned, owned by init.
	if err := intermediate.Wait(); err != nil {
		return fmt.Errorf("start the %s helper: %w", launcher.subcommand(), err)
	}
	return nil
}

// SystemdRunArgv is the systemd-run argv for a systemd version. --collect
// (unload the unit even after a failure) and --no-block arrived in
// systemd 236; older systemd-run waits until the service started, which
// is immediate, and a failed unit is reset before the next launch.
func SystemdRunArgv(systemdRun, binary string, systemdVersion int) []string {
	return Launcher{}.systemdRunArgv(systemdRun, binary, systemdVersion)
}

func (launcher Launcher) systemdRunArgv(systemdRun, binary string, systemdVersion int) []string {
	argv := []string{systemdRun, "--unit=" + launcher.unit()}
	if systemdVersion == 0 || systemdVersion >= 236 {
		argv = append(argv, "--collect", "--no-block")
	}
	return append(argv, "--description="+launcher.description(), binary, launcher.subcommand())
}

func (launcher Launcher) systemdRun(ctx context.Context, binary string) error {
	run := launcher.Run
	if run == nil {
		run = ExecRunner
	}
	systemdRun := launcher.Tools.Find("systemd-run")
	if systemdRun == "" {
		return fmt.Errorf("systemd-run not found")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	version := 0
	if systemctl := launcher.Tools.Find("systemctl"); systemctl != "" {
		if output, err := run(ctx, nil, []string{systemctl, "--version"}); err == nil {
			version = parseSystemdVersion(string(output.Stdout))
		}
		// A helper unit that failed earlier would block the unit name.
		_, _ = run(ctx, nil, []string{systemctl, "reset-failed", launcher.unit() + ".service"})
	}
	output, err := run(ctx, nil, launcher.systemdRunArgv(systemdRun, binary, version))
	if err != nil {
		return fmt.Errorf("systemd-run: %w", err)
	}
	if output.ExitCode != 0 {
		message := lastLines(output, 2)
		if strings.Contains(message, "already exists") || strings.Contains(message, "already loaded") {
			// The helper is running already; it handles the request.
			return nil
		}
		return fmt.Errorf("systemd-run exited with status %d: %s", output.ExitCode, message)
	}
	return nil
}

// parseSystemdVersion reads "systemd 252 (252.31-1~deb12u1)".
func parseSystemdVersion(output string) int {
	fields := strings.Fields(output)
	if len(fields) < 2 || fields[0] != "systemd" {
		return 0
	}
	version, _ := strconv.Atoi(fields[1])
	return version
}
