package collector

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

type stubCollector struct{ name string }

func (stub stubCollector) Name() string                    { return stub.name }
func (stubCollector) Collect(context.Context) (any, error) { return nil, nil }

func TestRegistry(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(stubCollector{"b"})
	registry.MustRegister(stubCollector{"a"})
	if err := registry.Register(stubCollector{"a"}); err == nil {
		t.Fatal("duplicate registration succeeded")
	}
	for _, invalid := range []string{"", "Bad", "has-dash", strings.Repeat("x", 65)} {
		if err := registry.Register(stubCollector{invalid}); err == nil {
			t.Fatalf("invalid name %q accepted", invalid)
		}
	}
	if names := registry.Names(); !reflect.DeepEqual(names, []string{"a", "b"}) {
		t.Fatalf("Names() = %v", names)
	}

	selected, err := registry.Select([]string{"b", "b"})
	if err != nil || len(selected) != 1 || selected[0].Name() != "b" {
		t.Fatalf("Select(b, b) = %v, %v", selected, err)
	}
	if all, _ := registry.Select(nil); len(all) != 2 {
		t.Fatalf("Select(nil) = %v", all)
	}
	if _, err := registry.Select([]string{"missing"}); err == nil || !strings.Contains(err.Error(), "available: a, b") {
		t.Fatalf("Select(missing) error = %v", err)
	}
}

func TestDefaultRegistryDatasets(t *testing.T) {
	registry := Default(&Host{FS: fstest.MapFS{}})
	if names := registry.Names(); !reflect.DeepEqual(names, []string{"certificates", "cloud", "hardware", "network", "packages", "repositories", "resolvers", "system", "updates", "vhosts"}) {
		t.Fatalf("Names() = %v", names)
	}
	if names := registry.DefaultNames(); !reflect.DeepEqual(names, []string{"hardware", "network", "resolvers", "system"}) {
		t.Fatalf("DefaultNames() = %v", names)
	}
	if selected, _ := registry.Select(nil); len(selected) != 4 {
		t.Fatalf("Select(nil) returned %d collectors; optional ones must be asked for", len(selected))
	}
	if selected, err := registry.Select([]string{"packages", "updates"}); err != nil || len(selected) != 2 {
		t.Fatalf("Select(packages, updates) = %v, %v", selected, err)
	}
	// cloud and vhosts cope with a host that has none of their sources.
	for _, name := range []string{"cloud", "vhosts"} {
		collector, _ := registry.Get(name)
		if _, err := collector.Collect(context.Background()); err != nil {
			t.Errorf("%s on an empty host: %v", name, err)
		}
	}
	// The package collectors report a missing package manager.
	for _, name := range []string{"packages", "updates"} {
		collector, _ := registry.Get(name)
		if _, err := collector.Collect(context.Background()); err == nil || !strings.Contains(err.Error(), "no supported package manager") {
			t.Errorf("%s on an empty host: %v", name, err)
		}
	}
	// Every default collector must cope with a host that has none of its sources.
	for _, name := range registry.DefaultNames() {
		collector, _ := registry.Get(name)
		if _, err := collector.Collect(context.Background()); err != nil {
			t.Errorf("%s on an empty host: %v", name, err)
		}
	}
}

func TestNewHostRootDisablesLiveSources(t *testing.T) {
	host := NewHost("testdata/debian-12")
	if host.Uname != nil || host.Interfaces != nil || host.LookupCNAME != nil || host.Statfs == nil {
		t.Fatalf("NewHost(root) = %#v", host)
	}
	if got := host.readOptional("/etc/debian_version"); got != "12.7" {
		t.Fatalf("read through root = %q", got)
	}
}
