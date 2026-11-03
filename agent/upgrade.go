package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"go.chief.tools/serverchief-agent/optout"
	"go.chief.tools/serverchief-agent/updater"
	"go.chief.tools/serverchief-agent/upgrader"
)

const (
	// upgradeLockPatience is how long upgrade_packages waits for another
	// package manager to finish before it hands over.
	upgradeLockPatience = time.Hour
	// upgradeLockRetry is how often the locks are inspected meanwhile.
	upgradeLockRetry = 30 * time.Second
	// upgradeLaunchTimeout bounds starting the detached helper.
	upgradeLaunchTimeout = 2 * time.Minute
)

// upgradeWork is the daemon's side of a package upgrade: a command waiting
// for the package manager, and polling for the helper's result while one
// is handed off.
type upgradeWork struct {
	waiting *commandRecord
	// deadline is the waiting upgrade's not_after, zero for none.
	deadline  time.Time
	waitUntil time.Time
	checkAt   time.Time
	// active makes the loop poll for the result.
	active bool
	pollAt time.Time
	// resumeDue checks once after the start that a pending request has a
	// helper.
	resumeDue bool
}

// upgradeArgs are the arguments of upgrade_packages. Unknown arguments are
// rejected; restart_services is required.
type upgradeArgs struct {
	Scope           *string `json:"scope"`
	RestartServices *bool   `json:"restart_services"`
	AgentVersion    *string `json:"agent_version"`
	// NotAfter is the deadline for starting, see parseDeadline.
	NotAfter json.RawMessage `json:"not_after"`
}

// upgradeDisabledDetail is the detail of an upgrade the operator turned off.
const upgradeDisabledDetail = "installing package updates is turned off on this server; `serverchief enable upgrade_packages` turns it on"

// upgradeCapability is the heartbeat's capabilities.upgrade_packages, nil
// when the operator turned it off.
func (daemon *daemon) upgradeCapability(ctx context.Context, state optout.State) *ActionCapability {
	if state.IsDisabled(optout.UpgradePackages) {
		return nil
	}
	capability := upgrader.Capability{Reason: "installing package updates is not supported by this build"}
	if controller := daemon.options.Upgrades; controller != nil {
		capability = controller.Capability(ctx)
	}
	result := &ActionCapability{Available: capability.Available}
	if !capability.Available {
		reason := capability.Reason
		result.Reason = &reason
	}
	return result
}

func upgradeData(code, scope string) json.RawMessage {
	encoded, _ := json.Marshal(UpgradePackagesData{Code: code, Scope: scope})
	return encoded
}

func (daemon *daemon) failUpgrade(id, code, scope, detail string) {
	daemon.finishCommandData(id, CommandFailed, detail, upgradeData(code, scope))
}

// parseUpgradeArgs validates upgrade_packages' arguments strictly, and
// returns the not_after deadline (zero for none) apart from the request the
// helper gets: it only applies until the hand-over.
func parseUpgradeArgs(raw json.RawMessage) (upgrader.Request, time.Time, error) {
	request, args, err := decodeUpgradeArgs(raw)
	if err != nil {
		return upgrader.Request{}, time.Time{}, err
	}
	deadline, err := parseDeadline(args.NotAfter)
	if err != nil {
		return upgrader.Request{}, time.Time{}, err
	}
	return request, deadline, nil
}

func decodeUpgradeArgs(raw json.RawMessage) (upgrader.Request, upgradeArgs, error) {
	var args upgradeArgs
	decoder := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return upgrader.Request{}, args, err
	}
	switch {
	case args.Scope == nil || !upgrader.ValidScope(*args.Scope):
		return upgrader.Request{}, args, errors.New(`scope must be "security" or "all"`)
	case args.RestartServices == nil:
		return upgrader.Request{}, args, errors.New("restart_services is required")
	case args.AgentVersion != nil && !updater.ValidVersion(*args.AgentVersion):
		return upgrader.Request{}, args, errors.New("agent_version must be null or a stable semantic version (X.Y.Z)")
	}
	request := upgrader.Request{Scope: *args.Scope, RestartServices: *args.RestartServices}
	if args.AgentVersion != nil {
		request.AgentVersion = *args.AgentVersion
	}
	return request, args, nil
}

// startUpgrade executes upgrade_packages: it validates the arguments,
// checks the opt-out and the capability, then hands over once no package
// manager runs, waiting up to an hour.
func (daemon *daemon) startUpgrade(record commandRecord) {
	request, deadline, err := parseUpgradeArgs(record.Args)
	if err != nil {
		daemon.finishCommand(record.ID, CommandFailed, fmt.Sprintf("invalid args: %v", err))
		return
	}
	if passed(deadline, daemon.options.Now()) {
		daemon.failUpgrade(record.ID, upgrader.CodeTooLate, request.Scope, tooLateDetail(deadline))
		return
	}
	if daemon.optOut().IsDisabled(optout.UpgradePackages) {
		daemon.failUpgrade(record.ID, upgrader.CodeDisabled, request.Scope, upgradeDisabledDetail)
		return
	}
	controller := daemon.options.Upgrades
	if controller == nil {
		daemon.failUpgrade(record.ID, upgrader.CodeUnsupported, request.Scope, "installing package updates is not supported by this build")
		return
	}
	if capability := controller.Capability(context.Background()); !capability.Available {
		daemon.failUpgrade(record.ID, upgrader.CodeUnsupported, request.Scope, capability.Reason)
		return
	}
	if other := daemon.upgradeInProgress(record.ID); other != "" {
		daemon.failUpgrade(record.ID, upgrader.CodeUpgradeFailed, request.Scope, "another package upgrade ("+other+") is in progress")
		return
	}
	now := daemon.options.Now()
	if lock, held := daemon.packageManagerBusy(); held {
		log.Printf("[commands] %s %s: waiting for the package manager (%s) before upgrading", record.Kind, record.ID, lock)
		daemon.upgrade.waiting = &record
		daemon.upgrade.deadline = deadline
		daemon.upgrade.waitUntil = waitUntil(now.Add(upgradeLockPatience), deadline)
		daemon.upgrade.checkAt = now.Add(upgradeLockRetry)
		return
	}
	daemon.upgradeNow(record, request)
}

// upgradeInProgress returns another upgrade that waits for the package
// manager, was handed to the helper or whose request is pending, or "".
func (daemon *daemon) upgradeInProgress(except string) string {
	if waiting := daemon.upgrade.waiting; waiting != nil && waiting.ID != except {
		return waiting.ID
	}
	for _, record := range daemon.commands.snapshot() {
		if record.Kind == CommandUpgradePackages && record.State == commandHandedOff && record.ID != except {
			return record.ID
		}
	}
	if controller := daemon.options.Upgrades; controller != nil {
		if pending, err := controller.PendingRequest(); err == nil && pending != nil && pending.CommandID != except {
			return pending.CommandID
		}
	}
	return ""
}

// upgradeNow journals the hand-over and starts the helper. From then on
// only the helper's result finishes the command.
func (daemon *daemon) upgradeNow(record commandRecord, request upgrader.Request) {
	if daemon.optOut().IsDisabled(optout.UpgradePackages) {
		daemon.failUpgrade(record.ID, upgrader.CodeDisabled, request.Scope, upgradeDisabledDetail)
		return
	}
	now := daemon.options.Now()
	// Journal the hand-over before the request exists: a crash in between
	// leaves a handed-off command without a request, which is reported
	// failed.
	record.State = commandHandedOff
	record.UpdatedAt = now.UTC()
	if err := daemon.commands.put(record); err != nil {
		daemon.failUpgrade(record.ID, upgrader.CodeUpgradeFailed, request.Scope, "journal the upgrade: "+err.Error())
		return
	}
	request.CommandID = record.ID
	request.RequestedAt = now.UTC().Truncate(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), upgradeLaunchTimeout)
	defer cancel()
	if err := daemon.options.Upgrades.Start(ctx, request); err != nil {
		daemon.failUpgrade(record.ID, upgrader.CodeUpgradeFailed, request.Scope, "start the package upgrade helper: "+err.Error())
		return
	}
	log.Printf("[commands] %s %s: handed the %s package upgrade to the helper", record.Kind, record.ID, request.Scope)
	daemon.upgrade.active = true
	daemon.upgrade.pollAt = now.Add(daemon.updatePoll())
}

// tickUpgrade retries an upgrade waiting for the package manager and polls
// for the helper's result.
func (daemon *daemon) tickUpgrade(ctx context.Context, now time.Time) {
	work := &daemon.upgrade
	if work.waiting != nil && !now.Before(work.checkAt) {
		record := *work.waiting
		request, _, _ := parseUpgradeArgs(record.Args)
		if passed(work.deadline, now) {
			work.waiting = nil
			daemon.failUpgrade(record.ID, upgrader.CodeTooLate, request.Scope, tooLateDetail(work.deadline))
		} else if lock, held := daemon.packageManagerBusy(); held {
			if !now.Before(work.waitUntil) {
				work.waiting = nil
				daemon.failUpgrade(record.ID, upgrader.CodePackageManagerBusy, request.Scope,
					fmt.Sprintf("a package manager held its lock (%s) for %s", lock, upgradeLockPatience))
			} else {
				work.checkAt = now.Add(upgradeLockRetry)
			}
		} else {
			work.waiting = nil
			daemon.upgradeNow(record, request)
		}
	}
	daemon.pollUpgrade(ctx, now)
}

// upgradeWakeAt is when tickUpgrade has work next, zero for none.
func (daemon *daemon) upgradeWakeAt() time.Time {
	work := &daemon.upgrade
	var at time.Time
	if work.waiting != nil {
		at = work.checkAt
	}
	if work.active && daemon.options.Upgrades != nil && (at.IsZero() || work.pollAt.Before(at)) {
		at = work.pollAt
	}
	return at
}

// pollUpgrade reports the helper's result, reports handed-off commands
// whose helper vanished, and relaunches a helper once after a start. The
// result goes into the journal, then its files go.
func (daemon *daemon) pollUpgrade(ctx context.Context, now time.Time) {
	controller := daemon.options.Upgrades
	work := &daemon.upgrade
	if controller == nil || !work.active || now.Before(work.pollAt) {
		return
	}
	work.pollAt = now.Add(daemon.updatePoll())
	if work.resumeDue {
		work.resumeDue = false
		if launched, err := controller.Resume(ctx); err != nil {
			log.Printf("[upgrade] relaunch the package upgrade helper: %v", err)
		} else if launched {
			log.Printf("[upgrade] relaunched the package upgrade helper")
		}
	}

	// The helper writes the result before it removes the request, so
	// reading in this order never sees neither for a live upgrade.
	request, requestErr := controller.PendingRequest()
	result, output, resultErr := controller.ReadResult()
	if requestErr != nil {
		log.Printf("[upgrade] %v", requestErr)
	}
	if resultErr != nil {
		log.Printf("[upgrade] %v", resultErr)
	}
	if result != nil {
		daemon.finishUpgrade(*result, output)
	}

	handedOff := 0
	for _, record := range daemon.commands.snapshot() {
		if record.Kind != CommandUpgradePackages || record.State != commandHandedOff {
			continue
		}
		if request != nil && request.CommandID == record.ID || requestErr != nil || resultErr != nil {
			handedOff++
			continue
		}
		scope := ""
		if parsed, _, err := parseUpgradeArgs(record.Args); err == nil {
			scope = parsed.Scope
		}
		log.Printf("[upgrade] %s has neither a request nor a result; reporting it failed", record.ID)
		daemon.failUpgrade(record.ID, upgrader.CodeUpgradeFailed, scope, "the package upgrade helper stopped without a result")
	}
	work.active = request != nil || handedOff > 0
}

// finishUpgrade turns the helper's result into the command's result,
// removes the helper's files and collects packages and updates now.
func (daemon *daemon) finishUpgrade(result upgrader.Result, output string) {
	record := daemon.commands.get(result.CommandID)
	switch {
	case record == nil:
		// The journal lost it; report it anyway.
		if err := daemon.commands.put(commandRecord{ID: result.CommandID, Kind: CommandUpgradePackages, State: commandHandedOff, UpdatedAt: daemon.options.Now().UTC()}); err != nil {
			log.Printf("[commands] %v", err)
		}
	case record.State == commandFinished:
		// Journaled already; only the files are left.
		if err := daemon.options.Upgrades.RemoveResult(); err != nil {
			log.Printf("[upgrade] %v", err)
		}
		return
	}
	data, _ := json.Marshal(result.Data)
	status := CommandFailed
	if result.Status == CommandSucceeded {
		status = CommandSucceeded
	}
	daemon.finishCommandOutput(result.CommandID, status, result.Detail, data, output, false)
	if err := daemon.options.Upgrades.RemoveResult(); err != nil {
		log.Printf("[upgrade] %v", err)
	}
	if daemon.options.Updates != nil {
		daemon.options.Updates.Invalidate()
	}
	now := daemon.options.Now()
	for _, name := range []string{"packages", "updates"} {
		if _, scheduled := daemon.next[name]; scheduled {
			daemon.next[name] = now
		}
	}
}
