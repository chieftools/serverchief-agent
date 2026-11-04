package upgrader

import (
	"context"
	"errors"
	"fmt"
	"log"

	"go.chief.tools/serverchief-agent/updater"
)

// Capability tells whether the agent can install package updates.
type Capability struct {
	Available bool
	Reason    string
}

// Controller is the daemon's side of package upgrades: the capability, the
// hand-over to the helper and its result.
type Controller struct {
	Files Files
	// Detect probes the capability, regardless of the operator's opt-out.
	Detect func(ctx context.Context) Capability
	// Launch starts the helper (Launcher, or the root helper's
	// start-package-upgrade action).
	Launch func(ctx context.Context) error
	// HelperRunning reports whether a helper holds the lock.
	HelperRunning func() bool
}

// NewController wires the controller of the packaged agent, with its
// files in directory owned by owner.
func NewController(directory string, owner int) *Controller {
	files := Files{Store: updater.Store{Directory: directory, Owner: owner}}
	return &Controller{
		Files:         files,
		HelperRunning: func() bool { return updater.LockHeld(files.LockPath()) },
	}
}

// Capability probes whether package updates can be installed.
func (controller *Controller) Capability(ctx context.Context) Capability {
	if controller.Detect == nil {
		return Capability{Reason: "installing package updates is not supported by this build"}
	}
	return controller.Detect(ctx)
}

// PendingRequest returns the request a helper is working on, or nil.
func (controller *Controller) PendingRequest() (*Request, error) {
	return controller.Files.ReadRequest()
}

// Start persists the request and launches the helper. When the launch
// fails the request is removed again, so no helper picks it up later.
func (controller *Controller) Start(ctx context.Context, request Request) error {
	if err := controller.Files.WriteRequest(request); err != nil {
		return fmt.Errorf("write the upgrade request: %w", err)
	}
	if controller.Launch == nil {
		_ = controller.Files.RemoveRequest()
		return errors.New("no way to launch the package upgrade helper")
	}
	if err := controller.Launch(ctx); err != nil {
		_ = controller.Files.RemoveRequest()
		return err
	}
	return nil
}

// Resume launches the helper again when a request is pending but no
// helper holds the lock (it crashed, or the host rebooted). It reports
// whether it launched one.
func (controller *Controller) Resume(ctx context.Context) (bool, error) {
	request, err := controller.Files.ReadRequest()
	if err != nil || request == nil {
		return false, err
	}
	if controller.HelperRunning != nil && controller.HelperRunning() {
		return false, nil
	}
	if controller.Launch == nil {
		return false, errors.New("no way to launch the package upgrade helper")
	}
	log.Printf("[upgrade] request %s is pending but no helper runs; launching it again", request.CommandID)
	return true, controller.Launch(ctx)
}

// ReadResult returns the helper's result, the end of its output and
// whether that was cut off, or nil.
func (controller *Controller) ReadResult() (*Result, string, bool, error) {
	return controller.Files.ReadResult()
}

// RemoveResult deletes the result and its output once they are journaled.
func (controller *Controller) RemoveResult() error { return controller.Files.RemoveResult() }
