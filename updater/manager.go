package updater

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ErrBusy means a package manager held its lock. errors.Is matches a
// *BusyError.
var ErrBusy = errors.New("package manager is busy")

// BusyError names what was locked.
type BusyError struct{ Lock string }

func (err *BusyError) Error() string        { return "package manager is busy: " + err.Lock }
func (err *BusyError) Is(target error) bool { return target == ErrBusy }

// Installed is the serverchief package as the package database reports it.
type Installed struct {
	// Version is "" when the package is not installed.
	Version string
	// Intact is false while dpkg or rpm report a half-installed package.
	Intact bool
}

// PackageManager is what the helper needs from apt, dnf or yum.
type PackageManager interface {
	Installed(ctx context.Context) (Installed, error)
	// Refresh refreshes the Server Chief repository's metadata.
	Refresh(ctx context.Context) error
	// Available lists the versions the Server Chief repository offers.
	Available(ctx context.Context) ([]string, error)
	// Install installs version, upgrading or downgrading from installed.
	Install(ctx context.Context, version, installed string) error
}

// Output is a finished command.
type Output struct {
	Stdout, Stderr []byte
	ExitCode       int
}

// Runner runs argv with extra environment variables. A non-zero exit is
// not an error; failing to start is.
type Runner func(ctx context.Context, env []string, argv []string) (Output, error)

// ExecRunner runs commands with os/exec, without a shell.
func ExecRunner(ctx context.Context, env []string, argv []string) (Output, error) {
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = append(os.Environ(), env...)
	command.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	output := Output{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		output.ExitCode = exit.ExitCode()
		return output, nil
	}
	return output, err
}

// Timeouts of the package-manager steps.
const (
	queryTimeout   = 2 * time.Minute
	refreshTimeout = 5 * time.Minute
	installTimeout = 20 * time.Minute
)

// aptEnvironment keeps apt and dpkg from prompting and needrestart from
// restarting services on its own.
var aptEnvironment = []string{"DEBIAN_FRONTEND=noninteractive", "APT_LISTCHANGES_FRONTEND=none", "NEEDRESTART_SUSPEND=1"}

// SystemManager drives the host's package manager through Commands.
type SystemManager struct {
	Commands Commands
	// DpkgQuery or RPM is the database query tool of the family.
	DpkgQuery string
	RPM       string
	Run       Runner
}

// Tools locates programs under a root directory, for tests.
type Tools struct {
	Root string
}

var toolDirectories = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin", "/usr/local/bin"}

// Find returns the absolute path (without Root) of an executable name, or "".
func (tools Tools) Find(name string) string {
	for _, directory := range toolDirectories {
		path := filepath.Join(directory, name)
		info, err := os.Stat(filepath.Join(tools.Root, path))
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path
		}
	}
	return ""
}

// Exists reports whether a regular file exists under Root.
func (tools Tools) Exists(path string) bool {
	info, err := os.Stat(filepath.Join(tools.Root, path))
	return err == nil && info.Mode().IsRegular()
}

// DetectManager returns the package manager and Server Chief repository
// of the host, or an error explaining why updates are not possible.
func DetectManager(tools Tools, run Runner) (*SystemManager, error) {
	if run == nil {
		run = ExecRunner
	}
	if tools.Exists("/var/lib/dpkg/status") && tools.Find("dpkg-query") != "" {
		manager := &SystemManager{DpkgQuery: tools.Find("dpkg-query"), Run: run}
		manager.Commands = Commands{Manager: ManagerApt, Tool: tools.Find("apt-get"), AptCache: tools.Find("apt-cache")}
		switch {
		case tools.Exists(AptListPath):
			manager.Commands.Sources = AptListPath
		case tools.Exists(AptSourcesPath):
			manager.Commands.Sources = AptSourcesPath
		default:
			return manager, errRepositoryMissing
		}
		if manager.Commands.Tool == "" || manager.Commands.AptCache == "" {
			return manager, errors.New("apt-get not found")
		}
		return manager, nil
	}
	if rpm := tools.Find("rpm"); rpm != "" {
		manager := &SystemManager{RPM: rpm, Run: run}
		switch {
		case tools.Find("dnf") != "":
			manager.Commands = Commands{Manager: ManagerDnf, Tool: tools.Find("dnf")}
		case tools.Find("yum") != "":
			manager.Commands = Commands{Manager: ManagerYum, Tool: tools.Find("yum")}
		default:
			return manager, errors.New("neither dnf nor yum found")
		}
		if !tools.Exists(YumRepoPath) {
			return manager, errRepositoryMissing
		}
		return manager, nil
	}
	return nil, errors.New("no supported package manager (dpkg/apt or rpm/dnf/yum)")
}

var errRepositoryMissing = errors.New("repository not configured")

// Installed queries the package database.
func (manager *SystemManager) Installed(ctx context.Context) (Installed, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	if manager.DpkgQuery != "" {
		output, err := manager.Run(ctx, nil, []string{manager.DpkgQuery, "-W", "-f=${db:Status-Abbrev}|${Version}\n", PackageName})
		if err != nil {
			return Installed{}, fmt.Errorf("dpkg-query: %w", err)
		}
		if output.ExitCode != 0 {
			// Exit 1: no such package.
			return Installed{}, nil
		}
		return parseDpkgQuery(output.Stdout), nil
	}
	output, err := manager.Run(ctx, nil, []string{manager.RPM, "-q", "--qf", "%{VERSION}\n", PackageName})
	if err != nil {
		return Installed{}, fmt.Errorf("rpm -q: %w", err)
	}
	if output.ExitCode != 0 {
		return Installed{}, nil
	}
	return parseRPMQuery(output.Stdout), nil
}

// parseDpkgQuery reads "ii |0.9.1": the desired action, the package
// status and an error flag. Status n (not installed) or c (only config
// files left) means nothing is installed; anything but i, or an error
// flag, is a package dpkg did not finish installing.
func parseDpkgQuery(stdout []byte) Installed {
	line, _, _ := strings.Cut(strings.TrimSpace(string(stdout)), "\n")
	status, version, found := strings.Cut(line, "|")
	status, version = strings.TrimSpace(status), strings.TrimSpace(version)
	if !found || len(status) < 2 || status[1] == 'n' || status[1] == 'c' || version == "" {
		return Installed{}
	}
	return Installed{Version: version, Intact: status[1] == 'i' && len(status) == 2}
}

// parseRPMQuery reads one version per installed instance; more than one
// means an interrupted transaction.
func parseRPMQuery(stdout []byte) Installed {
	versions := strings.Fields(string(stdout))
	switch len(versions) {
	case 0:
		return Installed{}
	case 1:
		return Installed{Version: versions[0], Intact: true}
	default:
		slices.SortFunc(versions, func(a, b string) int {
			if ValidVersion(a) && ValidVersion(b) {
				return CompareVersions(a, b)
			}
			return strings.Compare(a, b)
		})
		return Installed{Version: versions[len(versions)-1], Intact: false}
	}
}

// Refresh runs the restricted metadata refresh.
func (manager *SystemManager) Refresh(ctx context.Context) error {
	argv, err := manager.Commands.Refresh()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	return manager.check(manager.run(ctx, argv))
}

// Available runs the restricted availability query and parses it.
func (manager *SystemManager) Available(ctx context.Context) ([]string, error) {
	argv, err := manager.Commands.Available()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	output, err := manager.run(ctx, argv)
	if manager.Commands.Manager != ManagerApt && err == nil && output.ExitCode == 1 && len(bytes.TrimSpace(output.Stdout)) == 0 {
		// yum/dnf exit 1 when nothing matches.
		if _, busy := lockFailure(manager.Commands.Manager, output); !busy {
			return nil, nil
		}
	}
	if err := manager.check(output, err); err != nil {
		return nil, err
	}
	if manager.Commands.Manager == ManagerApt {
		return parseMadison(output.Stdout), nil
	}
	return parseRPMList(output.Stdout), nil
}

// Install installs version from the Server Chief repository.
func (manager *SystemManager) Install(ctx context.Context, version, installed string) error {
	argv, err := manager.Commands.Install(version, installed)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	return manager.check(manager.run(ctx, argv))
}

func (manager *SystemManager) run(ctx context.Context, argv []string) (Output, error) {
	var env []string
	if manager.Commands.Manager == ManagerApt {
		env = aptEnvironment
	}
	return manager.Run(ctx, env, argv)
}

// check turns a failed run into an error naming the cause, and a lock
// held by someone else into a BusyError.
func (manager *SystemManager) check(output Output, err error) error {
	name := filepath.Base(manager.Commands.Tool)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if message, busy := lockFailure(manager.Commands.Manager, output); busy {
		return &BusyError{Lock: message}
	}
	if output.ExitCode != 0 {
		return fmt.Errorf("%s exited with status %d: %s", name, output.ExitCode, lastLines(output, 3))
	}
	if manager.Commands.Manager == ManagerApt {
		// Old apt exits 0 when an index failed to download.
		for _, line := range strings.Split(string(output.Stderr), "\n") {
			if line = strings.TrimSpace(line); strings.HasPrefix(line, "E: ") || strings.HasPrefix(line, "W: Failed to fetch") {
				return fmt.Errorf("%s: %s", name, line)
			}
		}
	}
	return nil
}

// lockFailure recognises a run that gave up on a lock.
func lockFailure(manager Manager, output Output) (string, bool) {
	text := string(output.Stderr) + "\n" + string(output.Stdout)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case manager == ManagerApt && (strings.Contains(line, "Could not get lock") || strings.Contains(line, "Unable to acquire the dpkg frontend lock") || strings.Contains(line, "Unable to lock")):
			return line, true
		case manager != ManagerApt && (strings.HasPrefix(line, "Existing lock ") || strings.Contains(line, " already locked by ")):
			return line, true
		}
	}
	if manager != ManagerApt && output.ExitCode == 200 {
		return "exit status 200", true
	}
	return "", false
}

// parseMadison reads "serverchief |  0.9.1 | file:/repo ./ Packages".
func parseMadison(stdout []byte) []string {
	var versions []string
	for _, line := range strings.Split(string(stdout), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 3 || strings.TrimSpace(fields[0]) != PackageName {
			continue
		}
		if version := strings.TrimSpace(fields[1]); !slices.Contains(versions, version) {
			versions = append(versions, version)
		}
	}
	return versions
}

// parseRPMList reads `list available` rows "serverchief.x86_64 0.9.1-1
// serverchief". yum wraps a row whose name is too long, so fields are
// joined across lines. Only rows from the Server Chief repository count,
// and the release and epoch are dropped.
func parseRPMList(stdout []byte) []string {
	var versions []string
	var row []string
	for _, line := range strings.Split(string(stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasSuffix(strings.ToLower(line), "packages") {
			row = nil
			continue
		}
		row = append(row, fields...)
		if len(row) < 3 {
			continue
		}
		name, release, repository := row[0], row[1], row[2]
		row = nil
		if !strings.HasPrefix(name, PackageName+".") || repository != RepositoryID {
			continue
		}
		if _, withoutEpoch, found := strings.Cut(release, ":"); found {
			release = withoutEpoch
		}
		version, _, _ := strings.Cut(release, "-")
		if !slices.Contains(versions, version) {
			versions = append(versions, version)
		}
	}
	return versions
}

func lastLines(output Output, count int) string {
	text := strings.TrimSpace(string(output.Stderr))
	if text == "" {
		text = strings.TrimSpace(string(output.Stdout))
	}
	lines := strings.Split(text, "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	for index := range lines {
		lines[index] = strings.TrimSpace(lines[index])
	}
	return strings.Join(lines, " / ")
}
