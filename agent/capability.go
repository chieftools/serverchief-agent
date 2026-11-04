package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"go.chief.tools/serverchief-agent/optout"
)

// disableCapabilityTimeout bounds the root helper's disable-capability.
const disableCapabilityTimeout = time.Minute

// disableCapabilityArgs are the arguments of disable_capability. Unknown
// arguments are rejected.
type disableCapabilityArgs struct {
	Capability *string `json:"capability"`
}

// parseDisableCapabilityArgs validates disable_capability's arguments.
func parseDisableCapabilityArgs(raw json.RawMessage) (string, error) {
	var args disableCapabilityArgs
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return "", err
	}
	if args.Capability == nil || !slices.Contains(optout.Known, *args.Capability) {
		return "", errors.New("capability must be one of " + fmt.Sprint(optout.Known))
	}
	return *args.Capability, nil
}

func disableCapabilityData(code, capability string) json.RawMessage {
	encoded, _ := json.Marshal(DisableCapabilityData{Code: code, Capability: capability})
	return encoded
}

// disableCapability executes disable_capability: the panel turns a remote
// action off on the server. A capability that is already off, or an
// opt-out that cannot be trusted (which turns every capability off), is
// left alone; otherwise the root helper adds it. Only `serverchief
// enable` on the server turns it on again. A heartbeat goes out right
// away so the panel sees the change.
func (daemon *daemon) disableCapability(record commandRecord) {
	name, err := parseDisableCapabilityArgs(record.Args)
	if err != nil {
		daemon.finishCommand(record.ID, CommandFailed, fmt.Sprintf("invalid args: %v", err))
		return
	}
	if daemon.optOut().IsDisabled(name) {
		daemon.finishCommandData(record.ID, CommandSucceeded, name+" is already turned off on this server", disableCapabilityData(DisableCodeAlreadyDisabled, name))
		return
	}
	if daemon.options.DisableCapability == nil {
		daemon.finishCommandData(record.ID, CommandFailed, "turning capabilities off is not supported by this build", disableCapabilityData(DisableCodeFailed, name))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), disableCapabilityTimeout)
	defer cancel()
	changed, err := daemon.options.DisableCapability(ctx, name)
	if err != nil {
		daemon.finishCommandData(record.ID, CommandFailed, err.Error(), disableCapabilityData(DisableCodeFailed, name))
		return
	}
	code, detail := DisableCodeDisabled, "turned "+name+" off on this server; `serverchief enable "+name+"` turns it on again"
	if !changed {
		code, detail = DisableCodeAlreadyDisabled, name+" is already turned off on this server"
	}
	log.Printf("[commands] %s %s: %s", record.Kind, record.ID, detail)
	// Re-read the opt-out and tell the backend now, not at the next
	// heartbeat.
	daemon.optOut()
	daemon.heartbeatAt = daemon.options.Now()
	daemon.finishCommandData(record.ID, CommandSucceeded, detail, disableCapabilityData(code, name))
}
