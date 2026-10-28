package agent

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/x509roots/fallback/bundle"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaultsWhenMissing(t *testing.T) {
	config, err := LoadConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled("anything") || len(config.Intervals) != 0 || config.HeartbeatInterval != 0 {
		t.Fatalf("defaults = %#v", config)
	}
}

func TestSettingsCombineServerAndLocal(t *testing.T) {
	server := DefaultServerConfig()
	defaults := settings{server: server}
	if defaults.Interval("system") != 5*time.Minute || defaults.Interval("hardware") != time.Hour ||
		defaults.Interval("updates") != 6*time.Hour || defaults.Interval("unknown") != fallbackInterval || defaults.Heartbeat() != DefaultHeartbeatInterval {
		t.Fatal("protocol defaults wrong")
	}
	if !defaults.Enabled("system") || defaults.Enabled("packages") {
		t.Fatal("default datasets wrong")
	}

	server.Intervals = ServerIntervals{Heartbeat: 120, Datasets: map[string]int{"system": 60, "network": 1}}
	server.Datasets = []string{"system", "network", "packages"}
	combined := settings{server: server, local: Config{Intervals: map[string]Duration{"system": Duration(10 * time.Minute)}, Datasets: []string{"system", "packages"}}}
	if combined.Interval("system") != 10*time.Minute || combined.Interval("network") != MinimumInterval || combined.Heartbeat() != 2*time.Minute {
		t.Fatal("server intervals or local overrides not applied")
	}
	if !combined.Enabled("packages") || combined.Enabled("network") || combined.Enabled("hardware") {
		t.Fatal("local datasets must narrow the server's")
	}
}

func TestServerConfigCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server-config.json")
	config, found, err := LoadServerConfig(path)
	if err != nil || found || config.ETag != "" || len(config.Datasets) != 4 {
		t.Fatalf("missing cache = %+v, %v, %v", config, found, err)
	}
	config.ETag = `"c9"`
	config.Realtime = &Realtime{Key: "k", Host: "h", Port: 443, Scheme: "https"}
	if err := SaveServerConfig(path, config); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := LoadServerConfig(path)
	if err != nil || !found || loaded.ETag != `"c9"` || loaded.Realtime.Host != "h" {
		t.Fatalf("cache = %+v, %v, %v", loaded, found, err)
	}
	if mode := fileMode(t, path); mode != 0o600 {
		t.Fatalf("cache mode = %o", mode)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	path := writeConfig(t, `{"intervals": {"system": "1m"}, "heartbeat_interval": "2m", "datasets": ["system"]}`)
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Interval("system") != time.Minute || config.Heartbeat() != 2*time.Minute {
		t.Fatalf("config = %#v", config)
	}
	if config.Enabled("network") || !config.Enabled("system") {
		t.Fatal("datasets filter not applied")
	}
}

func TestLoadConfigRejectsInvalid(t *testing.T) {
	for name, content := range map[string]string{
		"unknown field":   `{"intervall": {}}`,
		"short interval":  `{"intervals": {"system": "1s"}}`,
		"bad duration":    `{"intervals": {"system": 300}}`,
		"endpoint moved":  `{"endpoint": "https://server.chief.app/agent/v1"}`,
		"short heartbeat": `{"heartbeat_interval": "5s"}`,
		"not json":        `{`,
	} {
		if _, err := LoadConfig(writeConfig(t, content)); err == nil {
			t.Errorf("%s: LoadConfig() succeeded", name)
		}
	}
}

func TestValidEndpoint(t *testing.T) {
	for endpoint, want := range map[string]bool{
		DefaultEndpoint:                    true,
		"http://localhost:8000/agent/v1":   true,
		"http://serverchief.test/agent/v1": true,
		"http://example.com/agent/v1":      false,
		"https://user:pass@example.com/":   false,
		"ftp://example.com":                false,
		"not a url":                        false,
	} {
		if got := validEndpoint(endpoint); got != want {
			t.Errorf("validEndpoint(%q) = %v, want %v", endpoint, got, want)
		}
	}
}

func TestRootCAsIncludesEmbeddedBundle(t *testing.T) {
	pool := RootCAs()
	if pool == nil {
		t.Fatal("RootCAs() returned nil")
	}
	for root := range bundle.Roots() {
		if root.Constraint != nil {
			continue
		}
		certificate, err := x509.ParseCertificate(root.Certificate)
		if err != nil {
			continue
		}
		options := x509.VerifyOptions{
			Roots:       pool,
			CurrentTime: certificate.NotBefore,
			KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		}
		if _, err := certificate.Verify(options); err != nil {
			t.Fatalf("RootCAs() does not trust embedded root %s: %v", certificate.Subject, err)
		}
	}
	client, err := NewHTTPClient("")
	if err != nil || client.Timeout == 0 {
		t.Fatalf("client = %v, %v", client, err)
	}
	if _, err := NewHTTPClient(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("a missing CA file was accepted")
	}
}
