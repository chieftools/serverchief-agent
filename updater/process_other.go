//go:build !unix

package updater

import (
	"errors"
	"io/fs"
	"os/exec"
)

var errUnsupported = errors.New("agent updates need a Unix system")

func fileOwner(fs.FileInfo) (int, bool) { return 0, false }

func processAlive(int) bool { return false }

func AcquireLock(string) (func(), bool, error) { return nil, false, errUnsupported }

func LockHeld(string) bool { return false }

func SpawnDetached(string, []string, string) (*exec.Cmd, error) { return nil, errUnsupported }
