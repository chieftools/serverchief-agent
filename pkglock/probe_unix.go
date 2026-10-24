//go:build unix

package pkglock

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// probeFcntl asks the kernel with F_GETLK whether a write lock on the
// whole file could be placed. It opens the file read-only and never
// creates it. F_GETLK does not take a lock, and the agent holds no locks
// on these files, so closing the descriptor releases nothing.
func probeFcntl(path string) (int, bool, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOCTTY, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer file.Close()
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_GETLK, &lock); err != nil {
		return 0, false, err
	}
	if lock.Type == syscall.F_UNLCK {
		return 0, false, nil
	}
	// An open file description lock reports pid -1.
	return max(int(lock.Pid), 0), true, nil
}

// processAlive uses kill(pid, 0): EPERM still means the process exists.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
