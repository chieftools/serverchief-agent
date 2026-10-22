// Package agent runs the Server Chief agent: it schedules collectors,
// tracks what the backend already has and delivers changed datasets.
package agent

import (
	"path/filepath"
	"time"
)

const (
	// ProtocolRevision is the agent ↔ backend protocol this build speaks.
	ProtocolRevision = "1.0.0"
	// ProtocolHeader carries ProtocolRevision on every request.
	ProtocolHeader = "ServerChief-Protocol-Revision"
	// LatestProtocolHeader is how the backend advertises the newest
	// revision it supports.
	LatestProtocolHeader = "ServerChief-Latest-Protocol-Revision"
	// DefaultEndpoint is the production agent API.
	DefaultEndpoint = "https://server.chief.app/agent/v1"
	// EnrollmentTokenPrefix starts every one-time setup token.
	EnrollmentTokenPrefix = "scenr_"
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
	// State records the hashes last delivered to the backend.
	State string
	// Spool will hold envelopes that could not be delivered yet.
	Spool string
	// Log is the file log used on hosts without journald.
	Log string
}

// DefaultPaths returns the packaged locations.
func DefaultPaths() Paths {
	return Paths{
		Identity: "/etc/serverchief/identity.json",
		Pending:  "/etc/serverchief/setup.json",
		Config:   "/etc/serverchief/config.json",
		State:    "/var/lib/serverchief/state.json",
		Spool:    "/var/lib/serverchief/spool",
		Log:      "/var/log/serverchief/agent.log",
	}
}

// WithStateDirectory moves the state files into directory, for running
// the agent unprivileged during development.
func (paths Paths) WithStateDirectory(directory string) Paths {
	paths.State = filepath.Join(directory, "state.json")
	paths.Spool = filepath.Join(directory, "spool")
	return paths
}

// Identity is the content of identity.json, mirroring Backup Chief's
// bootstrap document. Only a SHA-256 of Credential is stored server-side.
type Identity struct {
	ProtocolRevision string `json:"protocol_revision"`
	Endpoint         string `json:"endpoint"`
	ServerID         string `json:"server_id"`
	// Generation increases on every re-enrollment. Requests and realtime
	// channels carry it, so a stale agent (a cloned VM or a restored disk)
	// is fenced off instead of overwriting the current one.
	Generation     uint64 `json:"generation"`
	Credential     string `json:"credential"`
	Hostname       string `json:"hostname"`
	SetupTokenHash string `json:"setup_token_hash"`
}

// pendingEnrollment is the setup.json journal.
type pendingEnrollment struct {
	Endpoint           string `json:"endpoint"`
	TokenHash          string `json:"token_hash"`
	AttemptID          string `json:"attempt_id"`
	Credential         string `json:"credential"`
	PreviousServerID   string `json:"previous_server_id,omitempty"`
	PreviousGeneration uint64 `json:"previous_generation,omitempty"`
}

// EnrollmentRequest is the body of POST /enroll.
type EnrollmentRequest struct {
	ProtocolRevision string `json:"protocol_revision"`
	AttemptID        string `json:"attempt_id"`
	AgentVersion     string `json:"agent_version"`
	Hostname         string `json:"hostname"`
	Platform         string `json:"platform"`
	Architecture     string `json:"architecture"`
	InitSystem       string `json:"init_system"`
	Credential       string `json:"credential"`
}

// EnrollmentResponse is the answer to POST /enroll.
type EnrollmentResponse struct {
	ServerID   string `json:"server_id"`
	Generation uint64 `json:"generation"`
}

// Heartbeat is the body of POST /heartbeat. Hashes lets the backend spot
// drift and request a resync.
type Heartbeat struct {
	AgentVersion string            `json:"agent_version"`
	Hashes       map[string]string `json:"hashes"`
	SentAt       time.Time         `json:"sent_at"`
}

// SendResult is the backend's answer to an inventory envelope.
type SendResult struct {
	// Resync names datasets whose base hash the backend did not have
	// (409 resync). They are resent in full.
	Resync []string
}
