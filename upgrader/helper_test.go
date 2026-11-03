package upgrader

import (
	"context"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.chief.tools/serverchief-agent/collector"
	"go.chief.tools/serverchief-agent/execlog"
	"go.chief.tools/serverchief-agent/updater"
)

// fakePackages scripts the host's package manager: the pending updates
// before and after the upgrade command ran, and the tools it has.
type fakePackages struct {
	manager  string
	tools    map[string]string
	before   []collector.Update
	after    []collector.Update
	reboot   *bool
	security bool
	upgraded bool
}

func (fake *fakePackages) Manager() string { return fake.manager }

func (fake *fakePackages) Tool(name string) string { return fake.tools[name] }

func (fake *fakePackages) RefreshCommand() ([]string, error) {
	if fake.manager == ManagerApt {
		return []string{"/usr/bin/apt-get", "update", "-qq"}, nil
	}
	return []string{"/usr/bin/" + fake.manager, "makecache", "-q"}, nil
}

func (fake *fakePackages) PendingUpdates(context.Context) (*collector.UpdatesInfo, error) {
	items := fake.before
	if fake.upgraded {
		items = fake.after
	}
	return &collector.UpdatesInfo{Manager: fake.manager, Items: slices.Clone(items), RebootRequired: fake.reboot}, nil
}

func (fake *fakePackages) HasSecurityMetadata(context.Context, int) bool { return fake.security }

// fakeRunner records commands and answers them by their program name.
type fakeRunner struct {
	mu       sync.Mutex
	ran      [][]string
	env      [][]string
	answers  map[string]Output
	packages *fakePackages
}

func (fake *fakeRunner) run(_ context.Context, env []string, argv []string) (Output, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.ran = append(fake.ran, slices.Clone(argv))
	fake.env = append(fake.env, slices.Clone(env))
	key := argv[0]
	if len(argv) > 1 {
		key += " " + argv[1]
	}
	if key == "/usr/bin/apt-get install" || key == "/usr/bin/dnf upgrade" || key == "/usr/bin/yum upgrade" {
		fake.packages.upgraded = true
	}
	return fake.answers[key], nil
}

func (fake *fakeRunner) commands() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	lines := make([]string, len(fake.ran))
	for index, argv := range fake.ran {
		lines[index] = strings.Join(argv, " ")
	}
	return lines
}

type helperSetup struct {
	files    Files
	packages *fakePackages
	runner   *fakeRunner
	options  Options
	agent    []Request
}

func newHelperSetup(t *testing.T, manager string) *helperSetup {
	t.Helper()
	required := true
	packages := &fakePackages{manager: manager, reboot: &required, security: true, tools: map[string]string{
		"apt-get": "/usr/bin/apt-get", "dpkg": "/usr/bin/dpkg", "dnf": "/usr/bin/dnf", "yum": "/usr/bin/yum",
		"needrestart": "/usr/sbin/needrestart", "needs-restarting": "/usr/bin/needs-restarting", "systemctl": "/usr/bin/systemctl",
	}}
	runner := &fakeRunner{answers: map[string]Output{}, packages: packages}
	setup := &helperSetup{
		files:    Files{Store: updater.Store{Directory: t.TempDir(), Owner: -1}},
		packages: packages,
		runner:   runner,
	}
	setup.options = Options{
		Files: setup.files, Packages: packages, Run: runner.run, Systemd: true,
		Sleep: func(context.Context, time.Duration) error { return nil },
		AgentUpdate: func(_ context.Context, request Request, output *execlog.Log) *AgentUpdate {
			setup.agent = append(setup.agent, request)
			_, _ = output.Write([]byte("# agent update ran\n"))
			return &AgentUpdate{Code: updater.CodeReadinessFailedRolledBack, PreviousVersion: "0.4.0", InstalledVersion: "0.4.0"}
		},
	}
	return setup
}

func (setup *helperSetup) request(t *testing.T, request Request) {
	t.Helper()
	if request.CommandID == "" {
		request.CommandID = "u1"
	}
	if err := setup.files.WriteRequest(request); err != nil {
		t.Fatal(err)
	}
}

func (setup *helperSetup) run(t *testing.T) (*Result, string) {
	t.Helper()
	if err := RunHelper(context.Background(), setup.options); err != nil {
		t.Fatalf("RunHelper() = %v", err)
	}
	result, output, err := setup.files.ReadResult()
	if err != nil || result == nil {
		t.Fatalf("result = %v, %v", result, err)
	}
	if request, _ := setup.files.ReadRequest(); request != nil {
		t.Fatal("request kept after the result")
	}
	return result, output
}

func update(name, arch string, security bool) collector.Update {
	return collector.Update{Name: name, Arch: arch, CurrentVersion: "1", CandidateVersion: "2", Security: security}
}

func TestAptUpgradeInstallsTheScopeWithoutServerChiefAndReportsNeedrestart(t *testing.T) {
	setup := newHelperSetup(t, ManagerApt)
	setup.packages.before = []collector.Update{
		update("openssl", "amd64", true), update("libssl3", "amd64", true), update("tzdata", "all", false), update("serverchief", "amd64", true),
	}
	setup.packages.after = []collector.Update{update("tzdata", "all", false), update("serverchief", "amd64", true)}
	setup.runner.answers["/usr/bin/apt-get install"] = Output{Text: []byte("Reading package lists...\nRestarting services...\n systemctl restart nginx.service php8.3-fpm.service\nService restarts being deferred:\n systemctl restart dbus.service\n")}
	setup.request(t, Request{Scope: ScopeSecurity, RestartServices: true, AgentVersion: "0.4.1"})

	result, output := setup.run(t)

	want := []string{
		"/usr/bin/dpkg --audit",
		"/usr/bin/apt-get update -qq",
		"/usr/bin/apt-get install -y --only-upgrade -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dpkg::Use-Pty=0 libssl3:amd64 openssl:amd64",
	}
	if commands := setup.runner.commands(); !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %q", commands)
	}
	if env := setup.runner.env[2]; !slices.Contains(env, "NEEDRESTART_MODE=a") || slices.Contains(env, "NEEDRESTART_SUSPEND=1") || !slices.Contains(env, "DEBIAN_FRONTEND=noninteractive") {
		t.Fatalf("install env = %q", env)
	}
	data := result.Data
	if result.Status != StatusSucceeded || data.Code != CodeUpgraded || data.Scope != ScopeSecurity || *data.Upgraded != 2 || *data.HeldBack != 0 || !*data.RebootRequired {
		t.Fatalf("result = %+v, data %+v", result, data)
	}
	if !reflect.DeepEqual(data.ServicesRestarted, []string{"nginx.service", "php8.3-fpm.service"}) {
		t.Fatalf("services = %q", data.ServicesRestarted)
	}
	// The agent update ran first, and its failure did not stop the upgrade.
	if len(setup.agent) != 1 || data.AgentUpdate == nil || data.AgentUpdate.Code != updater.CodeReadinessFailedRolledBack {
		t.Fatalf("agent update = %+v, %+v", setup.agent, data.AgentUpdate)
	}
	if !strings.Contains(output, "# agent update ran\n") || strings.Index(output, "# agent update ran") > strings.Index(output, "apt-get install") ||
		!strings.Contains(output, "$ DEBIAN_FRONTEND=noninteractive APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a /usr/bin/apt-get install") {
		t.Fatalf("output = %s", output)
	}
}

func TestAptUpgradeWithoutRestartingServicesSuspendsNeedrestart(t *testing.T) {
	setup := newHelperSetup(t, ManagerApt)
	setup.packages.before = []collector.Update{update("tzdata", "all", false), update("curl", "arm64", false)}
	setup.packages.after = []collector.Update{update("curl", "arm64", false)}
	setup.request(t, Request{Scope: ScopeAll})

	result, _ := setup.run(t)

	commands := setup.runner.commands()
	if commands[len(commands)-1] != "/usr/bin/apt-get install -y --only-upgrade -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dpkg::Use-Pty=0 curl:arm64 tzdata" {
		t.Fatalf("commands = %q", commands)
	}
	if env := setup.runner.env[len(commands)-1]; !slices.Contains(env, "NEEDRESTART_SUSPEND=1") || slices.Contains(env, "NEEDRESTART_MODE=a") {
		t.Fatalf("env = %q", env)
	}
	if result.Data.ServicesRestarted != nil || *result.Data.Upgraded != 1 || *result.Data.HeldBack != 1 || result.Data.AgentUpdate != nil || len(setup.agent) != 0 {
		t.Fatalf("data = %+v, agent %v", result.Data, setup.agent)
	}
}

func TestRPMUpgradeRestartsTheServicesNeedsRestartingLists(t *testing.T) {
	setup := newHelperSetup(t, ManagerDnf)
	setup.packages.before = []collector.Update{update("openssl-libs", "x86_64", true), update("serverchief", "x86_64", false)}
	setup.runner.answers["/usr/bin/needs-restarting -s"] = Output{Text: []byte("nginx.service\nserverchief.service\ndbus-broker.service\nsystemd-logind.service\nphp-fpm\n")}
	setup.request(t, Request{Scope: ScopeSecurity, RestartServices: true})

	result, _ := setup.run(t)

	want := []string{
		"/usr/bin/dnf makecache -q",
		"/usr/bin/dnf upgrade -y --setopt=exit_on_lock=True --exclude=serverchief --security",
		"/usr/bin/needs-restarting -s",
		"/usr/bin/systemctl try-restart nginx.service",
		"/usr/bin/systemctl try-restart php-fpm.service",
	}
	if commands := setup.runner.commands(); !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %q", commands)
	}
	if !reflect.DeepEqual(result.Data.ServicesRestarted, []string{"nginx.service", "php-fpm.service"}) || result.Data.Code != CodeUpgraded {
		t.Fatalf("data = %+v", result.Data)
	}
}

func TestRPMSecurityUpgradeNeedsSecurityMetadata(t *testing.T) {
	setup := newHelperSetup(t, ManagerYum)
	setup.packages.security = false
	setup.packages.before = []collector.Update{update("openssl", "x86_64", false)}
	setup.request(t, Request{Scope: ScopeSecurity})

	result, _ := setup.run(t)

	if result.Status != StatusFailed || result.Data.Code != CodeSecurityUnavailable {
		t.Fatalf("result = %+v", result)
	}
	for _, command := range setup.runner.commands() {
		if strings.Contains(command, " upgrade ") {
			t.Fatalf("upgraded anyway: %q", command)
		}
	}

	// All updates need no advisories.
	setup.request(t, Request{CommandID: "u2", Scope: ScopeAll})
	result, _ = setup.run(t)
	if result.Data.Code != CodeUpgraded || !slices.Contains(setup.runner.commands(), "/usr/bin/yum upgrade -y --setopt=exit_on_lock=True --exclude=serverchief") {
		t.Fatalf("result = %+v, commands %q", result.Data, setup.runner.commands())
	}
}

func TestNothingPendingIsNothingToUpgrade(t *testing.T) {
	setup := newHelperSetup(t, ManagerApt)
	setup.packages.before = []collector.Update{update("serverchief", "amd64", false)}
	setup.packages.after = setup.packages.before
	setup.request(t, Request{Scope: ScopeAll, RestartServices: true})

	result, _ := setup.run(t)

	if result.Status != StatusSucceeded || result.Data.Code != CodeNothingToUpgrade || *result.Data.Upgraded != 0 || result.Data.ServicesRestarted != nil {
		t.Fatalf("result = %+v", result.Data)
	}
}

func TestInterruptedDpkgStopsBeforeAnything(t *testing.T) {
	setup := newHelperSetup(t, ManagerApt)
	setup.runner.answers["/usr/bin/dpkg --audit"] = Output{Text: []byte("The following packages are only half configured:\n openssl\n")}
	setup.request(t, Request{Scope: ScopeAll, AgentVersion: "0.4.1"})

	result, output := setup.run(t)

	if result.Data.Code != CodeDpkgInterrupted || !strings.Contains(result.Detail, "dpkg --configure -a") || len(setup.agent) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if commands := setup.runner.commands(); !reflect.DeepEqual(commands, []string{"/usr/bin/dpkg --audit"}) || !strings.Contains(output, "half configured") {
		t.Fatalf("commands = %q, output %s", commands, output)
	}
}

func TestFailedInstallIsUpgradeFailedWithItsLastLines(t *testing.T) {
	setup := newHelperSetup(t, ManagerApt)
	setup.packages.before = []collector.Update{update("openssl", "amd64", false)}
	setup.packages.after = setup.packages.before
	setup.runner.answers["/usr/bin/apt-get install"] = Output{Text: []byte("E: Sub-process /usr/bin/dpkg returned an error code (1)\n"), ExitCode: 100}
	setup.request(t, Request{Scope: ScopeAll})

	result, _ := setup.run(t)

	if result.Status != StatusFailed || result.Data.Code != CodeUpgradeFailed || !strings.Contains(result.Detail, "apt-get exited with status 100: E: Sub-process") || *result.Data.HeldBack != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestBusyPackageManagerEndsAfterTheWait(t *testing.T) {
	setup := newHelperSetup(t, ManagerApt)
	now := time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC)
	setup.options.Now = func() time.Time { return now }
	setup.options.Sleep = func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil }
	setup.options.Locks = func() (string, bool) { return "/var/lib/dpkg/lock-frontend (held by unattended-upgr)", true }
	setup.request(t, Request{Scope: ScopeAll})

	result, _ := setup.run(t)

	if result.Data.Code != CodePackageManagerBusy || !strings.Contains(result.Detail, "lock-frontend") || len(setup.runner.commands()) != 0 {
		t.Fatalf("result = %+v, commands %q", result, setup.runner.commands())
	}
}

func TestResumeKeepsTheOutputAndThePlan(t *testing.T) {
	setup := newHelperSetup(t, ManagerApt)
	setup.packages.before = []collector.Update{update("openssl", "amd64", false)}
	setup.packages.after = nil
	setup.request(t, Request{Scope: ScopeAll, AgentVersion: "0.4.1"})
	// A helper that stopped after the agent update, before the install.
	if err := setup.files.writeState(state{CommandID: "u1", Phase: phaseUpgrade, Starts: 1, AgentDone: true,
		AgentUpdate: &AgentUpdate{Code: updater.CodeUpdated, PreviousVersion: "0.4.0", InstalledVersion: "0.4.1"}}); err != nil {
		t.Fatal(err)
	}
	if err := setup.files.writeOutput("$ /usr/bin/apt-get update -qq\nearlier output\n"); err != nil {
		t.Fatal(err)
	}

	result, output := setup.run(t)

	if len(setup.agent) != 0 || result.Data.AgentUpdate == nil || result.Data.AgentUpdate.Code != updater.CodeUpdated || *result.Data.Upgraded != 1 {
		t.Fatalf("agent = %v, data %+v", setup.agent, result.Data)
	}
	if !strings.HasPrefix(output, "$ /usr/bin/apt-get update -qq\nearlier output\n") || !strings.Contains(output, "apt-get install") {
		t.Fatalf("output = %s", output)
	}
	if commands := setup.runner.commands(); len(commands) != 1 || !strings.Contains(commands[0], "apt-get install") {
		t.Fatalf("commands = %q", commands)
	}
	if _, err := os.Stat(setup.files.Store.Directory + "/" + StateFile); !os.IsNotExist(err) {
		t.Fatal("state kept")
	}
}

func TestRestartedHelperGivesUp(t *testing.T) {
	setup := newHelperSetup(t, ManagerApt)
	setup.request(t, Request{Scope: ScopeAll})
	if err := setup.files.writeState(state{CommandID: "u1", Phase: phaseUpgrade, Starts: maximumHelperStarts}); err != nil {
		t.Fatal(err)
	}
	result, _ := setup.run(t)
	if result.Data.Code != CodeUpgradeFailed || !strings.Contains(result.Detail, "stopped 5 times in phase upgrading") || len(setup.runner.commands()) != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestRequestValidation(t *testing.T) {
	for _, request := range []Request{
		{CommandID: "u1", Scope: "everything"},
		{CommandID: "u1", Scope: ScopeAll, AgentVersion: "1.0"},
		{CommandID: "u 1", Scope: ScopeAll},
		{CommandID: updater.NestedCommandID("u1"), Scope: ScopeAll},
	} {
		if request.Validate() == nil {
			t.Errorf("%+v validated", request)
		}
	}
	if err := (Request{CommandID: "01jb9a2c3d4e5f6g7h8j9k0m1r", Scope: ScopeSecurity, AgentVersion: "0.4.1"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCommandBuilders(t *testing.T) {
	if _, err := AptInstall("/usr/bin/apt-get", []string{"openssl", "serverchief:amd64"}); err == nil {
		t.Fatal("serverchief accepted")
	}
	if _, err := AptInstall("/usr/bin/apt-get", []string{"openssl; rm -rf /"}); err == nil {
		t.Fatal("invalid name accepted")
	}
	if _, err := AptInstall("apt-get", []string{"openssl"}); err == nil {
		t.Fatal("relative tool accepted")
	}
	if argv, err := RPMUpgrade("/usr/bin/yum", ScopeAll); err != nil || strings.Join(argv, " ") != "/usr/bin/yum upgrade -y --setopt=exit_on_lock=True --exclude=serverchief" {
		t.Fatalf("argv = %q, %v", argv, err)
	}
	for name, want := range map[string]string{
		"nginx.service": "nginx.service", "php-fpm": "php-fpm.service", "serverchief.service": "", "serverchief-helper.service": "",
		"dbus.service": "", "systemd-journald.service": "", "getty@tty1.service": "", "user@0.service": "", "foo.socket": "", "a b": "",
	} {
		if got := RestartableUnit(name); got != want {
			t.Errorf("RestartableUnit(%q) = %q, want %q", name, got, want)
		}
	}
}
