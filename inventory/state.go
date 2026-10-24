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
	Version int `json:"version"`
	// Seq is the last sequence number reserved for an envelope.
	Seq uint64 `json:"seq"`
	// AcknowledgedSeq is the last sequence number the backend answered.
	AcknowledgedSeq uint64 `json:"acknowledged_seq"`
	// Datasets holds the hashes the backend acknowledged.
	Datasets map[string]SentDataset `json:"datasets"`
	// Errors holds the collector errors last reported to the backend, so
	// an unchanged error is not reported again every round.
	Errors map[string]string `json:"errors,omitempty"`
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

// AdvanceSeq raises the sequence number to at least seq, for when the
// backend already stored a higher one (for example after a reinstall).
func (store *StateStore) AdvanceSeq(seq uint64) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if seq <= store.state.Seq {
		return nil
	}
	next := store.state
	next.Seq = seq
	if err := store.write(next); err != nil {
		return err
	}
	store.state = next
	return nil
}

// Seq returns the last reserved sequence number.
func (store *StateStore) Seq() uint64 {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.state.Seq
}

// ReportedErrors returns the collector errors last reported.
func (store *StateStore) ReportedErrors() map[string]string {
	store.mu.Lock()
	defer store.mu.Unlock()
	errors := make(map[string]string, len(store.state.Errors))
	for name, message := range store.state.Errors {
		errors[name] = message
	}
	return errors
}

// SetReportedErrors replaces the reported collector errors.
func (store *StateStore) SetReportedErrors(errors map[string]string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	next := store.state
	next.Errors = make(map[string]string, len(errors))
	for name, message := range errors {
		next.Errors[name] = message
	}
	if len(next.Errors) == 0 {
		next.Errors = nil
	}
	if err := store.write(next); err != nil {
		return err
	}
	store.state = next
	return nil
}

// AcknowledgedSeq returns the last sequence number the backend answered.
func (store *StateStore) AcknowledgedSeq() uint64 {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.state.AcknowledgedSeq
}

// MarkSent records hashes the backend acknowledged.
func (store *StateStore) MarkSent(hashes map[string]string, at time.Time) error {
	return store.Acknowledge(0, hashes, at)
}

// Acknowledge records the backend's answer to envelope seq: the hashes it
// now stores. A zero seq leaves AcknowledgedSeq alone.
func (store *StateStore) Acknowledge(seq uint64, hashes map[string]string, at time.Time) error {
	if len(hashes) == 0 && seq <= store.AcknowledgedSeq() {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	next := store.state
	next.AcknowledgedSeq = max(next.AcknowledgedSeq, seq)
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

// Reset clears everything except the sequence numbers, which must keep
// increasing for the backend's replay protection.
func (store *StateStore) Reset() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	next := emptyState()
	next.Seq = store.state.Seq
	next.AcknowledgedSeq = store.state.AcknowledgedSeq
	if err := store.write(next); err != nil {
		return err
	}
	store.state = next
	return nil
}

// Clear forgets everything, sequence numbers included. Enrollment uses it:
// the backend resets its last_inventory_seq for a new generation.
func (store *StateStore) Clear() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	next := emptyState()
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
	if store.state.Errors != nil {
		snapshot.Errors = make(map[string]string, len(store.state.Errors))
		for name, message := range store.state.Errors {
			snapshot.Errors[name] = message
		}
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

// RefreshStamp records when the agent's own package metadata refresh last
// succeeded, in the state directory. It belongs to the agent, so no file of
// apt, yum or dnf is ever written for it.
type RefreshStamp struct {
	path string

	mu sync.Mutex
	at time.Time
}

type refreshStampFile struct {
	RefreshedAt time.Time `json:"refreshed_at"`
}

// OpenRefreshStamp loads path. A missing file means "never"; an unreadable
// one is returned as an error and also treated as never.
func OpenRefreshStamp(path string) (*RefreshStamp, error) {
	stamp := &RefreshStamp{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return stamp, nil
	}
	if err != nil {
		return stamp, fmt.Errorf("read %s: %w", path, err)
	}
	var file refreshStampFile
	if err := json.Unmarshal(data, &file); err != nil {
		return stamp, fmt.Errorf("decode %s: %w", path, err)
	}
	stamp.at = file.RefreshedAt.UTC().Truncate(time.Second)
	return stamp, nil
}

// At returns the last recorded success, or zero.
func (stamp *RefreshStamp) At() time.Time {
	stamp.mu.Lock()
	defer stamp.mu.Unlock()
	return stamp.at
}

// Record atomically stores a success at whole-second precision, so the
// updates hash moves only when a refresh succeeded.
func (stamp *RefreshStamp) Record(at time.Time) error {
	at = at.UTC().Truncate(time.Second)
	data, err := json.Marshal(refreshStampFile{RefreshedAt: at})
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(stamp.path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	stamp.mu.Lock()
	defer stamp.mu.Unlock()
	stamp.at = at
	return nil
}
