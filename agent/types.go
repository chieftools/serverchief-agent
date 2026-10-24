// Package agent runs the Server Chief agent: it enrolls with the backend,
// schedules collectors, tracks what the backend already has and delivers
// changed datasets over the /agent/v1 protocol.
package agent

import (
	"encoding/json"
	"path/filepath"
	"time"
)

const (
	// ProtocolRevision is the agent ↔ backend protocol this build speaks.
	ProtocolRevision = "1.0.0"
	// ProtocolHeader carries ProtocolRevision on every request.
	ProtocolHeader = "ServerChief-Protocol-Revision"
	// DefaultEndpoint is the production agent API.
	DefaultEndpoint = "https://server.chief.app/agent/v1"
	// EnrollmentTokenPrefix starts every one-time setup token.
	EnrollmentTokenPrefix = "scenr_"
	// CredentialPrefix starts every agent credential.
	CredentialPrefix = "scagt_"

	// EndpointEnv and CAFileEnv provide defaults for the setup flags.
	EndpointEnv = "SERVERCHIEF_ENDPOINT"
	CAFileEnv   = "SERVERCHIEF_CA_FILE"
)

// Paths locates the agent's files.
type Paths struct {
	// Identity is written by setup: the server ID, generation and credential.
	Identity string
	// Pending journals an in-flight enrollment so a retried setup reuses
	// the same credential and attempt ID.
	Pending string
	// Config holds optional operator overrides.
	Config string
	// State records the sequence number and the hashes the backend
	// acknowledged.
	State string
	// ServerConfig caches the last configuration the backend sent, with
	// its ETag.
	ServerConfig string
	// Payloads keeps the last acknowledged payload of delta datasets, the
	// base their next delta is computed against.
	Payloads string
	// Commands journals recently received commands, so a restart neither
	// executes one twice nor loses its result.
	Commands string
	// MetadataRefresh records the agent's last successful package metadata
	// refresh, which counts towards cache_updated_at.
	MetadataRefresh string
	// Log is the file log used on hosts without journald.
	Log string
}

// DefaultPaths returns the packaged locations.
func DefaultPaths() Paths {
	return Paths{
		Identity:     "/etc/serverchief/identity.json",
		Pending:      "/etc/serverchief/setup.json",
		Config:       "/etc/serverchief/config.json",
		State:        "/var/lib/serverchief/state.json",
		ServerConfig: "/var/lib/serverchief/server-config.json",
		Payloads:     "/var/lib/serverchief/payloads",
		Commands:     "/var/lib/serverchief/commands.json",

		MetadataRefresh: "/var/lib/serverchief/metadata-refresh.json",
		Log:             "/var/log/serverchief/agent.log",
	}
}

// WithStateDirectory moves the state files into directory, for running
// the agent unprivileged during development.
func (paths Paths) WithStateDirectory(directory string) Paths {
	paths.State = filepath.Join(directory, "state.json")
	paths.ServerConfig = filepath.Join(directory, "server-config.json")
	paths.Payloads = filepath.Join(directory, "payloads")
	paths.Commands = filepath.Join(directory, "commands.json")
	paths.MetadataRefresh = filepath.Join(directory, "metadata-refresh.json")
	return paths
}

// Identity is the content of identity.json. Only a SHA-256 of Credential
// is stored server-side.
type Identity struct {
	ProtocolRevision string `json:"protocol_revision"`
	// Endpoint is the API the agent enrolled with; every later request
	// goes there.
	Endpoint string `json:"endpoint"`
	// CAFile is an extra PEM CA bundle trusted for Endpoint, for a local
	// development backend such as Laravel Valet.
	CAFile   string `json:"ca_file,omitempty"`
	ServerID string `json:"server_id"`
	// Generation increases on every re-enrollment. The realtime channel
	// carries it, and the backend revokes the previous credential, so a
	// stale agent (a cloned VM or a restored disk) is fenced off.
	Generation     uint64    `json:"generation"`
	Credential     string    `json:"credential"`
	Hostname       string    `json:"hostname"`
	SetupTokenHash string    `json:"setup_token_hash"`
	EnrolledAt     time.Time `json:"enrolled_at"`
}

// pendingEnrollment is the setup.json journal.
type pendingEnrollment struct {
	Endpoint           string    `json:"endpoint"`
	TokenHash          string    `json:"token_hash"`
	AttemptID          string    `json:"attempt_id"`
	Credential         string    `json:"credential"`
	PreviousServerID   string    `json:"previous_server_id,omitempty"`
	PreviousGeneration uint64    `json:"previous_generation,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

// EnrollmentRequest is the body of POST /enroll
// (fixtures/enroll-request.json).
type EnrollmentRequest struct {
	ProtocolRevision   string  `json:"protocol_revision"`
	Token              string  `json:"token"`
	AttemptID          string  `json:"attempt_id"`
	Credential         string  `json:"credential"`
	AgentVersion       string  `json:"agent_version"`
	Hostname           string  `json:"hostname"`
	Platform           string  `json:"platform"`
	Architecture       string  `json:"architecture"`
	InitSystem         string  `json:"init_system"`
	PreviousServerID   *string `json:"previous_server_id"`
	PreviousGeneration *uint64 `json:"previous_generation"`
}

// EnrollmentResponse is the answer to POST /enroll
// (fixtures/enroll-response.json).
type EnrollmentResponse struct {
	ProtocolRevision string          `json:"protocol_revision"`
	ServerID         string          `json:"server_id"`
	Generation       uint64          `json:"generation"`
	Endpoint         string          `json:"endpoint"`
	ConfigETag       string          `json:"config_etag"`
	Datasets         []string        `json:"datasets"`
	Intervals        ServerIntervals `json:"intervals"`
	Realtime         *Realtime       `json:"realtime"`
	// PackageMetadataRefresh is optional here; GET /config carries it.
	PackageMetadataRefresh *PackageMetadataRefresh `json:"package_metadata_refresh,omitempty"`
}

// ServerConfig returns the configuration embedded in the response.
func (response EnrollmentResponse) ServerConfig() ServerConfig {
	return ServerConfig{
		ETag:      response.ConfigETag,
		Datasets:  response.Datasets,
		Intervals: response.Intervals,
		Realtime:  response.Realtime,

		PackageMetadataRefresh: response.PackageMetadataRefresh,
	}
}

// ServerConfig is the body of GET /config (fixtures/config-response.json).
type ServerConfig struct {
	ETag      string          `json:"etag"`
	Intervals ServerIntervals `json:"intervals"`
	// Datasets lists the datasets the backend wants. Others are not sent.
	Datasets []string  `json:"datasets"`
	Realtime *Realtime `json:"realtime"`
	// PackageMetadataRefresh is absent (nil) for "enabled every 6 hours".
	PackageMetadataRefresh *PackageMetadataRefresh `json:"package_metadata_refresh,omitempty"`
}

// PackageMetadataRefresh configures the agent's own apt-get update /
// makecache.
type PackageMetadataRefresh struct {
	Enabled bool `json:"enabled"`
	// Interval is in seconds.
	Interval int `json:"interval"`
}

// ServerIntervals are in seconds.
type ServerIntervals struct {
	Heartbeat int            `json:"heartbeat"`
	Datasets  map[string]int `json:"datasets"`
}

// Realtime describes the Socket Chief (Pusher protocol 7) connection.
type Realtime struct {
	Key          string `json:"key"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Scheme       string `json:"scheme"`
	AuthEndpoint string `json:"auth_endpoint"`
}

// HeartbeatRequest is the body of POST /heartbeat
// (fixtures/heartbeat-request.json). Hashes are the last hashes the
// backend acknowledged, so it can spot drift and request a resync.
type HeartbeatRequest struct {
	AgentVersion string            `json:"agent_version"`
	StartedAt    time.Time         `json:"started_at"`
	SentAt       time.Time         `json:"sent_at"`
	Hashes       map[string]string `json:"hashes"`
	Seq          uint64            `json:"seq"`
}

// HeartbeatResponse is the answer to POST /heartbeat.
type HeartbeatResponse struct {
	Resync     []string `json:"resync"`
	ConfigETag string   `json:"config_etag"`
	// CommandsAvailable makes the agent call GET /commands right away.
	CommandsAvailable bool `json:"commands_available"`
}

// Command kinds. Kinds are a closed list; there is no shell execution.
const (
	// CommandRefreshInventory collects {"datasets": [names] | null} now
	// and pushes what changed.
	CommandRefreshInventory = "refresh_inventory"
	// CommandResync sends {"datasets": [names] | null} in full now.
	CommandResync = "resync"
	// CommandRefreshConfig fetches GET /config now.
	CommandRefreshConfig = "refresh_config"
)

// Command result statuses.
const (
	CommandSucceeded = "succeeded"
	CommandFailed    = "failed"
)

// MaximumCommands is the most commands GET /commands returns at once.
const MaximumCommands = 20

// CommandsResponse is the answer to GET /commands
// (fixtures/commands-response.json), oldest first.
type CommandsResponse struct {
	Commands []Command `json:"commands"`
}

// Command is one entry of GET /commands.
type Command struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind"`
	Args      json.RawMessage `json:"args"`
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// CommandResult is the body of POST /commands/{id}/result
// (fixtures/command-result-request.json). The first result wins.
type CommandResult struct {
	Status     string    `json:"status"`
	Detail     *string   `json:"detail"`
	FinishedAt time.Time `json:"finished_at"`
}

// InventoryResponse is the answer to POST /inventory
// (fixtures/inventory-response.json).
type InventoryResponse struct {
	// Accepted maps dataset names to the hash the backend now stores.
	Accepted map[string]string `json:"accepted"`
	// Resync names datasets the backend could not apply or whose hash it
	// recomputed differently. They are sent in full right away.
	Resync []string `json:"resync"`
	// Ignored names datasets the backend does not know.
	Ignored []string `json:"ignored"`
	// Seq is the backend's last_inventory_seq.
	Seq uint64 `json:"seq"`
}
