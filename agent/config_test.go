package agent

import (
	"context"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if config.Endpoint != DefaultEndpoint || config.Interval("system") != 5*time.Minute || config.Interval("hardware") != 15*time.Minute {
		t.Fatalf("defaults = %#v", config)
	}
	if config.Interval("packages") != fallbackInterval || config.Heartbeat() != DefaultHeartbeatInterval {
		t.Fatal("fallback intervals wrong")
	}
	if !config.Enabled("anything") {
		t.Fatal("datasets disabled by default")
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
		"http endpoint":   `{"endpoint": "http://server.chief.app/agent/v1"}`,
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

func TestEnrollValidatesPreconditions(t *testing.T) {
	root := func() bool { return true }
	cases := []struct {
		name    string
		token   string
		options EnrollOptions
		want    string
	}{
		{"not root", "scenr_abc", EnrollOptions{RootCheck: func() bool { return false }}, "must run as root"},
		{"darwin", "scenr_abc", EnrollOptions{RootCheck: root, Platform: "darwin", Architecture: "arm64"}, "requires Linux"},
		{"386", "scenr_abc", EnrollOptions{RootCheck: root, Platform: "linux", Architecture: "386"}, "requires Linux"},
		{"bad token", "abc", EnrollOptions{RootCheck: root, Platform: "linux", Architecture: "amd64"}, "must start with scenr_"},
		{"empty token", "scenr_", EnrollOptions{RootCheck: root, Platform: "linux", Architecture: "amd64"}, "must start with scenr_"},
	}
	for _, test := range cases {
		_, err := Enroll(context.Background(), test.token, test.options)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: Enroll() = %v, want %q", test.name, err, test.want)
		}
	}
	_, err := Enroll(context.Background(), "scenr_abc", EnrollOptions{RootCheck: root, Platform: "linux", Architecture: "arm64"})
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Enroll() = %v, want ErrNotImplemented", err)
	}
}

func TestRootCAsIncludesEmbeddedBundle(t *testing.T) {
	pool := RootCAs()
	system, err := x509.SystemCertPool()
	if err != nil {
		system = x509.NewCertPool()
	}
	if pool == nil || pool.Equal(system) {
		t.Fatal("RootCAs() did not add the embedded roots")
	}
	client := NewHTTPClient()
	if client.Timeout == 0 {
		t.Fatal("client without timeout")
	}
}
