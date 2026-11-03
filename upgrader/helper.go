package upgrader

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"go.chief.tools/serverchief-agent/collector"
	"go.chief.tools/serverchief-agent/execlog"
	"go.chief.tools/serverchief-agent/updater"
)

// Timings of the helper. Zero fields take these defaults.
const (
	// DefaultLockWait is how long the helper waits for a package manager
	// that holds its lock. The daemon already waited for up to an hour
	// before it handed the command over.
	DefaultLockWait = 15 * time.Minute
	// DefaultLockPoll is how often the locks are checked while waiting.
	DefaultLockPoll = 30 * time.Second

	auditTimeout   = 2 * time.Minute
	refreshTimeout = 15 * time.Minute
	installTimeout = 90 * time.Minute
	serviceTimeout = 2 * time.Minute
)

// Packages is what the helper needs from the host's package manager
// (collector.PackageSystem).
type Packages interface {
	// Manager is apt, dnf or yum, "" without a supported one.
	Manager() string
	// Tool is the absolute path of a program, "" when missing.
	Tool(name string) string
	// RefreshCommand refreshes every repository's metadata.
	RefreshCommand() ([]string, error)
	// PendingUpdates lists the pending updates from the metadata cache.
	PendingUpdates(ctx context.Context) (*collector.UpdatesInfo, error)
	// HasSecurityMetadata tells whether `upgrade --security` can work.
	HasSecurityMetadata(ctx context.Context, pending int) bool
}

// Options configures RunHelper.
type Options struct {
	Files    Files
	Packages Packages
	// Run runs package-manager and service commands (ExecRunner).
	Run Runner
	// Locks reports a held package-manager lock (pkglock.Inspector.Busy).
	Locks func() (string, bool)
	// AgentUpdate updates the agent to request.AgentVersion first
	// (UpdateAgent). It returns nil when no update was needed.
	AgentUpdate func(ctx context.Context, request Request, output *execlog.Log) *AgentUpdate
	// Systemd is set when services can be restarted with systemctl.
	Systemd bool

	LockWait time.Duration
	LockPoll time.Duration
	Now      func() time.Time
	// Sleep waits for d or until ctx ends.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (options *Options) defaults() {
	if options.LockWait <= 0 {
		options.LockWait = DefaultLockWait
	}
	if options.LockPoll <= 0 {
		options.LockPoll = DefaultLockPoll
	}
	if options.Run == nil {
		options.Run = ExecRunner
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Sleep == nil {
		options.Sleep = func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	if options.Locks == nil {
		options.Locks = func() (string, bool) { return "", false }
	}
}

// helper runs one request through the phases.
type helper struct {
	options Options
	request Request
	state   state
	output  execlog.Log
}

// RunHelper carries out the pending request, resuming from the persisted
// state and output. It is idempotent: without a request, or with a result
// already written for it, it only cleans up. Cancelling ctx (SIGTERM)
// stops a wait, never a running package manager, and leaves the state for
// the next run. The caller holds the helper lock.
func RunHelper(ctx context.Context, options Options) error {
	options.defaults()
	files := options.Files
	request, err := files.ReadRequest()
	if err != nil {
		return fmt.Errorf("read the upgrade request: %w", err)
	}
	if request == nil {
		log.Printf("[upgrade] no pending upgrade request")
		return nil
	}
	if result, _, err := files.ReadResult(); err == nil && result != nil && result.CommandID == request.CommandID {
		log.Printf("[upgrade] %s already finished with %s; cleaning up", request.CommandID, result.Data.Code)
		return files.cleanup()
	}

	run := &helper{options: options, request: *request}
	current, err := files.readState()
	if err != nil {
		log.Printf("[upgrade] %v; starting over", err)
	}
	if current != nil && current.CommandID == request.CommandID {
		run.state = *current
		_, _ = run.output.Write([]byte(files.readOutput()))
		log.Printf("[upgrade] resuming %s (%s) in phase %s", request.CommandID, request.Scope, current.Phase)
	} else {
		run.state = state{CommandID: request.CommandID, Phase: phaseWaiting, WaitStartedAt: options.Now().UTC()}
		log.Printf("[upgrade] starting %s: %s updates, restart services %t, agent %q", request.CommandID, request.Scope, request.RestartServices, request.AgentVersion)
	}
	run.state.Starts++
	if err := run.save(); err != nil {
		return err
	}
	if options.Packages == nil || options.Packages.Manager() == "" {
		return run.finish(CodeUnsupported, "no supported package manager (apt, dnf or yum) found")
	}
	if run.state.Starts > maximumHelperStarts {
		return run.finish(CodeUpgradeFailed, fmt.Sprintf("the package upgrade helper stopped %d times in phase %s", run.state.Starts-1, run.state.Phase))
	}
	return run.loop(ctx)
}

func (run *helper) save() error {
	run.state.UpdatedAt = run.options.Now().UTC()
	text, _ := run.output.Text()
	if err := run.options.Files.writeOutput(text); err != nil {
		return fmt.Errorf("save the upgrade output: %w", err)
	}
	if err := run.options.Files.writeState(run.state); err != nil {
		return fmt.Errorf("save the upgrade state: %w", err)
	}
	return nil
}

func (run *helper) enter(phase string) error {
	log.Printf("[upgrade] %s: %s → %s", run.request.CommandID, run.state.Phase, phase)
	run.state.Phase = phase
	return run.save()
}

func (run *helper) note(message string) {
	log.Printf("[upgrade] %s: %s", run.request.CommandID, message)
	run.state.Notes = append(run.state.Notes, message)
	_, _ = run.output.Write([]byte("# " + message + "\n"))
}

func (run *helper) apt() bool { return run.options.Packages.Manager() == ManagerApt }

// command runs argv and adds it, with its environment, to the output. A
// package manager is never interrupted by ctx: only its own timeout ends
// it.
func (run *helper) command(ctx context.Context, env, argv []string, timeout time.Duration) (Output, error) {
	run.output.Command(append(slices.Clone(env), argv...))
	commandContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	output, err := run.options.Run(commandContext, env, argv)
	_, _ = run.output.Write(output.Text)
	if err != nil {
		_, _ = run.output.Write([]byte(err.Error() + "\n"))
	} else if output.ExitCode != 0 {
		_, _ = run.output.Write([]byte(fmt.Sprintf("(exit status %d)\n", output.ExitCode)))
	}
	return output, err
}

func (run *helper) loop(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			log.Printf("[upgrade] stopping in phase %s; the next run resumes from here", run.state.Phase)
			return err
		}
		var next string
		var err error
		switch run.state.Phase {
		case phaseWaiting:
			var done bool
			if done, err = run.waitForPackageManager(ctx); done || err != nil {
				return err
			}
			next = phaseAudit
		case phaseAudit:
			if next, err = run.audit(ctx); next == "" || err != nil {
				return err
			}
		case phaseRefresh:
			next = run.refresh(ctx)
		case phaseAgent:
			next = run.agentUpdate(ctx)
		case phaseUpgrade:
			if next, err = run.upgrade(ctx); next == "" || err != nil {
				return err
			}
		case phaseServices:
			next = run.restartServices(ctx)
		case phaseFinishing:
			return run.finishUpgrade(ctx)
		default:
			log.Printf("[upgrade] unknown phase %q; starting over", run.state.Phase)
			next = phaseWaiting
		}
		if err := run.enter(next); err != nil {
			return err
		}
	}
}

// waitForPackageManager waits until no package-manager lock is held. It
// finishes with package_manager_busy (done) once the wait, counted from
// the first attempt, exceeds LockWait.
func (run *helper) waitForPackageManager(ctx context.Context) (bool, error) {
	options := run.options
	deadline := run.state.WaitStartedAt.Add(options.LockWait)
	for {
		lock, held := options.Locks()
		if !held {
			return false, nil
		}
		if !options.Now().Before(deadline) {
			return true, run.finish(CodePackageManagerBusy, fmt.Sprintf("%s was still held after waiting %s", lock, options.LockWait))
		}
		if err := options.Sleep(ctx, min(options.LockPoll, max(deadline.Sub(options.Now()), time.Millisecond))); err != nil {
			return false, err
		}
	}
}

// audit stops when dpkg reports an interrupted install, which the agent
// never repairs on its own.
func (run *helper) audit(ctx context.Context) (string, error) {
	if !run.apt() {
		return phaseRefresh, nil
	}
	dpkg := run.options.Packages.Tool("dpkg")
	if dpkg == "" {
		return phaseRefresh, nil
	}
	output, err := run.command(ctx, nil, DpkgAudit(dpkg), auditTimeout)
	if err != nil {
		return "", run.finish(CodeUpgradeFailed, "dpkg --audit: "+err.Error())
	}
	if output.ExitCode != 0 || strings.TrimSpace(string(output.Text)) != "" {
		return "", run.finish(CodeDpkgInterrupted, "dpkg reports an interrupted install; run `dpkg --configure -a` on the server and try again")
	}
	return phaseRefresh, nil
}

// refresh refreshes the metadata of every repository. A failure (an EOL
// mirror, no network) is noted; the existing metadata still tells what is
// pending.
func (run *helper) refresh(ctx context.Context) string {
	argv, err := run.options.Packages.RefreshCommand()
	if err != nil {
		run.note("the package lists were not refreshed: " + err.Error())
		return phaseAgent
	}
	env := []string{}
	if run.apt() {
		env = AptEnvironment(false)
	}
	output, err := run.command(ctx, env, argv, refreshTimeout)
	switch {
	case err != nil:
		run.note("refreshing the package lists failed: " + err.Error())
	case output.ExitCode != 0:
		run.note(fmt.Sprintf("refreshing the package lists exited with status %d", output.ExitCode))
	}
	return phaseAgent
}

// agentUpdate updates the agent first when the request names a version.
// Its outcome is reported, never a reason to stop.
func (run *helper) agentUpdate(ctx context.Context) string {
	if run.state.AgentDone || run.request.AgentVersion == "" || run.options.AgentUpdate == nil {
		run.state.AgentDone = true
		return phaseUpgrade
	}
	run.state.AgentUpdate = run.options.AgentUpdate(ctx, run.request, &run.output)
	run.state.AgentDone = true
	return phaseUpgrade
}

// pending lists the pending updates, waiting briefly while a package
// manager holds its lock.
func (run *helper) pending(ctx context.Context) (*collector.UpdatesInfo, error) {
	for attempt := 0; ; attempt++ {
		info, err := run.options.Packages.PendingUpdates(ctx)
		if !errors.Is(err, collector.ErrPackageManagerBusy) || attempt >= 3 {
			return info, err
		}
		if err := run.options.Sleep(ctx, run.options.LockPoll); err != nil {
			return nil, err
		}
	}
}

// inScope returns the pending updates of the scope, never serverchief, as
// name.arch keys and as the apt-get arguments that upgrade them.
func (run *helper) inScope(info *collector.UpdatesInfo) (keys []string, arguments []string) {
	for _, item := range info.Items {
		if item.Name == updater.PackageName || run.request.Scope == ScopeSecurity && !item.Security {
			continue
		}
		keys = append(keys, item.Name+"."+item.Arch)
		argument := item.Name
		if item.Arch != "" && item.Arch != "all" {
			argument += ":" + item.Arch
		}
		arguments = append(arguments, argument)
	}
	slices.Sort(keys)
	slices.Sort(arguments)
	return slices.Compact(keys), slices.Compact(arguments)
}

// upgrade installs the updates of the scope.
func (run *helper) upgrade(ctx context.Context) (string, error) {
	packages := run.options.Packages
	info, err := run.pending(ctx)
	if errors.Is(err, collector.ErrPackageManagerBusy) {
		run.state.WaitStartedAt = run.options.Now().UTC()
		return phaseWaiting, nil
	}
	if err != nil {
		return "", run.finish(CodeUpgradeFailed, "list the pending updates: "+err.Error())
	}
	keys, arguments := run.inScope(info)
	if run.state.Planned == nil {
		run.state.Planned = append([]string{}, keys...)
	}
	manager := packages.Manager()
	if manager != ManagerApt && run.request.Scope == ScopeSecurity && !packages.HasSecurityMetadata(ctx, len(info.Items)) {
		return "", run.finish(CodeSecurityUnavailable, "the enabled repositories publish no security advisories (updateinfo), so security updates cannot be told apart; install all updates instead")
	}
	if len(keys) == 0 {
		run.state.Ran = true
		return phaseFinishing, nil
	}

	var env, argv []string
	if manager == ManagerApt {
		env = AptEnvironment(run.request.RestartServices)
		argv, err = AptInstall(packages.Tool("apt-get"), arguments)
	} else {
		argv, err = RPMUpgrade(packages.Tool(manager), run.request.Scope)
	}
	if err != nil {
		return "", run.finish(CodeUpgradeFailed, err.Error())
	}
	output, err := run.command(ctx, env, argv, installTimeout)
	run.state.Ran, run.state.ExitCode = true, output.ExitCode
	if err != nil {
		run.state.Failure = err.Error()
	} else if output.ExitCode != 0 {
		run.state.Failure = fmt.Sprintf("%s exited with status %d: %s", baseName(argv[0]), output.ExitCode, lastLines(output.Text, 3))
	}
	if manager == ManagerApt && run.request.RestartServices {
		if packages.Tool("needrestart") == "" {
			run.note("needrestart is not installed, so no services were restarted")
		} else {
			run.state.Restarted = true
			run.state.Services = capServices(needrestartServices(output.Text))
		}
	}
	return phaseServices, nil
}

// restartServices restarts the services that use updated files on dnf and
// yum hosts; on apt hosts needrestart did it during the install.
func (run *helper) restartServices(ctx context.Context) string {
	if !run.request.RestartServices || run.apt() || len(run.state.Planned) == 0 || run.state.Restarted {
		return phaseFinishing
	}
	packages := run.options.Packages
	tool, systemctl := packages.Tool("needs-restarting"), packages.Tool("systemctl")
	switch {
	case !run.options.Systemd || systemctl == "":
		run.note("services are only restarted on systemd hosts, so none were restarted")
		return phaseFinishing
	case tool == "":
		run.note("needs-restarting (yum-utils or dnf-utils) is not installed, so no services were restarted")
		return phaseFinishing
	}
	output, err := run.command(ctx, nil, NeedsRestartingServices(tool), serviceTimeout)
	if err != nil || output.ExitCode != 0 {
		run.note("needs-restarting -s failed, so no services were restarted")
		return phaseFinishing
	}
	restarted := []string{}
	var units []string
	for _, line := range strings.Split(string(output.Text), "\n") {
		if unit := RestartableUnit(line); unit != "" && !slices.Contains(units, unit) {
			units = append(units, unit)
		}
	}
	for _, unit := range units {
		result, err := run.command(ctx, nil, TryRestart(systemctl, unit), serviceTimeout)
		if err == nil && result.ExitCode == 0 {
			restarted = append(restarted, unit)
		}
	}
	run.state.Restarted, run.state.Services = true, capServices(restarted)
	return phaseFinishing
}

// finishUpgrade counts what changed, checks whether a reboot is required
// and writes the result.
func (run *helper) finishUpgrade(ctx context.Context) error {
	data := run.data()
	info, err := run.pending(ctx)
	if err != nil {
		run.note("the pending updates could not be listed afterwards: " + err.Error())
	} else {
		remaining, _ := run.inScope(info)
		upgraded := 0
		for _, key := range run.state.Planned {
			if !slices.Contains(remaining, key) {
				upgraded++
			}
		}
		heldBack := len(remaining)
		data.Upgraded, data.HeldBack, data.RebootRequired = &upgraded, &heldBack, info.RebootRequired
	}
	switch {
	case run.state.Failure != "":
		data.Code = CodeUpgradeFailed
	case len(run.state.Planned) == 0:
		data.Code = CodeNothingToUpgrade
	default:
		data.Code = CodeUpgraded
	}
	if run.state.Restarted {
		data.ServicesRestarted = run.state.Services
		if data.ServicesRestarted == nil {
			data.ServicesRestarted = []string{}
		}
	}
	return run.write(data, run.state.Failure)
}

// data is the result data so far.
func (run *helper) data() Data {
	return Data{Scope: run.request.Scope, AgentUpdate: run.state.AgentUpdate}
}

// finish ends the request early with code.
func (run *helper) finish(code, detail string) error {
	data := run.data()
	data.Code = code
	return run.write(data, detail)
}

// write writes the output, then the result, then removes the request and
// state. The daemon reads the request before the result, so it never sees
// neither.
func (run *helper) write(data Data, detail string) error {
	status := StatusFailed
	if data.Code == CodeUpgraded || data.Code == CodeNothingToUpgrade {
		status = StatusSucceeded
	}
	parts := []string{}
	if detail != "" {
		parts = append(parts, detail)
	}
	parts = append(parts, run.state.Notes...)
	result := Result{
		CommandID:  run.request.CommandID,
		Status:     status,
		Detail:     truncateRunes(strings.Join(parts, "; "), maximumDetailRunes),
		Data:       data,
		FinishedAt: run.options.Now().UTC().Truncate(time.Second),
	}
	text, _ := run.output.Text()
	if err := run.options.Files.writeOutput(text); err != nil {
		return fmt.Errorf("write the upgrade output: %w", err)
	}
	if err := run.options.Files.writeResult(result); err != nil {
		return fmt.Errorf("write the upgrade result: %w", err)
	}
	log.Printf("[upgrade] %s finished: %s %s", run.request.CommandID, status, data.Code)
	return run.options.Files.cleanup()
}

// needrestartServices reads the services needrestart restarted from its
// output in mode a:
//
//	Restarting services...
//	 systemctl restart nginx.service php8.3-fpm.service
//
// Services it only lists ("Service restarts being deferred:") are left
// out.
func needrestartServices(output []byte) []string {
	services := []string{}
	restarting := false
	for _, line := range strings.Split(string(output), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "Restarting services...":
			restarting = true
		case restarting && strings.HasPrefix(trimmed, "systemctl restart "):
			for _, unit := range strings.Fields(trimmed)[2:] {
				if unitPattern.MatchString(unit) && !slices.Contains(services, unit) {
					services = append(services, unit)
				}
			}
		default:
			restarting = false
		}
	}
	return services
}

func capServices(services []string) []string {
	if len(services) > MaximumServices {
		return services[:MaximumServices]
	}
	return services
}

func lastLines(output []byte, count int) string {
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) > count {
		kept = kept[len(kept)-count:]
	}
	return strings.Join(kept, " / ")
}

func baseName(path string) string {
	if index := strings.LastIndex(path, "/"); index >= 0 {
		return path[index+1:]
	}
	return path
}

func truncateRunes(message string, maximum int) string {
	runes := []rune(message)
	if len(runes) <= maximum {
		return message
	}
	return string(runes[:maximum-1]) + "…"
}
