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
	// SourceVersion is the source package's version, when it differs from
	// Version: dpkg's "Source: name (version)" (binNMUs, binaries with
	// their own version), or rpm's SOURCERPM version-release, which has no
	// epoch.
	SourceVersion string `json:"source_version,omitempty"`
	// Versions lists every installed version, ascending, when more than
	// one is installed under the same name and arch (rpm installonly
	// packages such as kernel-core).
	Versions []string `json:"versions,omitempty"`
	// Origins are the sorted ids of the repositories (the repositories
	// dataset) that offer the installed version. Empty means none does
	// (a manually installed package, a removed repository, a version gone
	// from the mirror); nil, which is left out, means it could not be
	// determined, for example before the first apt-get update.
	Origins []string `json:"origins,omitzero"`
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

// Package database locations. The first existing one is what the daemon's
// package activity trigger stats (PackageSystem.DatabaseToken).
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
	host    *Host
	origins *Origins
}

// NewPackages returns the collector with a private origin resolver.
func NewPackages(host *Host) *Packages { return NewPackagesWithOrigins(host, nil) }

// NewPackagesWithOrigins returns the collector sharing origins with the
// repositories collector; nil uses a private one.
func NewPackagesWithOrigins(host *Host, origins *Origins) *Packages {
	if origins == nil {
		origins = NewOrigins(host)
	}
	return &Packages{host: host, origins: origins}
}

func (*Packages) Name() string { return "packages" }

// DeltaKeyFields implements KeyedDataset: manager comes from the payload's
// top level, name and arch from each item.
func (*Packages) DeltaKeyFields() []string { return []string{"manager", "name", "arch"} }

func (collector *Packages) Collect(ctx context.Context) (any, error) {
	host := collector.host
	var info *PackagesInfo
	switch host.packageManager() {
	case ManagerDpkg:
		data, err := host.readFile(dpkgStatusPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", dpkgStatusPath, err)
		}
		info = &PackagesInfo{Manager: ManagerDpkg, Items: collapsePackages(parseDpkgStatus(data), compareDebianVersions)}
	case ManagerRPM:
		packages, err := host.queryRPM(ctx)
		if err != nil {
			return nil, err
		}
		info = &PackagesInfo{Manager: ManagerRPM, Items: collapsePackages(packages, compareRPMVersions)}
	default:
		return nil, errNoPackageManager
	}
	if collector.origins != nil {
		index, err := collector.origins.resolve(ctx)
		if err != nil {
			return nil, err
		}
		index.apply(info.Items)
	}
	info.Normalize()
	return info, nil
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
			source, version := parseDpkgSource(fields["Source"])
			if source != "" && source != item.Name {
				item.Source = source
			}
			if version != "" && version != item.Version {
				item.SourceVersion = version
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

// parseDpkgSource splits a Source field, "name" or "name (version)".
func parseDpkgSource(field string) (name, version string) {
	name, rest, _ := strings.Cut(strings.TrimSpace(field), " ")
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "(") && strings.HasSuffix(rest, ")") {
		version = strings.TrimSpace(rest[1 : len(rest)-1])
	}
	return name, version
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
		source, sourceVersion := rpmSource(rpmNone(parts[5]))
		if source != "" && source != item.Name {
			item.Source = source
		}
		// A source RPM's file name carries no epoch, and subpackages can have
		// their own (perl 4: versus perl-Carp 0:), so the source version is
		// sent without one and compared with the binary's version-release.
		if sourceVersion != "" && sourceVersion != rpmEVR("", parts[2], parts[3]) {
			item.SourceVersion = sourceVersion
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

// rpmEVR formats [epoch:]version-release; an epoch of 0 is left out,
// like an unset one.
func rpmEVR(epoch, version, release string) string {
	evr := version
	if release = rpmNone(release); release != "" {
		evr += "-" + release
	}
	if epoch = rpmNone(epoch); epoch != "" && epoch != "0" {
		evr = epoch + ":" + evr
	}
	return evr
}

// rpmSource splits "openssl-3.0.7-27.el9.src.rpm" into "openssl" and
// "3.0.7-27.el9".
func rpmSource(sourceRPM string) (name, versionRelease string) {
	base := strings.TrimSuffix(strings.TrimSuffix(sourceRPM, ".src.rpm"), ".nosrc.rpm")
	if base == sourceRPM {
		return "", ""
	}
	release := strings.LastIndexByte(base, '-')
	if release <= 0 {
		return "", ""
	}
	version := strings.LastIndexByte(base[:release], '-')
	if version <= 0 || release == len(base)-1 || version == release-1 {
		return "", ""
	}
	return base[:version], base[version+1:]
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
		// The source of the highest version describes the item.
		if compare(item.Version, existing.Version) >= 0 {
			if item.Source != "" {
				existing.Source = item.Source
			}
			existing.SourceVersion = item.SourceVersion
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
