package collector

import (
	"regexp"
	"strings"
)

// OSRelease describes the distribution.
type OSRelease struct {
	// Family groups distributions that share a package manager and layout:
	// debian, rhel, suse, alpine, arch or unknown.
	Family string `json:"family"`
	// ID is the lowercase distribution identifier (ubuntu, debian, centos,
	// almalinux, cloudlinux, …).
	ID     string   `json:"id"`
	IDLike []string `json:"id_like"`
	Name   string   `json:"name"`
	// Version is the major release as packaged (VERSION_ID): "16.04", "12", "7".
	Version string `json:"version"`
	// VersionFull is the most precise version the host reports, such as
	// "16.04.7", "12.7" or "7.9.2009".
	VersionFull string `json:"version_full"`
	Codename    string `json:"codename"`
	PrettyName  string `json:"pretty_name"`
	// Source names the file the identity came from.
	Source string `json:"source"`
}

var (
	redhatReleasePattern = regexp.MustCompile(`^(.+?)\s+(?:Linux\s+)?(?:Server\s+)?release\s+([0-9][0-9.]*)\s*(?:\(([^)]*)\))?`)
	versionNumberPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*`)
	embeddedVersion      = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)+`)
	parenthesisPattern   = regexp.MustCompile(`\(([^)]+)\)`)
)

// readOSRelease identifies the distribution from /etc/os-release, with
// fallbacks for hosts that predate it or ship a minimal one.
func (host *Host) readOSRelease() OSRelease {
	var release OSRelease
	for _, candidate := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		data, err := host.readFile(candidate)
		if err != nil {
			continue
		}
		release = parseOSRelease(parseKeyValue(data))
		release.Source = candidate
		break
	}

	if data, err := host.readFile("/etc/redhat-release"); err == nil {
		mergeRedhatRelease(&release, strings.TrimSpace(string(data)))
	}
	if data, err := host.readFile("/etc/lsb-release"); err == nil {
		mergeLSBRelease(&release, parseKeyValue(data))
	}
	if release.ID == "debian" {
		if version := host.readOptional("/etc/debian_version"); versionNumberPattern.MatchString(version) {
			release.VersionFull = version
		}
	}
	if release.ID == "" && host.exists("/etc/debian_version") {
		release.ID = "debian"
		release.Name = "Debian GNU/Linux"
		release.Source = "/etc/debian_version"
		if version := host.readOptional("/etc/debian_version"); versionNumberPattern.MatchString(version) {
			release.VersionFull = version
			release.Version = strings.SplitN(version, ".", 2)[0]
		}
	}

	if release.VersionFull == "" {
		release.VersionFull = release.Version
	}
	if release.PrettyName == "" && release.Name != "" {
		release.PrettyName = strings.TrimSpace(release.Name + " " + release.VersionFull)
	}
	if release.IDLike == nil {
		release.IDLike = []string{}
	}
	release.Family = osFamily(release.ID, release.IDLike)
	if release.ID == "" {
		release.ID = "unknown"
	}
	return release
}

func parseOSRelease(values map[string]string) OSRelease {
	release := OSRelease{
		ID:         strings.ToLower(values["ID"]),
		Name:       values["NAME"],
		Version:    values["VERSION_ID"],
		Codename:   strings.ToLower(values["VERSION_CODENAME"]),
		PrettyName: values["PRETTY_NAME"],
	}
	if like := strings.Fields(strings.ToLower(values["ID_LIKE"])); len(like) > 0 {
		release.IDLike = like
	}
	if release.Codename == "" {
		release.Codename = strings.ToLower(values["UBUNTU_CODENAME"])
	}

	// VERSION often carries the point release: "16.04.7 LTS (Xenial Xerus)".
	if version := versionNumberPattern.FindString(values["VERSION"]); strings.HasPrefix(version, release.Version) {
		release.VersionFull = version
	}

	// Older Debian and Ubuntu releases only name the codename inside VERSION:
	// "8 (jessie)", "14.04.6 LTS, Trusty Tahr".
	if release.Codename == "" && (release.ID == "debian" || release.ID == "ubuntu") {
		version := values["VERSION"]
		if match := parenthesisPattern.FindStringSubmatch(version); match != nil {
			release.Codename = strings.ToLower(strings.Fields(match[1])[0])
		} else if _, after, ok := strings.Cut(version, ", "); ok && after != "" {
			release.Codename = strings.ToLower(strings.Fields(after)[0])
		}
	}
	return release
}

// mergeRedhatRelease adds the precise version from lines such as
// "CentOS Linux release 7.9.2009 (Core)", and identifies hosts without an
// os-release file at all (CentOS 6 and older RHEL clones).
func mergeRedhatRelease(release *OSRelease, line string) {
	match := redhatReleasePattern.FindStringSubmatch(line)
	if match == nil {
		return
	}
	name, version := match[1], match[2]
	if release.ID == "" {
		release.ID = redhatID(name)
		release.Name = name
		release.Version = strings.SplitN(version, ".", 2)[0]
		release.PrettyName = line
		release.IDLike = []string{"rhel"}
		release.Source = "/etc/redhat-release"
	}
	if strings.HasPrefix(version, release.Version) && len(version) > len(release.VersionFull) {
		release.VersionFull = version
	}
}

func redhatID(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "centos"):
		return "centos"
	case strings.Contains(lower, "cloudlinux"):
		return "cloudlinux"
	case strings.Contains(lower, "almalinux"):
		return "almalinux"
	case strings.Contains(lower, "rocky"):
		return "rocky"
	case strings.Contains(lower, "oracle"):
		return "ol"
	case strings.Contains(lower, "fedora"):
		return "fedora"
	case strings.Contains(lower, "red hat"):
		return "rhel"
	default:
		return strings.ReplaceAll(lower, " ", "")
	}
}

// mergeLSBRelease fills gaps from /etc/lsb-release, which Ubuntu has
// shipped since long before os-release existed.
func mergeLSBRelease(release *OSRelease, values map[string]string) {
	id := strings.ToLower(values["DISTRIB_ID"])
	if id == "" {
		return
	}
	if release.ID == "" {
		release.ID = id
		release.Name = values["DISTRIB_ID"]
		release.Version = values["DISTRIB_RELEASE"]
		release.PrettyName = values["DISTRIB_DESCRIPTION"]
		release.Source = "/etc/lsb-release"
		if id == "ubuntu" {
			release.IDLike = []string{"debian"}
		}
	}
	if id != release.ID {
		return
	}
	if release.Codename == "" {
		release.Codename = strings.ToLower(values["DISTRIB_CODENAME"])
	}
	if version := embeddedVersion.FindString(values["DISTRIB_DESCRIPTION"]); strings.HasPrefix(version, release.Version) && len(version) > len(release.VersionFull) {
		release.VersionFull = version
	}
}

func osFamily(id string, like []string) string {
	for _, candidate := range append([]string{id}, like...) {
		switch candidate {
		case "debian", "ubuntu", "raspbian", "linuxmint":
			return "debian"
		case "rhel", "centos", "fedora", "almalinux", "rocky", "cloudlinux", "ol", "amzn", "scientific":
			return "rhel"
		case "suse", "opensuse", "sles", "sled":
			return "suse"
		case "alpine":
			return "alpine"
		case "arch", "archlinux", "manjaro":
			return "arch"
		}
	}
	return "unknown"
}
