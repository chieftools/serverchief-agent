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
	// CacheAgeSeconds is the age of the package metadata the result is
	// based on; null when there is no cache. The agent never refreshes it.
	CacheAgeSeconds *int64 `json:"cache_age_seconds"`
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
// metadata cache. It never runs apt-get update or makecache.
type Updates struct {
	host *Host
}

func NewUpdates(host *Host) *Updates { return &Updates{host: host} }

func (*Updates) Name() string { return "updates" }

// ChangeToken implements ChangeDetector: installing or upgrading packages
// changes what is pending.
func (collector *Updates) ChangeToken() string {
	return collector.host.packageDatabaseToken()
}

func (collector *Updates) Collect(ctx context.Context) (any, error) {
	host := collector.host
	var info *UpdatesInfo
	var err error
	switch {
	case host.exists(dpkgStatusPath) && host.findCommand("apt-get") != "":
		info, err = host.aptUpdates(ctx)
	case host.findCommand("dnf") != "":
		info, err = host.rpmUpdates(ctx, ManagerDnf)
	case host.findCommand("yum") != "":
		info, err = host.rpmUpdates(ctx, ManagerYum)
	default:
		return nil, errors.New("no supported package manager (apt, dnf or yum) found")
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
func (host *Host) aptUpdates(ctx context.Context) (*UpdatesInfo, error) {
	result, err := host.run(ctx, "apt-get", "-s", "-q", "-o", "Debug::NoLocking=1", "dist-upgrade")
	if err != nil {
		return nil, fmt.Errorf("apt-get -s dist-upgrade: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("apt-get -s dist-upgrade exited with status %d: %s", result.ExitCode, firstLine(result.Stderr))
	}
	info := &UpdatesInfo{Manager: ManagerApt, Items: parseAptSimulation(result.Stdout)}
	required := host.exists("/var/run/reboot-required") || host.exists("/run/reboot-required")
	info.RebootRequired = &required
	info.CacheAgeSeconds = host.aptCacheAge()
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

// aptCacheAge uses the update-notifier stamp when present, otherwise the
// lists directory, whose partial/ subdirectory changes on every update.
func (host *Host) aptCacheAge() *int64 {
	entries, err := host.readDir("/var/lib/apt/lists")
	if err != nil {
		return nil
	}
	hasIndexes := slices.ContainsFunc(entries, func(entry fs.DirEntry) bool {
		name := entry.Name()
		return strings.Contains(name, "_Packages") || strings.HasSuffix(name, "Release")
	})
	if !hasIndexes {
		return nil
	}
	return host.ageOf(host.newestModTime("/var/lib/apt/periodic/update-success-stamp", "/var/lib/apt/lists", "/var/lib/apt/lists/partial"))
}

// rpmUpdates lists pending updates from yum or dnf with -C (cache only), so
// the metadata is never refreshed.
func (host *Host) rpmUpdates(ctx context.Context, manager string) (*UpdatesInfo, error) {
	result, err := host.run(ctx, manager, "-C", "-q", "check-update")
	if err != nil {
		return nil, fmt.Errorf("%s check-update: %w", manager, err)
	}
	switch result.ExitCode {
	case 0, 100: // 100: updates are available
	default:
		// yum explains at length and ends with the cause.
		message := lastLine(result.Stderr)
		if message == "" {
			message = lastLine(result.Stdout)
		}
		return nil, fmt.Errorf("%s -C check-update exited with status %d: %s (the agent never refreshes the metadata cache; run `%s makecache` or enable its timer)",
			manager, result.ExitCode, message, manager)
	}

	installed, err := host.queryRPM(ctx)
	if err != nil {
		return nil, err
	}
	current := make(map[string]string, len(installed))
	for _, item := range collapsePackages(installed, compareRPMVersions) {
		current[item.Name+"."+item.Arch] = item.Version
	}
	security := host.rpmSecurityUpdates(ctx, manager)

	info := &UpdatesInfo{Manager: manager, Items: []Update{}}
	for _, available := range parseCheckUpdate(result.Stdout) {
		key := available.Name + "." + available.Arch
		available.CurrentVersion = current[key]
		available.Security = security[key]
		info.Items = append(info.Items, available)
	}
	info.RebootRequired = host.needsRestarting(ctx)
	info.CacheAgeSeconds = host.rpmCacheAge(manager)
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
// advisories. Repositories without updateinfo (CentOS 7) yield none.
func (host *Host) rpmSecurityUpdates(ctx context.Context, manager string) map[string]bool {
	args := []string{"-C", "-q", "updateinfo", "list", "security"}
	if manager == ManagerDnf {
		args = []string{"-C", "-q", "updateinfo", "list", "--security"}
	}
	result, err := host.run(ctx, manager, args...)
	if err != nil || result.ExitCode != 0 {
		return map[string]bool{}
	}
	return parseUpdateinfo(result.Stdout)
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

// needsRestarting runs `needs-restarting -r` (yum-utils, dnf-utils):
// exit status 1 means a reboot is required, 0 that it is not.
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
		required := true
		return &required
	default:
		return nil
	}
}

// rpmCacheAge is the age of the newest repomd.xml (and yum's cachecookie)
// in the manager's cache directory.
func (host *Host) rpmCacheAge(manager string) *int64 {
	root := relative("/var/cache/" + manager)
	var newest time.Time
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
		if base := path.Base(name); base != "repomd.xml" && base != "cachecookie" {
			return nil
		}
		if info, err := entry.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return host.ageOf(newest)
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

func (host *Host) ageOf(at time.Time) *int64 {
	if at.IsZero() {
		return nil
	}
	age := max(int64(host.now().Sub(at)/time.Second), 0)
	return &age
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
