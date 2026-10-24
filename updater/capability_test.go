package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.chief.tools/serverchief-agent/initsys"
)

// fakeRoot builds a host tree. Names ending in "*" are executables.
func fakeRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, "*") {
			name, mode = strings.TrimSuffix(name, "*"), 0o755
		}
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const serviceCgroup = "0::/system.slice/serverchief.service\n"

func debianHost() map[string]string {
	return map[string]string{
		"/var/lib/dpkg/status": "", "/usr/bin/dpkg-query*": "", "/usr/bin/apt-get*": "", "/usr/bin/apt-cache*": "",
		AptListPath: "deb [trusted=yes] file:/srv/repo ./\n", "/usr/bin/systemd-run*": "", "/proc/self/cgroup": serviceCgroup,
	}
}

func almaHost() map[string]string {
	return map[string]string{
		"/usr/bin/rpm*": "", "/usr/bin/dnf*": "", YumRepoPath: "[serverchief]\n", "/usr/bin/systemd-run*": "", "/proc/self/cgroup": serviceCgroup,
	}
}

func dpkgAnswers(serverchief, apt Output) map[string]Output {
	return map[string]Output{
		"/usr/bin/dpkg-query -W -f=${db:Status-Abbrev}|${Version}\n serverchief": serverchief,
		"/usr/bin/dpkg-query -W -f=${Version} apt":                               apt,
	}
}

var (
	installed091 = Output{Stdout: []byte("ii |0.9.1\n")}
	apt26        = Output{Stdout: []byte("2.6.1")}
	noPackage    = Output{Stderr: []byte("dpkg-query: no packages found matching serverchief\n"), ExitCode: 1}
)

func TestCapabilityMatrix(t *testing.T) {
	with := func(base map[string]string, changes map[string]string, removed ...string) map[string]string {
		files := map[string]string{}
		for name, content := range base {
			if !slices.Contains(removed, name) {
				files[name] = content
			}
		}
		for name, content := range changes {
			files[name] = content
		}
		return files
	}
	for _, test := range []struct {
		name    string
		version string
		goos    string
		init    initsys.Kind
		pid     int
		files   map[string]string
		answers map[string]Output
		want    Capability
	}{
		{"apt list under systemd", "0.9.1", "", initsys.Systemd, 0, debianHost(), dpkgAnswers(installed091, apt26), Capability{Available: true}},
		{"apt deb822", "0.9.1", "", initsys.Systemd, 0, with(debianHost(), map[string]string{AptSourcesPath: "Types: deb\n"}, AptListPath), dpkgAnswers(installed091, apt26), Capability{Available: true}},
		{"dev build", "dev", "", initsys.Systemd, 0, debianHost(), dpkgAnswers(installed091, apt26), Capability{Reason: "dev build"}},
		{"pre-release build", "0.9.1-rc.1", "", initsys.Systemd, 0, debianHost(), dpkgAnswers(installed091, apt26), Capability{Reason: "not a stable release build (0.9.1-rc.1)"}},
		{"not linux", "0.9.1", "darwin", initsys.Systemd, 0, debianHost(), dpkgAnswers(installed091, apt26), Capability{Reason: "unsupported platform darwin"}},
		{"tarball", "0.9.1", "", initsys.Systemd, 0, debianHost(), dpkgAnswers(noPackage, apt26), Capability{Reason: "installed from a tarball, not the serverchief package"}},
		{"other version installed", "0.9.1", "", initsys.Systemd, 0, debianHost(), dpkgAnswers(Output{Stdout: []byte("ii |0.9.2\n")}, apt26),
			Capability{Reason: "the installed serverchief package (0.9.2) is not the running version (0.9.1)"}},
		{"half-configured", "0.9.1", "", initsys.Systemd, 0, debianHost(), dpkgAnswers(Output{Stdout: []byte("iF |0.9.1\n")}, apt26), Capability{Reason: "the serverchief package is not fully installed"}},
		{"no repository", "0.9.1", "", initsys.Systemd, 0, with(debianHost(), nil, AptListPath), dpkgAnswers(installed091, apt26), Capability{Reason: "repository not configured"}},
		{"apt 1.0", "0.9.1", "", initsys.Systemd, 0, debianHost(), dpkgAnswers(installed091, Output{Stdout: []byte("1.0.1ubuntu2.24")}), Capability{Reason: "apt 1.1 or later is required"}},
		{"no systemd-run", "0.9.1", "", initsys.Systemd, 0, with(debianHost(), nil, "/usr/bin/systemd-run*"), dpkgAnswers(installed091, apt26), Capability{Reason: "systemd-run not found"}},
		{"run by hand on a systemd host", "0.9.1", "", initsys.Systemd, 0, with(debianHost(), map[string]string{"/proc/self/cgroup": "0::/user.slice/user-0.slice/session-3.scope\n"}), dpkgAnswers(installed091, apt26),
			Capability{Reason: "not running as serverchief.service"}},
		{"cgroup v1", "0.9.1", "", initsys.Systemd, 0, with(debianHost(), map[string]string{"/proc/self/cgroup": "11:memory:/system.slice/serverchief.service\n1:name=systemd:/system.slice/serverchief.service\n"}), dpkgAnswers(installed091, apt26), Capability{Available: true}},
		{"sysv init script", "0.9.1", "", initsys.SysV, 4242, with(debianHost(), map[string]string{"/etc/init.d/serverchief*": "", "/var/run/serverchief.pid": "4242\n"}), dpkgAnswers(installed091, apt26), Capability{Available: true}},
		{"upstart host", "0.9.1", "", initsys.Upstart, 4242, with(debianHost(), map[string]string{"/etc/init.d/serverchief*": "", "/var/run/serverchief.pid": "4242\n"}), dpkgAnswers(installed091, apt26), Capability{Available: true}},
		{"sysv, started by hand", "0.9.1", "", initsys.SysV, 4242, with(debianHost(), map[string]string{"/etc/init.d/serverchief*": "", "/var/run/serverchief.pid": "17\n"}), dpkgAnswers(installed091, apt26),
			Capability{Reason: "not running under the serverchief init script"}},
		{"sysv without the script", "0.9.1", "", initsys.SysV, 4242, debianHost(), dpkgAnswers(installed091, apt26), Capability{Reason: "the serverchief init script is missing"}},
		{"container without init", "0.9.1", "", initsys.Unknown, 1, debianHost(), dpkgAnswers(installed091, apt26), Capability{Reason: "no init system manages the agent (a container?)"}},
		{"dnf", "0.9.1", "", initsys.Systemd, 0, almaHost(), map[string]Output{"/usr/bin/rpm -q": {Stdout: []byte("0.9.1\n")}}, Capability{Available: true}},
		{"yum (EL7)", "0.9.1", "", initsys.Systemd, 0, with(almaHost(), map[string]string{"/usr/bin/yum*": ""}, "/usr/bin/dnf*"), map[string]Output{"/usr/bin/rpm -q": {Stdout: []byte("0.9.1\n")}}, Capability{Available: true}},
		{"rpm without the repository", "0.9.1", "", initsys.Systemd, 0, with(almaHost(), nil, YumRepoPath), map[string]Output{"/usr/bin/rpm -q": {Stdout: []byte("0.9.1\n")}}, Capability{Reason: "repository not configured"}},
		{"rpm tarball", "0.9.1", "", initsys.Systemd, 0, almaHost(), map[string]Output{"/usr/bin/rpm -q": {Stdout: []byte("package serverchief is not installed\n"), ExitCode: 1}},
			Capability{Reason: "installed from a tarball, not the serverchief package"}},
		{"no package manager", "0.9.1", "", initsys.Systemd, 0, map[string]string{"/usr/bin/systemd-run*": ""}, nil, Capability{Reason: "no supported package manager (dpkg/apt or rpm/dnf/yum)"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &scriptedRunner{answers: test.answers}
			probe := Probe{Version: test.version, GOOS: test.goos, Tools: Tools{Root: fakeRoot(t, test.files)}, Run: runner.run, Init: test.init, PID: test.pid}
			if test.goos == "" {
				probe.GOOS = "linux"
			}
			if got := probe.Detect(context.Background()); got != test.want {
				t.Fatalf("Detect() = %+v, want %+v (ran %q)", got, test.want, runner.ran)
			}
		})
	}
}

func TestControllerCachesTheCapability(t *testing.T) {
	probes := 0
	controller := &Controller{Detect: func(context.Context) Capability { probes++; return Capability{Available: true} }}
	ctx := context.Background()
	for range 3 {
		controller.Capability(ctx, false)
	}
	controller.Capability(ctx, true)
	if probes != 2 {
		t.Fatalf("%d probes", probes)
	}
	controller.Invalidate()
	controller.Capability(ctx, false)
	if probes != 3 {
		t.Fatalf("%d probes after Invalidate", probes)
	}

	// An unavailable answer (a package dpkg is still configuring) is
	// cached for a minute only.
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	answer := Capability{Reason: "the serverchief package is not fully installed"}
	controller = &Controller{Now: func() time.Time { return now }, Detect: func(context.Context) Capability { probes++; return answer }}
	probes = 0
	controller.Capability(ctx, false)
	now = now.Add(30 * time.Second)
	controller.Capability(ctx, false)
	answer = Capability{Available: true}
	now = now.Add(31 * time.Second)
	if got := controller.Capability(ctx, false); !got.Available || probes != 2 {
		t.Fatalf("capability = %+v after %d probes", got, probes)
	}
	now = now.Add(9 * time.Minute)
	controller.Capability(ctx, false)
	if probes != 2 {
		t.Fatalf("an available answer was probed again within 10 minutes (%d probes)", probes)
	}
	if got := (&Controller{}).Capability(ctx, false); got.Available || got.Reason == "" {
		t.Fatalf("capability without a probe = %+v", got)
	}
}

func TestControllerStartAndResume(t *testing.T) {
	store := Store{Directory: t.TempDir(), Owner: -1}
	launches, running := 0, false
	var launchErr error
	controller := &Controller{Store: store, Version: "0.9.1",
		Launch:        func(context.Context) error { launches++; return launchErr },
		HelperRunning: func() bool { return running },
	}
	ctx := context.Background()
	request := Request{CommandID: "c1", PreviousVersion: "0.9.0", TargetVersion: "0.9.1"}

	launchErr = errors.New("systemd-run: boom")
	if err := controller.Start(ctx, request); err == nil {
		t.Fatal("Start() succeeded")
	}
	if pending, _ := controller.PendingRequest(); pending != nil {
		t.Fatal("a failed launch left its request")
	}
	launchErr = nil
	if err := controller.Start(ctx, request); err != nil {
		t.Fatal(err)
	}
	running = true
	if launched, err := controller.Resume(ctx); launched || err != nil {
		t.Fatalf("Resume() with a running helper = %t, %v", launched, err)
	}
	running = false
	if launched, err := controller.Resume(ctx); !launched || err != nil || launches != 3 {
		t.Fatalf("Resume() = %t, %v after %d launches", launched, err, launches)
	}
	if err := controller.WriteReadiness("c1"); err != nil {
		t.Fatal(err)
	}
	if readiness, _ := store.ReadReadiness(); readiness == nil || readiness.Version != "0.9.1" || readiness.PID != os.Getpid() {
		t.Fatalf("readiness = %+v", readiness)
	}
}

func TestSystemdRunArgv(t *testing.T) {
	if got := strings.Join(SystemdRunArgv("/usr/bin/systemd-run", Binary, 252), " "); got != "/usr/bin/systemd-run --unit=serverchief-update --collect --no-block --description=Server Chief agent update /usr/bin/serverchief internal-update" {
		t.Fatalf("argv = %s", got)
	}
	if got := strings.Join(SystemdRunArgv("/usr/bin/systemd-run", Binary, 219), " "); got != "/usr/bin/systemd-run --unit=serverchief-update --description=Server Chief agent update /usr/bin/serverchief internal-update" {
		t.Fatalf("CentOS 7 argv = %s", got)
	}
	if parseSystemdVersion("systemd 252 (252.31-1~deb12u1)\n+PAM +AUDIT") != 252 || parseSystemdVersion("garbage") != 0 {
		t.Fatal("parseSystemdVersion")
	}
}

func TestSystemdLaunchTreatsARunningHelperAsLaunched(t *testing.T) {
	root := fakeRoot(t, map[string]string{"/usr/bin/systemd-run*": "", "/usr/bin/systemctl*": ""})
	runner := &scriptedRunner{answers: map[string]Output{
		"/usr/bin/systemctl --version":     {Stdout: []byte("systemd 252 (252.31-1~deb12u1)\n")},
		"/usr/bin/systemctl reset-failed":  {ExitCode: 1},
		"/usr/bin/systemd-run --unit=serv": {Stderr: []byte("Failed to start transient service unit: Unit serverchief-update.service was already loaded or has a fragment file.\n"), ExitCode: 1},
	}}
	launcher := Launcher{Init: initsys.Systemd, Tools: Tools{Root: root}, Run: runner.run}
	if err := launcher.Launch(context.Background()); err != nil {
		t.Fatalf("Launch() = %v", err)
	}
	if want := "/usr/bin/systemd-run --unit=serverchief-update --collect --no-block --description=Server Chief agent update /usr/bin/serverchief internal-update"; runner.ran[len(runner.ran)-1] != want {
		t.Fatalf("ran %q", runner.ran)
	}
	runner.answers["/usr/bin/systemd-run --unit=serv"] = Output{Stderr: []byte("Failed to connect to bus\n"), ExitCode: 1}
	if err := launcher.Launch(context.Background()); err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatalf("Launch() = %v", err)
	}
}
