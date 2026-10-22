package inventory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const stateVersion = 1

// SentDataset is what the agent last delivered for one dataset.
type SentDataset struct {
	Hash   string    `json:"hash"`
	SentAt time.Time `json:"sent_at"`
}

// State is the persisted delivery state.
type State struct {
	Version  int                    `json:"version"`
	Seq      uint64                 `json:"seq"`
	Datasets map[string]SentDataset `json:"datasets"`
}

// StateStore keeps State in a JSON file. Every change is written to a
// temporary file in the same directory, synced and renamed over the old
// file, so a crash leaves either the old or the new state.
type StateStore struct {
	path string

	mu    sync.Mutex
	state State
}

// OpenStateStore loads path, starting empty when it does not exist yet.
// A corrupt file is moved aside rather than failing the agent: the worst
// outcome of lost state is one full resend.
func OpenStateStore(path string) (*StateStore, error) {
	store := &StateStore{path: path, state: emptyState()}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var state State
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&state); err != nil || state.Version != stateVersion {
		corrupt := path + ".corrupt"
		log.Printf("[state] %s is unreadable; moving it to %s and resending every dataset", path, corrupt)
		_ = os.Rename(path, corrupt)
		return store, nil
	}
	if state.Datasets == nil {
		state.Datasets = make(map[string]SentDataset)
	}
	store.state = state
	return store, nil
}

func emptyState() State {
	return State{Version: stateVersion, Datasets: make(map[string]SentDataset)}
}

// Path returns the file backing the store.
func (store *StateStore) Path() string { return store.path }

// Hash returns the last sent hash of a dataset, or "".
func (store *StateStore) Hash(name string) string {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.state.Datasets[name].Hash
}

// Hashes returns every last sent hash, for heartbeats.
func (store *StateStore) Hashes() map[string]string {
	store.mu.Lock()
	defer store.mu.Unlock()
	hashes := make(map[string]string, len(store.state.Datasets))
	for name, dataset := range store.state.Datasets {
		hashes[name] = dataset.Hash
	}
	return hashes
}

// NextSeq reserves and persists the next envelope sequence number. It is
// persisted before use so a restart never reuses a number.
func (store *StateStore) NextSeq() (uint64, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	next := store.state
	next.Seq++
	if err := store.write(next); err != nil {
		return 0, err
	}
	store.state = next
	return next.Seq, nil
}

// MarkSent records hashes the backend acknowledged.
func (store *StateStore) MarkSent(hashes map[string]string, at time.Time) error {
	if len(hashes) == 0 {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	next := store.state
	next.Datasets = make(map[string]SentDataset, len(store.state.Datasets)+len(hashes))
	for name, dataset := range store.state.Datasets {
		next.Datasets[name] = dataset
	}
	for name, hash := range hashes {
		next.Datasets[name] = SentDataset{Hash: hash, SentAt: at.UTC().Truncate(time.Second)}
	}
	if err := store.write(next); err != nil {
		return err
	}
	store.state = next
	return nil
}

// Forget drops datasets so they are sent in full next time, for example
// after the backend answers 409 resync.
func (store *StateStore) Forget(names ...string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	next := store.state
	next.Datasets = make(map[string]SentDataset, len(store.state.Datasets))
	for name, dataset := range store.state.Datasets {
		next.Datasets[name] = dataset
	}
	for _, name := range names {
		delete(next.Datasets, name)
	}
	if err := store.write(next); err != nil {
		return err
	}
	store.state = next
	return nil
}

// Reset clears everything except the sequence number, which must keep
// increasing for the backend's replay protection.
func (store *StateStore) Reset() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	next := emptyState()
	next.Seq = store.state.Seq
	if err := store.write(next); err != nil {
		return err
	}
	store.state = next
	return nil
}

// Snapshot returns a copy of the current state.
func (store *StateStore) Snapshot() State {
	store.mu.Lock()
	defer store.mu.Unlock()
	snapshot := store.state
	snapshot.Datasets = make(map[string]SentDataset, len(store.state.Datasets))
	for name, dataset := range store.state.Datasets {
		snapshot.Datasets[name] = dataset
	}
	return snapshot
}

func (store *StateStore) write(state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	return WriteFileAtomic(store.path, append(data, '\n'), 0o600)
}

// WriteFileAtomic replaces path with data via a synced temporary file and
// rename, then syncs the directory so the rename survives a crash.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", directory, err)
	}

	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", filepath.Base(path), err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}

	if err := temporary.Chmod(mode); err != nil {
		cleanup()
		return fmt.Errorf("set %s permissions: %w", filepath.Base(path), err)
	}
	if _, err := io.Copy(temporary, bytes.NewReader(data)); err != nil {
		cleanup()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync %s: %w", filepath.Base(path), err)
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		cleanup()
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}

	directoryHandle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open %s for sync: %w", directory, err)
	}
	defer directoryHandle.Close()
	if err := directoryHandle.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", directory, err)
	}
	return nil
}
