package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"go.chief.tools/serverchief-agent/execlog"
	"go.chief.tools/serverchief-agent/inventory"
)

const (
	// commandRetention is how long a reported command stays in the
	// journal. Unacknowledged commands expire on the server after 24
	// hours, so it cannot hand out an older one again.
	commandRetention = 48 * time.Hour
	// maximumJournalRecords caps the journal; the oldest reported
	// records go first.
	maximumJournalRecords = 100
	// maximumDetailRunes caps a result's detail; the spec sets no limit,
	// and a rejected result would never be reported.
	maximumDetailRunes     = 1000
	commandsJournalVersion = 1
)

// commandState is where a command is in the agent's lifecycle.
type commandState string

const (
	// commandReceived: journaled, not yet acknowledged, not executed.
	commandReceived commandState = "received"
	// commandAcknowledged: acknowledged and executing. A restart in this
	// state reports failed instead of executing it a second time.
	commandAcknowledged commandState = "acknowledged"
	// commandHandedOff: an update_agent the detached helper carries out,
	// or a reboot the root helper scheduled. A restart neither runs it
	// again nor reports it interrupted; the helper's result file, or the
	// boot ID, finishes it.
	commandHandedOff commandState = "handed_off"
	// commandFinished: executed; Result holds the outcome.
	commandFinished commandState = "finished"
)

// commandRecord is one journaled command.
type commandRecord struct {
	ID    string          `json:"id"`
	Kind  string          `json:"kind"`
	Args  json.RawMessage `json:"args,omitempty"`
	State commandState    `json:"state"`
	// Result is set once the command finished; Reported once the backend
	// accepted it.
	Result    *CommandResult `json:"result,omitempty"`
	Reported  bool           `json:"reported,omitempty"`
	UpdatedAt time.Time      `json:"updated_at"`
}

type journalFile struct {
	Version  int              `json:"version"`
	Commands []*commandRecord `json:"commands"`
}

// CommandJournal persists recently received commands and their results.
// Acknowledgements and results are idempotent on the backend, so the
// journal only has to guarantee that a command is executed at most once
// and that its result is eventually reported, across restarts. Without a
// path it keeps the records in memory only.
type CommandJournal struct {
	path string

	mu      sync.Mutex
	records map[string]*commandRecord
}

// OpenCommandJournal loads path, starting empty when it does not exist. A
// corrupt file is moved aside: the worst outcome is that a command the
// backend still lists as unacknowledged runs again.
func OpenCommandJournal(path string) (*CommandJournal, error) {
	journal := &CommandJournal{path: path, records: make(map[string]*commandRecord)}
	if path == "" {
		return journal, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return journal, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var file journalFile
	if err := json.Unmarshal(data, &file); err != nil || file.Version != commandsJournalVersion {
		corrupt := path + ".corrupt"
		log.Printf("[commands] %s is unreadable; moving it to %s", path, corrupt)
		_ = os.Rename(path, corrupt)
		return journal, nil
	}
	for _, record := range file.Commands {
		if record != nil && record.ID != "" {
			journal.records[record.ID] = record
		}
	}
	return journal, nil
}

func (journal *CommandJournal) get(id string) *commandRecord {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if record, ok := journal.records[id]; ok {
		copied := *record
		return &copied
	}
	return nil
}

// put stores record and persists the journal.
func (journal *CommandJournal) put(record commandRecord) error {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.records[record.ID] = &record
	return journal.saveLocked(record.UpdatedAt)
}

func (journal *CommandJournal) remove(id string, now time.Time) error {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	delete(journal.records, id)
	return journal.saveLocked(now)
}

// snapshot returns copies of every record, oldest first.
func (journal *CommandJournal) snapshot() []commandRecord {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	records := make([]commandRecord, 0, len(journal.records))
	for _, record := range journal.records {
		records = append(records, *record)
	}
	slices.SortFunc(records, func(a, b commandRecord) int {
		if result := a.UpdatedAt.Compare(b.UpdatedAt); result != 0 {
			return result
		}
		return strings.Compare(a.ID, b.ID)
	})
	return records
}

// saveLocked prunes reported records past their retention, caps the
// journal and writes it atomically.
func (journal *CommandJournal) saveLocked(now time.Time) error {
	var reported []*commandRecord
	for id, record := range journal.records {
		if !record.Reported {
			continue
		}
		if now.Sub(record.UpdatedAt) > commandRetention {
			delete(journal.records, id)
			continue
		}
		reported = append(reported, record)
	}
	if excess := len(journal.records) - maximumJournalRecords; excess > 0 {
		slices.SortFunc(reported, func(a, b *commandRecord) int { return a.UpdatedAt.Compare(b.UpdatedAt) })
		for _, record := range reported[:min(excess, len(reported))] {
			delete(journal.records, record.ID)
		}
	}
	if journal.path == "" {
		return nil
	}
	file := journalFile{Version: commandsJournalVersion, Commands: make([]*commandRecord, 0, len(journal.records))}
	for _, id := range slices.Sorted(maps.Keys(journal.records)) {
		file.Commands = append(file.Commands, journal.records[id])
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode command journal: %w", err)
	}
	if err := inventory.WriteFileAtomic(journal.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write command journal: %w", err)
	}
	return nil
}

// commandQueue holds command IDs waiting for a request (ack or result),
// in order. A transient failure delays the whole queue, so a backend that
// is down is not asked once per command.
type commandQueue struct {
	ids     []string
	at      time.Time
	backoff time.Duration
}

func (queue *commandQueue) add(id string, now time.Time) {
	if slices.Contains(queue.ids, id) {
		return
	}
	if len(queue.ids) == 0 {
		queue.at = now
	}
	queue.ids = append(queue.ids, id)
}

func (queue *commandQueue) due(now time.Time) bool {
	return len(queue.ids) > 0 && !now.Before(queue.at)
}

// commandWaiter is an executing command that finishes with the push or
// config fetch it triggered.
type commandWaiter struct {
	id   string
	kind string
	// remaining are datasets that have yet to go through a push.
	remaining map[string]bool
	// skipped are requested datasets that are not collected.
	skipped []string
	// failures collects collector and delivery errors.
	failures []string
}

// datasetArgs are the arguments of refresh_inventory and resync.
type datasetArgs struct {
	Datasets []string `json:"datasets"`
}

// requestCommands makes the loop call GET /commands now, unless a fetch
// is already pending (possibly backing off).
func (daemon *daemon) requestCommands(now time.Time) {
	if !daemon.commandsPending {
		daemon.commandsPending = true
		daemon.commandsAt = now
	}
}

// recoverCommands resumes the journal after a start: unacknowledged
// commands are acknowledged and executed, commands that were executing
// when the agent stopped are reported failed rather than run twice,
// handed-off reboots are checked against the boot ID, and unreported
// results are sent.
func (daemon *daemon) recoverCommands(now time.Time) {
	defer daemon.checkReboots(now)
	for _, record := range daemon.commands.snapshot() {
		switch record.State {
		case commandReceived:
			daemon.acks.add(record.ID, now)
		case commandAcknowledged:
			log.Printf("[commands] %s %s was interrupted by a restart; reporting it failed", record.Kind, record.ID)
			daemon.finishCommand(record.ID, CommandFailed, "the agent restarted before the command finished")
		case commandHandedOff:
			if record.Kind == CommandReboot {
				continue
			}
			if record.Kind == CommandUpgradePackages {
				log.Printf("[commands] %s %s is with the package upgrade helper; waiting for its result", record.Kind, record.ID)
				daemon.upgrade.active, daemon.upgrade.pollAt, daemon.upgrade.resumeDue = true, now, true
				continue
			}
			log.Printf("[commands] %s %s is with the update helper; waiting for its result", record.Kind, record.ID)
			daemon.update.active = true
			daemon.update.inProgress = true
		case commandFinished:
			if !record.Reported && record.Result != nil {
				daemon.results.add(record.ID, now)
			}
		}
	}
}

// fetchCommands calls GET /commands and queues the acknowledgement of
// every command not seen before. A command already known to this agent is
// never executed again.
func (daemon *daemon) fetchCommands(ctx context.Context) error {
	options := daemon.options
	commands, err := options.Client.FetchCommands(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		kind, _ := classify(err)
		wait, fatal := daemon.failure("commands", err, &daemon.commandsBackoff, 0)
		if fatal != nil {
			return fatal
		}
		if kind == failureRejected {
			// The next heartbeat flag or realtime event tries again.
			daemon.commandsPending = false
		}
		daemon.commandsAt = options.Now().Add(wait)
		return nil
	}
	daemon.commandsPending = false
	daemon.commandsBackoff = initialBackoff
	daemon.commandsFull = len(commands) >= MaximumCommands

	now := options.Now()
	for _, command := range commands {
		if record := daemon.commands.get(command.ID); record != nil {
			// Known: in flight, with the update helper, or finished but
			// still listed as unacknowledged (an ack the backend lost).
			// Acknowledge it again and resend its result; never execute
			// it twice.
			if record.State == commandFinished || record.State == commandHandedOff {
				daemon.acks.add(command.ID, now)
			}
			continue
		}
		log.Printf("[commands] received %s %s", command.Kind, command.ID)
		record := commandRecord{ID: command.ID, Kind: command.Kind, Args: command.Args, State: commandReceived, UpdatedAt: now.UTC()}
		if err := daemon.commands.put(record); err != nil {
			log.Printf("[commands] %v", err)
		}
		daemon.acks.add(command.ID, now)
	}
	return nil
}

// sendAcks acknowledges queued commands in order and executes each one
// once its acknowledgement succeeded.
func (daemon *daemon) sendAcks(ctx context.Context) error {
	options := daemon.options
	for daemon.acks.due(options.Now()) {
		id := daemon.acks.ids[0]
		err := options.Client.AckCommand(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			kind, _ := classify(err)
			wait, fatal := daemon.failure("ack command "+id, err, &daemon.acks.backoff, 0)
			if fatal != nil {
				return fatal
			}
			if kind != failureRejected {
				daemon.acks.at = options.Now().Add(wait)
				return nil
			}
			// Rejected (404: unknown, expired or another server's): the
			// command cannot be acknowledged, so it is not executed.
			daemon.acks.ids = daemon.acks.ids[1:]
			if err := daemon.commands.remove(id, options.Now()); err != nil {
				log.Printf("[commands] %v", err)
			}
			continue
		}
		daemon.acks.ids = daemon.acks.ids[1:]
		daemon.acks.backoff = initialBackoff

		record := daemon.commands.get(id)
		switch {
		case record == nil:
		case record.State == commandReceived:
			record.State = commandAcknowledged
			record.UpdatedAt = options.Now().UTC()
			if err := daemon.commands.put(*record); err != nil {
				log.Printf("[commands] %v", err)
			}
			daemon.execute(*record)
		case record.State == commandFinished:
			record.Reported = false
			if err := daemon.commands.put(*record); err != nil {
				log.Printf("[commands] %v", err)
			}
			daemon.results.add(id, options.Now())
		}
	}
	if len(daemon.acks.ids) == 0 && daemon.commandsFull {
		// GET /commands returned a full page; there may be more.
		daemon.commandsFull = false
		daemon.requestCommands(options.Now())
	}
	return nil
}

// execute starts an acknowledged command. Inventory and config commands
// finish when the push or config fetch they trigger completes.
func (daemon *daemon) execute(record commandRecord) {
	now := daemon.options.Now()
	switch record.Kind {
	case CommandRefreshInventory, CommandResync:
		var args datasetArgs
		if trimmed := bytes.TrimSpace(record.Args); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
			if err := json.Unmarshal(trimmed, &args); err != nil {
				daemon.finishCommand(record.ID, CommandFailed, fmt.Sprintf("invalid args: %v", err))
				return
			}
		}
		waiter := &commandWaiter{id: record.ID, kind: record.Kind, remaining: make(map[string]bool)}
		requested := args.Datasets
		if requested == nil {
			requested = daemon.options.Registry.Names()
		}
		for _, name := range requested {
			_, registered := daemon.options.Registry.Get(name)
			if registered && daemon.settings.Enabled(name) {
				waiter.remaining[name] = true
			} else if args.Datasets != nil {
				waiter.skipped = append(waiter.skipped, name)
			}
		}
		targets := slices.Sorted(maps.Keys(waiter.remaining))
		if len(targets) == 0 && len(waiter.skipped) > 0 {
			daemon.finishCommand(record.ID, CommandFailed, "no requested dataset is enabled: "+strings.Join(waiter.skipped, ", "))
			return
		}
		log.Printf("[commands] %s %s: collecting %s", record.Kind, record.ID, strings.Join(targets, ", "))
		if record.Kind == CommandResync {
			if err := daemon.resync(targets, now); err != nil {
				daemon.finishCommand(record.ID, CommandFailed, err.Error())
				return
			}
		}
		for _, name := range targets {
			daemon.next[name] = now
		}
		if len(targets) == 0 {
			daemon.finishCommand(record.ID, CommandSucceeded, "no datasets are enabled")
			return
		}
		daemon.waiters = append(daemon.waiters, waiter)
	case CommandUpdateAgent:
		daemon.startUpdate(record)
	case CommandReboot:
		daemon.startReboot(record)
	case CommandUpgradePackages:
		daemon.startUpgrade(record)
	case CommandDisableCapability:
		daemon.disableCapability(record)
	case CommandRefreshConfig:
		log.Printf("[commands] %s %s: fetching the configuration", record.Kind, record.ID)
		daemon.configPending = true
		daemon.configAt = now
		daemon.waiters = append(daemon.waiters, &commandWaiter{id: record.ID, kind: record.Kind})
	default:
		daemon.finishCommand(record.ID, CommandFailed, fmt.Sprintf("unsupported command kind %q", record.Kind))
	}
}

// settleInventory finishes inventory waiters once every dataset they wait
// for went through a push. err is the delivery error; failures are
// collector errors by dataset.
func (daemon *daemon) settleInventory(attempted []string, err error, failures map[string]string) {
	daemon.settle(func(waiter *commandWaiter) bool {
		if waiter.kind != CommandRefreshInventory && waiter.kind != CommandResync {
			return false
		}
		for _, name := range attempted {
			if !waiter.remaining[name] {
				continue
			}
			delete(waiter.remaining, name)
			if message, failed := failures[name]; failed {
				waiter.failures = append(waiter.failures, name+": "+message)
			}
			if err != nil {
				waiter.failures = append(waiter.failures, name+": "+err.Error())
			}
		}
		return len(waiter.remaining) == 0
	})
}

// settleConfig finishes refresh_config waiters with the outcome of a
// config fetch.
func (daemon *daemon) settleConfig(err error) {
	daemon.settle(func(waiter *commandWaiter) bool {
		if waiter.kind != CommandRefreshConfig {
			return false
		}
		if err != nil {
			waiter.failures = append(waiter.failures, err.Error())
		}
		return true
	})
}

// pruneWaiters drops datasets that are no longer scheduled (disabled by a
// configuration change) from the inventory waiters.
func (daemon *daemon) pruneWaiters() {
	daemon.settle(func(waiter *commandWaiter) bool {
		if waiter.kind == CommandRefreshConfig {
			return false
		}
		for name := range waiter.remaining {
			if _, scheduled := daemon.next[name]; !scheduled {
				delete(waiter.remaining, name)
				waiter.skipped = append(waiter.skipped, name)
			}
		}
		return len(waiter.remaining) == 0
	})
}

// settle finishes the waiters for which done reports true.
func (daemon *daemon) settle(done func(*commandWaiter) bool) {
	kept := daemon.waiters[:0]
	for _, waiter := range daemon.waiters {
		if !done(waiter) {
			kept = append(kept, waiter)
			continue
		}
		status, detail := CommandSucceeded, ""
		if len(waiter.failures) > 0 {
			status, detail = CommandFailed, strings.Join(waiter.failures, "; ")
		} else if len(waiter.skipped) > 0 {
			slices.Sort(waiter.skipped)
			detail = "not enabled: " + strings.Join(slices.Compact(waiter.skipped), ", ")
		}
		daemon.finishCommand(waiter.id, status, detail)
	}
	clear(daemon.waiters[len(kept):])
	daemon.waiters = kept
}

// finishCommand records a command's outcome and queues its result.
func (daemon *daemon) finishCommand(id, status, detail string) {
	daemon.finishCommandData(id, status, detail, nil)
}

// finishCommandData is finishCommand with kind-specific result data.
func (daemon *daemon) finishCommandData(id, status, detail string, data json.RawMessage) {
	daemon.finishCommandOutput(id, status, detail, data, "", false)
}

// finishCommandOutput is finishCommandData with the output of what the
// command ran (execlog), left out when empty.
func (daemon *daemon) finishCommandOutput(id, status, detail string, data json.RawMessage, output string, truncated bool) {
	now := daemon.options.Now()
	record := daemon.commands.get(id)
	if record == nil {
		record = &commandRecord{ID: id}
	}
	result := CommandResult{Status: status, FinishedAt: now.UTC().Truncate(time.Second), Data: data}
	if output != "" {
		text, cut := execlog.Cap(output)
		truncated = truncated || cut
		result.Output, result.OutputTruncated = &text, &truncated
	}
	if runes := []rune(detail); len(runes) > maximumDetailRunes {
		detail = string(runes[:maximumDetailRunes-1]) + "…"
	}
	if detail != "" {
		result.Detail = &detail
	}
	record.State = commandFinished
	record.Result = &result
	record.Reported = false
	record.UpdatedAt = now.UTC()
	if err := daemon.commands.put(*record); err != nil {
		log.Printf("[commands] %v", err)
	}
	if detail != "" {
		log.Printf("[commands] %s %s %s: %s", record.Kind, id, status, detail)
	} else {
		log.Printf("[commands] %s %s %s", record.Kind, id, status)
	}
	daemon.results.add(id, now)
}

// sendResults reports finished commands in order. The backend keeps the
// first result, so a resend after a lost answer is harmless.
func (daemon *daemon) sendResults(ctx context.Context) error {
	options := daemon.options
	for daemon.results.due(options.Now()) {
		id := daemon.results.ids[0]
		record := daemon.commands.get(id)
		if record == nil || record.Result == nil || record.Reported {
			daemon.results.ids = daemon.results.ids[1:]
			continue
		}
		err := options.Client.ReportCommandResult(ctx, id, *record.Result)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			kind, _ := classify(err)
			wait, fatal := daemon.failure("report command "+id, err, &daemon.results.backoff, 0)
			if fatal != nil {
				return fatal
			}
			if kind != failureRejected {
				daemon.results.at = options.Now().Add(wait)
				return nil
			}
			// Rejected: resending the same body cannot succeed.
		}
		daemon.results.ids = daemon.results.ids[1:]
		daemon.results.backoff = initialBackoff
		record.Reported = true
		record.UpdatedAt = options.Now().UTC()
		if err := daemon.commands.put(*record); err != nil {
			log.Printf("[commands] %v", err)
		}
		if record.Kind == CommandUpdateAgent && options.Updates != nil {
			// The journal holds the reported result now; the helper's
			// copy goes.
			if err := options.Updates.RemoveResult(id); err != nil {
				log.Printf("[update] %v", err)
			}
		}
	}
	return nil
}
