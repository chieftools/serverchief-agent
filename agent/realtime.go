package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"go.chief.tools/serverchief-agent/pusher"
)

// Realtime events. They only wake the agent up; metrics.live carries until
// how long someone watches the server's charts.
const (
	EventConfigUpdated     = "config.updated"
	EventCommandsAvailable = "commands.available"
	EventMetricsLive       = "metrics.live"
	eventSubscribed        = "pusher_internal:subscription_succeeded"
)

// RealtimeChannel is the private channel of a server generation.
func RealtimeChannel(serverID string, generation uint64) string {
	return fmt.Sprintf("private-agent.server_%s.generation_%d", serverID, generation)
}

// RealtimeAuthorizer signs a channel subscription for a socket.
type RealtimeAuthorizer func(ctx context.Context, socketID, channel string) (string, error)

// RealtimeConnector connects to the realtime service, subscribes to
// channel and calls onEvent for every event received on it, with the
// event's decoded data (nil when it has none). The returned function
// disconnects.
type RealtimeConnector func(config Realtime, channel string, authorize RealtimeAuthorizer, onEvent func(event string, data []byte)) (stop func())

// PusherConnector returns a RealtimeConnector backed by the pusher client.
func PusherConnector(version, serverID, credential string, tlsConfig *tls.Config) RealtimeConnector {
	return func(config Realtime, channel string, authorize RealtimeAuthorizer, onEvent func(string, []byte)) func() {
		pusherConfig := &pusher.Config{
			Key:       config.Key,
			Host:      config.Host,
			Port:      config.Port,
			Channel:   channel,
			AuthURL:   config.AuthEndpoint,
			Encrypted: config.Scheme == "https" || config.Scheme == "wss",
		}
		if pusherConfig.Port == 0 {
			pusherConfig.Port = 80
			if pusherConfig.Encrypted {
				pusherConfig.Port = 443
			}
		}
		client := pusher.NewClient(pusherConfig, UserAgent(version, serverID), credential, ProtocolRevision, nil, false)
		if client == nil {
			return func() {}
		}
		client.SetAuthorizer(pusher.Authorizer(authorize))
		client.SetTLSConfig(tlsConfig)
		for _, event := range []string{EventConfigUpdated, EventCommandsAvailable, EventMetricsLive, eventSubscribed} {
			client.OnEvent(event, func(message pusher.Message) {
				if message.Channel == "" || message.Channel == channel {
					var data json.RawMessage
					if message.UnmarshalData(&data) != nil {
						data = nil
					}
					onEvent(message.Event, data)
				}
			})
		}
		log.Printf("[realtime] connecting to %s:%d, channel %s", config.Host, pusherConfig.Port, channel)
		go client.Start()
		return client.Disconnect
	}
}

// LiveUntil decodes the payload of metrics.live: {"until": <Unix seconds>}.
func LiveUntil(data []byte) (time.Time, bool) {
	var payload struct {
		Until *int64 `json:"until"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Until == nil || *payload.Until <= 0 {
		return time.Time{}, false
	}
	return time.Unix(*payload.Until, 0), true
}
