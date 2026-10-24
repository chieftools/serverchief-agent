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

// KeyedDataset is implemented by collectors whose payload holds a keyed
// list under "items" that is sent as a delta (inventory.Delta) once the
// backend has a base. An item's key joins DeltaKeyFields with "/"; a field
// an item lacks is taken from the payload's top level (packages:
// manager/name/arch).
type KeyedDataset interface {
	DeltaKeyFields() []string
}

// ChangeDetector is implemented by collectors that can cheaply tell that
// their source changed, such as a package database mtime. The daemon polls
// ChangeToken and collects early when it changes.
type ChangeDetector interface {
	ChangeToken() string
}

// Registry holds collectors by dataset name.
type Registry struct {
	mu         sync.RWMutex
	collectors map[string]Collector
	optional   map[string]bool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{collectors: make(map[string]Collector), optional: make(map[string]bool)}
}

// Default returns a registry with every built-in collector bound to host.
// packages, repositories, updates, cloud, vhosts and certificates are optional: they are
// collected only when selected explicitly or enabled by the backend.
func Default(host *Host) *Registry {
	registry := NewRegistry()
	registry.MustRegister(NewSystem(host))
	registry.MustRegister(NewHardware(host))
	registry.MustRegister(NewNetwork(host))
	registry.MustRegister(NewResolvers(host))
	// packages and repositories share the package origins.
	origins := NewOrigins(host)
	registry.MustRegisterOptional(NewPackagesWithOrigins(host, origins))
	registry.MustRegisterOptional(NewRepositories(host, origins))
	registry.MustRegisterOptional(NewUpdates(host))
	registry.MustRegisterOptional(NewCloud(host))
	vhosts := NewVHosts(host)
	registry.MustRegisterOptional(vhosts)
	// certificates reuses the sites and parsed certificate files of vhosts.
	registry.MustRegisterOptional(NewCertificates(host, vhosts))
	return registry
}

// RegisterOptional adds a collector that is left out of DefaultNames.
func (registry *Registry) RegisterOptional(collector Collector) error {
	if err := registry.Register(collector); err != nil {
		return err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.optional[collector.Name()] = true
	return nil
}

// MustRegisterOptional is RegisterOptional that panics on a programming error.
func (registry *Registry) MustRegisterOptional(collector Collector) {
	if err := registry.RegisterOptional(collector); err != nil {
		panic(err)
	}
}

// DefaultNames returns the sorted names of the non-optional collectors.
func (registry *Registry) DefaultNames() []string {
	names := registry.Names()
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	defaults := names[:0]
	for _, name := range names {
		if !registry.optional[name] {
			defaults = append(defaults, name)
		}
	}
	return defaults
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
// every non-optional collector.
func (registry *Registry) Select(names []string) ([]Collector, error) {
	if len(names) == 0 {
		names = registry.DefaultNames()
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
