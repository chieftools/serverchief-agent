// Package pkglock tells whether a package manager is running by inspecting
// its locks. It never takes a lock: fcntl locks are probed with F_GETLK
// and PID lock files are read and checked for a live process, so the agent
// can never be the reason an administrator sees "apt is locked".
package pkglock

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FcntlLocks are the lock files package managers hold with fcntl(2) while
// they run.
var FcntlLocks = []string{
	"/var/lib/dpkg/lock-frontend",  // apt and dpkg frontends (dpkg 1.19+)
	"/var/lib/dpkg/lock",           // dpkg
	"/var/lib/apt/lists/lock",      // apt-get update
	"/var/cache/apt/archives/lock", // apt downloads
	"/var/lib/rpm/.rpm.lock",       // rpm transactions (EL7-EL9)
	"/usr/lib/sysimage/rpm/.rpm.lock",
	"/run/dnf/rpmtransaction.lock", // dnf5
}

// PIDLocks are lock files that hold the PID of the running process. yum
// keeps its PID file for the whole run. dnf 4 writes one per lock and
// removes it on unlock; PIDLockGlobs catches the ones not listed here.
var PIDLocks = []string{
	"/var/run/yum.pid",
	"/var/cache/dnf/metadata_lock.pid",
	"/var/cache/dnf/download_lock.pid",
	"/var/lib/dnf/rpmdb_lock.pid",
}

// PIDLockGlobs match further dnf lock PID files.
var PIDLockGlobs = []string{
	"/var/cache/dnf/*_lock.pid",
	"/var/lib/dnf/*_lock.pid",
}

// Lock is a held package-manager lock.
type Lock struct {
	Path string
	// Kind is "fcntl" or "pid".
	Kind string
	// PID holds the lock, or 0 when unknown (an OFD lock).
	PID int
}

func (lock Lock) String() string {
	if lock.PID > 0 {
		return fmt.Sprintf("%s (%s, pid %d)", lock.Path, lock.Kind, lock.PID)
	}
	return fmt.Sprintf("%s (%s)", lock.Path, lock.Kind)
}

// Inspector checks the lock files. The zero value of a function field
// uses the real system call.
type Inspector struct {
	// Root prefixes every path, for tests.
	Root string
	// Fcntl, PIDFiles and Globs default to the package lists above.
	Fcntl    []string
	PIDFiles []string
	Globs    []string

	// Probe reports the PID of a process holding a lock that conflicts with
	// a write lock on the whole file. A missing file is not held.
	Probe func(path string) (pid int, held bool, err error)
	// Alive reports whether a process exists.
	Alive func(pid int) bool
	// ReadFile and Glob read PID lock files.
	ReadFile func(path string) ([]byte, error)
	Glob     func(pattern string) ([]string, error)
}

// New returns an inspector for the running system.
func New() *Inspector { return &Inspector{} }

// Busy returns the first held lock. Errors other than a missing file are
// treated as "not held": an unreadable lock must not stop collection
// forever.
func (inspector *Inspector) Busy() (Lock, bool) {
	probe := inspector.Probe
	if probe == nil {
		probe = probeFcntl
	}
	for _, name := range inspector.or(inspector.Fcntl, FcntlLocks) {
		path := inspector.path(name)
		if pid, held, err := probe(path); err == nil && held {
			return Lock{Path: name, Kind: "fcntl", PID: pid}, true
		}
	}

	names := slices.Clone(inspector.or(inspector.PIDFiles, PIDLocks))
	glob := inspector.Glob
	if glob == nil {
		glob = filepath.Glob
	}
	for _, pattern := range inspector.or(inspector.Globs, PIDLockGlobs) {
		matches, _ := glob(inspector.path(pattern))
		for _, match := range matches {
			name := match
			if inspector.Root != "" {
				name = "/" + strings.TrimPrefix(strings.TrimPrefix(match, inspector.Root), "/")
			}
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	for _, name := range names {
		if pid, ok := inspector.livePID(inspector.path(name)); ok {
			return Lock{Path: name, Kind: "pid", PID: pid}, true
		}
	}
	return Lock{}, false
}

// livePID reads a PID lock file and reports whether its process runs.
// A missing, empty or malformed file, or a stale PID, is not held.
func (inspector *Inspector) livePID(path string) (int, bool) {
	read := inspector.ReadFile
	if read == nil {
		read = readSmallFile
	}
	data, err := read(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return 0, false
	}
	alive := inspector.Alive
	if alive == nil {
		alive = processAlive
	}
	return pid, alive(pid)
}

func (inspector *Inspector) or(configured, defaults []string) []string {
	if configured != nil {
		return configured
	}
	return defaults
}

func (inspector *Inspector) path(name string) string {
	if inspector.Root == "" {
		return name
	}
	return filepath.Join(inspector.Root, name)
}

// readSmallFile reads at most 64 bytes: a PID file is never longer, and a
// huge file must not be read in full.
func readSmallFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	buffer := make([]byte, 64)
	count, err := file.Read(buffer)
	if count == 0 && err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buffer[:count], nil
}

// Tracker remembers since when a lock has been held, so callers can stop
// waiting after a cap (a hung dpkg must not stop collection forever).
type Tracker struct {
	Inspector *Inspector
	// Patience is how long a held lock defers collection; after that the
	// collectors that can run next to a package manager run anyway.
	Patience time.Duration
	Now      func() time.Time

	mu    sync.Mutex
	since time.Time
}

// DefaultPatience is the protocol's two hours.
const DefaultPatience = 2 * time.Hour

// NewTracker returns a tracker for the running system.
func NewTracker() *Tracker {
	return &Tracker{Inspector: New(), Patience: DefaultPatience}
}

// Check inspects the locks. heldFor is how long some lock has been held
// without an observation of every lock free in between.
func (tracker *Tracker) Check() (lock Lock, heldFor time.Duration, held bool) {
	now := time.Now()
	if tracker.Now != nil {
		now = tracker.Now()
	}
	lock, held = tracker.Inspector.Busy()
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if !held {
		tracker.since = time.Time{}
		return Lock{}, 0, false
	}
	if tracker.since.IsZero() {
		tracker.since = now
	}
	return lock, now.Sub(tracker.since), true
}

// Blocking reports a held lock that has not yet outlasted Patience.
func (tracker *Tracker) Blocking() (Lock, bool) {
	lock, heldFor, held := tracker.Check()
	patience := tracker.Patience
	if patience <= 0 {
		patience = DefaultPatience
	}
	return lock, held && heldFor < patience
}
