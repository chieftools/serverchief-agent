package inventory

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
)

// Delta describes how a keyed list changed. Added and Changed carry the new
// items; Removed carries only keys. Each list is sorted by key.
type Delta[T any] struct {
	Added   []T      `json:"added"`
	Removed []string `json:"removed"`
	Changed []T      `json:"changed"`
}

// Empty reports whether the delta changes nothing.
func (delta Delta[T]) Empty() bool {
	return len(delta.Added) == 0 && len(delta.Removed) == 0 && len(delta.Changed) == 0
}

// Diff compares two keyed lists, such as installed packages keyed by
// manager/name/arch. Items are compared by their canonical JSON, so field
// order and normalizable sub-lists do not cause spurious changes. Keys must
// be unique within each list.
func Diff[T any](previous, current []T, key func(T) string) (Delta[T], error) {
	delta := Delta[T]{Added: []T{}, Removed: []string{}, Changed: []T{}}

	before, err := index(previous, key)
	if err != nil {
		return delta, fmt.Errorf("previous: %w", err)
	}
	after, err := index(current, key)
	if err != nil {
		return delta, fmt.Errorf("current: %w", err)
	}

	for name, item := range after {
		old, existed := before[name]
		if !existed {
			delta.Added = append(delta.Added, item.value)
			continue
		}
		if !bytes.Equal(old.canonical, item.canonical) {
			delta.Changed = append(delta.Changed, item.value)
		}
	}
	for name := range before {
		if _, exists := after[name]; !exists {
			delta.Removed = append(delta.Removed, name)
		}
	}

	byKey := func(a, b T) int { return strings.Compare(key(a), key(b)) }
	slices.SortFunc(delta.Added, byKey)
	slices.SortFunc(delta.Changed, byKey)
	slices.Sort(delta.Removed)
	return delta, nil
}

// Apply returns base with delta applied, sorted by key. It is the inverse
// of Diff and mirrors what the backend does with a delta payload.
func Apply[T any](base []T, delta Delta[T], key func(T) string) ([]T, error) {
	items := make(map[string]T, len(base)+len(delta.Added))
	for _, item := range base {
		items[key(item)] = item
	}
	for _, name := range delta.Removed {
		if _, ok := items[name]; !ok {
			return nil, fmt.Errorf("removed key %q is not in the base", name)
		}
		delete(items, name)
	}
	for _, item := range delta.Changed {
		if _, ok := items[key(item)]; !ok {
			return nil, fmt.Errorf("changed key %q is not in the base", key(item))
		}
		items[key(item)] = item
	}
	for _, item := range delta.Added {
		if _, ok := items[key(item)]; ok {
			return nil, fmt.Errorf("added key %q is already in the base", key(item))
		}
		items[key(item)] = item
	}

	result := make([]T, 0, len(items))
	for _, item := range items {
		result = append(result, item)
	}
	slices.SortFunc(result, func(a, b T) int { return strings.Compare(key(a), key(b)) })
	return result, nil
}

type indexedItem[T any] struct {
	value     T
	canonical []byte
}

func index[T any](items []T, key func(T) string) (map[string]indexedItem[T], error) {
	indexed := make(map[string]indexedItem[T], len(items))
	for _, item := range items {
		name := key(item)
		if _, duplicate := indexed[name]; duplicate {
			return nil, fmt.Errorf("duplicate key %q", name)
		}
		canonical, err := Canonical(item)
		if err != nil {
			return nil, fmt.Errorf("encode %q: %w", name, err)
		}
		indexed[name] = indexedItem[T]{value: item, canonical: canonical}
	}
	return indexed, nil
}
