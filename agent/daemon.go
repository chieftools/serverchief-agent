package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"maps"
	"math/big"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"go.chief.tools/serverchief-agent/collector"
	"go.chief.tools/serverchief-agent/inventory"
	"go.chief.tools/serverchief-agent/updater"
)

const (
	initialBackoff        = 2 * time.Second
	maximumBackoff        = 5 * time.Minute
	unsupportedRetry      = time.Hour
	defaultCollectTimeout = 2 * time.Minute
	defaultChangePoll     = time.Minute
)

// RunOptions configures Run.
type RunOptions struct {
	// Privilege reports how the agent runs; nil leaves it out.
	Privilege *PrivilegeProbe
	Registry  *collector.Registry
	Store     *inventory.StateStore
	// Payloads keeps the bases of delta datasets. Without it every dataset
	// is sent in full.
	Payloads *inventory.PayloadStore
	// Commands journals received commands across restarts. Nil keeps them
	// in memory only.
	Commands *CommandJournal
	Client   Client
	Identity Identity
	// Config holds the local overrides.
	Config Config
	// ServerConfig is the cached backend configuration. A zero ETag makes
	// the daemon fetch /config right away.
	ServerConfig ServerConfig
	// SaveServerConfig persists a new backend configuration.
	SaveServerConfig func(ServerConfig) error
	// Realtime connects to the realtime service when the backend
	// configures one. Nil means polling only.
	Realtime RealtimeConnector
	Version  string

	// Reload delivers new local overrides (SIGHUP). Every dataset is
	// collected again right after a reload.
	Reload <-chan Config
	// Ready is called once, after the first round of requests.
	Ready func()

	CollectTimeout time.Duration
	// ChangePoll is how often ChangeDetector collectors are polled.
	ChangePoll time.Duration
	// Packages drives the package metadata refresh and the package
	// activity trigger. Nil disables both.
	Packages       PackageSystem
	PackageTimings PackageTimings
	// RecordRefresh persists a successful metadata refresh (the state
	// directory's metadata-refresh.json) before updates is collected.
	RecordRefresh func(time.Time) error
	// Updates hands update_agent to the detached helper and reports the
	// capability. Nil reports updates unavailable.
	Updates *updater.Controller
	// Metrics receives the backend's metrics setting. Nil leaves metrics
	// out.
	Metrics Metrics
	// UpdatePoll is how often the helper's result is looked for (5 s).
	UpdatePoll time.Duration
	Now        func() time.Time
	// Jitter returns a random duration in [0, maximum].
	Jitter func(maximum time.Duration) time.Duration
}

type daemon struct {
	options   RunOptions
	settings  settings
	startedAt time.Time

	next         map[string]time.Time
	changeTokens map[string]string
	changePollAt time.Time

	heartbeatAt      time.Time
	heartbeatBackoff time.Duration
	inventoryBackoff time.Duration
	configPending    bool
	configAt         time.Time
	configBackoff    time.Duration
	// unsupported is set by 426: inventory pauses and the heartbeat drops
	// to hourly until a heartbeat succeeds again.
	unsupported bool

	// ignored holds datasets the backend answered as unknown. They are not
	// sent again until its configuration changes.
	ignored map[string]bool

	// Commands: a pending GET /commands, the acknowledgements and results
	// still to send, and executing commands waiting for a push or a
	// config fetch.
	commands        *CommandJournal
	commandsPending bool
	commandsAt      time.Time
	commandsBackoff time.Duration
	// commandsFull is set when GET /commands returned a full page.
	commandsFull bool
	acks         commandQueue
	results      commandQueue
	waiters      []*commandWaiter

	commandsWake chan struct{}
	configWake   chan struct{}
	realtimeKey  string
	stopRealtime func()

	// packages is the metadata refresh and package activity state.
	packages packageWork
	// update is the agent update hand-over and readiness state.
	update updateWork
}

// Run heartbeats, collects datasets on their schedules and delivers changes
// until ctx is cancelled, which is a clean shutdown and returns nil. It
// returns an error wrapping ErrStopped when the backend rejects the
// credential.
func Run(ctx context.Context, options RunOptions) error {
	if options.Registry == nil || options.Store == nil || options.Client == nil {
		return errors.New("run: registry, store and client are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Jitter == nil {
		options.Jitter = fullJitter
	}
	if options.CollectTimeout <= 0 {
		options.CollectTimeout = defaultCollectTimeout
	}
	if options.ChangePoll <= 0 {
		options.ChangePoll = defaultChangePoll
	}
	if options.ServerConfig.Datasets == nil && options.ServerConfig.ETag == "" {
		options.ServerConfig = DefaultServerConfig()
	}

	daemon := newDaemon(options)
	defer daemon.closeRealtime()
	daemon.updateRealtime(options.ServerConfig.Realtime)
	if options.Metrics != nil {
		options.Metrics.Configure(options.ServerConfig.Metrics)
	}
	return daemon.loop(ctx)
}

func newDaemon(options RunOptions) *daemon {
	now := options.Now()
	daemon := &daemon{
		options:          options,
		settings:         settings{server: options.ServerConfig, local: options.Config},
		startedAt:        now.UTC().Truncate(time.Second),
		next:             make(map[string]time.Time),
		changeTokens:     make(map[string]string),
		ignored:          make(map[string]bool),
		heartbeatAt:      now,
		heartbeatBackoff: initialBackoff,
		inventoryBackoff: initialBackoff,
		configBackoff:    initialBackoff,
		configPending:    options.ServerConfig.ETag == "",
		configAt:         now,
		changePollAt:     now.Add(options.ChangePoll),
		commands:         options.Commands,
		commandsBackoff:  initialBackoff,
		acks:             commandQueue{backoff: initialBackoff},
		results:          commandQueue{backoff: initialBackoff},
		commandsWake:     make(chan struct{}, 1),
		configWake:       make(chan struct{}, 1),
		realtimeKey:      "unset",
		packages: packageWork{
			timings: options.PackageTimings.withDefaults(),
			pollAt:  now,
			done:    make(chan error, 1),
		},
	}
	if daemon.commands == nil {
		daemon.commands, _ = OpenCommandJournal("")
	}
	daemon.scheduleAll(now)
	daemon.scheduleRefresh(now)
	daemon.initUpdates(now)
	daemon.recoverCommands(now)
	return daemon
}

func (daemon *daemon) loop(ctx context.Context) error {
	options := daemon.options
	ready := false
	defer daemon.awaitRefresh()
	for {
		if err := daemon.tick(ctx); err != nil {
			return daemon.stopped(ctx, err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if !ready {
			ready = true
			if options.Ready != nil {
				options.Ready()
			}
		}

		timer := time.NewTimer(max(daemon.wakeAt().Sub(options.Now()), 0))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		case config := <-options.Reload:
			timer.Stop()
			daemon.settings.local = config
			daemon.scheduleAll(options.Now())
			daemon.scheduleRefresh(options.Now())
			daemon.pruneWaiters()
			log.Printf("[config] reloaded; collecting %d datasets now", len(daemon.next))
		case <-daemon.commandsWake:
			timer.Stop()
			daemon.requestCommands(options.Now())
		case <-daemon.configWake:
			timer.Stop()
			daemon.configPending = true
			daemon.configAt = options.Now()
		case err := <-daemon.packages.done:
			timer.Stop()
			daemon.finishRefresh(err, options.Now())
		}
	}
}

// tick runs everything that is due: heartbeat, config, commands,
// inventory and command results. It returns only fatal errors.
func (daemon *daemon) tick(ctx context.Context) error {
	options := daemon.options
	now := options.Now()
	if !now.Before(daemon.heartbeatAt) {
		if err := daemon.heartbeat(ctx); err != nil {
			return err
		}
	}
	if daemon.configPending && !options.Now().Before(daemon.configAt) {
		if err := daemon.refreshConfig(ctx); err != nil {
			return err
		}
	}
	if !now.Before(daemon.changePollAt) {
		daemon.pollChanges(now)
	}
	if !daemon.unsupported {
		if daemon.commandsPending && !options.Now().Before(daemon.commandsAt) {
			if err := daemon.fetchCommands(ctx); err != nil {
				return err
			}
		}
		if err := daemon.sendAcks(ctx); err != nil {
			return err
		}
	}
	if !daemon.unsupported {
		daemon.tickPackages(ctx, options.Now())
	}
	if !daemon.unsupported {
		if due := daemon.due(options.Now()); len(due) > 0 {
			if err := daemon.deliver(ctx, due); err != nil {
				return err
			}
		}
	}
	daemon.pollUpdate(ctx, options.Now())
	if !daemon.unsupported {
		if err := daemon.sendResults(ctx); err != nil {
			return err
		}
	}
	return nil
}

// stopped turns a fatal error into Run's result. Cancellation is a clean
// shutdown.
func (daemon *daemon) stopped(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (daemon *daemon) wakeAt() time.Time {
	wakeAt := daemon.heartbeatAt
	if daemon.configPending && daemon.configAt.Before(wakeAt) {
		wakeAt = daemon.configAt
	}
	if daemon.hasChangeDetectors() && daemon.changePollAt.Before(wakeAt) {
		wakeAt = daemon.changePollAt
	}
	if daemon.options.Updates != nil && daemon.update.active && daemon.update.pollAt.Before(wakeAt) {
		wakeAt = daemon.update.pollAt
	}
	if !daemon.unsupported {
		if daemon.commandsPending && daemon.commandsAt.Before(wakeAt) {
			wakeAt = daemon.commandsAt
		}
		for _, queue := range []*commandQueue{&daemon.acks, &daemon.results} {
			if len(queue.ids) > 0 && queue.at.Before(wakeAt) {
				wakeAt = queue.at
			}
		}
		for _, at := range daemon.next {
			if at.Before(wakeAt) {
				wakeAt = at
			}
		}
		if at := daemon.packagesWakeAt(); !at.IsZero() && at.Before(wakeAt) {
			wakeAt = at
		}
	}
	return wakeAt
}

// scheduleAll makes every enabled dataset due at now and drops disabled ones.
func (daemon *daemon) scheduleAll(now time.Time) {
	clear(daemon.next)
	for _, name := range daemon.options.Registry.Names() {
		if daemon.settings.Enabled(name) {
			daemon.next[name] = now
		}
	}
}

// reschedule applies new settings: newly enabled datasets are due now,
// disabled ones are dropped and shorter intervals take effect at once.
func (daemon *daemon) reschedule(now time.Time) {
	for _, name := range daemon.options.Registry.Names() {
		at, scheduled := daemon.next[name]
		switch {
		case !daemon.settings.Enabled(name):
			delete(daemon.next, name)
		case !scheduled:
			daemon.next[name] = now
		case at.After(now.Add(daemon.settings.Interval(name))):
			daemon.next[name] = now.Add(daemon.settings.Interval(name))
		}
	}
	if limit := now.Add(daemon.settings.Heartbeat()); !daemon.unsupported && daemon.heartbeatAt.After(limit) {
		daemon.heartbeatAt = limit
	}
	daemon.scheduleRefresh(now)
}

func (daemon *daemon) due(now time.Time) []string {
	var due []string
	for name, at := range daemon.next {
		if !at.After(now) {
			due = append(due, name)
		}
	}
	slices.Sort(due)
	return due
}

// failure applies the protocol's behaviour table to a failed request and
// returns when to try again. A 401 returns a fatal error instead.
func (daemon *daemon) failure(operation string, err error, backoff *time.Duration, normal time.Duration) (time.Duration, error) {
	kind, problem := classify(err)
	switch kind {
	case failureFatal:
		log.Printf("[agent] %s: %v", operation, err)
		log.Printf("[agent] Server Chief no longer accepts this agent's credential (%s). Stopping; re-run `serverchief setup <token>` with a new token from the dashboard.", problem.Type)
		return 0, fmt.Errorf("%w: %s rejected with %d %s; re-run `serverchief setup`", ErrStopped, operation, problem.Status, problem.Type)
	case failureUnsupported:
		if !daemon.unsupported {
			log.Printf("[agent] %s: %v; this agent's protocol revision %s is no longer supported. Pausing inventory and heartbeating hourly until the agent is updated.", operation, err, ProtocolRevision)
		}
		daemon.unsupported = true
		daemon.heartbeatAt = daemon.options.Now().Add(unsupportedRetry)
		return unsupportedRetry, nil
	case failureThrottled:
		wait := problem.RetryAfter
		if wait <= 0 {
			wait = daemon.options.Jitter(*backoff)
			*backoff = min(*backoff*2, maximumBackoff)
		}
		log.Printf("[agent] %s throttled: %v; retrying in %s", operation, err, wait.Round(time.Second))
		return wait, nil
	case failureRejected:
		log.Printf("[agent] %s rejected: %v; not retrying it unchanged", operation, err)
		*backoff = initialBackoff
		return normal, nil
	default:
		wait := daemon.options.Jitter(*backoff)
		*backoff = min(*backoff*2, maximumBackoff)
		log.Printf("[agent] %s failed: %v; retrying in %s", operation, err, wait.Round(time.Second))
		return wait, nil
	}
}

func (daemon *daemon) heartbeat(ctx context.Context) error {
	options := daemon.options
	now := options.Now()
	request := HeartbeatRequest{
		AgentVersion: normalizeVersion(options.Version),
		StartedAt:    daemon.startedAt,
		SentAt:       now.UTC().Truncate(time.Second),
		Hashes:       options.Store.Hashes(),
		Seq:          options.Store.AcknowledgedSeq(),
		Capabilities: daemon.capabilities(ctx),
	}
	response, err := options.Client.Heartbeat(ctx, request)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		normal := normalJitter(daemon.settings.Heartbeat(), JitterFraction, options.Jitter)
		wait, fatal := daemon.failure("heartbeat", err, &daemon.heartbeatBackoff, normal)
		if fatal != nil {
			return fatal
		}
		if !daemon.unsupported {
			daemon.heartbeatAt = options.Now().Add(wait)
		}
		return nil
	}

	if daemon.unsupported {
		log.Printf("[agent] heartbeat accepted again; resuming inventory")
		daemon.unsupported = false
		daemon.scheduleAll(now)
	}
	daemon.heartbeatBackoff = initialBackoff
	daemon.heartbeatAt = options.Now().Add(normalJitter(daemon.settings.Heartbeat(), JitterFraction, options.Jitter))
	daemon.markReady()

	if len(response.Resync) > 0 {
		log.Printf("[heartbeat] backend requested a full resend of %s", strings.Join(response.Resync, ", "))
		if err := daemon.resync(response.Resync, options.Now()); err != nil {
			log.Printf("[state] %v", err)
		}
	}
	if response.ConfigETag != "" && response.ConfigETag != daemon.settings.server.ETag {
		daemon.configPending = true
		daemon.configAt = options.Now()
	}
	if response.CommandsAvailable {
		daemon.requestCommands(options.Now())
	}
	return nil
}

// resync forgets the acknowledged state of names and makes the enabled
// ones due now, so they are sent in full.
func (daemon *daemon) resync(names []string, now time.Time) error {
	if err := daemon.options.Store.Forget(names...); err != nil {
		return fmt.Errorf("forget resynced datasets: %w", err)
	}
	if daemon.options.Payloads != nil {
		if err := daemon.options.Payloads.Remove(names...); err != nil {
			return err
		}
	}
	for _, name := range names {
		if daemon.settings.Enabled(name) {
			if _, ok := daemon.options.Registry.Get(name); ok {
				daemon.next[name] = now
			}
		}
	}
	return nil
}

func (daemon *daemon) refreshConfig(ctx context.Context) error {
	options := daemon.options
	result, err := options.Client.FetchConfig(ctx, daemon.settings.server.ETag)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		wait, fatal := daemon.failure("config", err, &daemon.configBackoff, 0)
		if fatal != nil {
			return fatal
		}
		if wait == 0 {
			daemon.configPending = false
		}
		daemon.configAt = options.Now().Add(wait)
		daemon.settleConfig(err)
		return nil
	}
	daemon.configPending = false
	daemon.configBackoff = initialBackoff
	if !result.NotModified {
		daemon.applyConfig(result.Config)
	}
	daemon.settleConfig(nil)
	return nil
}

func (daemon *daemon) applyConfig(config ServerConfig) {
	if config.Datasets == nil {
		config.Datasets = []string{}
	}
	daemon.settings.server = config
	clear(daemon.ignored)
	if save := daemon.options.SaveServerConfig; save != nil {
		if err := save(config); err != nil {
			log.Printf("[config] cache the backend configuration: %v", err)
		}
	}
	daemon.reschedule(daemon.options.Now())
	daemon.pruneWaiters()
	daemon.updateRealtime(config.Realtime)
	if daemon.options.Metrics != nil {
		daemon.options.Metrics.Configure(config.Metrics)
	}
	log.Printf("[config] applied backend configuration %s: datasets %s, heartbeat %s",
		config.ETag, strings.Join(config.Datasets, ", "), daemon.settings.Heartbeat())
}

func (daemon *daemon) updateRealtime(config *Realtime) {
	key := ""
	if config != nil {
		key = fmt.Sprintf("%s|%s|%d|%s|%s", config.Key, config.Host, config.Port, config.Scheme, config.AuthEndpoint)
	}
	if key == daemon.realtimeKey && (key == "" || daemon.stopRealtime != nil) {
		return
	}
	daemon.closeRealtime()
	daemon.realtimeKey = key
	if config == nil {
		log.Printf("[realtime] not configured; polling only")
		return
	}
	if daemon.options.Realtime == nil {
		return
	}
	identity := daemon.options.Identity
	channel := RealtimeChannel(identity.ServerID, identity.Generation)
	client := daemon.options.Client
	authorize := func(ctx context.Context, socketID, channel string) (string, error) {
		return client.AuthorizeChannel(ctx, config.AuthEndpoint, socketID, channel)
	}
	daemon.stopRealtime = daemon.options.Realtime(*config, channel, authorize, daemon.onRealtimeEvent)
}

func (daemon *daemon) closeRealtime() {
	if daemon.stopRealtime != nil {
		daemon.stopRealtime()
		daemon.stopRealtime = nil
	}
}

// onRealtimeEvent runs on the realtime client's goroutines, so it only
// signals the loop.
func (daemon *daemon) onRealtimeEvent(event string) {
	switch event {
	case EventConfigUpdated:
		notify(daemon.configWake)
	case EventCommandsAvailable:
		notify(daemon.commandsWake)
	case eventSubscribed:
		// A fresh subscription may have missed events: check both.
		notify(daemon.configWake)
		notify(daemon.commandsWake)
	}
}

func notify(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (daemon *daemon) hasChangeDetectors() bool {
	for name := range daemon.next {
		if instance, ok := daemon.options.Registry.Get(name); ok {
			if _, detects := instance.(collector.ChangeDetector); detects {
				return true
			}
		}
	}
	return false
}

// pollChanges makes a dataset due when its change token moved, for example
// when the package database was written.
func (daemon *daemon) pollChanges(now time.Time) {
	daemon.changePollAt = now.Add(daemon.options.ChangePoll)
	for name := range daemon.next {
		instance, _ := daemon.options.Registry.Get(name)
		detector, ok := instance.(collector.ChangeDetector)
		if !ok {
			continue
		}
		if daemon.packages.running && slices.Contains(refreshDatasets, name) {
			// Our own refresh is moving the cache and holds the lock; its
			// completion collects these and records the new token.
			continue
		}
		token := detector.ChangeToken()
		if previous, seen := daemon.changeTokens[name]; seen && previous != token {
			log.Printf("[collector] %s source changed; collecting now", name)
			daemon.next[name] = now
		}
		daemon.changeTokens[name] = token
	}
}

// deliver runs one inventory round for due datasets and schedules them
// again according to the outcome.
func (daemon *daemon) deliver(ctx context.Context, due []string) error {
	options := daemon.options
	outcome, err := daemon.cycle(ctx, due)
	now := options.Now()
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		wait, fatal := daemon.failure("inventory", err, &daemon.inventoryBackoff, 0)
		if fatal != nil {
			return fatal
		}
		daemon.settleInventory(due, err, nil)
		for _, name := range due {
			if wait == 0 {
				wait = normalJitter(daemon.settings.Interval(name), JitterFraction, options.Jitter)
			}
			daemon.next[name] = now.Add(wait)
		}
		return nil
	}

	daemon.inventoryBackoff = initialBackoff
	// A command that asked for a skipped dataset reports why.
	commandFailures := maps.Clone(outcome.failures)
	if len(outcome.deferred) > 0 {
		if commandFailures == nil {
			commandFailures = map[string]string{}
		}
		maps.Copy(commandFailures, outcome.deferred)
	}
	daemon.settleInventory(due, nil, commandFailures)
	for _, name := range due {
		if _, enabled := daemon.next[name]; enabled {
			daemon.next[name] = now.Add(normalJitter(daemon.settings.Interval(name), JitterFraction, options.Jitter))
		}
	}
	for _, name := range outcome.retryNow {
		if _, enabled := daemon.next[name]; enabled {
			daemon.next[name] = now
		}
	}
	for name := range outcome.deferred {
		if _, enabled := daemon.next[name]; enabled {
			daemon.next[name] = now.Add(daemon.lockRetry())
		}
	}
	return nil
}

// cycleOutcome lists datasets to send again right away, datasets skipped
// because a package manager was busy, and the collector errors of the
// round.
type cycleOutcome struct {
	retryNow []string
	// deferred maps a skipped dataset to the reason, for command results.
	deferred map[string]string
	failures map[string]string
}

// pendingDataset is a dataset in flight.
type pendingDataset struct {
	dataset inventory.Dataset
	mode    inventory.Mode
	keyed   bool
}

// cycle collects names and sends every dataset whose hash differs from the
// acknowledged one, plus collector errors that changed. A failed delivery
// returns the error and leaves the acknowledged state untouched.
func (daemon *daemon) cycle(ctx context.Context, names []string) (cycleOutcome, error) {
	options := daemon.options
	envelope := inventory.NewEnvelope(0)
	pending := make(map[string]pendingDataset)
	reported := options.Store.ReportedErrors()
	failures := make(map[string]string)
	deferred := make(map[string]string)
	recovered := false

	for _, name := range names {
		instance, ok := options.Registry.Get(name)
		if !ok || daemon.ignored[name] {
			continue
		}
		dataset, err := collectDataset(ctx, instance, options.CollectTimeout)
		if errors.Is(err, collector.ErrPackageManagerBusy) {
			// Not an error: the previous dataset (and any reported error)
			// stands until the package manager is done.
			log.Printf("[collector] %s skipped: %v; keeping the previous dataset", name, err)
			deferred[name] = err.Error()
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return cycleOutcome{}, ctx.Err()
			}
			log.Printf("[collector] %s failed: %v", name, err)
			failures[name] = err.Error()
			continue
		}
		_, hadError := reported[name]
		recovered = recovered || hadError
		acknowledged := options.Store.Hash(name)
		if acknowledged == dataset.Hash && !hadError {
			continue
		}

		entry := pendingDataset{dataset: dataset, mode: inventory.ModeFull}
		keyed, isKeyed := instance.(collector.KeyedDataset)
		entry.keyed = isKeyed && options.Payloads != nil
		if entry.keyed && acknowledged != "" && acknowledged != dataset.Hash {
			if added, err := daemon.addDelta(envelope, dataset, acknowledged, keyed.DeltaKeyFields()); err != nil {
				log.Printf("[inventory] %s: sending in full: %v", name, err)
			} else if added {
				entry.mode = inventory.ModeDelta
			}
		}
		if entry.mode == inventory.ModeFull {
			envelope.AddFull(dataset)
		}
		pending[name] = entry
	}

	// Errors are reported when they change; an unchanged error is already
	// stored by the backend and stays in the local log.
	nextReported := maps.Clone(reported)
	errorsChanged := recovered
	for _, name := range names {
		if _, skipped := deferred[name]; !skipped {
			delete(nextReported, name)
		}
	}
	for name, message := range failures {
		nextReported[name] = message
		if reported[name] != message {
			errorsChanged = true
		}
	}
	if len(failures) > 0 {
		envelope.Errors = failures
	}
	if len(envelope.Datasets) == 0 && !errorsChanged {
		return cycleOutcome{deferred: deferred, failures: failures}, nil
	}

	seq, err := options.Store.NextSeq()
	if err != nil {
		return cycleOutcome{}, fmt.Errorf("reserve sequence number: %w", err)
	}
	envelope.Seq = seq
	response, err := options.Client.SendInventory(ctx, envelope)
	if err != nil {
		return cycleOutcome{}, err
	}
	outcome, err := daemon.acknowledge(envelope, pending, response, nextReported)
	outcome.deferred = deferred
	outcome.failures = failures
	return outcome, err
}

func (daemon *daemon) addDelta(envelope *inventory.Envelope, dataset inventory.Dataset, acknowledged string, keyFields []string) (bool, error) {
	base, err := daemon.options.Payloads.Load(dataset.Name)
	if err != nil || base == nil {
		return false, err
	}
	if inventory.Hash(base) != acknowledged {
		return false, nil
	}
	delta, ok, err := buildDelta(base, dataset.Canonical, keyFields)
	if err != nil || !ok {
		return false, err
	}
	return true, envelope.AddDelta(dataset, acknowledged, delta)
}

// acknowledge records the backend's answer. Hashes advance only for
// datasets listed in accepted with the hash that was sent.
func (daemon *daemon) acknowledge(envelope *inventory.Envelope, pending map[string]pendingDataset, response InventoryResponse, reported map[string]string) (cycleOutcome, error) {
	options := daemon.options
	var outcome cycleOutcome

	acknowledged := make(map[string]string)
	for name, entry := range pending {
		if response.Accepted[name] != entry.dataset.Hash {
			continue
		}
		acknowledged[name] = entry.dataset.Hash
		if entry.keyed {
			if err := options.Payloads.Save(name, entry.dataset.Canonical); err != nil {
				log.Printf("[state] %v", err)
			}
		}
	}

	replayed := response.Seq > envelope.Seq
	ackSeq := envelope.Seq
	if replayed {
		// The backend already stored a higher seq (a reinstall restarted
		// ours), so it did not apply this envelope. Catch up and resend.
		ackSeq = 0
		if err := options.Store.AdvanceSeq(response.Seq); err != nil {
			return outcome, fmt.Errorf("advance sequence number: %w", err)
		}
		log.Printf("[inventory] backend is at seq %d, ahead of envelope %d; resending", response.Seq, envelope.Seq)
		for name := range pending {
			if _, ok := acknowledged[name]; !ok {
				outcome.retryNow = append(outcome.retryNow, name)
			}
		}
	}
	if err := options.Store.Acknowledge(ackSeq, acknowledged, options.Now()); err != nil {
		return outcome, fmt.Errorf("record acknowledged hashes: %w", err)
	}
	if !replayed {
		if err := options.Store.SetReportedErrors(reported); err != nil {
			return outcome, fmt.Errorf("record reported errors: %w", err)
		}
	}

	if len(response.Ignored) > 0 {
		// Not an error: the backend does not (yet) enable these datasets.
		// They wait for a configuration change instead of being resent.
		log.Printf("[inventory] backend does not accept %s; not sending until its configuration changes", strings.Join(response.Ignored, ", "))
		for _, name := range response.Ignored {
			daemon.ignored[name] = true
		}
	}
	if len(response.Resync) > 0 {
		log.Printf("[inventory] backend requested a full resend of %s", strings.Join(response.Resync, ", "))
		if err := daemon.resync(response.Resync, options.Now()); err != nil {
			return outcome, err
		}
		for _, name := range response.Resync {
			// A dataset that was just sent in full and still needs a
			// resync would loop; it waits for its next interval instead.
			if entry, sent := pending[name]; sent && entry.mode == inventory.ModeFull {
				continue
			}
			outcome.retryNow = append(outcome.retryNow, name)
		}
	}
	slices.Sort(outcome.retryNow)
	outcome.retryNow = slices.Compact(outcome.retryNow)

	names := slices.Sorted(maps.Keys(envelope.Datasets))
	modes := make([]string, len(names))
	for index, name := range names {
		modes[index] = name + ":" + string(envelope.Datasets[name].Mode)
	}
	summary := strings.Join(modes, " ")
	if summary == "" {
		summary = "collector errors only"
	}
	log.Printf("[inventory] seq=%d sent %s; accepted %d", envelope.Seq, summary, len(acknowledged))
	return outcome, nil
}

// CollectDatasets runs collectors once and returns their hashed datasets
// plus per-dataset errors. It backs `serverchief collect`.
func CollectDatasets(ctx context.Context, collectors []collector.Collector, timeout time.Duration) ([]inventory.Dataset, map[string]error) {
	if timeout <= 0 {
		timeout = defaultCollectTimeout
	}
	datasets := make([]inventory.Dataset, 0, len(collectors))
	failures := make(map[string]error)
	for _, instance := range collectors {
		dataset, err := collectDataset(ctx, instance, timeout)
		if err != nil {
			failures[instance.Name()] = err
			continue
		}
		datasets = append(datasets, dataset)
	}
	return datasets, failures
}

// collectDataset runs one collector with a timeout and turns a panic into
// an error, so one broken collector cannot take the daemon down.
func collectDataset(ctx context.Context, instance collector.Collector, timeout time.Duration) (dataset inventory.Dataset, err error) {
	collectContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			// The stack goes to the local log only: it differs on every
			// run, and the reported error must stay stable.
			log.Printf("[collector] %s panicked: %v\n%s", instance.Name(), recovered, debug.Stack())
			err = fmt.Errorf("collector panicked: %v", recovered)
		}
	}()
	payload, err := instance.Collect(collectContext)
	if err != nil {
		return inventory.Dataset{}, err
	}
	return inventory.NewDataset(instance.Name(), payload)
}

// normalJitter spreads interval uniformly over interval ± fraction.
func normalJitter(interval time.Duration, fraction float64, jitter func(time.Duration) time.Duration) time.Duration {
	window := time.Duration(float64(interval) * fraction)
	if window == 0 {
		return interval
	}
	return interval - window + jitter(2*window)
}

func fullJitter(maximum time.Duration) time.Duration {
	if maximum <= 0 {
		return 0
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(maximum)+1))
	if err != nil {
		return maximum
	}
	return time.Duration(value.Int64())
}
