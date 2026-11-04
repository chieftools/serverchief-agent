package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.chief.tools/serverchief-agent/optout"
)

// fakeDisabler records the capabilities the root helper was asked to turn
// off.
type fakeDisabler struct {
	mu      sync.Mutex
	calls   []string
	changed bool
	err     error
}

func (fake *fakeDisabler) disable(_ context.Context, name string) (bool, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls = append(fake.calls, name)
	return fake.changed, fake.err
}

func (fake *fakeDisabler) count() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return len(fake.calls)
}

// startWithDisabler starts a daemon whose opt-out is state and whose root
// helper is disabler.
func startWithDisabler(t *testing.T, setup *rebootSetup, state *optout.State, disabler *fakeDisabler) *daemon {
	t.Helper()
	options := setup.options(t, setup.server.client(), "system")
	journal, err := OpenCommandJournal(setup.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	options.Commands = journal
	options.Now = setup.clock.Now
	options.OptOut = func() optout.State { return *state }
	options.DisableCapability = disabler.disable
	return newDaemon(options)
}

func disableCommand(t *testing.T, setup *rebootSetup, daemon *daemon, id, args string) DisableCapabilityData {
	t.Helper()
	setup.server.addCommand(id, CommandDisableCapability, args)
	daemon.heartbeatAt = setup.clock.Now()
	tick(t, daemon)
	results := setup.server.resultsOf(id)
	if len(results) != 1 {
		t.Fatalf("%s results = %+v", id, results)
	}
	var data DisableCapabilityData
	if results[0].Data != nil {
		if err := json.Unmarshal(results[0].Data, &data); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

func TestDisableCapabilityFixtureRoundTrips(t *testing.T) {
	fixture := readFixture(t, "disable-capability-result-request.json")
	var result CommandResult
	if err := json.Unmarshal(fixture, &result); err != nil {
		t.Fatal(err)
	}
	var data DisableCapabilityData
	if err := json.Unmarshal(result.Data, &data); err != nil || data.Code != DisableCodeDisabled || data.Capability != optout.Reboot {
		t.Fatalf("data = %+v (%v)", data, err)
	}
	if encoded, _ := json.Marshal(result); !sameJSON(t, encoded, fixture) {
		t.Fatalf("disable_capability CommandResult does not round-trip the fixture:\n%s", encoded)
	}
}

func TestDisableCapabilityTurnsItOffThroughTheHelperAndHeartbeats(t *testing.T) {
	setup := newRebootSetup(t)
	state := optout.State{Disabled: []string{}}
	disabler := &fakeDisabler{changed: true}
	daemon := startWithDisabler(t, setup, &state, disabler)
	tick(t, daemon)

	before := len(setup.server.requestLog())
	data := disableCommand(t, setup, daemon, "d1", `{"capability":"reboot"}`)
	if data.Code != DisableCodeDisabled || data.Capability != optout.Reboot || !slices.Equal(disabler.calls, []string{optout.Reboot}) {
		t.Fatalf("data = %+v, calls %v", data, disabler.calls)
	}
	if results := setup.server.resultsOf("d1"); results[0].Status != CommandSucceeded || !strings.Contains(*results[0].Detail, "serverchief enable reboot") {
		t.Fatalf("results = %+v", results)
	}
	if at := daemon.wakeAt(); at.After(setup.clock.Now()) {
		t.Fatalf("next heartbeat at %v", at)
	}
	state = optout.State{Disabled: []string{optout.Reboot}}
	tick(t, daemon)
	if got := requestsSince(setup.server, before); !slices.Contains(got, "/heartbeat") {
		t.Fatalf("requests = %q", got)
	}
}

func TestDisableCapabilityLeavesAnAlreadyOffOrUntrustedOptOutAlone(t *testing.T) {
	for name, state := range map[string]optout.State{
		"already off": {Disabled: []string{optout.UpgradePackages}},
		"untrusted":   {Disabled: slices.Clone(optout.Known), Err: errors.New("owned by uid 1000")},
	} {
		setup := newRebootSetup(t)
		disabler := &fakeDisabler{changed: true}
		daemon := startWithDisabler(t, setup, &state, disabler)
		tick(t, daemon)
		data := disableCommand(t, setup, daemon, "d1", `{"capability":"upgrade_packages"}`)
		if data.Code != DisableCodeAlreadyDisabled || disabler.count() != 0 {
			t.Fatalf("%s: data = %+v, helper called %d times", name, data, disabler.count())
		}
	}
}

func TestDisableCapabilityReportsHelperFailures(t *testing.T) {
	setup := newRebootSetup(t)
	state := optout.State{Disabled: []string{}}
	daemon := startWithDisabler(t, setup, &state, &fakeDisabler{err: errors.New("the root helper does not answer")})
	tick(t, daemon)
	data := disableCommand(t, setup, daemon, "d1", `{"capability":"reboot"}`)
	results := setup.server.resultsOf("d1")
	if data.Code != DisableCodeFailed || results[0].Status != CommandFailed || *results[0].Detail != "the root helper does not answer" {
		t.Fatalf("data = %+v, results = %+v", data, results)
	}
}

func TestDisableCapabilityArgumentsAreValidatedStrictly(t *testing.T) {
	setup := newRebootSetup(t)
	state := optout.State{Disabled: []string{}}
	disabler := &fakeDisabler{changed: true}
	daemon := startWithDisabler(t, setup, &state, disabler)
	tick(t, daemon)
	for index, args := range []string{`{}`, `{"capability":"update_agent"}`, `{"capability":"reboot","also":"x"}`, `{"capability":1}`} {
		id := "bad" + string(rune('a'+index))
		setup.server.addCommand(id, CommandDisableCapability, args)
		daemon.heartbeatAt = setup.clock.Now()
		tick(t, daemon)
		results := setup.server.resultsOf(id)
		if len(results) != 1 || results[0].Status != CommandFailed || !strings.HasPrefix(*results[0].Detail, "invalid args:") || results[0].Data != nil {
			t.Fatalf("%s: results = %+v", args, results)
		}
	}
	if disabler.count() != 0 {
		t.Fatal("the helper was asked for an invalid capability")
	}
}
