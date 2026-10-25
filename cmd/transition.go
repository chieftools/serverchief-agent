package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"go.chief.tools/serverchief-agent/updater"
)

const (
	// rootOverride is where the package installs the root unit.
	rootOverride   = "/etc/systemd/system/serverchief.service"
	transitionUnit = "serverchief-unprivilege"
)

var transitionalMarker = regexp.MustCompile(`(?m)^# serverchief-root-override: transitional$`)

// watchPrivilegeTransition finishes an update from a version that ran as
// root: the package left a transitional root override so that the old
// update helper accepted this daemon's readiness. Once that update is
// settled, a transient unit hands the state to the serverchief user and
// restarts the service unprivileged.
func watchPrivilegeTransition(ctx context.Context, updates *updater.Controller) {
	if os.Geteuid() != 0 || !isTransitional() {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			if updateSettled(updates) {
				log.Printf("[agent] switching to the unprivileged serverchief user")
				if err := exec.CommandContext(ctx, "systemd-run", "--unit="+transitionUnit, "--quiet", updater.Binary, "internal-unprivilege").Run(); err != nil {
					log.Printf("[agent] start the switch to the unprivileged user: %v", err)
				}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func isTransitional() bool {
	data, err := os.ReadFile(rootOverride)
	return err == nil && transitionalMarker.Match(data)
}

// updateSettled is true once no update is pending, running or waiting to
// be reported.
func updateSettled(updates *updater.Controller) bool {
	request, err := updates.PendingRequest()
	if err != nil || request != nil {
		return false
	}
	result, err := updates.ReadResult()
	if err != nil || result != nil {
		return false
	}
	return !updater.LockHeld(updates.Store.LockPath())
}

// newInternalUnprivilegeCommand performs the switch outside the root
// unit's sandbox, which keeps /etc read-only.
func newInternalUnprivilegeCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "internal-unprivilege",
		Short:  "Switch the agent from root to the serverchief user (run by the agent)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if os.Geteuid() != 0 {
				return errors.New("internal-unprivilege must run as root")
			}
			if !isTransitional() {
				return nil
			}
			account, err := user.Lookup("serverchief")
			if err != nil {
				return fmt.Errorf("the serverchief user is missing: %w", err)
			}
			uid, _ := strconv.Atoi(account.Uid)
			gid, _ := strconv.Atoi(account.Gid)
			for _, directory := range []string{updater.DefaultDirectory, "/var/log/serverchief"} {
				if err := chownTree(directory, uid, gid); err != nil {
					return err
				}
			}
			if err := os.Remove(rootOverride); err != nil {
				return err
			}
			ctx := command.Context()
			if err := exec.CommandContext(ctx, "systemctl", "daemon-reload").Run(); err != nil {
				return err
			}
			for _, args := range [][]string{{"enable", "serverchief-helper.path"}, {"start", "serverchief-helper.path"}, {"--no-block", "restart", "serverchief.service"}} {
				if err := exec.CommandContext(ctx, "systemctl", args...).Run(); err != nil {
					return fmt.Errorf("systemctl %v: %w", args, err)
				}
			}
			return nil
		},
	}
}

// chownTree hands a directory tree to uid:gid without following links.
func chownTree(root string, uid, gid int) error {
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
