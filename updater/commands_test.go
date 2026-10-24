package updater

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestValidVersion(t *testing.T) {
	for _, version := range []string{"0.0.0", "0.9.1", "1.2.3", "10.20.30", "999999999.0.1"} {
		if !ValidVersion(version) {
			t.Errorf("ValidVersion(%q) = false", version)
		}
	}
	for _, version := range []string{
		"", "dev", "1", "1.2", "1.2.3.4", "v1.2.3", "01.2.3", "1.02.3", "1.2.03", "1.2.3-rc.1", "1.2.3+build",
		"1.2.3 ", " 1.2.3", "1.2.3\n", "1.2.3;id", "1.2.3 curl", "$(id).1.1", "1.2.*", "1.2.x", "１.2.3", "1.2.٣",
		"1000000000.0.0", "-1.2.3", "1..3",
	} {
		if ValidVersion(version) {
			t.Errorf("ValidVersion(%q) = true", version)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	for _, test := range []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0}, {"1.2.3", "1.2.4", -1}, {"1.10.0", "1.9.9", 1}, {"2.0.0", "10.0.0", -1}, {"0.9.2", "0.9.1", 1},
	} {
		if got := CompareVersions(test.a, test.b); got != test.want {
			t.Errorf("CompareVersions(%s, %s) = %d", test.a, test.b, got)
		}
	}
}

var (
	aptList    = Commands{Manager: ManagerApt, Tool: "/usr/bin/apt-get", AptCache: "/usr/bin/apt-cache", Sources: AptListPath}
	aptSources = Commands{Manager: ManagerApt, Tool: "/usr/bin/apt-get", AptCache: "/usr/bin/apt-cache", Sources: AptSourcesPath}
	dnf        = Commands{Manager: ManagerDnf, Tool: "/usr/bin/dnf"}
	yum        = Commands{Manager: ManagerYum, Tool: "/usr/bin/yum"}
)

func joined(t *testing.T, argv []string, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(argv, " ")
}

func TestCommandArgv(t *testing.T) {
	for _, test := range []struct {
		name     string
		commands Commands
		refresh  string
		query    string
		upgrade  string
		rollback string
	}{
		{"apt list", aptList,
			"/usr/bin/apt-get update -qq -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/serverchief.list -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0",
			"/usr/bin/apt-cache -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/serverchief.list -o Dir::Etc::sourceparts=- madison serverchief",
			"/usr/bin/apt-get install -y --only-upgrade --allow-downgrades -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/serverchief.list -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 serverchief=1.3.0",
			"/usr/bin/apt-get install -y --only-upgrade --allow-downgrades -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/serverchief.list -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 serverchief=1.2.3",
		},
		{"apt deb822", aptSources,
			"/usr/bin/apt-get update -qq -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/serverchief.sources -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0",
			"/usr/bin/apt-cache -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/serverchief.sources -o Dir::Etc::sourceparts=- madison serverchief",
			"/usr/bin/apt-get install -y --only-upgrade --allow-downgrades -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/serverchief.sources -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 serverchief=1.3.0",
			"/usr/bin/apt-get install -y --only-upgrade --allow-downgrades -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/serverchief.sources -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 serverchief=1.2.3",
		},
		{"dnf", dnf,
			"/usr/bin/dnf -q --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True --refresh makecache",
			"/usr/bin/dnf -q --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True --showduplicates list available serverchief",
			"/usr/bin/dnf --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True install -y serverchief-1.3.0",
			"/usr/bin/dnf --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True downgrade -y serverchief-1.2.3",
		},
		{"yum", yum,
			"/usr/bin/yum -q --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True clean expire-cache",
			"/usr/bin/yum -q --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True --showduplicates list available serverchief",
			"/usr/bin/yum --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True install -y serverchief-1.3.0",
			"/usr/bin/yum --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True downgrade -y serverchief-1.2.3",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			argv, err := test.commands.Refresh()
			if got := joined(t, argv, err); got != test.refresh {
				t.Errorf("refresh = %s", got)
			}
			argv, err = test.commands.Available()
			if got := joined(t, argv, err); got != test.query {
				t.Errorf("query = %s", got)
			}
			argv, err = test.commands.Install("1.3.0", "1.2.3")
			if got := joined(t, argv, err); got != test.upgrade {
				t.Errorf("upgrade = %s", got)
			}
			argv, err = test.commands.Install("1.2.3", "1.3.0")
			if got := joined(t, argv, err); got != test.rollback {
				t.Errorf("rollback = %s", got)
			}
		})
	}
}

func TestCommandsRejectAnythingButTheirOwnInputs(t *testing.T) {
	for _, version := range []string{"1.3.0 curl", "1.3.0;id", "*", "1.3", "serverchief=1.3.0", ""} {
		for _, commands := range []Commands{aptList, dnf, yum} {
			if argv, err := commands.Install(version, "1.2.3"); err == nil {
				t.Errorf("%s Install(%q) = %q", commands.Manager, version, argv)
			}
		}
	}
	for _, commands := range []Commands{
		{Manager: ManagerApt, Tool: "/usr/bin/apt-get", AptCache: "/usr/bin/apt-cache", Sources: "/etc/apt/sources.list"},
		{Manager: ManagerApt, Tool: "/usr/bin/apt-get", AptCache: "/usr/bin/apt-cache", Sources: "/tmp/serverchief.list"},
		{Manager: ManagerApt, Tool: "apt-get", AptCache: "/usr/bin/apt-cache", Sources: AptListPath},
		{Manager: ManagerApt, Tool: "/usr/bin/aptitude", AptCache: "/usr/bin/apt-cache", Sources: AptListPath},
		{Manager: ManagerDnf, Tool: "/usr/bin/yum"},
		{Manager: ManagerYum, Tool: "/bin/sh"},
		{Manager: "zypper", Tool: "/usr/bin/zypper"},
	} {
		if argv, err := commands.Install("1.3.0", "1.2.3"); err == nil {
			t.Errorf("%+v Install = %q", commands, argv)
		}
		if argv, err := commands.Refresh(); err == nil {
			t.Errorf("%+v Refresh = %q", commands, argv)
		}
	}
}

func TestParseMadison(t *testing.T) {
	output := " serverchief |      0.9.2 | file:/srv/repo ./ Packages\n serverchief |      0.9.1 | file:/srv/repo ./ Packages\nserverchief-extra | 1.0.0 | file:/srv/repo ./ Packages\n"
	if got := parseMadison([]byte(output)); !slices.Equal(got, []string{"0.9.2", "0.9.1"}) {
		t.Fatalf("versions = %q", got)
	}
}

func TestParseRPMList(t *testing.T) {
	output := `Available Packages
serverchief.x86_64                    0.9.0-1                     serverchief
serverchief.x86_64                    1:0.9.1-2.el9               serverchief
serverchief.x86_64                    0.9.2-1                     @serverchief
serverchief-tools.x86_64              0.9.3-1                     serverchief
serverchief.x86_64
                                      0.9.4-1                     serverchief
serverchief.x86_64                    0.9.5-1                     other
`
	if got := parseRPMList([]byte(output)); !slices.Equal(got, []string{"0.9.0", "0.9.1", "0.9.4"}) {
		t.Fatalf("versions = %q", got)
	}
}

func TestParseInstalled(t *testing.T) {
	for _, test := range []struct {
		output string
		want   Installed
	}{
		{"ii |0.9.1\n", Installed{Version: "0.9.1", Intact: true}},
		{"hi |0.9.1", Installed{Version: "0.9.1", Intact: true}},
		{"iU |0.9.2", Installed{Version: "0.9.2"}},
		{"iF |0.9.2", Installed{Version: "0.9.2"}},
		{"iiR|0.9.2", Installed{Version: "0.9.2"}},
		{"rc |0.9.1", Installed{}},
		{"un |", Installed{}},
		{"", Installed{}},
	} {
		if got := parseDpkgQuery([]byte(test.output)); got != test.want {
			t.Errorf("parseDpkgQuery(%q) = %+v", test.output, got)
		}
	}
	if got := parseRPMQuery([]byte("0.9.1\n")); got != (Installed{Version: "0.9.1", Intact: true}) {
		t.Errorf("rpm = %+v", got)
	}
	if got := parseRPMQuery([]byte("0.9.1\n0.9.2\n")); got != (Installed{Version: "0.9.2"}) {
		t.Errorf("rpm with two instances = %+v", got)
	}
}

// scriptedRunner answers argv lines with canned outputs.
type scriptedRunner struct {
	answers map[string]Output
	env     map[string][]string
	ran     []string
}

func (runner *scriptedRunner) run(_ context.Context, env []string, argv []string) (Output, error) {
	line := strings.Join(argv, " ")
	runner.ran = append(runner.ran, line)
	if runner.env == nil {
		runner.env = map[string][]string{}
	}
	runner.env[line] = env
	for prefix, output := range runner.answers {
		if strings.HasPrefix(line, prefix) {
			return output, nil
		}
	}
	return Output{ExitCode: 127, Stderr: []byte("not scripted: " + line)}, nil
}

func TestSystemManagerRecognisesFailuresAndLocks(t *testing.T) {
	runner := &scriptedRunner{answers: map[string]Output{
		"/usr/bin/apt-get update":  {Stderr: []byte("E: Could not get lock /var/lib/apt/lists/lock. It is held by process 42 (apt-get)\n"), ExitCode: 100},
		"/usr/bin/apt-cache":       {Stdout: []byte(" serverchief | 0.9.1 | file:/repo ./ Packages\n")},
		"/usr/bin/apt-get install": {Stderr: []byte("E: Version '9.9.9' for 'serverchief' was not found\n"), ExitCode: 100},
		"/usr/bin/dpkg-query":      {Stdout: []byte("ii |0.9.1\n")},
	}}
	manager := &SystemManager{Commands: aptList, DpkgQuery: "/usr/bin/dpkg-query", Run: runner.run}
	ctx := context.Background()
	if err := manager.Refresh(ctx); err == nil || !strings.Contains(err.Error(), "Could not get lock") {
		t.Fatalf("Refresh() = %v", err)
	} else if _, busy := err.(*BusyError); !busy {
		t.Fatalf("Refresh() = %T, want a BusyError", err)
	}
	if versions, err := manager.Available(ctx); err != nil || !slices.Equal(versions, []string{"0.9.1"}) {
		t.Fatalf("Available() = %q, %v", versions, err)
	}
	if err := manager.Install(ctx, "9.9.9", "0.9.1"); err == nil || !strings.Contains(err.Error(), "apt-get exited with status 100: E: Version '9.9.9'") {
		t.Fatalf("Install() = %v", err)
	}
	if installed, err := manager.Installed(ctx); err != nil || installed != (Installed{Version: "0.9.1", Intact: true}) {
		t.Fatalf("Installed() = %+v, %v", installed, err)
	}
	install := "/usr/bin/apt-get install -y --only-upgrade --allow-downgrades -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dir::Etc::sourcelist=/etc/apt/sources.list.d/serverchief.list -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 serverchief=9.9.9"
	if !slices.Contains(runner.env[install], "DEBIAN_FRONTEND=noninteractive") {
		t.Fatalf("install env = %q", runner.env[install])
	}

	rpm := &scriptedRunner{answers: map[string]Output{
		"/usr/bin/dnf -q --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True --showduplicates": {Stderr: []byte("Error: No matching Packages to list\n"), ExitCode: 1},
		"/usr/bin/dnf --disablerepo=*": {Stderr: []byte("Waiting for process with pid 3 to finish.\nmetadata already locked by 3\n"), ExitCode: 200},
		"/usr/bin/rpm -q":              {Stdout: []byte("package serverchief is not installed\n"), ExitCode: 1},
	}}
	manager = &SystemManager{Commands: dnf, RPM: "/usr/bin/rpm", Run: rpm.run}
	if versions, err := manager.Available(ctx); err != nil || len(versions) != 0 {
		t.Fatalf("Available() = %q, %v", versions, err)
	}
	if err := manager.Install(ctx, "0.9.1", ""); err == nil || !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("Install() = %v", err)
	}
	if installed, err := manager.Installed(ctx); err != nil || installed != (Installed{}) {
		t.Fatalf("Installed() = %+v, %v", installed, err)
	}
}

func TestStoreRefusesUnsafeFiles(t *testing.T) {
	store := Store{Directory: t.TempDir(), Owner: -1}
	request := Request{CommandID: "01jb9a2c3d4e5f6g7h8j9k0m1n", PreviousVersion: "0.9.0", TargetVersion: "0.9.1"}
	if err := store.WriteRequest(request); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Directory, RequestFile)
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode())
	}
	if got, err := store.ReadRequest(); err != nil || *got != request {
		t.Fatalf("ReadRequest() = %+v, %v", got, err)
	}
	if err := store.WriteRequest(Request{CommandID: "c1", PreviousVersion: "0.9.0", TargetVersion: "0.9.1; reboot"}); err == nil {
		t.Fatal("wrote an invalid request")
	}

	for name, content := range map[string]string{
		"unknown field": `{"command_id":"c1","previous_version":"0.9.0","target_version":"0.9.1","package":"curl"}`,
		"bad version":   `{"command_id":"c1","previous_version":"0.9.0","target_version":"latest"}`,
		"bad id":        `{"command_id":"../../etc","previous_version":"0.9.0","target_version":"0.9.1"}`,
		"trailing data": `{"command_id":"c1","previous_version":"0.9.0","target_version":"0.9.1"} {}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := store.ReadRequest(); err == nil {
			t.Errorf("%s: ReadRequest() = %+v", name, got)
		}
	}
	if err := os.WriteFile(path, []byte(`{"command_id":"c1","previous_version":"0.9.0","target_version":"0.9.1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadRequest(); err == nil {
		t.Error("read a world-readable request")
	}
	_ = os.Remove(path)
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"command_id":"c1","previous_version":"0.9.0","target_version":"0.9.1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadRequest(); err == nil {
		t.Error("followed a symlinked request")
	}
	owned := Store{Directory: store.Directory, Owner: os.Getuid() + 1}
	_ = os.Remove(path)
	if err := store.WriteRequest(request); err != nil {
		t.Fatal(err)
	}
	if _, err := owned.ReadRequest(); err == nil {
		t.Error("read a request owned by another user")
	}
}

func TestResultIsRemovedOnlyForItsCommand(t *testing.T) {
	store := Store{Directory: t.TempDir(), Owner: -1}
	if err := store.writeResult(Result{CommandID: "c1", Code: CodeUpdated}); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveResult("c2"); err != nil {
		t.Fatal(err)
	}
	if result, _ := store.ReadResult(); result == nil {
		t.Fatal("removed another command's result")
	}
	if err := store.RemoveResult("c1"); err != nil {
		t.Fatal(err)
	}
	if result, _ := store.ReadResult(); result != nil {
		t.Fatal("result kept")
	}
}
