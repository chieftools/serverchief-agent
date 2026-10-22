package inventory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// PayloadStore keeps the last acknowledged canonical payload of delta
// datasets, one file per dataset, as the base for the next delta. Losing
// a file only costs one full upload.
type PayloadStore struct {
	directory string
}

// NewPayloadStore stores payloads in directory, created on first save.
func NewPayloadStore(directory string) *PayloadStore {
	return &PayloadStore{directory: directory}
}

func (store *PayloadStore) path(name string) string {
	return filepath.Join(store.directory, name+".json")
}

// Load returns the stored payload, or nil when there is none.
func (store *PayloadStore) Load(name string) ([]byte, error) {
	data, err := os.ReadFile(store.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read stored %s payload: %w", name, err)
	}
	return data, nil
}

// Save atomically replaces the stored payload.
func (store *PayloadStore) Save(name string, canonical []byte) error {
	return WriteFileAtomic(store.path(name), canonical, 0o600)
}

// Remove deletes stored payloads.
func (store *PayloadStore) Remove(names ...string) error {
	for _, name := range names {
		if err := os.Remove(store.path(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stored %s payload: %w", name, err)
		}
	}
	return nil
}

// Clear deletes every stored payload.
func (store *PayloadStore) Clear() error {
	if err := os.RemoveAll(store.directory); err != nil {
		return fmt.Errorf("clear stored payloads: %w", err)
	}
	return nil
}
