package updater

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"go.chief.tools/serverchief-agent/initsys"
)

// capabilityCacheFor is how long a capability probe is reused for
// heartbeats. An unavailable result is probed again sooner: right after
// an update the new daemon starts inside the package's postinst, while
// dpkg still reports the package half-configured. An update command
// always probes again.
const (
	capabilityCacheFor            = 10 * time.Minute
	unavailableCapabilityCacheFor = time.Minute
)

// Controller is the daemon's side of agent updates: the capability, the
// hand-over to the helper, the readiness marker and the result.
type Controller struct {
	Store   Store
	Version string
	// Detect probes the capability (Probe.Detect).
	Detect func(ctx context.Context) Capability
	// Launch starts the helper (Launcher.Launch).
	Launch func(ctx context.Context) error
	// HelperRunning reports whether a helper holds the lock.
	HelperRunning func() bool
	Now           func() time.Time

	mu       sync.Mutex
	cached   *Capability
	cachedAt time.Time
}

// NewController wires the controller of the packaged agent, with its
// files in directory owned by owner (DefaultDirectory and root in
// production).
func NewController(version string, init initsys.Kind, directory string, owner int) *Controller {
	store := Store{Directory: directory, Owner: owner}
	probe := Probe{Version: version, Init: init, PID: os.Getpid()}
	launcher := Launcher{Init: init}
	return &Controller{
		Store:         store,
		Version:       version,
		Detect:        probe.Detect,
		Launch:        launcher.Launch,
		HelperRunning: func() bool { return LockHeld(store.LockPath()) },
	}
}

func (controller *Controller) now() time.Time {
	if controller.Now != nil {
		return controller.Now()
	}
	return time.Now()
}

// Capability returns the cached capability, probing when the cache is
// older than ten minutes or fresh is set.
func (controller *Controller) Capability(ctx context.Context, fresh bool) Capability {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	now := controller.now()
	if !fresh && controller.cached != nil && !now.Before(controller.cachedAt) {
		ttl := capabilityCacheFor
		if !controller.cached.Available {
			ttl = unavailableCapabilityCacheFor
		}
		if now.Sub(controller.cachedAt) < ttl {
			return *controller.cached
		}
	}
	capability := unavailable("agent updates are not supported by this build")
	if controller.Detect != nil {
		capability = controller.Detect(ctx)
	}
	if controller.cached == nil || *controller.cached != capability {
		if capability.Available {
			log.Printf("[update] agent updates are available")
		} else {
			log.Printf("[update] agent updates are unavailable: %s", capability.Reason)
		}
	}
	controller.cached, controller.cachedAt = &capability, now
	return capability
}

// Invalidate makes the next heartbeat probe the capability again, after
// an update changed the installation.
func (controller *Controller) Invalidate() {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.cachedAt = time.Time{}
}

// PendingRequest returns the request a helper is working on, or nil.
func (controller *Controller) PendingRequest() (*Request, error) {
	return controller.Store.ReadRequest()
}

// Start persists the request and launches the helper. When the launch
// fails the request is removed again, so no helper picks it up later.
func (controller *Controller) Start(ctx context.Context, request Request) error {
	if err := controller.Store.WriteRequest(request); err != nil {
		return fmt.Errorf("write the update request: %w", err)
	}
	if controller.Launch == nil {
		_ = controller.Store.RemoveRequest()
		return errors.New("no way to launch the update helper")
	}
	if err := controller.Launch(ctx); err != nil {
		_ = controller.Store.RemoveRequest()
		return err
	}
	return nil
}

// Resume launches the helper again when a request is pending but no
// helper holds the lock (it crashed, or the host rebooted). It reports
// whether it launched one.
func (controller *Controller) Resume(ctx context.Context) (bool, error) {
	request, err := controller.Store.ReadRequest()
	if err != nil || request == nil {
		return false, err
	}
	if controller.HelperRunning != nil && controller.HelperRunning() {
		return false, nil
	}
	if controller.Launch == nil {
		return false, errors.New("no way to launch the update helper")
	}
	log.Printf("[update] request %s is pending but no helper runs; launching it again", request.CommandID)
	return true, controller.Launch(ctx)
}

// WriteReadiness tells the helper that this daemon, running Version,
// started and sent a successful heartbeat.
func (controller *Controller) WriteReadiness(commandID string) error {
	if e2eNeverReady {
		log.Printf("[update] test build: not writing the readiness marker for %s", commandID)
		return nil
	}
	return controller.Store.WriteReadiness(Readiness{
		CommandID: commandID, Version: controller.Version, PID: os.Getpid(), WrittenAt: controller.now().UTC().Truncate(time.Second),
	})
}

// ReadResult returns the helper's result, or nil.
func (controller *Controller) ReadResult() (*Result, error) { return controller.Store.ReadResult() }

// RemoveResult deletes the result of commandID once it was reported.
func (controller *Controller) RemoveResult(commandID string) error {
	return controller.Store.RemoveResult(commandID)
}
