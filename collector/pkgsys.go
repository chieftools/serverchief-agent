package collector

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.chief.tools/serverchief-agent/helper"
	"go.chief.tools/serverchief-agent/pkglock"
)

// ErrPackageManagerBusy means a package manager holds its lock. Collection
// is skipped rather than waiting, and the previous dataset is kept.
var ErrPackageManagerBusy = errors.New("package manager is busy")

// BusyError names the held lock. errors.Is(err, ErrPackageManagerBusy)
// matches it.
type BusyError struct {
	Lock string
}

func (err *BusyError) Error() string {
	return "package manager is busy: " + err.Lock
}

func (err *BusyError) Is(target error) bool { return target == ErrPackageManagerBusy }

// rpmCommandTimeout caps every yum/dnf query, on top of the collector's
// timeout, in case exit_on_lock is not honoured.
const rpmCommandTimeout = 90 * time.Second

// exitOnLock makes yum and dnf (4 and 5) exit instead of waiting when
// another instance holds their lock. yum then exits 1 with "Existing
// lock", dnf exits 200 with "already locked by".
const exitOnLock = "--setopt=exit_on_lock=True"

// rpmBusy recognises a yum/dnf run that gave up on a lock.
func rpmBusy(result CommandResult) (string, bool) {
	output := string(result.Stderr) + "\n" + string(result.Stdout)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Existing lock ") || strings.Contains(line, " already locked by ") {
			return line, true
		}
	}
	return "", result.ExitCode == 200
}

// packageLockBlocking reports a held package-manager lock that has not
// yet been held for longer than the tracker's patience.
func (host *Host) packageLockBlocking() (pkglock.Lock, bool) {
	if host.PackageLocks == nil {
		return pkglock.Lock{}, false
	}
	return host.PackageLocks.Blocking()
}

// updateManager returns the manager the updates dataset reports from: apt,
// dnf, yum or "".
func (host *Host) updateManager() string {
	switch {
	case host.exists(dpkgStatusPath) && host.findCommand("apt-get") != "":
		return ManagerApt
	case host.findCommand("dnf") != "":
		return ManagerDnf
	case host.findCommand("yum") != "":
		return ManagerYum
	}
	return ""
}

// PackageSystem is the package manager as the daemon's metadata refresh
// and package activity trigger see it.
type PackageSystem struct {
	host *Host
}

// NewPackageSystem returns the package system of host.
func NewPackageSystem(host *Host) *PackageSystem { return &PackageSystem{host: host} }

// DatabaseToken is the package database's path, mtime and size, from a
// cheap stat: /var/lib/dpkg/status or the first rpm database. It is ""
// without one.
func (system *PackageSystem) DatabaseToken() string {
	return system.host.packageDatabaseToken()
}

// BusyLock inspects the package-manager locks. heldFor is how long some
// lock has been held continuously, as far as the observations tell.
func (system *PackageSystem) BusyLock() (lock string, heldFor time.Duration, held bool) {
	if system.host.PackageLocks == nil {
		return "", 0, false
	}
	found, heldFor, held := system.host.PackageLocks.Check()
	if !held {
		return "", 0, false
	}
	return found.String(), heldFor, true
}

// CacheUpdatedAt is the updates dataset's cache_updated_at.
func (system *PackageSystem) CacheUpdatedAt() *time.Time {
	host := system.host
	switch host.updateManager() {
	case ManagerApt:
		return host.aptCacheUpdatedAt()
	case ManagerDnf:
		return host.rpmCacheUpdatedAt(ManagerDnf)
	case ManagerYum:
		return host.rpmCacheUpdatedAt(ManagerYum)
	}
	return nil
}

// RefreshCommand returns the argv of the metadata refresh: apt-get update,
// yum makecache (fast on EL7) or dnf makecache, at the lowest CPU and IO
// priority. It never returns a command that installs, upgrades or removes
// packages; TestRefreshCommandAllowList pins every variant.
func (system *PackageSystem) RefreshCommand() ([]string, error) {
	host := system.host
	var command []string
	switch manager := host.updateManager(); manager {
	case ManagerApt:
		command = []string{host.findCommand("apt-get"), "update", "-qq"}
	case ManagerDnf:
		command = []string{host.findCommand("dnf"), "makecache", "-q", exitOnLock}
	case ManagerYum:
		command = []string{host.findCommand("yum"), "makecache"}
		if host.yumHasMakecacheFast() {
			command = append(command, "fast")
		}
		command = append(command, "-q", exitOnLock)
	default:
		return nil, errors.New("no supported package manager (apt, dnf or yum) found")
	}
	// ionice -t: an IO scheduler without priorities is not a failure.
	if ionice := host.findCommand("ionice"); ionice != "" {
		command = append([]string{ionice, "-c3", "-t"}, command...)
	}
	if nice := host.findCommand("nice"); nice != "" {
		command = append([]string{nice, "-n", "19"}, command...)
	}
	return command, nil
}

// yumHasMakecacheFast is true on EL7 and Amazon Linux 2, whose yum 3.4.3
// has `makecache fast`. Newer RHEL-likes run dnf.
func (host *Host) yumHasMakecacheFast() bool {
	release := host.OSRelease()
	major, _, _ := strings.Cut(release.Version, ".")
	return major == "7" || release.ID == "amzn" && major == "2"
}

// RefreshMetadata runs RefreshCommand. ctx carries the timeout. A held
// lock returns a BusyError; any other failure returns a one-line error.
// It writes nothing itself: the daemon records a success in its own state
// (Host.MetadataRefreshedAt), because apt keeps the lists' mtimes when
// nothing changed upstream and cache_updated_at would otherwise not move.
func (system *PackageSystem) RefreshMetadata(ctx context.Context) error {
	host := system.host
	argv, err := system.RefreshCommand()
	if err != nil {
		return err
	}
	name := filepath.Base(refreshVerb(argv)[0])
	result, err := host.runRefresh(ctx, argv)
	if ctx.Err() != nil {
		return fmt.Errorf("%s timed out or was cancelled: %w", name, ctx.Err())
	}
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	manager := host.updateManager()
	if manager != ManagerApt {
		if message, busy := rpmBusy(result); busy && result.ExitCode != 0 {
			return &BusyError{Lock: message}
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("%s makecache exited with status %d: %s", name, result.ExitCode, rpmFailure(result))
		}
		return nil
	}

	failure := aptFailure(result.Stderr)
	if strings.Contains(failure, "Could not get lock") {
		return &BusyError{Lock: failure}
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("apt-get update exited with status %d: %s", result.ExitCode, failure)
	}
	if failure != "" {
		// Old apt exits 0 when an index failed to download and keeps the
		// old one, so that is a failure too.
		return fmt.Errorf("apt-get update: %s", failure)
	}
	return nil
}

// refreshVerb returns the package-manager part of a refresh argv, after
// nice and ionice.
func refreshVerb(argv []string) []string {
	for index, arg := range argv {
		switch filepath.Base(arg) {
		case "apt-get", "yum", "dnf", "dnf5":
			return argv[index:]
		}
	}
	return argv
}

// aptFailure returns the first error, or the first failed download, of
// apt-get update's stderr; "" when there is none. Plain warnings, such as
// a repository without a Release file, are not failures.
func aptFailure(stderr []byte) string {
	for _, line := range lines(stderr) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "E: ") {
			return line
		}
	}
	for _, line := range lines(stderr) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "W: Failed to fetch") || strings.HasPrefix(line, "W: Some index files failed to download") {
			return line
		}
	}
	return ""
}

// rpmFailure is the last line yum or dnf printed, which names the cause.
func rpmFailure(result CommandResult) string {
	if message := lastLine(result.Stderr); message != "" {
		return message
	}
	return lastLine(result.Stdout)
}

// runRefresh runs the refresh argv, through the root helper when the
// daemon runs unprivileged: the helper builds the same argv itself.
func (host *Host) runRefresh(ctx context.Context, argv []string) (CommandResult, error) {
	if host.Privileged == nil {
		return host.run(ctx, argv[0], argv[1:]...)
	}
	result, err := host.Privileged.Do(ctx, helper.ActionRefreshMetadata, nil)
	if err != nil {
		return CommandResult{}, err
	}
	return CommandResult{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}, nil
}

// Manager is the host's package manager (ManagerApt, ManagerDnf or
// ManagerYum), "" without a supported one.
func (system *PackageSystem) Manager() string { return system.host.updateManager() }

// Tool returns the absolute path of a program in the usual binary
// directories, "" when it is not installed.
func (system *PackageSystem) Tool(name string) string { return system.host.findCommand(name) }

// PendingUpdates collects the updates dataset now: the pending updates
// from the existing metadata cache and whether a reboot is required. It
// returns a BusyError while a package manager holds its lock.
func (system *PackageSystem) PendingUpdates(ctx context.Context) (*UpdatesInfo, error) {
	payload, err := NewUpdates(system.host).Collect(ctx)
	if err != nil {
		return nil, err
	}
	info, ok := payload.(*UpdatesInfo)
	if !ok {
		return nil, errors.New("unexpected updates payload")
	}
	return info, nil
}

// HasSecurityMetadata reports whether the enabled yum/dnf repositories
// publish advisories (updateinfo), which `upgrade --security` needs. With
// pending updates but no advisory of any kind, the repositories carry
// none (CentOS 7's do not); without pending updates it cannot be told
// and counts as available. A failing query counts as unavailable. apt
// always has security pockets.
func (system *PackageSystem) HasSecurityMetadata(ctx context.Context, pending int) bool {
	host := system.host
	manager := host.updateManager()
	if manager == ManagerApt {
		return true
	}
	if manager == "" {
		return false
	}
	result, err := host.rpmQuery(ctx, manager, "updateinfo", "list")
	if err != nil || result.ExitCode != 0 {
		return false
	}
	if pending == 0 {
		return true
	}
	for _, line := range lines(result.Stdout) {
		// "ADVISORY TYPE NEVRA"; yum ends with "updateinfo list done".
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] == "updateinfo" {
			continue
		}
		if _, _, ok := splitNEVRA(fields[len(fields)-1]); ok {
			return true
		}
	}
	return false
}
