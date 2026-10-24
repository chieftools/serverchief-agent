package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	// MinimumInterval stops a configuration from turning the agent into a
	// busy loop.
	MinimumInterval = 30 * time.Second
	// JitterFraction spreads every interval by ±10% so a fleet restarted at
	// once does not report in lockstep.
	JitterFraction = 0.1
)

// DefaultIntervals is the per-dataset collection cadence from the
// protocol's defaults, used until the backend sends its own.
var DefaultIntervals = map[string]time.Duration{
	"system":    5 * time.Minute,
	"network":   5 * time.Minute,
	"resolvers": 5 * time.Minute,
	"hardware":  time.Hour,
	"packages":  time.Hour,
	"updates":   6 * time.Hour,
	"vhosts":    15 * time.Minute,
	// certificates is also collected early when renewal or certificate
	// directories change.
	"certificates": time.Hour,
	// cloud is also collected at agent start, like every enabled dataset.
	"cloud": 24 * time.Hour,
}

// DefaultHeartbeatInterval is how often the agent checks in.
const DefaultHeartbeatInterval = 5 * time.Minute

const (
	// DefaultRefreshInterval is the package metadata refresh cadence when
	// the backend does not configure one.
	DefaultRefreshInterval = 6 * time.Hour
	// MinimumRefreshInterval keeps a configuration from hammering mirrors.
	MinimumRefreshInterval = time.Hour
)

// fallbackInterval applies to datasets without a configured interval.
const fallbackInterval = time.Hour

// Duration is a time.Duration that encodes as a Go duration string.
type Duration time.Duration

func (duration Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(duration).String())
}

func (duration *Duration) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("duration must be a string such as \"5m\": %w", err)
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	*duration = Duration(parsed)
	return nil
}

// Config is /etc/serverchief/config.json: operator overrides on top of
// what the backend configures. Every field is optional.
type Config struct {
	// Intervals override the backend's per-dataset intervals.
	Intervals map[string]Duration `json:"intervals,omitempty"`
	// HeartbeatInterval overrides the backend's heartbeat interval.
	HeartbeatInterval Duration `json:"heartbeat_interval,omitempty"`
	// Datasets further restricts the datasets the backend enabled.
	Datasets []string `json:"datasets,omitempty"`
	// PackageMetadataRefresh overrides the backend's
	// package_metadata_refresh; either field can be left out.
	PackageMetadataRefresh *LocalMetadataRefresh `json:"package_metadata_refresh,omitempty"`
}

// LocalMetadataRefresh is the local package_metadata_refresh override.
type LocalMetadataRefresh struct {
	Enabled  *bool    `json:"enabled,omitempty"`
	Interval Duration `json:"interval,omitempty"`
}

// DefaultConfig returns the built-in configuration: no overrides.
func DefaultConfig() Config {
	return Config{}
}

// LoadConfig reads path on top of the defaults. A missing file is not an
// error; unknown fields are, so typos do not go unnoticed.
func LoadConfig(path string) (Config, error) {
	config := DefaultConfig()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return config, nil
}

// Validate checks the configuration.
func (config Config) Validate() error {
	for name, interval := range config.Intervals {
		if time.Duration(interval) < MinimumInterval {
			return fmt.Errorf("interval for %s must be at least %s", name, MinimumInterval)
		}
	}
	if config.HeartbeatInterval != 0 && time.Duration(config.HeartbeatInterval) < MinimumInterval {
		return fmt.Errorf("heartbeat_interval must be at least %s", MinimumInterval)
	}
	if refresh := config.PackageMetadataRefresh; refresh != nil && refresh.Interval != 0 && time.Duration(refresh.Interval) < MinimumRefreshInterval {
		return fmt.Errorf("package_metadata_refresh.interval must be at least %s", MinimumRefreshInterval)
	}
	return nil
}

// Interval returns the collection interval for a dataset.
func (config Config) Interval(name string) time.Duration {
	if interval, ok := config.Intervals[name]; ok {
		return time.Duration(interval)
	}
	if interval, ok := DefaultIntervals[name]; ok {
		return interval
	}
	return fallbackInterval
}

// Heartbeat returns the heartbeat interval.
func (config Config) Heartbeat() time.Duration {
	if config.HeartbeatInterval != 0 {
		return time.Duration(config.HeartbeatInterval)
	}
	return DefaultHeartbeatInterval
}

// Enabled reports whether a dataset should be collected.
func (config Config) Enabled(name string) bool {
	return len(config.Datasets) == 0 || slices.Contains(config.Datasets, name)
}

func validEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	// Plain HTTP is accepted for a local development backend only.
	host := parsed.Hostname()
	return parsed.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".test"))
}
