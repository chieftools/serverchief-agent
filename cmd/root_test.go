package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.chief.tools/serverchief-agent/inventory"
)

func execute(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := NewRootCommand("1.2.3")
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

func TestVersionCommand(t *testing.T) {
	stdout, _, err := execute(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout, "Server Chief agent 1.2.3 (") || !strings.Contains(stdout, "protocol 1.1.0") {
		t.Fatalf("version output = %q", stdout)
	}
}

func TestCollectCommandAgainstFixture(t *testing.T) {
	stdout, _, err := execute(t, "collect", "--root", "../collector/testdata/debian-12", "--dataset", "system,resolvers")
	if err != nil {
		t.Fatal(err)
	}
	var envelope inventory.Envelope
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("output is not an envelope: %v\n%s", err, stdout)
	}
	if len(envelope.Datasets) != 2 || envelope.Seq != 0 {
		t.Fatalf("envelope = %#v", envelope)
	}
	system := envelope.Datasets["system"]
	if system.Mode != inventory.ModeFull || inventory.Hash(system.Payload) != system.Hash {
		t.Fatalf("system hash %s does not match its payload", system.Hash)
	}
	var payload struct {
		OS struct {
			ID          string `json:"id"`
			VersionFull string `json:"version_full"`
		} `json:"os"`
		Init string `json:"init"`
	}
	if err := json.Unmarshal(system.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.OS.ID != "debian" || payload.OS.VersionFull != "12.7" || payload.Init != "systemd" {
		t.Fatalf("system payload = %+v", payload)
	}
}

func TestCollectCommandIsDeterministic(t *testing.T) {
	first, _, err := execute(t, "collect", "--root", "../collector/testdata/x86-kvm", "--pretty")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := execute(t, "collect", "--root", "../collector/testdata/x86-kvm", "--pretty")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("collect output differs between runs")
	}
	if !strings.Contains(first, "\n  \"datasets\": {") {
		t.Fatalf("--pretty output is not indented:\n%s", first)
	}
}

func TestCollectCommandUnknownDataset(t *testing.T) {
	_, _, err := execute(t, "collect", "--dataset", "software")
	if err == nil || !strings.Contains(err.Error(), `unknown dataset "software"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestSetupCommandValidatesToken(t *testing.T) {
	_, _, err := execute(t, "setup", "not-a-token")
	if err == nil {
		t.Fatal("setup accepted an invalid token")
	}
}

func TestSetupCommandReadsEnvironmentDefaults(t *testing.T) {
	t.Setenv("SERVERCHIEF_ENDPOINT", "https://server.chief.test/agent/v1")
	t.Setenv("SERVERCHIEF_CA_FILE", "/tmp/valet.pem")
	command := newSetupCommand("1.2.3")
	if got := command.Flags().Lookup("endpoint").DefValue; got != "https://server.chief.test/agent/v1" {
		t.Fatalf("endpoint default = %q", got)
	}
	if got := command.Flags().Lookup("ca-file").DefValue; got != "/tmp/valet.pem" {
		t.Fatalf("ca-file default = %q", got)
	}
}

func TestRunCommandRequiresEnrollment(t *testing.T) {
	directory := t.TempDir()
	_, _, err := execute(t, "run", "--identity", directory+"/identity.json", "--state-dir", directory, "--config", directory+"/config.json")
	if err == nil || err.Error() != "not enrolled, run `serverchief setup <token>`" {
		t.Fatalf("err = %v", err)
	}
}

func TestRootCommandRejectsArguments(t *testing.T) {
	if _, _, err := execute(t, "bogus"); err == nil {
		t.Fatal("unknown command accepted")
	}
}
