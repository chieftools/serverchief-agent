// Package optout is the operator's opt-out of remote actions: the
// capabilities `serverchief disable` turned off on this server.
//
// Remote actions such as rebooting are available by default. The state
// lives in its own root-owned file, apart from config.json, which the agent
// only reads: nothing the backend sends changes it. A missing file means
// nothing is turned off. A file that cannot be read or trusted turns every
// capability off, so a damaged opt-out never becomes an opt-in.
package optout

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"go.chief.tools/serverchief-agent/inventory"
)

// Path is the packaged location of the opt-out file.
const Path = "/etc/serverchief/disabled-capabilities.json"

// Capabilities the operator can turn off. Agent self-updates are not among
// them: the team or server setting for automatic updates covers those.
const (
	// Reboot is the reboot command.
	Reboot = "reboot"
	// UpgradePackages is reserved for installing package updates, which a
	// later protocol revision adds; it can be turned off before then.
	UpgradePackages = "upgrade_packages"
)

// Known lists the capabilities the operator can turn off, sorted.
var Known = []string{Reboot, UpgradePackages}

const (
	fileVersion = 1
	// maxFileBytes caps what is read; the file holds a few names.
	maxFileBytes = 64 << 10
)

// namePattern accepts names a newer agent may have written, so a
// downgrade keeps them turned off rather than failing closed on all.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type file struct {
	Version  int      `json:"version"`
	Disabled []string `json:"disabled"`
}

// State is the opt-out as read from the file.
type State struct {
	// Disabled are the turned-off capabilities, sorted and never nil.
	Disabled []string
	// Err is why the file could not be trusted; every known capability is
	// then turned off.
	Err error
}

// IsDisabled reports whether name is turned off.
func (state State) IsDisabled(name string) bool {
	return slices.Contains(state.Disabled, name)
}

// Load reads the opt-out at path. With owner >= 0 the file must belong to
// that user (root in production) and must not be writable by its group or
// others; owner -1 skips the check, for tests.
func Load(path string, owner int) State {
	disabled, err := read(path, owner)
	if err != nil {
		return State{Disabled: slices.Clone(Known), Err: err}
	}
	return State{Disabled: disabled}
}

func read(path string, owner int) ([]string, error) {
	handle, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if owner >= 0 {
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != owner {
			return nil, fmt.Errorf("%s does not belong to uid %d", path, owner)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return nil, fmt.Errorf("%s is writable by its group or others", path)
		}
	}
	if info.Size() > maxFileBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxFileBytes)
	}
	data := make([]byte, info.Size())
	if _, err := handle.ReadAt(data, 0); err != nil && info.Size() > 0 {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var content file
	if err := json.Unmarshal(data, &content); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if content.Version != fileVersion {
		return nil, fmt.Errorf("%s has version %d, expected %d", path, content.Version, fileVersion)
	}
	for _, name := range content.Disabled {
		if !namePattern.MatchString(name) {
			return nil, fmt.Errorf("%s lists the invalid capability %q", path, name)
		}
	}
	return normalize(content.Disabled), nil
}

// Validate rejects names that are not known capabilities.
func Validate(names []string) error {
	var unknown []string
	for _, name := range names {
		if !slices.Contains(Known, name) {
			unknown = append(unknown, fmt.Sprintf("%q", name))
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("unknown capability %s; known capabilities are %s", strings.Join(unknown, ", "), strings.Join(Known, ", "))
	}
	return nil
}

// Save writes disabled to path atomically, readable by everyone (the
// unprivileged daemon reads it) and writable by its owner only.
func Save(path string, disabled []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(file{Version: fileVersion, Disabled: normalize(disabled)}, "", "  ")
	if err != nil {
		return err
	}
	return inventory.WriteFileAtomic(path, append(data, '\n'), 0o644)
}

// normalize sorts and deduplicates names, never returning nil.
func normalize(names []string) []string {
	result := append([]string{}, names...)
	slices.Sort(result)
	return slices.Compact(result)
}
