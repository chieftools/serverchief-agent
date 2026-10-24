// Package updater implements agent self-updates: the capability the daemon
// reports, the request it hands over, and the detached helper
// (`serverchief internal-update`) that installs the requested version of
// the serverchief package from the Server Chief repository, waits for the
// new daemon to report ready and rolls back when it does not.
//
// Nothing from a command reaches a package manager except a version that
// passed ValidVersion. Package names, repositories and file paths are
// constants of this package.
package updater

import (
	"regexp"
	"strconv"
	"strings"
)

// versionPattern is a stable semantic version: three numeric components
// without leading zeros, no pre-release or build suffix. Components are
// capped at nine digits so they always fit an int.
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

// ValidVersion reports whether version is a stable semantic version
// (X.Y.Z). It is the only value from a command that the updater uses.
func ValidVersion(version string) bool {
	return versionPattern.MatchString(version)
}

// CompareVersions orders two valid versions: -1, 0 or 1.
func CompareVersions(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for index := range 3 {
		if index >= len(left) || index >= len(right) {
			break
		}
		x, _ := strconv.Atoi(left[index])
		y, _ := strconv.Atoi(right[index])
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}
