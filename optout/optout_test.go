package optout

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disabled-capabilities.json")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMissingFileDisablesNothing(t *testing.T) {
	state := Load(filepath.Join(t.TempDir(), "missing.json"), -1)
	if state.Err != nil || state.Disabled == nil || len(state.Disabled) != 0 || state.IsDisabled(Reboot) {
		t.Fatalf("state = %+v", state)
	}
}

func TestLoadValidFile(t *testing.T) {
	path := writeFile(t, `{"version":1,"disabled":["upgrade_packages","reboot","reboot","future_thing"]}`, 0o644)
	state := Load(path, -1)
	if state.Err != nil || !reflect.DeepEqual(state.Disabled, []string{"future_thing", "reboot", "upgrade_packages"}) {
		t.Fatalf("state = %+v", state)
	}
	if !state.IsDisabled(Reboot) {
		t.Fatal("reboot is not disabled")
	}
}

func TestLoadFailsClosed(t *testing.T) {
	for name, content := range map[string]string{
		"invalid json":   `{"version":1,"disabled":[`,
		"empty":          ``,
		"other version":  `{"version":2,"disabled":[]}`,
		"no version":     `{"disabled":[]}`,
		"invalid name":   `{"version":1,"disabled":["Reboot; rm -rf"]}`,
		"unknown fields": `[1,2]`,
	} {
		t.Run(name, func(t *testing.T) {
			state := Load(writeFile(t, content, 0o644), -1)
			if state.Err == nil || !reflect.DeepEqual(state.Disabled, Known) {
				t.Fatalf("state = %+v", state)
			}
		})
	}
}

func TestLoadChecksOwnerAndMode(t *testing.T) {
	path := writeFile(t, `{"version":1,"disabled":[]}`, 0o644)
	if state := Load(path, os.Getuid()); state.Err != nil || len(state.Disabled) != 0 {
		t.Fatalf("own file: %+v", state)
	}
	if state := Load(path, os.Getuid()+1); state.Err == nil || !state.IsDisabled(Reboot) {
		t.Fatalf("foreign owner: %+v", state)
	}
	writable := writeFile(t, `{"version":1,"disabled":[]}`, 0o666)
	if state := Load(writable, os.Getuid()); state.Err == nil || !strings.Contains(state.Err.Error(), "writable") {
		t.Fatalf("world-writable: %+v", state)
	}
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if state := Load(link, -1); state.Err == nil {
		t.Fatalf("symlink: %+v", state)
	}
}

func TestSaveRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "disabled-capabilities.json")
	if err := Save(path, []string{"reboot", "upgrade_packages", "reboot"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v (%v)", info.Mode(), err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "{\n  \"version\": 1,\n  \"disabled\": [\n    \"reboot\",\n    \"upgrade_packages\"\n  ]\n}\n" {
		t.Fatalf("file = %s", data)
	}
	if err := Save(path, nil); err != nil {
		t.Fatal(err)
	}
	if state := Load(path, os.Getuid()); state.Err != nil || state.Disabled == nil || len(state.Disabled) != 0 {
		t.Fatalf("after enabling everything: %+v", state)
	}
}

func TestValidate(t *testing.T) {
	if err := Validate([]string{"reboot", "upgrade_packages"}); err != nil {
		t.Fatal(err)
	}
	err := Validate([]string{"reboot", "update_agent", "shell"})
	if err == nil || !strings.Contains(err.Error(), `"update_agent", "shell"`) || !strings.Contains(err.Error(), "reboot, upgrade_packages") {
		t.Fatalf("err = %v", err)
	}
}
