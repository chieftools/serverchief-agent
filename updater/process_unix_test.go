//go:build unix

package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSpawnDetachedCreatesAMissingLogDirectory(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log", "serverchief", "upgrade.log")
	command, err := SpawnDetached("/bin/sh", []string{"-c", "echo started"}, logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil || string(data) != "started\n" {
		t.Fatalf("log = %q, %v", data, err)
	}
	if info, err := os.Stat(filepath.Dir(logPath)); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("directory = %v, %v", info, err)
	}
}
