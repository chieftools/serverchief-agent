// Package collector gathers read-only inventory facts from a Linux host.
//
// Every collector reads the host through a Host value. Its file system is an
// fs.FS rooted at "/", so tests can point collectors at fixtures under
// testdata/ instead of the machine running the tests.
package collector

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
)

// Collector produces the payload of one inventory dataset.
//
// Payloads must be deterministic for an unchanged host: volatile values such
// as free memory or uptime do not belong in a dataset, because every change
// to the payload changes its hash and triggers an upload.
type Collector interface {
	Name() string
	Collect(ctx context.Context) (any, error)
}

// Registry holds collectors by dataset name.
type Registry struct {
	mu         sync.RWMutex
	collectors map[string]Collector
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{collectors: make(map[string]Collector)}
}

// Default returns a registry with every built-in collector bound to host.
func Default(host *Host) *Registry {
	registry := NewRegistry()
	registry.MustRegister(NewSystem(host))
	registry.MustRegister(NewHardware(host))
	registry.MustRegister(NewNetwork(host))
	registry.MustRegister(NewResolvers(host))
	return registry
}

// Register adds a collector. Dataset names must be unique.
func (registry *Registry) Register(collector Collector) error {
	name := collector.Name()
	if !validDatasetName(name) {
		return fmt.Errorf("invalid dataset name %q", name)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.collectors[name]; exists {
		return fmt.Errorf("dataset %q is already registered", name)
	}
	registry.collectors[name] = collector
	return nil
}

// MustRegister adds a collector and panics on a programming error.
func (registry *Registry) MustRegister(collector Collector) {
	if err := registry.Register(collector); err != nil {
		panic(err)
	}
}

// Get returns the collector for a dataset.
func (registry *Registry) Get(name string) (Collector, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	collector, ok := registry.collectors[name]
	return collector, ok
}

// Names returns the registered dataset names in sorted order.
func (registry *Registry) Names() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	names := make([]string, 0, len(registry.collectors))
	for name := range registry.collectors {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Select resolves dataset names to collectors. An empty selection returns
// every registered collector.
func (registry *Registry) Select(names []string) ([]Collector, error) {
	if len(names) == 0 {
		names = registry.Names()
	}
	selected := make([]Collector, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		collector, ok := registry.Get(name)
		if !ok {
			return nil, fmt.Errorf("unknown dataset %q (available: %s)", name, strings.Join(registry.Names(), ", "))
		}
		selected = append(selected, collector)
	}
	return selected, nil
}

func validDatasetName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, character := range name {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

// ignorable reports whether a read error means "this source is absent or
// unreadable on this host", which collectors treat as missing data rather
// than a failed collection.
func ignorable(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrInvalid)
}
