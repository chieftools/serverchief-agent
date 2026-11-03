package upgrader

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"go.chief.tools/serverchief-agent/execlog"
	"go.chief.tools/serverchief-agent/updater"
)

// fakeAgentPackage is the serverchief package as the agent update sees it.
// Installing a version makes the "new daemon" write its readiness marker
// unless ready is false.
type fakeAgentPackage struct {
	store     updater.Store
	installed string
	available []string
	ready     bool
	installs  []string
}

func (fake *fakeAgentPackage) Installed(context.Context) (updater.Installed, error) {
	return updater.Installed{Version: fake.installed, Intact: fake.installed != ""}, nil
}

func (fake *fakeAgentPackage) Refresh(context.Context) error { return nil }

func (fake *fakeAgentPackage) Available(context.Context) ([]string, error) {
	return fake.available, nil
}

func (fake *fakeAgentPackage) Install(_ context.Context, version, _ string) error {
	fake.installs = append(fake.installs, version)
	fake.installed = version
	if fake.ready {
		request, _ := fake.store.ReadRequest()
		if request != nil {
			return fake.store.WriteReadiness(updater.Readiness{CommandID: request.CommandID, Version: version, PID: os.Getpid(), WrittenAt: time.Now()})
		}
	}
	return nil
}

func agentUpdateSetup(t *testing.T, installed string) (*fakeAgentPackage, AgentUpdateOptions) {
	t.Helper()
	store := updater.Store{Directory: t.TempDir(), Owner: -1}
	fake := &fakeAgentPackage{store: store, installed: installed, available: []string{"0.4.0", "0.4.1"}, ready: true}
	return fake, AgentUpdateOptions{Store: store, Manager: fake, Helper: func(options *updater.HelperOptions) {
		options.ReadyTimeout, options.ReadyPoll = 50*time.Millisecond, time.Millisecond
	}}
}

func TestUpdateAgentRunsTheAgentUpdateUnderItsNestedID(t *testing.T) {
	fake, options := agentUpdateSetup(t, "0.4.0")
	var output execlog.Log

	update := UpdateAgent(context.Background(), Request{CommandID: "u1", Scope: ScopeAll, AgentVersion: "0.4.1"}, &output, options)

	if update == nil || *update != (AgentUpdate{Code: updater.CodeUpdated, PreviousVersion: "0.4.0", InstalledVersion: "0.4.1"}) {
		t.Fatalf("update = %+v", update)
	}
	if len(fake.installs) != 1 || fake.installs[0] != "0.4.1" {
		t.Fatalf("installs = %v", fake.installs)
	}
	// The nested result is consumed; nothing is left for the daemon.
	if result, _ := options.Store.ReadResult(); result != nil {
		t.Fatalf("result left behind: %+v", result)
	}
	if request, _ := options.Store.ReadRequest(); request != nil {
		t.Fatalf("request left behind: %+v", request)
	}
	if text, _ := output.Text(); !strings.Contains(text, "updating the Server Chief agent from 0.4.0 to 0.4.1 first") {
		t.Fatalf("output = %s", text)
	}
}

func TestUpdateAgentRollsBackLikeUpdateAgent(t *testing.T) {
	fake, options := agentUpdateSetup(t, "0.4.0")
	fake.ready = false
	var output execlog.Log

	update := UpdateAgent(context.Background(), Request{CommandID: "u1", Scope: ScopeAll, AgentVersion: "0.4.1"}, &output, options)

	if update == nil || update.Code != updater.CodeRollbackFailed && update.Code != updater.CodeReadinessFailedRolledBack {
		t.Fatalf("update = %+v", update)
	}
	if len(fake.installs) != 2 || fake.installs[1] != "0.4.0" {
		t.Fatalf("installs = %v", fake.installs)
	}
}

func TestUpdateAgentSkipsOrRefuses(t *testing.T) {
	var output execlog.Log
	request := Request{CommandID: "u1", Scope: ScopeAll, AgentVersion: "0.4.1"}

	_, options := agentUpdateSetup(t, "0.4.1")
	if update := UpdateAgent(context.Background(), request, &output, options); update != nil {
		t.Fatalf("up to date: %+v", update)
	}

	_, options = agentUpdateSetup(t, "0.4.0")
	options.DetectError = errors.New("repository not configured")
	if update := UpdateAgent(context.Background(), request, &output, options); update == nil || update.Code != updater.CodeUnsupportedInstall || update.InstalledVersion != "0.4.0" {
		t.Fatalf("no repository: %+v", update)
	}

	_, options = agentUpdateSetup(t, "0.4.0")
	if err := options.Store.WriteRequest(updater.Request{CommandID: "other", PreviousVersion: "0.4.0", TargetVersion: "0.4.1"}); err != nil {
		t.Fatal(err)
	}
	if update := UpdateAgent(context.Background(), request, &output, options); update == nil || update.Code != updater.CodePackageManagerBusy {
		t.Fatalf("another update pending: %+v", update)
	}
}
