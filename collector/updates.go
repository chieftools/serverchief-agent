package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Update managers.
const (
	ManagerApt = "apt"
	ManagerYum = "yum"
	ManagerDnf = "dnf"
)

// UpdatesInfo is the payload of the updates dataset.
type UpdatesInfo struct {
	Manager       string   `json:"manager"`
	Items         []Update `json:"items"`
	SecurityCount int      `json:"security_count"`
	// RebootRequired is null when the host cannot tell (RHEL without
	// needs-restarting).
	RebootRequired *bool `json:"reboot_required"`
	// CacheUpdatedAt is when the package metadata the result is based on
	// was last refreshed (UTC, whole seconds); null when there is no cache
	// or it cannot be told. The daemon's metadata refresh (PackageSystem)
	// moves it; collecting never does. It is a timestamp rather than an
	// age, so the dataset's hash only changes when the cache does.
	CacheUpdatedAt *time.Time `json:"cache_updated_at"`
}

// Update is one pending package upgrade.
type Update struct {
	Name             string `json:"name"`
	CurrentVersion   string `json:"current_version"`
	CandidateVersion string `json:"candidate_version"`
	Arch             string `json:"arch"`
	Security         bool   `json:"security"`
}

// Normalize sorts items by name and arch.
func (info *UpdatesInfo) Normalize() {
	slices.SortFunc(info.Items, func(a, b Update) int {
		if result := strings.Compare(a.Name, b.Name); result != 0 {
			return result
		}
		return strings.Compare(a.Arch, b.Arch)
	})
}

// Updates collects the updates dataset from the package manager's existing
// metadata cache. It never runs apt-get update or makecache (that is the
// daemon's metadata refresh), and it never waits for a package manager:
// while one holds its lock, Collect returns a BusyError so the previous
// dataset is kept.
type Updates struct {
	host *Host
}

func NewUpdates(host *Host) *Updates { return &Updates{host: host} }

func (*Updates) Name() string { return "updates" }

// ChangeToken implements ChangeDetector: a metadata refresh by someone
// else (apt's daily timer, an administrator) changes what is pending.
// Package database changes are the daemon's package activity trigger,
// which waits until the package manager is done.
func (collector *Updates) ChangeToken() string {
	host := collector.host
	var cache *time.Time
	switch {
	case host.exists(dpkgStatusPath):
		cache = host.aptCacheUpdatedAt()
	case host.isDir("/var/cache/dnf"):
		cache = host.rpmCacheUpdatedAt(ManagerDnf)
	case host.isDir("/var/cache/yum"):
		cache = host.rpmCacheUpdatedAt(ManagerYum)
	}
	if cache == nil {
		return "cache@none"
	}
	return "cache@" + cache.Format(time.RFC3339)
}

func (collector *Updates) Collect(ctx context.Context) (any, error) {
	host := collector.host
	manager := host.updateManager()
	if manager == "" {
		return nil, errors.New("no supported package manager (apt, dnf or yum) found")
	}
	if lock, blocking := host.packageLockBlocking(); blocking {
		return nil, &BusyError{Lock: lock.String()}
	}
	var info *UpdatesInfo
	var err error
	if manager == ManagerApt {
		info, err = host.aptUpdates(ctx)
	} else {
		info, err = host.rpmUpdates(ctx, manager)
	}
	if err != nil {
		return nil, err
	}
	if info.Items == nil {
		info.Items = []Update{}
	}
	for _, item := range info.Items {
		if item.Security {
			info.SecurityCount++
		}
	}
	info.Normalize()
	return info, nil
}

// aptInstPattern matches the "Inst" lines of an apt-get simulation:
//
//	Inst libapparmor1 [2.10.95-0ubuntu2.11] (2.10.95-0ubuntu2.12 Ubuntu:16.04/xenial-updates, Ubuntu:16.04/xenial-security [arm64])
var aptInstPattern = regexp.MustCompile(`^Inst (\S+) (?:\[([^\]]*)\] )?\((\S+)(?: (.*?))? \[([^\]]+)\]\)`)

// aptUpdates simulates a full upgrade. dist-upgrade rather than upgrade,
// so updates held back by new dependencies (kernel meta packages) count.
// Debug::NoLocking lets the simulation run next to a running apt.
//
// Dir::Bin::ischroot keeps phased updates this host is not yet selected for
// out, as apt-get upgrade outside the agent does. apt (2.1.16 and later)
// includes every phased update when `ischroot -t` reports a chroot, and it
// does in the agent's unit: its own mount namespace (ProtectSystem,
// PrivateTmp) makes /proc/self/mountinfo differ from /proc/1/mountinfo.
// Older apt ignores the option.
func (host *Host) aptUpdates(ctx context.Context) (*UpdatesInfo, error) {
	result, err := host.run(ctx, "apt-get", "-s", "-q", "-o", "Debug::NoLocking=1", "-o", "Dir::Bin::ischroot=/bin/false", "dist-upgrade")
	if err != nil {
		return nil, fmt.Errorf("apt-get -s dist-upgrade: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("apt-get -s dist-upgrade exited with status %d: %s", result.ExitCode, firstLine(result.Stderr))
	}
	info := &UpdatesInfo{Manager: ManagerApt, Items: parseAptSimulation(result.Stdout)}
	info.RebootRequired = host.aptRebootRequired()
	info.CacheUpdatedAt = host.aptCacheUpdatedAt()
	return info, nil
}

// parseAptSimulation returns the upgrades in apt-get -s output. Packages
// that would be newly installed have no current version and are skipped.
func parseAptSimulation(data []byte) []Update {
	var updates []Update
	for _, line := range lines(data) {
		match := aptInstPattern.FindStringSubmatch(line)
		if match == nil || match[2] == "" {
			continue
		}
		name, _, _ := strings.Cut(match[1], ":")
		updates = append(updates, Update{
			Name:             name,
			CurrentVersion:   match[2],
			CandidateVersion: match[3],
			Arch:             match[5],
			Security:         aptSecurityOrigin(match[4]),
		})
	}
	return updates
}

// aptSecurityOrigin reports whether any origin is a security pocket, such
// as "Ubuntu:16.04/xenial-security" or "Debian-Security:12/stable-security".
func aptSecurityOrigin(origins string) bool {
	for _, origin := range strings.Split(origins, ",") {
		origin = strings.TrimSpace(origin)
		if strings.HasSuffix(origin, "-security") || strings.Contains(origin, "-security/") ||
			strings.HasPrefix(strings.ToLower(origin), "debian-security:") {
			return true
		}
	}
	return false
}

// aptStamps are touched only after a successful apt-get update: by
// apt.systemd.daily (APT::Periodic) and by Ubuntu's update-notifier hook
// (APT::Update::Post-Invoke-Success).
var aptStamps = []string{
	"/var/lib/apt/periodic/update-success-stamp",
	"/var/lib/apt/periodic/update-stamp",
}

// aptCacheUpdatedAt is the newest of the success stamps, the lists
// directory and its Release/InRelease files and the agent's own last
// successful refresh, or nil without package indexes.
//
// Measured on Ubuntu 14.04, 16.04 and 24.04: apt sets a downloaded file's
// mtime to the archive's Last-Modified, so Release files only give a lower
// bound. The lists directory changes whenever an update replaces an index
// (on 14.04 on every successful update). lists/partial is not used: it
// also changes on failed updates, so it would claim metadata that apt
// never got. Our own apt-get -s simulation touches none of these.
func (host *Host) aptCacheUpdatedAt() *time.Time {
	const lists = "/var/lib/apt/lists"
	entries, err := host.readDir(lists)
	if err != nil {
		return nil
	}
	hasIndexes := false
	var newest time.Time
	for _, entry := range entries {
		name := entry.Name()
		if strings.Contains(name, "_Packages") {
			hasIndexes = true
		}
		if !strings.HasSuffix(name, "_Release") && !strings.HasSuffix(name, "_InRelease") {
			continue
		}
		hasIndexes = true
		if info, err := entry.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if !hasIndexes {
		return nil
	}
	if stamp := host.newestModTime(append([]string{lists}, aptStamps...)...); stamp.After(newest) {
		newest = stamp
	}
	return timestamp(host.withOwnRefresh(newest))
}

// withOwnRefresh returns the newer of a cache mtime and the agent's own
// last successful metadata refresh. An update that found nothing new
// upstream leaves the cache files alone, but the metadata was still
// checked then. Only call it when a cache exists.
func (host *Host) withOwnRefresh(newest time.Time) time.Time {
	if host.MetadataRefreshedAt == nil {
		return newest
	}
	if refreshed := host.MetadataRefreshedAt(); refreshed.After(newest) {
		return refreshed
	}
	return newest
}

// rpmQuery runs a cache-only yum/dnf query that gives up instead of
// waiting when another yum/dnf holds its lock (both wait forever by
// default), with its own timeout.
func (host *Host) rpmQuery(ctx context.Context, manager string, args ...string) (CommandResult, error) {
	queryContext, cancel := context.WithTimeout(ctx, rpmCommandTimeout)
	defer cancel()
	result, err := host.run(queryContext, manager, append([]string{"-C", "-q", exitOnLock}, args...)...)
	if err != nil {
		return result, err
	}
	if message, busy := rpmBusy(result); busy && result.ExitCode != 0 && result.ExitCode != 100 {
		return result, &BusyError{Lock: message}
	}
	return result, nil
}

// rpmUpdates lists pending updates from yum or dnf with -C (cache only), so
// the metadata is never refreshed.
func (host *Host) rpmUpdates(ctx context.Context, manager string) (*UpdatesInfo, error) {
	result, err := host.rpmQuery(ctx, manager, "check-update")
	if errors.Is(err, ErrPackageManagerBusy) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%s check-update: %w", manager, err)
	}
	switch result.ExitCode {
	case 0, 100: // 100: updates are available
	default:
		// yum explains at length and ends with the cause.
		return nil, fmt.Errorf("%s -C check-update exited with status %d: %s (without a metadata cache, the agent's metadata refresh creates one when package_metadata_refresh is enabled)",
			manager, result.ExitCode, rpmFailure(result))
	}

	installed, err := host.queryRPM(ctx)
	if err != nil {
		return nil, err
	}
	current := make(map[string]string, len(installed))
	for _, item := range collapsePackages(installed, compareRPMVersions) {
		current[item.Name+"."+item.Arch] = item.Version
	}
	security, err := host.rpmSecurityUpdates(ctx, manager)
	if err != nil {
		return nil, err
	}

	info := &UpdatesInfo{Manager: manager, Items: []Update{}}
	for _, available := range parseCheckUpdate(result.Stdout) {
		key := available.Name + "." + available.Arch
		available.CurrentVersion = current[key]
		available.Security = security[key]
		info.Items = append(info.Items, available)
	}
	info.RebootRequired = host.needsRestarting(ctx)
	info.CacheUpdatedAt = host.rpmCacheUpdatedAt(manager)
	return info, nil
}

// parseCheckUpdate parses `yum/dnf check-update` columns (name.arch,
// [epoch:]version-release, repository). yum wraps long names onto their
// own line, so fields are accumulated across lines. Obsoletes are skipped.
func parseCheckUpdate(data []byte) []Update {
	var updates []Update
	var fields []string
	for _, line := range lines(data) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Obsoleting Packages") {
			break
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "Last metadata expiration") || strings.HasPrefix(trimmed, "Security:") ||
			strings.HasPrefix(trimmed, "Loaded plugins") || strings.HasPrefix(trimmed, "Loading mirror") {
			fields = nil
			continue
		}
		fields = append(fields, strings.Fields(trimmed)...)
		if len(fields) < 3 {
			continue
		}
		name, arch, ok := cutLast(fields[0], ".")
		if ok && name != "" {
			updates = append(updates, Update{Name: name, Arch: arch, CandidateVersion: fields[1]})
		}
		fields = nil
	}
	return updates
}

// rpmSecurityUpdates returns name.arch of updates that fix security
// advisories. Repositories without updateinfo (CentOS 7) yield none. A
// held lock is an error: reporting every update as non-security would
// change the dataset.
func (host *Host) rpmSecurityUpdates(ctx context.Context, manager string) (map[string]bool, error) {
	args := []string{"updateinfo", "list", "security"}
	if manager == ManagerDnf {
		args = []string{"updateinfo", "list", "--security"}
	}
	result, err := host.rpmQuery(ctx, manager, args...)
	if errors.Is(err, ErrPackageManagerBusy) {
		return nil, err
	}
	if err != nil || result.ExitCode != 0 {
		return map[string]bool{}, nil
	}
	return parseUpdateinfo(result.Stdout), nil
}

// parseUpdateinfo parses "ADVISORY TYPE NEVRA" lines such as
// "ALSA-2026:77396 Important/Sec. openssl-libs-1:3.5.8-2.el9_8.aarch64".
func parseUpdateinfo(data []byte) map[string]bool {
	security := make(map[string]bool)
	for _, line := range lines(data) {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		kind := strings.ToLower(fields[1])
		if !strings.Contains(kind, "sec") {
			continue
		}
		if name, arch, ok := splitNEVRA(fields[len(fields)-1]); ok {
			security[name+"."+arch] = true
		}
	}
	return security
}

// splitNEVRA turns name-[epoch:]version-release.arch into name and arch.
func splitNEVRA(nevra string) (string, string, bool) {
	rest, arch, ok := cutLast(nevra, ".")
	if !ok {
		return "", "", false
	}
	for range 2 {
		index := strings.LastIndexByte(rest, '-')
		if index <= 0 {
			return "", "", false
		}
		rest = rest[:index]
	}
	return rest, arch, true
}

// RebootRequired tells whether the host needs a reboot, the way the updates
// dataset does: Debian and Ubuntu's reboot-required flag file, or
// `needs-restarting -r` on RHEL and derivatives. Nil when it cannot tell.
func (host *Host) RebootRequired(ctx context.Context) *bool {
	if host.exists(dpkgStatusPath) {
		return host.aptRebootRequired()
	}
	return host.needsRestarting(ctx)
}

func (host *Host) aptRebootRequired() *bool {
	required := host.exists("/var/run/reboot-required") || host.exists("/run/reboot-required")
	return &required
}

// needsRestarting runs `needs-restarting -r` (yum-utils, dnf-utils):
// exit status 1 means a reboot is required, 0 that it is not. A crash
// also exits with 1, so 1 only counts when the tool says a reboot is
// required. CloudLinux mounts /proc with hidepid, which hides /proc/1,
// where it reads the boot time, from the unprivileged agent.
func (host *Host) needsRestarting(ctx context.Context) *bool {
	command := host.findCommand("needs-restarting")
	if command == "" {
		return nil
	}
	result, err := host.run(ctx, command, "-r")
	if err != nil {
		return nil
	}
	switch result.ExitCode {
	case 0:
		required := false
		return &required
	case 1:
		output := string(result.Stdout) + string(result.Stderr)
		if !strings.Contains(output, "Reboot is required") {
			return nil
		}
		required := true
		return &required
	default:
		return nil
	}
}

// rpmCacheUpdatedAt is the newest metadata timestamp in the manager's
// cache directory, or the agent's own last successful refresh when that
// is newer: repomd.xml, yum's per-repository cachecookie (touched on
// every metadata check) and dnf's primary metadata, whose mtime libdnf
// resets on every check that finds the metadata current (it is what dnf
// shows as "Last metadata expiration check"). Directory mtimes are not
// used: `dnf -C`, which the collector runs, writes to the cache directory.
func (host *Host) rpmCacheUpdatedAt(manager string) *time.Time {
	directories := []string{"/var/cache/" + manager}
	if manager == ManagerDnf {
		directories = append(directories, "/var/cache/libdnf5")
	}
	var newest time.Time
	for _, directory := range directories {
		root := relative(directory)
		_ = fs.WalkDir(host.FS, root, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				if strings.Count(strings.TrimPrefix(name, root), "/") > 4 {
					return fs.SkipDir
				}
				return nil
			}
			base := path.Base(name)
			primary := path.Base(path.Dir(name)) == "repodata" && strings.Contains(base, "primary.xml")
			if base != "repomd.xml" && base != "cachecookie" && !primary {
				return nil
			}
			if info, err := entry.Info(); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			return nil
		})
	}
	if newest.IsZero() {
		return nil // no cache, whatever the agent recorded
	}
	return timestamp(host.withOwnRefresh(newest))
}

func (host *Host) newestModTime(names ...string) time.Time {
	var newest time.Time
	for _, name := range names {
		if info, err := fs.Stat(host.FS, relative(name)); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// timestamp returns at in UTC, truncated to whole seconds, or nil when it
// is zero.
func timestamp(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	at = at.UTC().Truncate(time.Second)
	return &at
}

func lastLine(data []byte) string {
	all := lines(data)
	for index := len(all) - 1; index >= 0; index-- {
		if line := strings.TrimSpace(all[index]); line != "" {
			return line
		}
	}
	return ""
}
