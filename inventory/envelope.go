package inventory

import (
	"encoding/json"
	"fmt"
)

// Mode says how a dataset payload relates to what the backend stores.
type Mode string

const (
	// ModeFull replaces the stored dataset with the payload.
	ModeFull Mode = "full"
	// ModeDelta applies a Delta payload to the dataset stored under BaseHash.
	// The backend answers 409 resync when its hash differs, and the agent
	// then sends the dataset in full.
	ModeDelta Mode = "delta"
)

// Envelope is the body of POST /inventory:
// {seq, datasets: {name: {hash, mode, base_hash?, payload}}}.
type Envelope struct {
	// Seq increases with every envelope the agent builds, so the backend can
	// drop replays and out-of-order deliveries from the spool.
	Seq      uint64                     `json:"seq"`
	Datasets map[string]DatasetEnvelope `json:"datasets"`
	// Errors lists datasets whose collector failed this round, keyed by
	// name. The backend keeps their previous state.
	Errors map[string]string `json:"errors,omitempty"`
}

// DatasetEnvelope carries one dataset. Hash is always the hash of the
// complete dataset after the payload is applied, in either mode.
type DatasetEnvelope struct {
	Hash     string          `json:"hash"`
	Mode     Mode            `json:"mode"`
	BaseHash string          `json:"base_hash,omitempty"`
	Payload  json.RawMessage `json:"payload"`
}

// NewEnvelope returns an empty envelope.
func NewEnvelope(seq uint64) *Envelope {
	return &Envelope{Seq: seq, Datasets: make(map[string]DatasetEnvelope)}
}

// AddFull adds a dataset in full mode.
func (envelope *Envelope) AddFull(dataset Dataset) {
	envelope.Datasets[dataset.Name] = DatasetEnvelope{
		Hash:    dataset.Hash,
		Mode:    ModeFull,
		Payload: dataset.Canonical,
	}
}

// AddDelta adds a dataset as a delta against baseHash. delta is encoded
// canonically; dataset.Hash remains the hash of the full new state.
func (envelope *Envelope) AddDelta(dataset Dataset, baseHash string, delta any) error {
	if baseHash == "" {
		return fmt.Errorf("dataset %s: delta requires a base hash", dataset.Name)
	}
	payload, err := Canonical(delta)
	if err != nil {
		return fmt.Errorf("dataset %s: encode delta: %w", dataset.Name, err)
	}
	envelope.Datasets[dataset.Name] = DatasetEnvelope{
		Hash:     dataset.Hash,
		Mode:     ModeDelta,
		BaseHash: baseHash,
		Payload:  payload,
	}
	return nil
}

// AddError records a failed collector.
func (envelope *Envelope) AddError(name string, err error) {
	if envelope.Errors == nil {
		envelope.Errors = make(map[string]string)
	}
	envelope.Errors[name] = err.Error()
}

// Empty reports whether the envelope carries neither datasets nor errors.
func (envelope *Envelope) Empty() bool {
	return len(envelope.Datasets) == 0 && len(envelope.Errors) == 0
}
