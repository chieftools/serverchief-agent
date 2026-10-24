package pkglock

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

const holdEnv = "PKGLOCK_TEST_HOLD"

// TestMain doubles as the lock-holding helper process: fcntl locks belong
// to a process, so F_GETLK only sees a lock taken by another one.
func TestMain(m *testing.M) {
	if path := os.Getenv(holdEnv); path != "" {
		os.Exit(holdLock(path))
	}
	os.Exit(m.Run())
}

func holdLock(path string) int {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return 3
	}
	lock := syscall.Flock_t{Type: syscall.F_WRLCK}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock); err != nil {
		return 4
	}
	os.Stdout.WriteString("locked\n")
	_, _ = io.Copy(io.Discard, os.Stdin) // hold until the test closes stdin
	return 0
}

// holder is a child process holding an fcntl write lock.
type holder struct {
	command *exec.Cmd
	stdin   io.Closer
	pid     int
	stopped bool
}

// startHolder takes an fcntl write lock on path in a child process and
// returns once the lock is held.
func startHolder(t *testing.T, path string) *holder {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^$")
	command.Env = append(os.Environ(), holdEnv+"="+path)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	child := &holder{command: command, stdin: stdin, pid: command.Process.Pid}
	t.Cleanup(func() {
		if !child.stopped {
			stdin.Close()
			_ = command.Wait()
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("helper did not lock %s: %q %v", path, line, err)
	}
	return child
}

func (child *holder) stop(t *testing.T) {
	t.Helper()
	child.stopped = true
	child.stdin.Close()
	if err := child.command.Wait(); err != nil {
		t.Fatalf("helper exited with %v", err)
	}
}

func TestProbeFcntlBusyAndFree(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "lock-frontend")
	if _, held, err := probeFcntl(path); err != nil || held {
		t.Fatalf("missing file: held %v, err %v", held, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("probing created the lock file")
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, held, err := probeFcntl(path); err != nil || held {
		t.Fatalf("free file: held %v, err %v", held, err)
	}

	holder := startHolder(t, path)
	pid, held, err := probeFcntl(path)
	if err != nil || !held || pid != holder.pid {
		t.Fatalf("held file: pid %d (holder %d), held %v, err %v", pid, holder.pid, held, err)
	}
	// Probing twice must not disturb the holder's lock.
	if _, held, _ := probeFcntl(path); !held {
		t.Fatal("second probe lost the lock")
	}
	holder.stop(t)
	if _, held, err := probeFcntl(path); err != nil || held {
		t.Fatalf("released file: held %v, err %v", held, err)
	}
}

func TestInspectorFindsFcntlLockUnderRoot(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"var/lib/dpkg", "var/lib/apt/lists"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"var/lib/dpkg/lock-frontend", "var/lib/dpkg/lock", "var/lib/apt/lists/lock"} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inspector := &Inspector{Root: root, Alive: func(int) bool { return false }}
	if lock, busy := inspector.Busy(); busy {
		t.Fatalf("free tree reported %s", lock)
	}
	holder := startHolder(t, filepath.Join(root, "var/lib/apt/lists/lock"))
	lock, busy := inspector.Busy()
	if !busy || lock.Path != "/var/lib/apt/lists/lock" || lock.Kind != "fcntl" || lock.PID != holder.pid {
		t.Fatalf("busy = %v %+v", busy, lock)
	}
	if lock.String() != "/var/lib/apt/lists/lock (fcntl, pid "+strconv.Itoa(holder.pid)+")" {
		t.Fatalf("String() = %s", lock)
	}
	holder.stop(t)
	if lock, busy := inspector.Busy(); busy {
		t.Fatalf("released tree reported %s", lock)
	}
}

func TestInspectorPIDFiles(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A real exited process gives a PID that is (almost certainly) dead.
	exited := exec.Command(os.Args[0], "-test.run=^$")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	dead := exited.Process.Pid
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() })
	live := sleeper.Process.Pid

	inspector := &Inspector{Root: root, Fcntl: []string{}}
	if _, busy := inspector.Busy(); busy {
		t.Fatal("no PID files reported busy")
	}
	write("var/run/yum.pid", strconv.Itoa(dead)+"\n")
	write("var/lib/dnf/rpmdb_lock.pid", "")
	write("var/cache/dnf/metadata_lock.pid", "not a pid")
	if lock, busy := inspector.Busy(); busy {
		t.Fatalf("stale, empty and malformed PID files reported %s", lock)
	}
	write("var/run/yum.pid", strconv.Itoa(os.Getpid()))
	if lock, busy := inspector.Busy(); busy {
		t.Fatalf("our own PID reported %s", lock)
	}

	// dnf's PID files are found by glob as well.
	write("var/cache/dnf/download_lock.pid", strconv.Itoa(live))
	lock, busy := inspector.Busy()
	if !busy || lock.Path != "/var/cache/dnf/download_lock.pid" || lock.Kind != "pid" || lock.PID != live {
		t.Fatalf("live dnf lock = %v %+v", busy, lock)
	}
	if err := os.Remove(filepath.Join(root, "var/cache/dnf/download_lock.pid")); err != nil {
		t.Fatal(err)
	}
	write("var/cache/dnf/custom_lock.pid", strconv.Itoa(live))
	if lock, busy := inspector.Busy(); !busy || lock.Path != "/var/cache/dnf/custom_lock.pid" {
		t.Fatalf("globbed dnf lock = %v %+v", busy, lock)
	}
	if err := os.Remove(filepath.Join(root, "var/cache/dnf/custom_lock.pid")); err != nil {
		t.Fatal(err)
	}
	write("var/run/yum.pid", strconv.Itoa(live)+"\n")
	if lock, busy := inspector.Busy(); !busy || lock.Path != "/var/run/yum.pid" || lock.PID != live {
		t.Fatalf("live yum lock = %v %+v", busy, lock)
	}
	_ = sleeper.Process.Kill()
	_ = sleeper.Wait()
	if lock, busy := inspector.Busy(); busy {
		t.Fatalf("killed yum reported %s", lock)
	}
}

func TestInspectorFakes(t *testing.T) {
	inspector := &Inspector{
		Fcntl:    []string{"/a", "/b"},
		PIDFiles: []string{"/yum.pid"},
		Globs:    []string{},
		Probe: func(path string) (int, bool, error) {
			if path == "/a" {
				return 0, false, os.ErrPermission // unreadable: not held
			}
			return 0, path == "/b", nil
		},
		ReadFile: func(string) ([]byte, error) { return []byte("42"), nil },
		Alive:    func(pid int) bool { return pid == 42 },
	}
	if lock, busy := inspector.Busy(); !busy || lock != (Lock{Path: "/b", Kind: "fcntl"}) || lock.String() != "/b (fcntl)" {
		t.Fatalf("busy = %v %+v", busy, lock)
	}
	inspector.Fcntl = []string{"/a"}
	if lock, busy := inspector.Busy(); !busy || lock != (Lock{Path: "/yum.pid", Kind: "pid", PID: 42}) {
		t.Fatalf("busy = %v %+v", busy, lock)
	}
}

func TestTrackerHeldForAndPatience(t *testing.T) {
	held := false
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	tracker := &Tracker{
		Inspector: &Inspector{
			Fcntl: []string{"/lock"}, PIDFiles: []string{}, Globs: []string{},
			Probe: func(string) (int, bool, error) { return 7, held, nil },
		},
		Patience: 2 * time.Hour,
		Now:      func() time.Time { return now },
	}
	if _, _, busy := tracker.Check(); busy {
		t.Fatal("free lock reported held")
	}
	held = true
	if _, heldFor, busy := tracker.Check(); !busy || heldFor != 0 {
		t.Fatalf("first observation: %v %s", busy, heldFor)
	}
	now = now.Add(90 * time.Minute)
	if lock, blocking := tracker.Blocking(); !blocking || lock.PID != 7 {
		t.Fatal("lock held 90 minutes is not blocking")
	}
	now = now.Add(31 * time.Minute)
	if _, heldFor, busy := tracker.Check(); !busy || heldFor != 121*time.Minute {
		t.Fatalf("held for %s", heldFor)
	}
	if _, blocking := tracker.Blocking(); blocking {
		t.Fatal("lock held over the patience still blocks")
	}
	// One free observation resets the clock.
	held = false
	tracker.Check()
	held = true
	if _, heldFor, _ := tracker.Check(); heldFor != 0 {
		t.Fatalf("held for %s after a free observation", heldFor)
	}
}
