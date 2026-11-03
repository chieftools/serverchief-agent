package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.chief.tools/serverchief-agent/helper"
	"go.chief.tools/serverchief-agent/initsys"
	"go.chief.tools/serverchief-agent/optout"
	"go.chief.tools/serverchief-agent/updater"
)

type optOutTest struct {
	commands optOutCommands
	reloads  int
}

func newOptOutTest(t *testing.T, euid int) *optOutTest {
	t.Helper()
	test := &optOutTest{}
	test.commands = optOutCommands{
		path:  filepath.Join(t.TempDir(), "etc", "disabled-capabilities.json"),
		owner: os.Getuid(),
		euid:  func() int { return euid },
		reload: func(context.Context) error {
			test.reloads++
			return nil
		},
	}
	return test
}

func (test *optOutTest) run(t *testing.T, build func(optOutCommands) *cobra.Command, args ...string) (string, error) {
	t.Helper()
	command := build(test.commands)
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs(args)
	command.SilenceUsage, command.SilenceErrors = true, true
	err := command.ExecuteContext(context.Background())
	return out.String(), err
}

func TestDisableAndEnableChangeTheOptOut(t *testing.T) {
	test := newOptOutTest(t, 0)
	output, err := test.run(t, newDisableCommand, "reboot", "upgrade_packages")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "Turned off: reboot, upgrade_packages.") || !strings.Contains(output, "reports the change to Server Chief now") ||
		!strings.Contains(output, "reboot            off") || test.reloads != 1 {
		t.Fatalf("output = %q, reloads %d", output, test.reloads)
	}
	state := optout.Load(test.commands.path, os.Getuid())
	if state.Err != nil || !reflect.DeepEqual(state.Disabled, []string{"reboot", "upgrade_packages"}) {
		t.Fatalf("state = %+v", state)
	}
	if info, err := os.Stat(test.commands.path); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("file mode = %v (%v)", info, err)
	}

	output, err = test.run(t, newEnableCommand, "reboot")
	if err != nil || !strings.Contains(output, "Turned on: reboot.") {
		t.Fatalf("output = %q (%v)", output, err)
	}
	if state := optout.Load(test.commands.path, os.Getuid()); !reflect.DeepEqual(state.Disabled, []string{"upgrade_packages"}) {
		t.Fatalf("state = %+v", state)
	}

	output, err = test.run(t, newCapabilitiesCommand)
	if err != nil || !strings.Contains(output, "reboot            on   reboot this server") || !strings.Contains(output, "upgrade_packages  off  install package updates") {
		t.Fatalf("capabilities output = %q (%v)", output, err)
	}
}

func TestDisableRejectsUnknownCapabilitiesAndNonRoot(t *testing.T) {
	test := newOptOutTest(t, 0)
	if _, err := test.run(t, newDisableCommand, "reboot", "update_agent"); err == nil || !strings.Contains(err.Error(), `unknown capability "update_agent"`) {
		t.Fatalf("err = %v", err)
	}
	if _, err := test.run(t, newDisableCommand); err == nil {
		t.Fatal("disable without a capability succeeded")
	}
	if _, err := os.Stat(test.commands.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file written after a rejected change: %v", err)
	}

	user := newOptOutTest(t, 1000)
	if _, err := user.run(t, newEnableCommand, "reboot"); err == nil || !strings.Contains(err.Error(), "requires root") {
		t.Fatalf("err = %v", err)
	}
}

func TestDisableReplacesAnInvalidFileAndToleratesAStoppedAgent(t *testing.T) {
	test := newOptOutTest(t, 0)
	test.commands.reload = func(context.Context) error { return errors.New("the agent is not running") }
	if err := os.MkdirAll(filepath.Dir(test.commands.path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(test.commands.path, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := test.run(t, newCapabilitiesCommand)
	if err != nil || !strings.Contains(output, "every remote action counts as turned off") || !strings.Contains(output, "reboot            off") {
		t.Fatalf("output = %q (%v)", output, err)
	}
	output, err = test.run(t, newDisableCommand, "upgrade_packages")
	if err != nil || !strings.Contains(output, "starting from an empty list") || !strings.Contains(output, "within 5 minutes (the agent is not running)") {
		t.Fatalf("output = %q (%v)", output, err)
	}
	if state := optout.Load(test.commands.path, os.Getuid()); state.Err != nil || !reflect.DeepEqual(state.Disabled, []string{"upgrade_packages"}) {
		t.Fatalf("state = %+v", state)
	}
}

func TestRebootCommandAndCapability(t *testing.T) {
	root := t.TempDir()
	tools := updater.Tools{Root: root}
	if _, err := rebootCommand(tools, initsys.Systemd); err == nil || err.Error() != "shutdown is not installed" {
		t.Fatalf("err = %v", err)
	}
	install := func(name string) {
		t.Helper()
		path := filepath.Join(root, "usr", "bin", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	install("systemctl")
	if argv, err := rebootCommand(tools, initsys.Systemd); err != nil || !reflect.DeepEqual(argv, []string{"/usr/bin/systemctl", "reboot"}) {
		t.Fatalf("argv = %q (%v)", argv, err)
	}
	if _, err := rebootCommand(tools, initsys.SysV); err == nil {
		t.Fatal("systemctl used without systemd")
	}
	install("shutdown")
	if argv, err := rebootCommand(tools, initsys.SysV); err != nil || !reflect.DeepEqual(argv, []string{"/usr/bin/shutdown", "-r", "+1", rebootMessage}) {
		t.Fatalf("argv = %q (%v)", argv, err)
	}

	if capability := rebootCapability("", true, nil); !capability.Available || capability.Reason != nil {
		t.Fatalf("vm: %+v", capability)
	}
	if capability := rebootCapability("lxc", true, nil); !capability.Available {
		t.Fatalf("lxc: %+v", capability)
	}
	for container, reason := range map[string]string{
		"docker": "running in a docker container, which has no init system to reboot",
		"podman": "running in a podman container, which has no init system to reboot",
	} {
		if capability := rebootCapability(container, true, nil); capability.Available || *capability.Reason != reason {
			t.Fatalf("%s: %+v", container, capability)
		}
	}
	if capability := rebootCapability("", false, nil); capability.Available || *capability.Reason != "the root helper does not answer" {
		t.Fatalf("no helper: %+v", capability)
	}
	if capability := rebootCapability("", true, errors.New("shutdown is not installed")); capability.Available || *capability.Reason != "shutdown is not installed" {
		t.Fatalf("no shutdown: %+v", capability)
	}
}

func TestHelperOffersReboot(t *testing.T) {
	if names := helperActions().Names(); !slices.Contains(names, "reboot") {
		t.Fatalf("helper actions = %q", names)
	}
}

func TestRebootActionChecksTheOptOutItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disabled-capabilities.json")
	if err := optout.Save(path, []string{optout.Reboot}); err != nil {
		t.Fatal(err)
	}
	result := helper.Local{Actions: helper.Actions{helper.ActionReboot: rebootAction(path, os.Getuid())}}
	if _, err := result.Do(context.Background(), helper.ActionReboot, nil); err == nil || err.Error() != "rebooting is turned off on this server" {
		t.Fatalf("err = %v", err)
	}
	// A file that cannot be trusted refuses too.
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := result.Do(context.Background(), helper.ActionReboot, nil); err == nil || !strings.HasPrefix(err.Error(), "rebooting is turned off on this server: decode") {
		t.Fatalf("err = %v", err)
	}
	// Arguments are refused before anything runs.
	if _, err := result.Do(context.Background(), helper.ActionReboot, map[string]string{"delay": "0"}); err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Fatalf("err = %v", err)
	}
}
