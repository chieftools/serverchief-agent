package collector

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"go.chief.tools/serverchief-agent/pkglock"
	"go.chief.tools/serverchief-agent/updater"
)

// fakeTracker returns a lock tracker whose only lock is held while *held.
func fakeTracker(held *bool, now *time.Time) *pkglock.Tracker {
	return &pkglock.Tracker{
		Inspector: &pkglock.Inspector{
			Fcntl: []string{"/var/lib/dpkg/lock-frontend"}, PIDFiles: []string{}, Globs: []string{},
			Probe: func(string) (int, bool, error) { return 4242, *held, nil },
		},
		Patience: 2 * time.Hour,
		Now:      func() time.Time { return *now },
	}
}

func TestUpdatesSkipsWhileLocked(t *testing.T) {
	host, ran := fakeHost(t, "ubuntu-16.04", map[string]string{
		"var/lib/dpkg/status": "@var/lib/dpkg/status",
		"usr/bin/apt-get":     "",
		"var/lib/apt/lists/archive.ubuntu.com_ubuntu_dists_xenial_InRelease": "",
	}, map[string]fakeCommand{"apt-get -s -q -o Debug::NoLocking=1 dist-upgrade": {file: "apt-get-dist-upgrade.txt"}})
	held, now := true, time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	host.PackageLocks = fakeTracker(&held, &now)

	_, err := NewUpdates(host).Collect(context.Background())
	var busy *BusyError
	if !errors.As(err, &busy) || !errors.Is(err, ErrPackageManagerBusy) || busy.Lock != "/var/lib/dpkg/lock-frontend (fcntl, pid 4242)" {
		t.Fatalf("locked collection = %v", err)
	}
	if len(*ran) != 0 {
		t.Fatalf("ran %v while locked", *ran)
	}

	// A lock held past the patience (a hung dpkg) no longer stops the
	// simulation, which runs next to apt with Debug::NoLocking.
	now = now.Add(2*time.Hour + time.Minute)
	if _, err := NewUpdates(host).Collect(context.Background()); err != nil {
		t.Fatalf("collection after the patience = %v", err)
	}
	held = false
	if _, err := NewUpdates(host).Collect(context.Background()); err != nil {
		t.Fatalf("unlocked collection = %v", err)
	}
}

func TestRPMUpdatesGiveUpOnLocks(t *testing.T) {
	yumFiles := map[string]string{"usr/bin/yum": "", "var/lib/rpm/Packages": ""}
	yumLocked := CommandResult{Stderr: []byte("Existing lock /var/run/yum.pid: another copy is running as pid 22.\n"), ExitCode: 1}
	dnfLocked := CommandResult{Stderr: []byte("metadata already locked by 34\n  The application with PID 34 is: sleep\n"), ExitCode: 200}
	for _, test := range []struct {
		name    string
		files   map[string]string
		results map[string]CommandResult
		want    string
	}{
		{"yum check-update", yumFiles, map[string]CommandResult{"check-update": yumLocked}, "Existing lock /var/run/yum.pid: another copy is running as pid 22."},
		{"dnf check-update", map[string]string{"usr/bin/dnf": "", "var/lib/rpm/rpmdb.sqlite": ""}, map[string]CommandResult{"check-update": dnfLocked}, "metadata already locked by 34"},
		{"yum updateinfo", yumFiles, map[string]CommandResult{"check-update": {ExitCode: 0}, "rpm": {}, "updateinfo": yumLocked}, "Existing lock"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for name := range test.files {
				fsys[name] = &fstest.MapFile{}
			}
			var deadlines []bool
			host := &Host{FS: fsys, Command: func(ctx context.Context, name string, args ...string) (CommandResult, error) {
				_, hasDeadline := ctx.Deadline()
				deadlines = append(deadlines, hasDeadline)
				if name == "rpm" {
					return test.results["rpm"], nil
				}
				if !slices.Contains(args, "--setopt=exit_on_lock=True") {
					t.Errorf("%s %v may wait on a lock", name, args)
				}
				for key, result := range test.results {
					if slices.Contains(args, key) {
						return result, nil
					}
				}
				return CommandResult{}, os.ErrNotExist
			}}
			_, err := NewUpdates(host).Collect(context.Background())
			var busy *BusyError
			if !errors.As(err, &busy) || !strings.HasPrefix(busy.Lock, test.want) {
				t.Fatalf("err = %v", err)
			}
			if !deadlines[0] {
				t.Fatal("yum/dnf ran without a timeout")
			}
		})
	}
}

// refreshHost is a host with the given distribution files and binaries.
func refreshHost(osRelease string, binaries ...string) (*Host, *[][]string) {
	fsys := fstest.MapFS{"etc/os-release": &fstest.MapFile{Data: []byte(osRelease)}}
	for _, binary := range binaries {
		fsys[strings.TrimPrefix(binary, "/")] = &fstest.MapFile{}
		if path.Base(binary) == "apt-get" {
			fsys["var/lib/dpkg/status"] = &fstest.MapFile{}
		}
	}
	var ran [][]string
	host := &Host{FS: fsys, Now: func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) }}
	host.Command = func(_ context.Context, name string, args ...string) (CommandResult, error) {
		ran = append(ran, append([]string{name}, args...))
		return CommandResult{}, nil
	}
	return host, &ran
}

const (
	osUbuntu1604 = "ID=ubuntu\nVERSION_ID=\"16.04\"\n"
	osCentOS7    = "ID=\"centos\"\nID_LIKE=\"rhel fedora\"\nVERSION_ID=\"7\"\n"
	osAmazon2    = "ID=\"amzn\"\nID_LIKE=\"centos rhel fedora\"\nVERSION_ID=\"2\"\n"
	osAlma9      = "ID=\"almalinux\"\nID_LIKE=\"rhel centos fedora\"\nVERSION_ID=\"9.8\"\n"
	osFedora     = "ID=fedora\nVERSION_ID=42\n"
)

func TestRefreshCommand(t *testing.T) {
	for _, test := range []struct {
		name      string
		osRelease string
		binaries  []string
		want      string
	}{
		{"ubuntu", osUbuntu1604, []string{"/usr/bin/apt-get", "/usr/bin/nice", "/usr/bin/ionice"}, "/usr/bin/nice -n 19 /usr/bin/ionice -c3 -t /usr/bin/apt-get update -qq"},
		{"no ionice", osUbuntu1604, []string{"/usr/bin/apt-get", "/bin/nice"}, "/bin/nice -n 19 /usr/bin/apt-get update -qq"},
		{"bare", osUbuntu1604, []string{"/usr/bin/apt-get"}, "/usr/bin/apt-get update -qq"},
		{"centos 7", osCentOS7, []string{"/usr/bin/yum", "/usr/bin/nice", "/usr/bin/ionice"}, "/usr/bin/nice -n 19 /usr/bin/ionice -c3 -t /usr/bin/yum makecache fast -q --setopt=exit_on_lock=True"},
		{"amazon linux 2", osAmazon2, []string{"/usr/bin/yum"}, "/usr/bin/yum makecache fast -q --setopt=exit_on_lock=True"},
		{"other yum", osFedora, []string{"/usr/bin/yum"}, "/usr/bin/yum makecache -q --setopt=exit_on_lock=True"},
		{"alma 9", osAlma9, []string{"/usr/bin/dnf", "/usr/bin/yum", "/usr/bin/nice", "/usr/bin/ionice"}, "/usr/bin/nice -n 19 /usr/bin/ionice -c3 -t /usr/bin/dnf makecache -q --setopt=exit_on_lock=True"},
	} {
		t.Run(test.name, func(t *testing.T) {
			host, _ := refreshHost(test.osRelease, test.binaries...)
			argv, err := NewPackageSystem(host).RefreshCommand()
			if err != nil || strings.Join(argv, " ") != test.want {
				t.Fatalf("argv = %q, %v", argv, err)
			}
		})
	}
	host, _ := refreshHost(osUbuntu1604)
	if _, err := NewPackageSystem(host).RefreshCommand(); err == nil {
		t.Fatal("refresh command without a package manager")
	}
}

func TestRefreshMetadata(t *testing.T) {
	// RefreshMetadata runs exactly one command and writes no file: the
	// daemon records a success in its own state directory.
	run := func(t *testing.T, osRelease string, result CommandResult, binaries ...string) (error, [][]string) {
		t.Helper()
		host, ran := refreshHost(osRelease, binaries...)
		before := len(host.FS.(fstest.MapFS))
		host.Command = func(_ context.Context, name string, args ...string) (CommandResult, error) {
			*ran = append(*ran, append([]string{name}, args...))
			return result, nil
		}
		err := NewPackageSystem(host).RefreshMetadata(context.Background())
		if len(*ran) != 1 || len(host.FS.(fstest.MapFS)) != before {
			t.Fatalf("ran %q", *ran)
		}
		return err, *ran
	}

	err, ran := run(t, osUbuntu1604, CommandResult{}, "/usr/bin/apt-get")
	if err != nil || strings.Join(ran[0], " ") != "/usr/bin/apt-get update -qq" {
		t.Fatalf("apt success: %v %q", err, ran)
	}

	eol := CommandResult{ExitCode: 100, Stderr: []byte("W: The repository 'http://archive.ubuntu.com/ubuntu xenial Release' does not have a Release file.\nE: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/xenial/main/binary-amd64/Packages  404  Not Found\nE: Some index files failed to download.\n")}
	if err, _ = run(t, osUbuntu1604, eol, "/usr/bin/apt-get"); err == nil || err.Error() != "apt-get update exited with status 100: E: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/xenial/main/binary-amd64/Packages  404  Not Found" {
		t.Fatalf("apt failure: %v", err)
	}
	oldApt := CommandResult{Stderr: []byte("W: Failed to fetch http://archive.ubuntu.com/ubuntu/dists/trusty/InRelease  Could not resolve 'archive.ubuntu.com'\n")}
	if err, _ = run(t, osUbuntu1604, oldApt, "/usr/bin/apt-get"); err == nil || !strings.Contains(err.Error(), "Could not resolve") {
		t.Fatalf("old apt failed fetch: %v", err)
	}
	locked := CommandResult{ExitCode: 100, Stderr: []byte("E: Could not get lock /var/lib/apt/lists/lock - open (11: Resource temporarily unavailable)\n")}
	if err, _ = run(t, osUbuntu1604, locked, "/usr/bin/apt-get"); !errors.Is(err, ErrPackageManagerBusy) {
		t.Fatalf("apt lock race: %v", err)
	}

	yumLocked := CommandResult{ExitCode: 1, Stderr: []byte("Existing lock /var/run/yum.pid: another copy is running as pid 22.\n")}
	if err, _ = run(t, osCentOS7, yumLocked, "/usr/bin/yum"); !errors.Is(err, ErrPackageManagerBusy) {
		t.Fatalf("yum lock race: %v", err)
	}
	dnfFailed := CommandResult{ExitCode: 1, Stderr: []byte("Errors during downloading metadata for repository 'baseos':\nError: Failed to download metadata for repo 'baseos': Cannot download repomd.xml\n")}
	if err, _ = run(t, osAlma9, dnfFailed, "/usr/bin/dnf"); err == nil || err.Error() != "dnf makecache exited with status 1: Error: Failed to download metadata for repo 'baseos': Cannot download repomd.xml" {
		t.Fatalf("dnf failure: %v", err)
	}
	if err, _ = run(t, osAlma9, CommandResult{}, "/usr/bin/dnf"); err != nil {
		t.Fatalf("dnf success: %v", err)
	}

	// A timeout reports as such.
	host, _ := refreshHost(osUbuntu1604, "/usr/bin/apt-get")
	host.Command = func(ctx context.Context, _ string, _ ...string) (CommandResult, error) {
		<-ctx.Done()
		return CommandResult{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := NewPackageSystem(host).RefreshMetadata(ctx); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout: %v", err)
	}
}

// TestCacheUpdatedAtIncludesOwnRefresh: a refresh that found nothing new
// upstream leaves the cache files alone, so the agent's recorded refresh
// moves cache_updated_at. It never invents a cache, and an unchanged
// record keeps the value (and the hash) stable.
func TestCacheUpdatedAtIncludesOwnRefresh(t *testing.T) {
	cache := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	refreshed := time.Time{}
	recorded := func() time.Time { return refreshed }

	apt := &Host{FS: fstest.MapFS{
		"var/lib/dpkg/status": {}, "usr/bin/apt-get": {},
		"var/lib/apt/lists/deb.debian.org_debian_dists_bookworm_InRelease": {ModTime: cache},
	}, MetadataRefreshedAt: recorded}
	dnf := &Host{FS: fstest.MapFS{
		"usr/bin/dnf": {},
		"var/cache/dnf/baseos-09b2e93483836b7c/repodata/repomd.xml": {ModTime: cache},
	}, MetadataRefreshedAt: recorded}
	for name, host := range map[string]*Host{"apt": apt, "dnf": dnf} {
		system := NewPackageSystem(host)
		refreshed = time.Time{}
		if got := system.CacheUpdatedAt(); got == nil || !got.Equal(cache) {
			t.Fatalf("%s never refreshed: %v", name, got)
		}
		token := NewUpdates(host).ChangeToken()
		refreshed = cache.Add(-time.Hour) // older than the cache: ignored
		if got := system.CacheUpdatedAt(); !got.Equal(cache) || NewUpdates(host).ChangeToken() != token {
			t.Fatalf("%s older refresh: %v", name, got)
		}
		refreshed = time.Date(2026, 10, 24, 12, 0, 0, 500, time.UTC)
		if got := system.CacheUpdatedAt(); got == nil || got.Format(time.RFC3339Nano) != "2026-10-24T12:00:00Z" {
			t.Fatalf("%s after a refresh: %v", name, got)
		}
		if NewUpdates(host).ChangeToken() == token {
			t.Fatalf("%s change token ignores the refresh", name)
		}
	}

	// Without indexes or a cache there is nothing to report.
	empty := &Host{FS: fstest.MapFS{"var/lib/dpkg/status": {}, "usr/bin/apt-get": {}, "var/lib/apt/lists/lock": {}}, MetadataRefreshedAt: recorded}
	if got := NewPackageSystem(empty).CacheUpdatedAt(); got != nil {
		t.Fatalf("apt without indexes: %v", got)
	}
	if got := (&Host{FS: fstest.MapFS{"var/cache/dnf/x": {}}, MetadataRefreshedAt: recorded}).rpmCacheUpdatedAt(ManagerDnf); got != nil {
		t.Fatalf("dnf without metadata: %v", got)
	}
}

func TestPackageSystemCacheAndLocks(t *testing.T) {
	host, _ := refreshHost(osUbuntu1604, "/usr/bin/apt-get")
	fsys := host.FS.(fstest.MapFS)
	stamp := time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC)
	fsys["var/lib/apt/lists/deb.debian.org_debian_dists_bookworm_InRelease"] = &fstest.MapFile{ModTime: stamp}
	system := NewPackageSystem(host)
	if got := system.CacheUpdatedAt(); got == nil || !got.Equal(stamp) {
		t.Fatalf("cache updated at = %v", got)
	}
	if _, _, held := system.BusyLock(); held {
		t.Fatal("host without a lock tracker reported a lock")
	}
	held, now := true, stamp
	host.PackageLocks = fakeTracker(&held, &now)
	system.BusyLock()
	now = now.Add(time.Minute)
	if lock, heldFor, busy := system.BusyLock(); !busy || heldFor != time.Minute || lock != "/var/lib/dpkg/lock-frontend (fcntl, pid 4242)" {
		t.Fatalf("lock = %q %s %v", lock, heldFor, busy)
	}
}

// packageManagerTools are the programs whose argv must be allow-listed.
var packageManagerTools = []string{"apt", "apt-get", "apt-cache", "aptitude", "dpkg", "dpkg-query", "yum", "dnf", "dnf5", "rpm", "needs-restarting", "nice", "ionice", "microdnf", "zypper"}

// allowedCollectionCommands are the package-manager queries collectors
// run, with program paths reduced to their base name: a simulation and
// cache-only queries.
var allowedCollectionCommands = []string{
	"apt-get -s -q -o Debug::NoLocking=1 dist-upgrade",
	"rpm -qa --queryformat " + rpmQueryFormat,
	"yum -C -q --setopt=exit_on_lock=True check-update",
	"yum -C -q --setopt=exit_on_lock=True updateinfo list security",
	"dnf -C -q --setopt=exit_on_lock=True check-update",
	"dnf -C -q --setopt=exit_on_lock=True updateinfo list --security",
	"needs-restarting -r",
}

// allowedRefreshCommands are the metadata refreshes, each run as is or
// behind one of allowedRefreshPrefixes.
var (
	allowedRefreshCommands = []string{
		"apt-get update -qq",
		"yum makecache fast -q --setopt=exit_on_lock=True",
		"yum makecache -q --setopt=exit_on_lock=True",
		"dnf makecache -q --setopt=exit_on_lock=True",
	}
	allowedRefreshPrefixes = []string{"", "nice -n 19 ", "ionice -c3 -t ", "nice -n 19 ionice -c3 -t "}
)

// allowedPackageCommands is every package-manager argv the agent can run.
// None installs, upgrades or removes anything.
func allowedPackageCommands() []string {
	allowed := slices.Clone(allowedCollectionCommands)
	for _, command := range allowedRefreshCommands {
		for _, prefix := range allowedRefreshPrefixes {
			allowed = append(allowed, prefix+command)
		}
	}
	return allowed
}

// TestPackageCommandAllowList runs every collector and the metadata
// refresh on apt, yum and dnf hosts and checks that each package-manager
// command is allow-listed.
func TestPackageCommandAllowList(t *testing.T) {
	type scenario struct {
		distro    string
		osRelease string
		files     map[string]string
		commands  map[string]fakeCommand
	}
	scenarios := []scenario{
		{"ubuntu-16.04", osUbuntu1604, map[string]string{
			"var/lib/dpkg/status": "@var/lib/dpkg/status", "usr/bin/apt-get": "",
			"var/lib/apt/lists/archive.ubuntu.com_ubuntu_dists_xenial_InRelease": "",
		}, map[string]fakeCommand{"apt-get -s": {file: "apt-get-dist-upgrade.txt"}}},
		{"centos-7", osCentOS7, map[string]string{
			"usr/bin/yum": "", "usr/bin/needs-restarting": "", "var/lib/rpm/Packages": "",
			"var/cache/yum/x86_64/7/base/repomd.xml": "",
		}, map[string]fakeCommand{
			"yum -C -q --setopt=exit_on_lock=True check-update":             {file: "check-update.txt", exit: 100},
			"yum -C -q --setopt=exit_on_lock=True updateinfo list security": {file: "updateinfo-security.txt"},
			"rpm -qa":                      {file: "rpm-qa.txt"},
			"/usr/bin/needs-restarting -r": {file: "needs-restarting.txt"},
		}},
		{"almalinux-9", osAlma9, map[string]string{
			"usr/bin/dnf": "", "usr/bin/yum": "", "usr/bin/needs-restarting": "", "var/lib/rpm/rpmdb.sqlite": "",
		}, map[string]fakeCommand{
			"dnf -C -q --setopt=exit_on_lock=True check-update":               {file: "check-update.txt", exit: 100},
			"dnf -C -q --setopt=exit_on_lock=True updateinfo list --security": {file: "updateinfo-security.txt"},
			"rpm -qa":                      {file: "rpm-qa.txt"},
			"/usr/bin/needs-restarting -r": {file: "needs-restarting-required.txt", exit: 1},
		}},
	}
	seen := map[string]bool{}
	for _, test := range scenarios {
		files := map[string]string{"etc/os-release": test.osRelease, "usr/bin/nice": "", "usr/bin/ionice": ""}
		for name, content := range test.files {
			files[name] = content
		}
		host, ran := fakeHost(t, test.distro, files, test.commands)
		for _, name := range Default(host).Names() {
			instance, _ := Default(host).Get(name)
			_, _ = instance.Collect(context.Background())
		}
		_ = NewPackageSystem(host).RefreshMetadata(context.Background())
		without := *host
		without.FS = withoutPriorityTools(host.FS.(fstest.MapFS))
		_ = NewPackageSystem(&without).RefreshMetadata(context.Background())

		allowed := allowedPackageCommands()
		for _, line := range *ran {
			argv := strings.Split(line, " ")
			if !slices.Contains(packageManagerTools, path.Base(argv[0])) {
				continue
			}
			normalized := normalizeCommand(line)
			seen[normalized] = true
			if !slices.Contains(allowed, normalized) {
				t.Errorf("%s: package-manager command %q is not allow-listed", test.distro, normalized)
			}
		}
	}
	for _, command := range append(slices.Clone(allowedCollectionCommands),
		"nice -n 19 ionice -c3 -t apt-get update -qq", "apt-get update -qq",
		"nice -n 19 ionice -c3 -t yum makecache fast -q --setopt=exit_on_lock=True",
		"nice -n 19 ionice -c3 -t dnf makecache -q --setopt=exit_on_lock=True", "dnf makecache -q --setopt=exit_on_lock=True") {
		if !seen[command] {
			t.Errorf("allow-listed %q never ran; keep the list exact", command)
		}
	}
	for _, command := range allowedPackageCommands() {
		for _, word := range strings.Fields(command) {
			if forbiddenPackageVerbs[word] {
				t.Errorf("allow-list entry %q contains mutating verb %q", command, word)
			}
		}
	}
}

func withoutPriorityTools(fsys fstest.MapFS) fstest.MapFS {
	copied := fstest.MapFS{}
	for name, file := range fsys {
		if name != "usr/bin/nice" && name != "usr/bin/ionice" {
			copied[name] = file
		}
	}
	return copied
}

// normalizeCommand reduces every program path in a command line to its
// base name.
func normalizeCommand(line string) string {
	fields := strings.Split(line, " ")
	for index, field := range fields {
		if strings.HasPrefix(field, "/") {
			fields[index] = path.Base(field)
		}
	}
	return strings.Join(fields, " ")
}

// forbiddenPackageVerbs install, upgrade or remove packages.
var forbiddenPackageVerbs = map[string]bool{
	"install": true, "reinstall": true, "upgrade": true, "full-upgrade": true, "update-minimal": true,
	"remove": true, "erase": true, "autoremove": true, "purge": true, "downgrade": true,
	"distro-sync": true, "distribution-synchronization": true, "localinstall": true, "groupinstall": true,
	"-i": true, "-U": true, "-e": true, "-F": true, "--install": true, "--upgrade": true, "--erase": true,
	"--configure": true, "--unpack": true, "-y": true, "--assumeyes": true,
}

// allowedUpdateCommands are the agent update's package-manager argvs
// (updater.Commands and the queries around them), the only ones that
// install anything. <sources> is the Server Chief apt list or deb822
// file, <rpm> is dnf or yum, and <v> is a stable X.Y.Z version. Each one
// names the serverchief package and restricts the package manager to the
// Server Chief repository.
var allowedUpdateCommands = []string{
	"apt-get update -qq -o Dir::Etc::sourcelist=<sources> -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0",
	"apt-cache -o Dir::Etc::sourcelist=<sources> -o Dir::Etc::sourceparts=- madison serverchief",
	"apt-get install -y --only-upgrade --allow-downgrades -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dir::Etc::sourcelist=<sources> -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 serverchief=<v>",
	"dpkg-query -W -f=${db:Status-Abbrev}|${Version}\n serverchief",
	"dpkg-query -W -f=${Version} apt",
	"rpm -q --qf %{VERSION}\n serverchief",
	"dnf -q --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True --refresh makecache",
	"yum -q --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True clean expire-cache",
	"<rpm> -q --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True --showduplicates list available serverchief",
	"<rpm> --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True install -y serverchief-<v>",
	"<rpm> --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True downgrade -y serverchief-<v>",
}

// updateVerbs are the only mutating words an update argv may contain.
var updateVerbs = map[string]bool{"install": true, "downgrade": true, "-y": true}

// matchesUpdateCommand reports whether a normalized command line is an
// allow-listed update argv. Placeholders match only their exact values.
func matchesUpdateCommand(line string) bool {
	version := `(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})`
	for _, template := range allowedUpdateCommands {
		pattern := regexp.QuoteMeta(template)
		pattern = strings.ReplaceAll(pattern, "<sources>", "("+regexp.QuoteMeta(updater.AptListPath)+"|"+regexp.QuoteMeta(updater.AptSourcesPath)+")")
		pattern = strings.ReplaceAll(pattern, "<rpm>", "(dnf|yum)")
		pattern = strings.ReplaceAll(pattern, "<v>", version)
		if regexp.MustCompile(`\A` + pattern + `\z`).MatchString(line) {
			return true
		}
	}
	return false
}

// TestUpdateCommandAllowList builds every argv the agent update runs, on
// apt (list and deb822), dnf and yum hosts, and checks each against
// allowedUpdateCommands. Collectors and the metadata refresh stay on
// allowedPackageCommands, which cannot install anything.
func TestUpdateCommandAllowList(t *testing.T) {
	var ran []string
	record := func(_ context.Context, _ []string, argv []string) (updater.Output, error) {
		ran = append(ran, strings.Join(argv, " "))
		return updater.Output{ExitCode: 1}, nil
	}
	ctx := context.Background()
	managers := []*updater.SystemManager{
		{Commands: updater.Commands{Manager: updater.ManagerApt, Tool: "/usr/bin/apt-get", AptCache: "/usr/bin/apt-cache", Sources: updater.AptListPath}, DpkgQuery: "/usr/bin/dpkg-query"},
		{Commands: updater.Commands{Manager: updater.ManagerApt, Tool: "/usr/bin/apt-get", AptCache: "/usr/bin/apt-cache", Sources: updater.AptSourcesPath}, DpkgQuery: "/usr/bin/dpkg-query"},
		{Commands: updater.Commands{Manager: updater.ManagerDnf, Tool: "/usr/bin/dnf"}, RPM: "/usr/bin/rpm"},
		{Commands: updater.Commands{Manager: updater.ManagerYum, Tool: "/usr/bin/yum"}, RPM: "/bin/rpm"},
	}
	for _, manager := range managers {
		manager.Run = record
		_, _ = manager.Installed(ctx)
		_ = manager.Refresh(ctx)
		_, _ = manager.Available(ctx)
		_ = manager.Install(ctx, "1.3.0", "1.2.3") // upgrade
		_ = manager.Install(ctx, "1.2.3", "1.3.0") // rollback
		_ = manager.Install(ctx, "1.3.0", "")      // repair a missing package
		_ = manager.Install(ctx, "1.3.0; reboot", "1.2.3")
		_ = manager.Install(ctx, "1.3.0 nginx", "1.2.3")
	}
	// The capability probe's apt version query.
	root := t.TempDir()
	for _, name := range []string{"var/lib/dpkg/status", "usr/bin/dpkg-query", "usr/bin/apt-get", "usr/bin/apt-cache", "etc/apt/sources.list.d/serverchief.list"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	probeRun := func(ctx context.Context, env []string, argv []string) (updater.Output, error) {
		_, _ = record(ctx, env, argv)
		if slices.Contains(argv, "serverchief") {
			return updater.Output{Stdout: []byte("ii |1.2.3")}, nil
		}
		return updater.Output{Stdout: []byte("2.6.1")}, nil
	}
	_ = updater.Probe{Version: "1.2.3", GOOS: "linux", Tools: updater.Tools{Root: root}, Run: probeRun}.Detect(ctx)

	seen := map[string]bool{}
	for _, line := range ran {
		normalized := normalizeCommand(line)
		seen[normalized] = true
		if !matchesUpdateCommand(normalized) {
			t.Errorf("update command %q is not allow-listed", normalized)
		}
		if slices.Contains(allowedPackageCommands(), normalized) {
			t.Errorf("update command %q is on the collector allow-list", normalized)
		}
	}
	if len(seen) != 19 {
		t.Fatalf("%d distinct update commands ran, want 19: %q", len(seen), slices.Sorted(maps.Keys(seen)))
	}
	for _, template := range allowedUpdateCommands {
		matched := false
		for command := range seen {
			pattern := strings.NewReplacer("<sources>", updater.AptListPath, "<rpm>", "dnf", "<v>", "1.3.0").Replace(template)
			alternate := strings.NewReplacer("<sources>", updater.AptSourcesPath, "<rpm>", "yum", "<v>", "1.2.3").Replace(template)
			matched = matched || command == pattern || command == alternate
		}
		if !matched {
			t.Errorf("allow-listed %q never ran; keep the list exact", template)
		}
		fields := strings.Fields(template)
		mutating := false
		for _, word := range fields {
			if forbiddenPackageVerbs[word] {
				mutating = true
				if !updateVerbs[word] {
					t.Errorf("update entry %q contains %q", template, word)
				}
			}
		}
		if mutating {
			last := fields[len(fields)-1]
			restricted := strings.Contains(template, "--disablerepo=* --enablerepo=serverchief ") ||
				strings.Contains(template, "-o Dir::Etc::sourcelist=<sources> -o Dir::Etc::sourceparts=-")
			if (last != "serverchief=<v>" && last != "serverchief-<v>") || !restricted {
				t.Errorf("update entry %q does not install exactly serverchief from the Server Chief repository", template)
			}
		}
	}
	install := "apt-get install -y --only-upgrade --allow-downgrades -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/serverchief.list -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 serverchief=1.3.0"
	for _, rejected := range []string{
		"apt-get install -y serverchief=1.3.0",
		install + " nginx",
		strings.Replace(install, "serverchief=1.3.0", "serverchief=1.3.0;id", 1),
		strings.Replace(install, "serverchief=1.3.0", "serverchief=1.3", 1),
		strings.Replace(install, "serverchief=1.3.0", "nginx=1.3.0", 1),
		strings.Replace(install, "serverchief.list", "../../sources.list", 1),
		strings.Replace(install, "/etc/apt/sources.list.d/serverchief.list", "/etc/apt/sources.list", 1),
		strings.Replace(install, " -o Dir::Etc::sourceparts=-", "", 1),
		strings.Replace(install, "install", "dist-upgrade", 1),
		"dnf install -y serverchief-1.3.0",
		"dnf --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True install -y serverchief-1.3.0 nginx",
		"dnf --disablerepo=* --enablerepo=epel --setopt=exit_on_lock=True install -y serverchief-1.3.0",
		"yum --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True install -y serverchief-latest",
		"yum --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True upgrade -y serverchief-1.3.0",
		"zypper --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True install -y serverchief-1.3.0",
		"dnf --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True install -y serverchief-1.3.0\nid",
	} {
		if matchesUpdateCommand(rejected) {
			t.Errorf("%q matches the update allow-list", rejected)
		}
	}
}

// TestNoMutatingPackageVerbsInSource scans the agent's Go sources for a
// string literal that is a mutating package-manager verb, so a new code
// path cannot add one without this test noticing. ("update" is apt-get's
// metadata refresh and dist-upgrade only appears with -s.) initsys runs
// no package manager (its update-rc.d "remove" is a service command) and
// is not scanned. The agent update's argv builder, updater/commands.go,
// is the one file allowed to spell out install, downgrade and -y.
func TestNoMutatingPackageVerbsInSource(t *testing.T) {
	const updateCommands = "../updater/commands.go"
	allowedIn := map[string]map[string]bool{updateCommands: updateVerbs}
	used := map[string]bool{}
	var files []string
	for _, directory := range []string{".", "../agent", "../cmd", "../pkglock", "../updater"} {
		matches, err := filepath.Glob(filepath.Join(directory, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range matches {
			if !strings.HasSuffix(match, "_test.go") {
				files = append(files, match)
			}
		}
	}
	if len(files) < 20 {
		t.Fatalf("only %d source files found", len(files))
	}
	fileSet := token.NewFileSet()
	for _, name := range files {
		file, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			if forbiddenPackageVerbs[value] {
				if allowedIn[name][value] {
					used[value] = true
					return true
				}
				t.Errorf("%s: string literal %q is a mutating package-manager verb", fileSet.Position(literal.Pos()), value)
			}
			return true
		})
	}
	for verb := range updateVerbs {
		if !used[verb] {
			t.Errorf("%s no longer uses %q; drop it from the exemption", updateCommands, verb)
		}
	}
}
