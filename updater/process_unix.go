//go:build unix

package updater

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
)

func fileOwner(info fs.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}

// processAlive uses kill(pid, 0): EPERM still means the process exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// AcquireLock takes the helper lock: a write lock on the whole file with
// fcntl(F_SETLK), held until release or exit. acquired is false when
// another process holds it.
func AcquireLock(path string) (release func(), acquired bool, err error) {
	// O_NOFOLLOW: the state directory belongs to the unprivileged daemon,
	// so root never follows a link planted there.
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOCTTY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, false, err
	}
	lock := syscall.Flock_t{Type: syscall.F_WRLCK}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { _ = file.Close() }, true, nil
}

// LockHeld reports whether another process holds the helper lock. It
// probes with F_GETLK and never takes the lock, so it cannot keep a
// starting helper from getting it.
func LockHeld(path string) bool {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	lock := syscall.Flock_t{Type: syscall.F_WRLCK}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_GETLK, &lock); err != nil {
		return false
	}
	return lock.Type != syscall.F_UNLCK
}

// SpawnDetached starts binary with args in a new session, stdin from
// /dev/null and stdout/stderr appended to logPath, and does not wait for
// it. The caller reaps it (or exits) to complete a double fork.
func SpawnDetached(binary string, args []string, logPath string) (*exec.Cmd, error) {
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", logPath, err)
	}
	defer logFile.Close()
	command := exec.Command(binary, args...)
	command.Stdin = nil
	command.Stdout, command.Stderr = logFile, logFile
	command.Dir = "/"
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	return command, nil
}
