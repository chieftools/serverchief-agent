package cmd

import (
	"errors"
	"log"
	"os"

	"github.com/spf13/cobra"

	"go.chief.tools/serverchief-agent/logging"
	"go.chief.tools/serverchief-agent/pkglock"
	"go.chief.tools/serverchief-agent/updater"
)

// newInternalUpdateCommand is the detached update helper the daemon
// launches for update_agent. It is not meant to be run by hand: without a
// pending /var/lib/serverchief/update-request.json it does nothing.
func newInternalUpdateCommand() *cobra.Command {
	var detach bool
	command := &cobra.Command{
		Use:    "internal-update",
		Short:  "Install a requested agent update (run by the agent)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if os.Geteuid() != 0 {
				return errors.New("internal-update must run as root")
			}
			if detach {
				// Second fork: start the helper in its own session and exit,
				// so it is orphaned to init and outlives the daemon.
				_, err := updater.SpawnDetached(updater.Binary, []string{"internal-update"}, updater.DefaultLogPath)
				return err
			}
			if _, err := logging.Setup(""); err != nil {
				return err
			}
			store := updater.NewStore()
			release, acquired, err := updater.AcquireLock(store.LockPath())
			if err != nil {
				return err
			}
			if !acquired {
				log.Printf("[update] another update helper is running")
				return nil
			}
			defer release()
			inspector := pkglock.New()
			return updater.RunHelper(command.Context(), updater.HelperOptions{
				Store: store,
				Locks: func() (string, bool) {
					lock, held := inspector.Busy()
					return lock.String(), held
				},
			})
		},
	}
	command.Flags().BoolVar(&detach, "detach", false, "Start the helper detached and exit")
	_ = command.Flags().MarkHidden("detach")
	return command
}
