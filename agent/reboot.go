package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"go.chief.tools/serverchief-agent/execlog"
	"go.chief.tools/serverchief-agent/inventory"
	"go.chief.tools/serverchief-agent/optout"
)

const (
	// rebootLockPatience is how long reboot waits for a package manager
	// to finish; a reboot never interrupts one.
	rebootLockPatience = 15 * time.Minute
	// rebootLockRetry is how often the locks are inspected meanwhile.
	rebootLockRetry = 30 * time.Second
	// rebootVerifyAfter is how long after the request the boot ID must
	// have changed. shutdown -r +1 reboots after a minute; a cancelled
	// shutdown (shutdown -c) never does.
	rebootVerifyAfter = 10 * time.Minute
	// rebootHelperTimeout bounds the root helper's reboot action, which
	// only schedules the reboot.
	rebootHelperTimeout = time.Minute
	// rebootOutputPatience is how long the output of scheduling a reboot
	// is sent again after a failure: the server goes down a minute after.
	rebootOutputPatience = time.Minute
)

// Reboots is the daemon's side of the reboot command: the capability, the
// root helper's reboot action, and the boot ID that tells whether the
// server rebooted.
type Reboots struct {
	// RequestPath is reboot-request.json in the state directory.
	RequestPath string
	// Detect probes whether this server can reboot, regardless of the
	// operator's opt-out.
	Detect func(ctx context.Context) ActionCapability
	// Reboot runs the root helper's reboot action and returns what it
	// ran and printed (execlog). It returns an error when the reboot
	// could not be scheduled.
	Reboot func(ctx context.Context) (string, error)
	// BootID reads /proc/sys/kernel/random/boot_id.
	BootID func() (string, error)
	// BootTime is the kernel boot time, nil when unknown.
	BootTime func() *time.Time
	// Required tells whether the host needs a reboot, for if_required;
	// nil when it cannot tell.
	Required func(ctx context.Context) *bool
}

// rebootRequest is reboot-request.json: the reboot in progress, kept
// across the reboot so the next start can tell whether it happened and
// report the output of scheduling it.
type rebootRequest struct {
	CommandID       string     `json:"command_id"`
	BootID          string     `json:"boot_id"`
	BootTime        *time.Time `json:"boot_time"`
	RequestedAt     time.Time  `json:"requested_at"`
	Output          string     `json:"output,omitempty"`
	OutputTruncated bool       `json:"output_truncated,omitempty"`
}

// rebootWork is a reboot waiting for the package manager, and when to
// check next whether a handed-off reboot happened.
type rebootWork struct {
	waiting *commandRecord
	// deadline is the waiting reboot's not_after, zero for none.
	deadline  time.Time
	waitUntil time.Time
	checkAt   time.Time
	verifyAt  time.Time
	// output is the output of a scheduled reboot that still goes to the
	// backend, nil for none.
	output *rebootOutput
}

// rebootOutput is the output of scheduling a reboot, sent as live output
// right away since the result only follows after the reboot (protocol:
// Live output).
type rebootOutput struct {
	commandID string
	text      []byte
	offset    int64
	at        time.Time
	until     time.Time
	backoff   time.Duration
}

// optOut reads the operator's opt-out; without one nothing is turned off.
// A file that cannot be trusted is logged whenever its error changes.
func (daemon *daemon) optOut() optout.State {
	if daemon.options.OptOut == nil {
		return optout.State{Disabled: []string{}}
	}
	state := daemon.options.OptOut()
	message := ""
	if state.Err != nil {
		message = state.Err.Error()
	}
	if message != daemon.optOutError {
		daemon.optOutError = message
		if message != "" {
			log.Printf("[optout] %s; treating every remote action as turned off", message)
		} else {
			log.Printf("[optout] the opt-out file is readable again")
		}
	}
	if state.Disabled == nil {
		state.Disabled = []string{}
	}
	return state
}

// rebootCapability is the heartbeat's capabilities.reboot, nil when the
// operator turned it off.
func (daemon *daemon) rebootCapability(ctx context.Context, state optout.State) *ActionCapability {
	if state.IsDisabled(optout.Reboot) {
		return nil
	}
	if control := daemon.options.Reboots; control != nil {
		capability := control.Detect(ctx)
		return &capability
	}
	reason := "reboots are not supported by this build"
	return &ActionCapability{Reason: &reason}
}

func (daemon *daemon) bootTime() *time.Time {
	if control := daemon.options.Reboots; control != nil && control.BootTime != nil {
		if at := control.BootTime(); at != nil {
			utc := at.UTC().Truncate(time.Second)
			return &utc
		}
	}
	return nil
}

func rebootData(code string, previous, current *time.Time) json.RawMessage {
	encoded, _ := json.Marshal(RebootData{Code: code, PreviousBootTime: previous, BootTime: current})
	return encoded
}

// failReboot reports a reboot that did not happen, with the current boot
// time as both boot times.
func (daemon *daemon) failReboot(id, code, detail, output string, truncated bool) {
	bootTime := daemon.bootTime()
	daemon.finishCommandOutput(id, CommandFailed, detail, rebootData(code, bootTime, bootTime), output, truncated)
}

// rebootDisabledDetail is the detail of a reboot the operator turned off.
const rebootDisabledDetail = "rebooting is turned off on this server; `serverchief enable reboot` turns it on"

// startReboot executes reboot: it checks the opt-out and the capability,
// then reboots once no package manager runs, waiting up to 15 minutes.
func (daemon *daemon) startReboot(record commandRecord) {
	options, err := parseRebootArgs(record.Args)
	deadline := options.deadline
	if err != nil {
		daemon.finishCommand(record.ID, CommandFailed, fmt.Sprintf("invalid args: %v", err))
		return
	}
	if passed(deadline, daemon.options.Now()) {
		daemon.failReboot(record.ID, RebootCodeTooLate, tooLateDetail(deadline), "", false)
		return
	}
	if daemon.optOut().IsDisabled(optout.Reboot) {
		daemon.failReboot(record.ID, RebootCodeDisabled, rebootDisabledDetail, "", false)
		return
	}
	control := daemon.options.Reboots
	if control == nil {
		daemon.failReboot(record.ID, RebootCodeUnsupported, "reboots are not supported by this build", "", false)
		return
	}
	if capability := control.Detect(context.Background()); !capability.Available {
		reason := "this server cannot be rebooted"
		if capability.Reason != nil {
			reason = *capability.Reason
		}
		daemon.failReboot(record.ID, RebootCodeUnsupported, reason, "", false)
		return
	}
	if other := daemon.rebootInProgress(record.ID); other != "" {
		daemon.failReboot(record.ID, RebootCodeFailed, "another reboot ("+other+") is already in progress", "", false)
		return
	}
	now := daemon.options.Now()
	if lock, held := daemon.rebootBlocked(); held {
		log.Printf("[commands] %s %s: waiting for the package manager (%s) before rebooting", record.Kind, record.ID, lock)
		daemon.reboot.waiting = &record
		daemon.reboot.deadline = deadline
		daemon.reboot.waitUntil = waitUntil(now.Add(rebootLockPatience), deadline)
		daemon.reboot.checkAt = now.Add(rebootLockRetry)
		return
	}
	daemon.rebootNow(record)
}

// rebootInProgress returns another reboot that waits for the package
// manager or was handed to the root helper, or "".
func (daemon *daemon) rebootInProgress(except string) string {
	if waiting := daemon.reboot.waiting; waiting != nil && waiting.ID != except {
		return waiting.ID
	}
	for _, record := range daemon.commands.snapshot() {
		if record.Kind == CommandReboot && record.State == commandHandedOff && record.ID != except {
			return record.ID
		}
	}
	return ""
}

// rebootBlocked reports a held package-manager lock or a package upgrade
// in progress, which a reboot never interrupts: between its steps the
// locks are free.
func (daemon *daemon) rebootBlocked() (string, bool) {
	if other := daemon.upgradeInProgress(""); other != "" {
		return "the package upgrade " + other, true
	}
	return daemon.packageManagerBusy()
}

func (daemon *daemon) packageManagerBusy() (string, bool) {
	if daemon.options.Packages == nil {
		return "", false
	}
	lock, _, held := daemon.options.Packages.BusyLock()
	return lock, held
}

// rebootNow persists the request and has the root helper schedule the
// reboot. From then on only a changed boot ID, or ten minutes without one,
// finishes the command. With if_required it first checks, at this moment,
// whether the host needs a reboot at all.
func (daemon *daemon) rebootNow(record commandRecord) {
	if daemon.optOut().IsDisabled(optout.Reboot) {
		daemon.failReboot(record.ID, RebootCodeDisabled, rebootDisabledDetail, "", false)
		return
	}
	control := daemon.options.Reboots
	if options, _ := parseRebootArgs(record.Args); options.ifRequired {
		var required *bool
		if control.Required != nil {
			ctx, cancel := context.WithTimeout(context.Background(), rebootHelperTimeout)
			required = control.Required(ctx)
			cancel()
		}
		switch {
		case required == nil:
			// Rebooting blindly is worse than a run that shows it could not tell.
			daemon.failReboot(record.ID, RebootCodeRequiredUnknown, "it cannot be told whether this server needs a reboot, so it was not rebooted", "", false)
			return
		case !*required:
			bootTime := daemon.bootTime()
			daemon.finishCommandData(record.ID, CommandSucceeded, "the server does not need a reboot", rebootData(RebootCodeNotRequired, bootTime, bootTime))
			return
		}
	}
	bootID, err := control.BootID()
	if err != nil {
		daemon.failReboot(record.ID, RebootCodeFailed, "read the boot ID: "+err.Error(), "", false)
		return
	}
	now := daemon.options.Now()
	// Journal the hand-over before the request exists: a crash in between
	// leaves a handed-off reboot without a request, which is reported
	// failed, and the helper only runs once the request is written.
	record.State = commandHandedOff
	record.UpdatedAt = now.UTC()
	if err := daemon.commands.put(record); err != nil {
		daemon.failReboot(record.ID, RebootCodeFailed, "journal the reboot: "+err.Error(), "", false)
		return
	}
	request := rebootRequest{CommandID: record.ID, BootID: bootID, BootTime: daemon.bootTime(), RequestedAt: now.UTC().Truncate(time.Second)}
	if err := daemon.writeRebootRequest(request); err != nil {
		daemon.failReboot(record.ID, RebootCodeFailed, "write the reboot request: "+err.Error(), "", false)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rebootHelperTimeout)
	defer cancel()
	output, err := control.Reboot(ctx)
	text, truncated := execlog.Cap(output)
	if err != nil {
		daemon.removeRebootRequest()
		daemon.failReboot(record.ID, RebootCodeFailed, "the reboot could not be scheduled: "+err.Error(), text, truncated)
		return
	}
	request.Output, request.OutputTruncated = text, truncated
	if err := daemon.writeRebootRequest(request); err != nil {
		log.Printf("[reboot] keep the output of %s: %v", record.ID, err)
	}
	log.Printf("[commands] %s %s: the server reboots in one minute", record.Kind, record.ID)
	daemon.reboot.verifyAt = request.RequestedAt.Add(rebootVerifyAfter)
	if text != "" {
		at := daemon.options.Now()
		daemon.reboot.output = &rebootOutput{commandID: record.ID, text: []byte(text), at: at, until: at.Add(rebootOutputPatience), backoff: initialBackoff}
	}
	// Heartbeat right away: the backend measures from this contact whether
	// the server came back in time (protocol: Reboots).
	daemon.heartbeatAt = daemon.options.Now()
}

// tickReboot retries a reboot waiting for the package manager and checks
// whether a handed-off reboot happened once it is due.
func (daemon *daemon) tickReboot(now time.Time) {
	work := &daemon.reboot
	if work.waiting != nil && !now.Before(work.checkAt) {
		record := *work.waiting
		if passed(work.deadline, now) {
			work.waiting = nil
			daemon.failReboot(record.ID, RebootCodeTooLate, tooLateDetail(work.deadline), "", false)
		} else if lock, held := daemon.rebootBlocked(); held {
			if !now.Before(work.waitUntil) {
				work.waiting = nil
				daemon.failReboot(record.ID, RebootCodePackageManagerBusy,
					fmt.Sprintf("a package manager held its lock (%s) for %s; a reboot never interrupts one", lock, rebootLockPatience), "", false)
			} else {
				work.checkAt = now.Add(rebootLockRetry)
			}
		} else {
			work.waiting = nil
			daemon.rebootNow(record)
		}
	}
	if !work.verifyAt.IsZero() && !now.Before(work.verifyAt) {
		work.verifyAt = time.Time{}
		daemon.checkReboots(now)
	}
}

// sendRebootOutput sends the output of scheduling a reboot once it is
// due. A failed chunk is sent again until the server goes down; a
// rejection (an older backend) drops it, the result still carries it.
func (daemon *daemon) sendRebootOutput(ctx context.Context, now time.Time) {
	pending := daemon.reboot.output
	if pending == nil || now.Before(pending.at) {
		return
	}
	if !now.Before(pending.until) {
		daemon.reboot.output = nil
		return
	}
	for pending.offset < int64(len(pending.text)) {
		chunk, ok := outputChunk(pending.offset, pending.text[pending.offset:], true)
		if !ok {
			break
		}
		err := daemon.options.Client.ReportCommandOutput(ctx, pending.commandID, chunk)
		if err == nil {
			pending.offset += chunk.Length
			pending.backoff = initialBackoff
			continue
		}
		if ctx.Err() != nil {
			return
		}
		kind, problem := classify(err)
		if problem != nil && problem.Status == http.StatusConflict && problem.ExpectedOffset != nil {
			// Continue where the backend is, on the next tick.
			pending.offset = *problem.ExpectedOffset
			pending.at = now
			return
		}
		wait, _ := daemon.failure("send the output of "+pending.commandID, err, &pending.backoff, upgradeStreamRetry)
		if kind == failureRejected {
			daemon.reboot.output = nil
			return
		}
		pending.at = now.Add(wait)
		return
	}
	daemon.reboot.output = nil
}

// rebootWakeAt is when tickReboot or sendRebootOutput have work next,
// zero for none.
func (daemon *daemon) rebootWakeAt() time.Time {
	work := &daemon.reboot
	at := work.verifyAt
	if work.waiting != nil && (at.IsZero() || work.checkAt.Before(at)) {
		at = work.checkAt
	}
	if work.output != nil && (at.IsZero() || work.output.at.Before(at)) {
		at = work.output.at
	}
	return at
}

// rebootOutcome is the output of a finished reboot: that of scheduling it,
// which went out as live output, and a line on how it ended.
func rebootOutcome(request *rebootRequest, note string) (string, bool) {
	text, truncated := execlog.Cap(request.Output + "# " + note + "\n")
	return text, truncated || request.OutputTruncated
}

// rebootedNote tells when the server booted again, and how long after the
// reboot was scheduled.
func rebootedNote(request *rebootRequest, bootTime *time.Time) string {
	if bootTime == nil {
		return "The server rebooted: its boot ID changed"
	}
	note := "The server rebooted: it booted again at " + formatRebootTime(*bootTime)
	if after := bootTime.Sub(request.RequestedAt); after > 0 {
		note += ", " + after.Round(time.Second).String() + " after the reboot was scheduled"
	}
	return note
}

// notRebootedNote tells which boot the server still runs.
func notRebootedNote(bootTime *time.Time, bootIDErr error) string {
	switch {
	case bootIDErr != nil:
		return "The server did not reboot as far as can be told: its boot ID could not be read"
	case bootTime == nil:
		return "The server did not reboot: its boot ID is unchanged 10 minutes after the reboot was scheduled"
	default:
		return "The server did not reboot: it still runs the boot of " + formatRebootTime(*bootTime) + ", 10 minutes after the reboot was scheduled"
	}
}

func formatRebootTime(at time.Time) string {
	return at.UTC().Format("2006-01-02 15:04:05 UTC")
}

// checkReboots finishes handed-off reboots: a changed boot ID means the
// server rebooted, the same one ten minutes after the request means it did
// not. A younger request is checked again later. The request file goes
// once the result is journaled.
func (daemon *daemon) checkReboots(now time.Time) {
	control := daemon.options.Reboots
	request, requestErr := daemon.readRebootRequest()
	if requestErr != nil {
		log.Printf("[reboot] %v", requestErr)
	}
	if request != nil && daemon.commands.get(request.CommandID) == nil {
		// The journal lost it; report it anyway.
		if err := daemon.commands.put(commandRecord{ID: request.CommandID, Kind: CommandReboot, State: commandHandedOff, UpdatedAt: now.UTC()}); err != nil {
			log.Printf("[commands] %v", err)
		}
	}
	for _, record := range daemon.commands.snapshot() {
		if record.Kind != CommandReboot || record.State != commandHandedOff {
			continue
		}
		if control == nil {
			daemon.failReboot(record.ID, RebootCodeFailed, "reboots are not supported by this build", "", false)
			continue
		}
		if request == nil || request.CommandID != record.ID {
			detail := "the agent stopped before the reboot was scheduled"
			if requestErr != nil {
				detail = "the reboot request is unreadable: " + requestErr.Error()
			}
			daemon.failReboot(record.ID, RebootCodeFailed, detail, "", false)
			continue
		}
		bootID, err := control.BootID()
		switch {
		case err == nil && bootID != request.BootID:
			log.Printf("[reboot] %s: the server rebooted", record.ID)
			bootTime := daemon.bootTime()
			output, truncated := rebootOutcome(request, rebootedNote(request, bootTime))
			daemon.finishCommandOutput(record.ID, CommandSucceeded, "", rebootData(RebootCodeRebooted, request.BootTime, bootTime), output, truncated)
			daemon.removeRebootRequest()
		case !now.Before(request.RequestedAt.Add(rebootVerifyAfter)):
			detail := "the server did not reboot within 10 minutes; someone may have cancelled the shutdown"
			if err != nil {
				detail = "the server did not reboot within 10 minutes as far as the boot ID tells: " + err.Error()
			}
			bootTime := daemon.bootTime()
			output, truncated := rebootOutcome(request, notRebootedNote(bootTime, err))
			daemon.finishCommandOutput(record.ID, CommandFailed, detail, rebootData(RebootCodeNotRebooted, request.BootTime, bootTime), output, truncated)
			daemon.removeRebootRequest()
		default:
			at := request.RequestedAt.Add(rebootVerifyAfter)
			if daemon.reboot.verifyAt.IsZero() || at.Before(daemon.reboot.verifyAt) {
				daemon.reboot.verifyAt = at
			}
		}
	}
	if request != nil {
		if record := daemon.commands.get(request.CommandID); record != nil && record.State == commandFinished {
			daemon.removeRebootRequest()
		}
	}
}

func (daemon *daemon) readRebootRequest() (*rebootRequest, error) {
	control := daemon.options.Reboots
	if control == nil || control.RequestPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(control.RequestPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", control.RequestPath, err)
	}
	var request rebootRequest
	if err := json.Unmarshal(data, &request); err != nil || request.CommandID == "" {
		return nil, fmt.Errorf("decode %s: invalid reboot request", control.RequestPath)
	}
	return &request, nil
}

func (daemon *daemon) writeRebootRequest(request rebootRequest) error {
	path := daemon.options.Reboots.RequestPath
	if path == "" {
		return errors.New("no reboot request path")
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return inventory.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

func (daemon *daemon) removeRebootRequest() {
	control := daemon.options.Reboots
	if control == nil || control.RequestPath == "" {
		return
	}
	if err := os.Remove(control.RequestPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("[reboot] %v", err)
	}
}
