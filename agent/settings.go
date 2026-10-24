package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"go.chief.tools/serverchief-agent/inventory"
)

// DefaultDatasets are collected until the backend says otherwise. The
// packages and updates datasets are only collected when it enables them.
var DefaultDatasets = []string{"hardware", "network", "resolvers", "system"}

// DefaultServerConfig is the configuration assumed before the backend sent
// one: the protocol's defaults and no realtime connection.
func DefaultServerConfig() ServerConfig {
	intervals := make(map[string]int, len(DefaultIntervals))
	for name, interval := range DefaultIntervals {
		intervals[name] = int(interval / time.Second)
	}
	return ServerConfig{
		Intervals: ServerIntervals{Heartbeat: int(DefaultHeartbeatInterval / time.Second), Datasets: intervals},
		Datasets:  slices.Clone(DefaultDatasets),
	}
}

// LoadServerConfig reads the cached backend configuration. A missing file
// returns DefaultServerConfig and found = false.
func LoadServerConfig(path string) (config ServerConfig, found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return DefaultServerConfig(), false, nil
	}
	if err != nil {
		return DefaultServerConfig(), false, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return DefaultServerConfig(), false, fmt.Errorf("decode %s: %w", path, err)
	}
	return config, true, nil
}

// SaveServerConfig atomically replaces the cached backend configuration.
func SaveServerConfig(path string, config ServerConfig) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode server config: %w", err)
	}
	return inventory.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// settings combines the backend's configuration with local overrides:
// local intervals win, and local datasets can only narrow the backend's.
type settings struct {
	server ServerConfig
	local  Config
}

// Interval returns the collection interval for a dataset.
func (settings settings) Interval(name string) time.Duration {
	if interval, ok := settings.local.Intervals[name]; ok {
		return time.Duration(interval)
	}
	if seconds, ok := settings.server.Intervals.Datasets[name]; ok && seconds > 0 {
		return max(time.Duration(seconds)*time.Second, MinimumInterval)
	}
	if interval, ok := DefaultIntervals[name]; ok {
		return interval
	}
	return fallbackInterval
}

// Heartbeat returns the heartbeat interval.
func (settings settings) Heartbeat() time.Duration {
	if settings.local.HeartbeatInterval != 0 {
		return time.Duration(settings.local.HeartbeatInterval)
	}
	if settings.server.Intervals.Heartbeat > 0 {
		return max(time.Duration(settings.server.Intervals.Heartbeat)*time.Second, MinimumInterval)
	}
	return DefaultHeartbeatInterval
}

// Enabled reports whether a dataset is collected and sent.
func (settings settings) Enabled(name string) bool {
	return slices.Contains(settings.server.Datasets, name) && settings.local.Enabled(name)
}

// MetadataRefresh returns whether the agent refreshes the package metadata
// cache itself, and how often: local overrides first, then the backend's
// package_metadata_refresh, which is enabled every 6 hours when absent.
func (settings settings) MetadataRefresh() (bool, time.Duration) {
	enabled, interval := true, DefaultRefreshInterval
	if server := settings.server.PackageMetadataRefresh; server != nil {
		enabled = server.Enabled
		if server.Interval > 0 {
			interval = max(time.Duration(server.Interval)*time.Second, MinimumRefreshInterval)
		}
	}
	if local := settings.local.PackageMetadataRefresh; local != nil {
		if local.Enabled != nil {
			enabled = *local.Enabled
		}
		if local.Interval != 0 {
			interval = time.Duration(local.Interval)
		}
	}
	return enabled, interval
}
