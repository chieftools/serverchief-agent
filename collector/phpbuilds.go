package collector

import (
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

// PHPBuild is a PHP installation no package manager owns: a DirectAdmin
// custombuild in /usr/local/php82 or a build from source under /opt.
type PHPBuild struct {
	// Prefix is where the build is installed, such as /usr/local/php82.
	Prefix string `json:"prefix"`
	// Version is the full version the build reports, such as 8.2.35.
	Version string `json:"version"`
}

// maximumPHPBuilds caps the builds reported, which only hosts building
// every PHP release themselves come near.
const maximumPHPBuilds = 32

// phpBuildConfigs are where builds keep php-config, the shell script that
// records how PHP was built. Packages install theirs under /usr/bin or their
// own trees (/opt/plesk, /opt/cpanel, /opt/alt), which the package list
// covers already.
var phpBuildConfigs = []string{
	"usr/local/bin/php-config",
	"usr/local/php*/bin/php-config",
	"opt/php*/bin/php-config",
}

var (
	phpConfigVersion = regexp.MustCompile(`(?m)^version="([0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+~-]{0,32})"\s*$`)
	phpConfigPrefix  = regexp.MustCompile(`(?m)^prefix="(/[^"\s]{0,255})"\s*$`)
)

// readPHPBuilds finds PHP builds by reading their php-config scripts, never
// by running a binary. A build is reported once by its prefix.
func (host *Host) readPHPBuilds() []PHPBuild {
	builds := []PHPBuild{}
	seen := make(map[string]bool)
	for _, pattern := range phpBuildConfigs {
		matches, err := fs.Glob(host.FS, pattern)
		if err != nil {
			continue
		}
		for _, match := range matches {
			data, err := host.readFile("/" + match)
			if err != nil {
				continue
			}
			version := phpConfigVersion.FindSubmatch(data)
			if version == nil {
				continue
			}
			prefix := path.Dir(path.Dir("/" + match))
			if found := phpConfigPrefix.FindSubmatch(data); found != nil {
				prefix = path.Clean(string(found[1]))
			}
			if seen[prefix] {
				continue
			}
			seen[prefix] = true
			builds = append(builds, PHPBuild{Prefix: prefix, Version: strings.TrimSpace(string(version[1]))})
		}
	}
	sort.Slice(builds, func(i, j int) bool { return builds[i].Prefix < builds[j].Prefix })
	if len(builds) > maximumPHPBuilds {
		builds = builds[:maximumPHPBuilds]
	}
	return builds
}
