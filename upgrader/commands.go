package upgrader

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.chief.tools/serverchief-agent/updater"
)

// This is the package upgrade's only file that spells out package-manager
// verbs which change the system, next to the agent update's
// updater/commands.go. TestNoMutatingPackageVerbsInSource allows them here
// and nowhere else.

// Package managers, as collector reports them.
const (
	ManagerApt = "apt"
	ManagerDnf = "dnf"
	ManagerYum = "yum"
)

// aptPackagePattern is a Debian package name with an optional
// architecture qualifier. Names come from the host's own apt simulation.
var aptPackagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*(:[a-z0-9-]+)?$`)

// aptOptions keep local configuration files and never prompt.
var aptOptions = []string{"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}

// AptEnvironment is the environment of apt-get install. With
// restartServices needrestart restarts the services that use updated
// libraries on its own (mode a); without it needrestart is suspended.
func AptEnvironment(restartServices bool) []string {
	environment := []string{"DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none"}
	if restartServices {
		return append(environment, "NEEDRESTART_MODE=a")
	}
	return append(environment, "NEEDRESTART_SUSPEND=1")
}

// AptInstall upgrades exactly packages, which must be installed (apt
// installs new dependencies such as a new kernel). The serverchief
// package is never among them.
func AptInstall(aptGet string, packages []string) ([]string, error) {
	if !filepath.IsAbs(aptGet) || filepath.Base(aptGet) != "apt-get" {
		return nil, fmt.Errorf("invalid apt-get path %q", aptGet)
	}
	if len(packages) == 0 {
		return nil, errors.New("no packages to upgrade")
	}
	for _, name := range packages {
		if !aptPackagePattern.MatchString(name) || packageName(name) == updater.PackageName {
			return nil, fmt.Errorf("refusing to upgrade package %q", name)
		}
	}
	return slices.Concat([]string{aptGet, "install", "-y", "--only-upgrade"}, aptOptions, packages), nil
}

// RPMUpgrade upgrades every pending update, or the security updates, from
// every enabled repository except the serverchief package. It exits
// instead of waiting when another yum/dnf holds its lock.
func RPMUpgrade(tool string, scope string) ([]string, error) {
	if !filepath.IsAbs(tool) || (filepath.Base(tool) != "dnf" && filepath.Base(tool) != "yum") {
		return nil, fmt.Errorf("invalid dnf/yum path %q", tool)
	}
	if !ValidScope(scope) {
		return nil, fmt.Errorf("invalid scope %q", scope)
	}
	argv := []string{tool, "upgrade", "-y", "--setopt=exit_on_lock=True", "--exclude=" + updater.PackageName}
	if scope == ScopeSecurity {
		argv = append(argv, "--security")
	}
	return argv, nil
}

// DpkgAudit lists half-installed and half-configured packages.
func DpkgAudit(dpkg string) []string { return []string{dpkg, "--audit"} }

// NeedsRestartingServices lists the systemd services that use updated
// files (yum-utils, dnf-utils).
func NeedsRestartingServices(tool string) []string { return []string{tool, "-s"} }

// unitPattern is a systemd unit name as needs-restarting prints it.
var unitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:\\-]{0,127}$`)

// keepRunning are units restarting would disrupt the host or the agent:
// the agent itself, the message bus, logind and the other systemd
// services, and login sessions. needrestart leaves the same alone.
var keepRunning = []string{"serverchief", "serverchief-", "dbus", "dbus-broker", "systemd-", "getty@", "serial-getty@", "user@", "user-runtime-dir@"}

// RestartableUnit returns name as a .service unit when it may be
// restarted, "" otherwise.
func RestartableUnit(name string) string {
	name = strings.TrimSpace(name)
	if !unitPattern.MatchString(name) {
		return ""
	}
	if !strings.Contains(name, ".") {
		name += ".service"
	}
	if !strings.HasSuffix(name, ".service") {
		return ""
	}
	base := strings.TrimSuffix(name, ".service")
	for _, kept := range keepRunning {
		prefix := strings.HasSuffix(kept, "-") || strings.HasSuffix(kept, "@")
		if base == kept || prefix && strings.HasPrefix(base, kept) {
			return ""
		}
	}
	return name
}

// TryRestart restarts unit if it runs.
func TryRestart(systemctl, unit string) []string {
	return []string{systemctl, "try-restart", unit}
}

// packageName drops an architecture qualifier.
func packageName(name string) string {
	base, _, _ := strings.Cut(name, ":")
	return base
}
