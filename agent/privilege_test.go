package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.chief.tools/serverchief-agent/helper"
)

type pingRunner struct {
	pings int
	err   error
}

func (runner *pingRunner) Do(context.Context, string, map[string]string) (helper.Result, error) {
	runner.pings++
	return helper.Result{}, runner.err
}

func TestPrivilegeProbeReportsTheRootHelper(t *testing.T) {
	now := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	runner := &pingRunner{}
	probe := &PrivilegeProbe{Unprivileged: true, Runner: runner, Actions: []string{"ping", "refresh-metadata"}, Now: func() time.Time { return now }}

	capability := probe.Capability(context.Background())
	if capability.Mode != PrivilegeUnprivileged || len(capability.HelperActions) != 2 || capability.Reason != nil {
		t.Fatalf("Capability() = %+v", capability)
	}
	// An answering helper is pinged once an hour
	now = now.Add(59 * time.Minute)
	probe.Capability(context.Background())
	if runner.pings != 1 {
		t.Fatalf("pings = %d", runner.pings)
	}

	runner.err = errors.New("the root helper did not answer ping: context deadline exceeded")
	now = now.Add(2 * time.Minute)
	capability = probe.Capability(context.Background())
	if len(capability.HelperActions) != 0 || capability.Reason == nil || *capability.Reason != "the root helper does not answer: the root helper did not answer ping: context deadline exceeded" {
		t.Fatalf("Capability() = %+v", capability)
	}
	// A silent helper is asked again after five minutes
	runner.err = nil
	now = now.Add(5 * time.Minute)
	if !probe.HelperAnswers(context.Background()) || runner.pings != 3 {
		t.Fatalf("pings = %d", runner.pings)
	}
}

func TestPrivilegeProbeReportsARootDaemon(t *testing.T) {
	probe := &PrivilegeProbe{Actions: []string{"ping"}, RootReason: "systemd 219 cannot grant ambient capabilities (229 or later is required)"}

	capability := probe.Capability(context.Background())
	if capability.Mode != PrivilegeRoot || capability.Reason == nil || *capability.Reason != probe.RootReason || !probe.HelperAnswers(context.Background()) {
		t.Fatalf("Capability() = %+v", capability)
	}
}
