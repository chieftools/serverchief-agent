package cmd

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.chief.tools/serverchief-agent/helper"
	"go.chief.tools/serverchief-agent/initsys"
	"go.chief.tools/serverchief-agent/optout"
	"go.chief.tools/serverchief-agent/updater"
)

func TestUpgradeCapability(t *testing.T) {
	for _, test := range []struct {
		manager    string
		answers    bool
		init       initsys.Kind
		systemdRun string
		reason     string
	}{
		{"apt", true, initsys.Systemd, "/usr/bin/systemd-run", ""},
		{"yum", true, initsys.SysV, "", ""},
		{"", true, initsys.Systemd, "/usr/bin/systemd-run", "no supported package manager (apt, dnf or yum) found"},
		{"dnf", false, initsys.Systemd, "/usr/bin/systemd-run", "the root helper does not answer"},
		{"apt", true, initsys.Systemd, "", "systemd-run is not installed"},
	} {
		capability := upgradeCapability(test.manager, test.answers, test.init, test.systemdRun)
		if capability.Available != (test.reason == "") || capability.Reason != test.reason {
			t.Errorf("%+v: capability = %+v", test, capability)
		}
	}
}

func TestHelperOffersStartPackageUpgrade(t *testing.T) {
	if !slices.Contains(helperActions().Names(), helper.ActionStartPackageUpgrade) {
		t.Fatalf("actions = %v", helperActions().Names())
	}
}

func TestStartPackageUpgradeChecksTheOptOutItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disabled-capabilities.json")
	launches := 0
	action := startUpgradeAction(path, os.Getuid(), func(context.Context) error { launches++; return nil })
	local := helper.Local{Actions: helper.Actions{helper.ActionStartPackageUpgrade: action}}

	if _, err := local.Do(context.Background(), helper.ActionStartPackageUpgrade, nil); err != nil || launches != 1 {
		t.Fatalf("err = %v, launches %d", err, launches)
	}
	if err := optout.Save(path, []string{optout.UpgradePackages}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Do(context.Background(), helper.ActionStartPackageUpgrade, nil); err == nil || err.Error() != "installing package updates is turned off on this server" || launches != 1 {
		t.Fatalf("err = %v, launches %d", err, launches)
	}
	if _, err := local.Do(context.Background(), helper.ActionStartPackageUpgrade, map[string]string{"scope": "all"}); err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpgradeLauncherRunsInternalUpgradeInItsOwnUnit(t *testing.T) {
	root := t.TempDir()
	for _, tool := range []string{"systemd-run", "systemctl"} {
		path := filepath.Join(root, "usr/bin", tool)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var ran []string
	launcher := upgradeLauncher(initsys.Systemd)
	launcher.Tools = updater.Tools{Root: root}
	launcher.Run = func(_ context.Context, _ []string, argv []string) (updater.Output, error) {
		ran = append(ran, strings.Join(argv, " "))
		if argv[len(argv)-1] == "--version" {
			return updater.Output{Stdout: []byte("systemd 252 (252.31-1~deb12u1)\n")}, nil
		}
		return updater.Output{}, nil
	}
	if err := launcher.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The upgrade helper runs the agent's own executable, wherever it is installed.
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	want := "/usr/bin/systemd-run --unit=serverchief-upgrade --collect --no-block --description=Server Chief package upgrade " + executable + " internal-upgrade"
	if !slices.Contains(ran, want) || !slices.Contains(ran, "/usr/bin/systemctl reset-failed serverchief-upgrade.service") {
		t.Fatalf("ran = %q", ran)
	}
	// The agent update helper keeps its own unit.
	if argv := strings.Join(updater.SystemdRunArgv("/usr/bin/systemd-run", updater.Binary, 252), " "); !strings.Contains(argv, "--unit=serverchief-update ") || !strings.HasSuffix(argv, " internal-update") {
		t.Fatalf("update argv = %s", argv)
	}
}
