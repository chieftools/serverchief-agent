package agent

import (
	"context"
	"sync"
	"time"

	"go.chief.tools/serverchief-agent/helper"
)

// Privilege modes the heartbeat reports.
const (
	// PrivilegeUnprivileged is the serverchief user with only
	// CAP_DAC_READ_SEARCH, plus the root helper's fixed actions.
	PrivilegeUnprivileged = "unprivileged"
	// PrivilegeRoot is a daemon running as root, where the init system
	// cannot grant ambient capabilities (systemd before 229, SysV).
	PrivilegeRoot = "root"
)

const (
	helperAnswerFor   = time.Hour
	helperSilenceFor  = 5 * time.Minute
	helperPingTimeout = 15 * time.Second
)

// PrivilegeCapability is how the agent runs, reported with every heartbeat
// so the backend notices an escalation.
type PrivilegeCapability struct {
	Mode string `json:"mode"`
	// HelperActions are the root actions available: the root helper's
	// when unprivileged and it answers, every action in-process as root.
	HelperActions []string `json:"helper_actions"`
	// Reason tells why the agent runs as root, or why the helper is
	// unavailable.
	Reason *string `json:"reason"`
}

// PrivilegeProbe reports the privilege mode, pinging the root helper of an
// unprivileged daemon at most once an hour while it answers.
type PrivilegeProbe struct {
	// Unprivileged is true when the daemon does not run as root.
	Unprivileged bool
	// Runner reaches the root helper (Unprivileged) or runs in-process.
	Runner helper.Runner
	// Actions are the helper's action names.
	Actions []string
	// RootReason explains a root daemon.
	RootReason string
	Now        func() time.Time

	mutex     sync.Mutex
	checkedAt time.Time
	answers   bool
	failure   string
}

// Capability returns the current privilege report.
func (probe *PrivilegeProbe) Capability(ctx context.Context) PrivilegeCapability {
	if !probe.Unprivileged {
		reason := probe.RootReason
		return PrivilegeCapability{Mode: PrivilegeRoot, HelperActions: probe.Actions, Reason: &reason}
	}
	if probe.HelperAnswers(ctx) {
		return PrivilegeCapability{Mode: PrivilegeUnprivileged, HelperActions: probe.Actions}
	}
	probe.mutex.Lock()
	reason := "the root helper does not answer: " + probe.failure
	probe.mutex.Unlock()
	return PrivilegeCapability{Mode: PrivilegeUnprivileged, HelperActions: []string{}, Reason: &reason}
}

// HelperAnswers pings the root helper, cached. A root daemon always has
// its actions.
func (probe *PrivilegeProbe) HelperAnswers(ctx context.Context) bool {
	if !probe.Unprivileged {
		return true
	}
	probe.mutex.Lock()
	defer probe.mutex.Unlock()
	now := probe.now()
	ttl := helperSilenceFor
	if probe.answers {
		ttl = helperAnswerFor
	}
	if !probe.checkedAt.IsZero() && now.Sub(probe.checkedAt) < ttl {
		return probe.answers
	}
	pingContext, cancel := context.WithTimeout(ctx, helperPingTimeout)
	defer cancel()
	_, err := probe.Runner.Do(pingContext, helper.ActionPing, nil)
	probe.checkedAt = now
	probe.answers = err == nil
	probe.failure = ""
	if err != nil {
		probe.failure = err.Error()
	}
	return probe.answers
}

func (probe *PrivilegeProbe) now() time.Time {
	if probe.Now != nil {
		return probe.Now()
	}
	return time.Now()
}
