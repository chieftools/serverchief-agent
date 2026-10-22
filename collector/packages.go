package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

// Package managers.
const (
	ManagerDpkg = "dpkg"
	ManagerRPM  = "rpm"
)

// PackagesInfo is the payload of the packages dataset, sent as a delta
// keyed by manager/name/arch.
type PackagesInfo struct {
	Manager string    `json:"manager"`
	Items   []Package `json:"items"`
}

// Package is one installed package.
type Package struct {
	Name string `json:"name"`
	// Version is [epoch:]version-release on rpm and the Debian version on
	// dpkg. With several versions installed it is the highest.
	Version string `json:"version"`
	Arch    string `json:"arch"`
	// Source is the source package, when it differs from Name.
	Source string `json:"source,omitempty"`
	// Versions lists every installed version, ascending, when more than
	// one is installed under the same name and arch (rpm installonly
	// packages such as kernel-core).
	Versions []string `json:"versions,omitempty"`
}

// PackageKey is the delta key: manager/name/arch.
func PackageKey(manager string, item Package) string {
	return manager + "/" + item.Name + "/" + item.Arch
}

// Normalize sorts items by their delta key, bytewise, the order the
// backend restores after applying a delta.
func (info *PackagesInfo) Normalize() {
	slices.SortFunc(info.Items, func(a, b Package) int {
		return strings.Compare(PackageKey(info.Manager, a), PackageKey(info.Manager, b))
	})
}

// rpmQueryFormat is the explicit `rpm -qa --queryformat`: one tab separated
// line per package. %{EPOCH} prints "(none)" when unset; %{EPOCHNUM} is
// missing from older rpm releases.
const rpmQueryFormat = `%{NAME}\t%{EPOCH}\t%{VERSION}\t%{RELEASE}\t%{ARCH}\t%{SOURCERPM}\n`

// Package database locations. The first existing rpm database is used for
// change detection.
var (
	dpkgStatusPath = "/var/lib/dpkg/status"
	rpmDatabases   = []string{
		"/var/lib/rpm/rpmdb.sqlite",          // EL9
		"/usr/lib/sysimage/rpm/rpmdb.sqlite", // EL10, Fedora
		"/var/lib/rpm/Packages",              // EL7/EL8 Berkeley DB
		"/var/lib/rpm/Packages.db",
	}
)

// errNoPackageManager means neither dpkg nor rpm was found.
var errNoPackageManager = errors.New("no supported package manager (dpkg or rpm) found")

// Packages collects the packages dataset.
type Packages struct {
	host *Host
}

func NewPackages(host *Host) *Packages { return &Packages{host: host} }

func (*Packages) Name() string { return "packages" }

// DeltaKeyFields implements KeyedDataset: manager comes from the payload's
// top level, name and arch from each item.
func (*Packages) DeltaKeyFields() []string { return []string{"manager", "name", "arch"} }

// ChangeToken implements ChangeDetector with the package database's
// modification time and size.
func (collector *Packages) ChangeToken() string {
	return collector.host.packageDatabaseToken()
}

func (collector *Packages) Collect(ctx context.Context) (any, error) {
	host := collector.host
	switch host.packageManager() {
	case ManagerDpkg:
		data, err := host.readFile(dpkgStatusPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", dpkgStatusPath, err)
		}
		info := &PackagesInfo{Manager: ManagerDpkg, Items: collapsePackages(parseDpkgStatus(data), compareDebianVersions)}
		info.Normalize()
		return info, nil
	case ManagerRPM:
		packages, err := host.queryRPM(ctx)
		if err != nil {
			return nil, err
		}
		info := &PackagesInfo{Manager: ManagerRPM, Items: collapsePackages(packages, compareRPMVersions)}
		info.Normalize()
		return info, nil
	default:
		return nil, errNoPackageManager
	}
}

// packageManager returns dpkg when its status database exists, rpm when an
// rpm database exists, or "".
func (host *Host) packageManager() string {
	if host.exists(dpkgStatusPath) {
		return ManagerDpkg
	}
	for _, database := range rpmDatabases {
		if host.exists(database) {
			return ManagerRPM
		}
	}
	return ""
}

func (host *Host) packageDatabaseToken() string {
	candidates := append([]string{dpkgStatusPath}, rpmDatabases...)
	for _, candidate := range candidates {
		if info, err := fs.Stat(host.FS, relative(candidate)); err == nil {
			return fmt.Sprintf("%s@%d/%d", candidate, info.ModTime().UnixNano(), info.Size())
		}
	}
	return ""
}

// parseDpkgStatus returns the installed packages of a dpkg status file.
func parseDpkgStatus(data []byte) []Package {
	var packages []Package
	fields := map[string]string{}
	last := ""
	flush := func() {
		if dpkgInstalled(fields["Status"]) && fields["Package"] != "" {
			item := Package{Name: fields["Package"], Version: fields["Version"], Arch: fields["Architecture"]}
			if source, _, _ := strings.Cut(fields["Source"], " "); source != "" && source != item.Name {
				item.Source = source
			}
			packages = append(packages, item)
		}
		clear(fields)
		last = ""
	}
	for _, line := range lines(data) {
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case line[0] == ' ' || line[0] == '\t':
			// Continuation of a multi-line field such as Description.
			if last != "" && last != "Description" && last != "Conffiles" {
				fields[last] += "\n" + strings.TrimSpace(line)
			}
		default:
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			last = key
			fields[key] = strings.TrimSpace(value)
		}
	}
	flush()
	return packages
}

// dpkgInstalled reports whether a Status field ("want flag status") says
// the package is fully installed.
func dpkgInstalled(status string) bool {
	parts := strings.Fields(status)
	return len(parts) == 3 && parts[2] == "installed"
}

// queryRPM lists installed packages with rpm -qa.
func (host *Host) queryRPM(ctx context.Context) ([]Package, error) {
	result, err := host.run(ctx, "rpm", "-qa", "--queryformat", rpmQueryFormat)
	if err != nil {
		return nil, fmt.Errorf("rpm -qa: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("rpm -qa exited with status %d: %s", result.ExitCode, firstLine(result.Stderr))
	}
	return parseRPMQuery(result.Stdout), nil
}

// parseRPMQuery parses rpmQueryFormat output. gpg-pubkey pseudo packages
// (imported signing keys) are skipped.
func parseRPMQuery(data []byte) []Package {
	var packages []Package
	for _, line := range lines(data) {
		parts := strings.Split(line, "\t")
		if len(parts) != 6 || parts[0] == "" || parts[0] == "gpg-pubkey" {
			continue
		}
		item := Package{
			Name:    parts[0],
			Version: rpmEVR(parts[1], parts[2], parts[3]),
			Arch:    rpmNone(parts[4]),
		}
		if source := rpmSourceName(rpmNone(parts[5])); source != "" && source != item.Name {
			item.Source = source
		}
		packages = append(packages, item)
	}
	return packages
}

func rpmNone(value string) string {
	if value == "(none)" {
		return ""
	}
	return value
}

// rpmEVR formats [epoch:]version-release.
func rpmEVR(epoch, version, release string) string {
	evr := version
	if release = rpmNone(release); release != "" {
		evr += "-" + release
	}
	if epoch = rpmNone(epoch); epoch != "" {
		evr = epoch + ":" + evr
	}
	return evr
}

// rpmSourceName turns "openssl-1.0.2k-26.el7_9.src.rpm" into "openssl".
func rpmSourceName(sourceRPM string) string {
	name := strings.TrimSuffix(strings.TrimSuffix(sourceRPM, ".src.rpm"), ".nosrc.rpm")
	if name == sourceRPM {
		return ""
	}
	for range 2 {
		index := strings.LastIndexByte(name, '-')
		if index <= 0 {
			return ""
		}
		name = name[:index]
	}
	return name
}

// collapsePackages merges packages installed more than once under the same
// name and arch into one item with the highest Version and all Versions.
func collapsePackages(packages []Package, compare func(a, b string) int) []Package {
	type key struct{ name, arch string }
	index := make(map[key]int, len(packages))
	result := make([]Package, 0, len(packages))
	for _, item := range packages {
		position, seen := index[key{item.Name, item.Arch}]
		if !seen {
			index[key{item.Name, item.Arch}] = len(result)
			result = append(result, item)
			continue
		}
		existing := &result[position]
		versions := existing.Versions
		if versions == nil {
			versions = []string{existing.Version}
		}
		if !slices.Contains(versions, item.Version) {
			versions = append(versions, item.Version)
		}
		slices.SortFunc(versions, compare)
		existing.Versions = versions
		existing.Version = versions[len(versions)-1]
		if compare(item.Version, existing.Version) >= 0 && item.Source != "" {
			existing.Source = item.Source
		}
		if len(existing.Versions) == 1 {
			existing.Versions = nil
		}
	}
	return result
}

// compareRPMVersions orders [epoch:]version-release strings like rpm.
func compareRPMVersions(a, b string) int {
	epochA, restA := splitEpoch(a)
	epochB, restB := splitEpoch(b)
	if epochA != epochB {
		if epochA < epochB {
			return -1
		}
		return 1
	}
	versionA, releaseA, _ := cutLast(restA, "-")
	versionB, releaseB, _ := cutLast(restB, "-")
	if result := rpmvercmp(versionA, versionB); result != 0 {
		return result
	}
	return rpmvercmp(releaseA, releaseB)
}

// compareDebianVersions is a rough dpkg ordering. dpkg keeps one version
// per name and arch, so it only breaks ties in malformed databases.
func compareDebianVersions(a, b string) int {
	epochA, restA := splitEpoch(a)
	epochB, restB := splitEpoch(b)
	if epochA != epochB {
		if epochA < epochB {
			return -1
		}
		return 1
	}
	return rpmvercmp(restA, restB)
}

func splitEpoch(value string) (int, string) {
	if before, after, ok := strings.Cut(value, ":"); ok {
		if epoch, err := strconv.Atoi(before); err == nil {
			return epoch, after
		}
	}
	return 0, value
}

func cutLast(value, separator string) (string, string, bool) {
	index := strings.LastIndex(value, separator)
	if index < 0 {
		return value, "", false
	}
	return value[:index], value[index+len(separator):], true
}

// rpmvercmp compares version or release strings segment by segment the way
// rpm does: digits numerically, letters lexically, "~" sorts before
// anything and "^" after the end.
func rpmvercmp(a, b string) int {
	if a == b {
		return 0
	}
	isAlnum := func(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	for {
		for a != "" && !isAlnum(a[0]) && a[0] != '~' && a[0] != '^' {
			a = a[1:]
		}
		for b != "" && !isAlnum(b[0]) && b[0] != '~' && b[0] != '^' {
			b = b[1:]
		}
		if strings.HasPrefix(a, "~") || strings.HasPrefix(b, "~") {
			if !strings.HasPrefix(a, "~") {
				return 1
			}
			if !strings.HasPrefix(b, "~") {
				return -1
			}
			a, b = a[1:], b[1:]
			continue
		}
		if strings.HasPrefix(a, "^") || strings.HasPrefix(b, "^") {
			if a == "" {
				return -1
			}
			if b == "" {
				return 1
			}
			if !strings.HasPrefix(a, "^") {
				return 1
			}
			if !strings.HasPrefix(b, "^") {
				return -1
			}
			a, b = a[1:], b[1:]
			continue
		}
		if a == "" || b == "" {
			break
		}
		numeric := isDigit(a[0])
		take := func(value string) (string, string) {
			end := 0
			for end < len(value) && isAlnum(value[end]) && isDigit(value[end]) == numeric {
				end++
			}
			return value[:end], value[end:]
		}
		var segmentA, segmentB string
		segmentA, a = take(a)
		segmentB, b = take(b)
		if segmentB == "" {
			if numeric {
				return 1
			}
			return -1
		}
		if numeric {
			segmentA = strings.TrimLeft(segmentA, "0")
			segmentB = strings.TrimLeft(segmentB, "0")
			if len(segmentA) != len(segmentB) {
				if len(segmentA) < len(segmentB) {
					return -1
				}
				return 1
			}
		}
		if result := strings.Compare(segmentA, segmentB); result != 0 {
			return result
		}
	}
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return -1
	default:
		return 1
	}
}

func firstLine(data []byte) string {
	text := strings.TrimSpace(string(data))
	line, _, _ := strings.Cut(text, "\n")
	return line
}
