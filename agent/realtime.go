package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"

	"go.chief.tools/serverchief-agent/pusher"
)

// Realtime events. They carry no data; they only wake the agent up.
const (
	EventConfigUpdated      = "config.updated"
	EventInventoryRequested = "inventory.requested"
	EventCommandsAvailable  = "commands.available"
	eventSubscribed         = "pusher_internal:subscription_succeeded"
)

// RealtimeChannel is the private channel of a server generation.
func RealtimeChannel(serverID string, generation uint64) string {
	return fmt.Sprintf("private-agent.server_%s.generation_%d", serverID, generation)
}

// RealtimeAuthorizer signs a channel subscription for a socket.
type RealtimeAuthorizer func(ctx context.Context, socketID, channel string) (string, error)

// RealtimeConnector connects to the realtime service, subscribes to
// channel and calls onEvent for every event received on it. The returned
// function disconnects.
type RealtimeConnector func(config Realtime, channel string, authorize RealtimeAuthorizer, onEvent func(event string)) (stop func())

// PusherConnector returns a RealtimeConnector backed by the pusher client.
func PusherConnector(version, credential string, tlsConfig *tls.Config) RealtimeConnector {
	return func(config Realtime, channel string, authorize RealtimeAuthorizer, onEvent func(string)) func() {
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
		client := pusher.NewClient(pusherConfig, UserAgent(version), credential, ProtocolRevision, nil, false)
		if client == nil {
			return func() {}
		}
		client.SetAuthorizer(pusher.Authorizer(authorize))
		client.SetTLSConfig(tlsConfig)
		for _, event := range []string{EventConfigUpdated, EventInventoryRequested, EventCommandsAvailable, eventSubscribed} {
			client.OnEvent(event, func(message pusher.Message) {
				if message.Channel == "" || message.Channel == channel {
					onEvent(message.Event)
				}
			})
		}
		log.Printf("[realtime] connecting to %s:%d, channel %s", config.Host, pusherConfig.Port, channel)
		go client.Start()
		return client.Disconnect
	}
}
