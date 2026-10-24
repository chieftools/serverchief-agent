package updater

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
)

// This is the only file in the agent that spells out package-manager
// verbs which change the system. TestNoMutatingPackageVerbsInSource
// allows them here and nowhere else, and TestPackageCommandAllowList pins
// every argv built below.

// PackageName is the only package the updater installs.
const PackageName = "serverchief"

// RepositoryID is the yum/dnf repository the updater installs from.
const RepositoryID = "serverchief"

// The Server Chief repository configuration the updater restricts the
// package manager to.
const (
	AptListPath    = "/etc/apt/sources.list.d/serverchief.list"
	AptSourcesPath = "/etc/apt/sources.list.d/serverchief.sources"
	YumRepoPath    = "/etc/yum.repos.d/serverchief.repo"
)

// Manager names a package manager family the updater drives.
type Manager string

const (
	ManagerApt Manager = "apt"
	ManagerDnf Manager = "dnf"
	ManagerYum Manager = "yum"
)

// Commands builds the updater's package-manager argv for one host. Every
// field is detected locally; none comes from a command.
type Commands struct {
	Manager Manager
	// Tool is the absolute path of apt-get, dnf or yum.
	Tool string
	// AptCache is the absolute path of apt-cache (apt only).
	AptCache string
	// Sources is AptListPath or AptSourcesPath (apt only).
	Sources string
}

func (commands Commands) validate() error {
	tools := map[Manager]string{ManagerApt: "apt-get", ManagerDnf: "dnf", ManagerYum: "yum"}
	want, known := tools[commands.Manager]
	if !known {
		return fmt.Errorf("unsupported package manager %q", commands.Manager)
	}
	if !filepath.IsAbs(commands.Tool) || filepath.Base(commands.Tool) != want {
		return fmt.Errorf("%s: invalid tool path %q", commands.Manager, commands.Tool)
	}
	if commands.Manager == ManagerApt {
		if !filepath.IsAbs(commands.AptCache) || filepath.Base(commands.AptCache) != "apt-cache" {
			return fmt.Errorf("apt: invalid apt-cache path %q", commands.AptCache)
		}
		if commands.Sources != AptListPath && commands.Sources != AptSourcesPath {
			return fmt.Errorf("apt: sources must be %s or %s", AptListPath, AptSourcesPath)
		}
	}
	return nil
}

// aptSources restricts apt to the Server Chief source: the main list is
// replaced by the Server Chief file (apt parses a .sources file there as
// deb822), no sources.list.d parts are read, and apt-get update keeps
// the other repositories' lists instead of cleaning them up.
func (commands Commands) aptSources() []string {
	return []string{
		"-o", "Dir::Etc::sourcelist=" + commands.Sources,
		"-o", "Dir::Etc::sourceparts=-",
		"-o", "APT::Get::List-Cleanup=0",
	}
}

// rpmRepository restricts yum/dnf to the Server Chief repository and makes
// them exit instead of waiting when another instance holds their lock.
func rpmRepository() []string {
	return []string{"--disablerepo=*", "--enablerepo=" + RepositoryID, "--setopt=exit_on_lock=True"}
}

// Refresh refreshes the Server Chief repository's metadata only.
func (commands Commands) Refresh() ([]string, error) {
	if err := commands.validate(); err != nil {
		return nil, err
	}
	switch commands.Manager {
	case ManagerApt:
		return slices.Concat([]string{commands.Tool, "update", "-qq"}, commands.aptSources()), nil
	case ManagerDnf:
		return slices.Concat([]string{commands.Tool, "-q"}, rpmRepository(), []string{"--refresh", "makecache"}), nil
	default:
		// yum 3 has no --refresh: expire the repository's metadata, and
		// the availability query downloads it again.
		return slices.Concat([]string{commands.Tool, "-q"}, rpmRepository(), []string{"clean", "expire-cache"}), nil
	}
}

// Available lists the serverchief versions the Server Chief repository
// offers. Installed packages are not listed unless the repository still
// has them.
func (commands Commands) Available() ([]string, error) {
	if err := commands.validate(); err != nil {
		return nil, err
	}
	switch commands.Manager {
	case ManagerApt:
		return slices.Concat([]string{commands.AptCache}, commands.aptSources()[:4], []string{"madison", PackageName}), nil
	default:
		return slices.Concat([]string{commands.Tool, "-q"}, rpmRepository(), []string{"--showduplicates", "list", "available", PackageName}), nil
	}
}

// Install installs version of the serverchief package from the Server
// Chief repository, upgrading or downgrading from installed. Nothing else
// is installed or upgraded: apt only upgrades an installed package
// (--only-upgrade) and keeps modified configuration files, yum/dnf only
// see the Server Chief repository.
func (commands Commands) Install(version, installed string) ([]string, error) {
	if !ValidVersion(version) {
		return nil, errors.New("install: invalid version")
	}
	if err := commands.validate(); err != nil {
		return nil, err
	}
	switch commands.Manager {
	case ManagerApt:
		return slices.Concat(
			[]string{commands.Tool, "install", "-y", "--only-upgrade", "--allow-downgrades",
				"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"},
			commands.aptSources(),
			[]string{PackageName + "=" + version},
		), nil
	default:
		verb := "install"
		if ValidVersion(installed) && CompareVersions(version, installed) < 0 {
			verb = "downgrade"
		}
		return slices.Concat([]string{commands.Tool}, rpmRepository(), []string{verb, "-y", PackageName + "-" + version}), nil
	}
}
