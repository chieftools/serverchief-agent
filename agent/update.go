package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"go.chief.tools/serverchief-agent/updater"
)

// defaultUpdatePoll is how often the daemon looks for the helper's result
// while an update is in progress.
const defaultUpdatePoll = 5 * time.Second

// updateWork is the daemon's side of an agent update.
type updateWork struct {
	// readyFor is the request that was pending when this daemon started:
	// its readiness marker is written after the first successful
	// heartbeat. A daemon that hands a request over never writes one.
	readyFor  string
	readyDone bool
	// active makes the loop poll for the result; inProgress is set while
	// a request or a handed-off command is pending.
	active     bool
	inProgress bool
	pollAt     time.Time
	// resumeDue checks once after the start that a pending request has
	// a helper.
	resumeDue bool
}

// updateArgs are the arguments of update_agent. Nothing but the version
// is accepted.
type updateArgs struct {
	Version string `json:"version"`
}

func (daemon *daemon) updatePoll() time.Duration {
	if daemon.options.UpdatePoll > 0 {
		return daemon.options.UpdatePoll
	}
	return defaultUpdatePoll
}

// initUpdates looks for an update in progress at start.
func (daemon *daemon) initUpdates(now time.Time) {
	controller := daemon.options.Updates
	if controller == nil {
		return
	}
	// Poll once right away: a result may be waiting from before.
	daemon.update.active, daemon.update.pollAt = true, now
	request, err := controller.PendingRequest()
	if err != nil {
		log.Printf("[update] %v", err)
		return
	}
	if request != nil {
		log.Printf("[update] update %s (%s → %s) is in progress; reporting ready after the first heartbeat",
			request.CommandID, request.PreviousVersion, request.TargetVersion)
		daemon.update.readyFor = request.CommandID
		daemon.update.inProgress = true
		daemon.update.resumeDue = true
	}
}

// capabilities is the heartbeat's capabilities object.
func (daemon *daemon) capabilities(ctx context.Context) *Capabilities {
	capability := updater.Capability{Reason: "agent updates are not supported by this build"}
	if controller := daemon.options.Updates; controller != nil {
		capability = controller.Capability(ctx, false)
	}
	result := &Capabilities{AgentUpdate: AgentUpdateCapability{Available: capability.Available}}
	if !capability.Available {
		reason := capability.Reason
		result.AgentUpdate.Reason = &reason
	}
	if probe := daemon.options.Privilege; probe != nil {
		privilege := probe.Capability(ctx)
		result.Privilege = &privilege
	}
	if metrics := daemon.options.Metrics; metrics != nil {
		status := metrics.Status()
		result.Metrics = &status
	}
	return result
}

// markReady writes the readiness marker after a successful heartbeat.
func (daemon *daemon) markReady() {
	work := &daemon.update
	if work.readyFor == "" || work.readyDone {
		return
	}
	if err := daemon.options.Updates.WriteReadiness(work.readyFor); err != nil {
		log.Printf("[update] write the readiness marker: %v", err)
		return
	}
	work.readyDone = true
	log.Printf("[update] reported ready for update %s", work.readyFor)
}

// startUpdate executes update_agent: it validates the version and the
// capability, journals the command as handed off and starts the helper.
// From then on only the helper's result finishes the command, and a
// restart never starts it again.
func (daemon *daemon) startUpdate(record commandRecord) {
	running := normalizeVersion(daemon.options.Version)
	var args updateArgs
	decoder := json.NewDecoder(bytes.NewReader(record.Args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		daemon.finishCommand(record.ID, CommandFailed, fmt.Sprintf("invalid args: %v", err))
		return
	}
	if !updater.ValidVersion(args.Version) {
		daemon.finishCommand(record.ID, CommandFailed, "invalid args: version must be a stable semantic version (X.Y.Z)")
		return
	}
	target := args.Version
	fail := func(code, detail string) {
		daemon.finishCommandData(record.ID, CommandFailed, detail, updateData(code, running, target, running))
	}
	controller := daemon.options.Updates
	if controller == nil {
		fail(updater.CodeUnsupportedInstall, "agent updates are not supported by this build")
		return
	}
	// One update at a time; package_manager_busy tells the backend to try
	// again later.
	pending, err := controller.PendingRequest()
	if err != nil {
		fail(updater.CodePackageManagerBusy, "an update request is pending but unreadable: "+err.Error())
		return
	}
	other := daemon.handedOff(record.ID)
	if pending != nil {
		other = pending.CommandID
	}
	if other != "" {
		fail(updater.CodePackageManagerBusy, "another agent update ("+other+") is in progress")
		return
	}
	ctx := context.Background()
	capability := controller.Capability(ctx, true)
	if !capability.Available {
		fail(updater.CodeUnsupportedInstall, capability.Reason)
		return
	}
	if target == running {
		daemon.finishCommandData(record.ID, CommandSucceeded, "already running "+running, updateData(updater.CodeUpdated, running, target, running))
		return
	}

	// Journal the hand-over before the request exists: a crash in between
	// leaves a handed-off command without a request, which is reported
	// failed, never a request whose command is reported "interrupted".
	now := daemon.options.Now()
	record.State = commandHandedOff
	record.UpdatedAt = now.UTC()
	if err := daemon.commands.put(record); err != nil {
		fail(updater.CodeInstallFailed, "journal the update: "+err.Error())
		return
	}
	request := updater.Request{CommandID: record.ID, PreviousVersion: running, TargetVersion: target, RequestedAt: now.UTC().Truncate(time.Second)}
	if err := controller.Start(ctx, request); err != nil {
		fail(updater.CodeInstallFailed, "start the update helper: "+err.Error())
		return
	}
	log.Printf("[commands] %s %s: handed the update %s → %s to the helper", record.Kind, record.ID, running, target)
	daemon.update.active, daemon.update.inProgress = true, true
	daemon.update.pollAt = now.Add(daemon.updatePoll())
}

// handedOff returns another handed-off command's ID, or "".
func (daemon *daemon) handedOff(except string) string {
	for _, record := range daemon.commands.snapshot() {
		if record.State == commandHandedOff && record.ID != except {
			return record.ID
		}
	}
	return ""
}

// pollUpdate looks for the helper's result, reports handed-off commands
// whose helper vanished, and relaunches a helper once after a start.
func (daemon *daemon) pollUpdate(ctx context.Context, now time.Time) {
	controller := daemon.options.Updates
	work := &daemon.update
	if controller == nil || !work.active || now.Before(work.pollAt) {
		return
	}
	work.pollAt = now.Add(daemon.updatePoll())
	if work.resumeDue {
		work.resumeDue = false
		if launched, err := controller.Resume(ctx); err != nil {
			log.Printf("[update] relaunch the update helper: %v", err)
		} else if launched {
			log.Printf("[update] relaunched the update helper")
		}
	}

	// The helper writes the result before it removes the request, so
	// reading in this order never sees neither for a live update.
	request, requestErr := controller.PendingRequest()
	result, resultErr := controller.ReadResult()
	if requestErr != nil {
		log.Printf("[update] %v", requestErr)
	}
	if resultErr != nil {
		log.Printf("[update] %v", resultErr)
	}
	lingering := false
	if result != nil {
		record := daemon.commands.get(result.CommandID)
		switch {
		case record != nil && record.State == commandFinished && record.Reported:
			if err := controller.RemoveResult(result.CommandID); err != nil {
				log.Printf("[update] %v", err)
			}
		case record != nil && record.State == commandFinished:
			// sendResults removes it once reported.
			lingering = true
		default:
			daemon.finishUpdate(*result)
			lingering = true
		}
	}

	handedOff := 0
	for _, record := range daemon.commands.snapshot() {
		if record.State != commandHandedOff {
			continue
		}
		if request != nil && request.CommandID == record.ID || result != nil && result.CommandID == record.ID {
			handedOff++
			continue
		}
		if requestErr != nil || resultErr != nil {
			handedOff++
			continue
		}
		var args updateArgs
		_ = json.Unmarshal(record.Args, &args)
		running := normalizeVersion(daemon.options.Version)
		log.Printf("[update] %s has neither a request nor a result; reporting it failed", record.ID)
		daemon.finishCommandData(record.ID, CommandFailed, "the update helper stopped without a result",
			updateData(updater.CodeInstallFailed, running, args.Version, running))
	}
	work.inProgress = request != nil || handedOff > 0
	work.active = work.inProgress || lingering
}

// finishUpdate turns the helper's result into the command's result.
func (daemon *daemon) finishUpdate(result updater.Result) {
	if record := daemon.commands.get(result.CommandID); record == nil {
		// The journal lost it; report it anyway.
		if err := daemon.commands.put(commandRecord{ID: result.CommandID, Kind: CommandUpdateAgent, State: commandHandedOff, UpdatedAt: daemon.options.Now().UTC()}); err != nil {
			log.Printf("[commands] %v", err)
		}
	}
	status := CommandFailed
	if result.Succeeded() {
		status = CommandSucceeded
	}
	data := updateData(result.Code, result.PreviousVersion, result.TargetVersion, result.InstalledVersion)
	daemon.finishCommandData(result.CommandID, status, result.Detail, data)
	daemon.options.Updates.Invalidate()
}

func updateData(code, previous, target, installed string) json.RawMessage {
	encoded, _ := json.Marshal(AgentUpdateData{Code: code, PreviousVersion: previous, TargetVersion: target, InstalledVersion: installed})
	return encoded
}
