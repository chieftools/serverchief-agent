package agent

import (
	"context"
	"log"
	"slices"

	"go.chief.tools/serverchief-agent/inventory"
)

// Client delivers data to the backend. The HTTP implementation follows
// once the backend's /agent/v1 endpoints exist; until then the daemon
// runs with LogClient.
//
// TODO(protocol): implement HTTPClient on NewHTTPClient with
// POST /heartbeat and POST /inventory, the ProtocolHeader, bearer
// credential plus generation fencing, problem+json errors with
// retry_after_seconds, and 409 resync mapped to SendResult.Resync.
type Client interface {
	SendInventory(ctx context.Context, envelope *inventory.Envelope) (SendResult, error)
	Heartbeat(ctx context.Context, heartbeat Heartbeat) error
}

// LogClient logs what it would send and reports success. Its state store
// therefore advances as if the backend had accepted every envelope; setup
// resets the state, so nothing is lost when a real client takes over.
type LogClient struct{}

func (LogClient) SendInventory(_ context.Context, envelope *inventory.Envelope) (SendResult, error) {
	names := make([]string, 0, len(envelope.Datasets))
	for name := range envelope.Datasets {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		dataset := envelope.Datasets[name]
		log.Printf("[inventory] seq=%d dataset=%s mode=%s hash=%s bytes=%d (not sent: no backend client yet)",
			envelope.Seq, name, dataset.Mode, shortHash(dataset.Hash), len(dataset.Payload))
	}
	for name, message := range envelope.Errors {
		log.Printf("[inventory] seq=%d dataset=%s error=%q", envelope.Seq, name, message)
	}
	return SendResult{}, nil
}

func (LogClient) Heartbeat(_ context.Context, heartbeat Heartbeat) error {
	log.Printf("[heartbeat] version=%s datasets=%d (not sent: no backend client yet)", heartbeat.AgentVersion, len(heartbeat.Hashes))
	return nil
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
