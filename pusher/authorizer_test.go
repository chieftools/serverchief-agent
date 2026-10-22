package pusher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestAuthorizerSignsPrivateSubscriptions(t *testing.T) {
	subscriptions := make(chan map[string]string, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		if err := conn.WriteJSON(connectionEstablishedMessage("123.456")); err != nil {
			return
		}
		var subscribe Message
		if err := conn.ReadJSON(&subscribe); err != nil {
			return
		}
		var data map[string]string
		_ = json.Unmarshal(subscribe.Data, &data)
		subscriptions <- data
		event, _ := json.Marshal("{}")
		_ = conn.WriteJSON(Message{Event: "inventory.requested", Channel: data["channel"], Data: event})
		var ignored Message
		_ = conn.ReadJSON(&ignored)
	}))
	defer server.Close()

	rts := &realtimeTestServer{server: server}
	config := rts.config(t, "private-agent.server_x.generation_2")
	config.AuthURL = "http://unused.invalid/auth"
	client := NewClient(config, "serverchief-agent/test", "scagt_secret", "1.0.0", nil, false)
	authorized := make(chan string, 4)
	client.SetAuthorizer(func(_ context.Context, socketID, channel string) (string, error) {
		authorized <- socketID + " " + channel
		return "app-key:signature", nil
	})
	events := make(chan string, 1)
	client.OnEvent("inventory.requested", func(message Message) { events <- message.Event })
	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect()

	select {
	case data := <-subscriptions:
		if data["channel"] != "private-agent.server_x.generation_2" || data["auth"] != "app-key:signature" {
			t.Fatalf("subscribe data = %v", data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no subscription")
	}
	if call := <-authorized; !strings.HasPrefix(call, "123.456 ") || len(authorized) != 0 {
		t.Fatalf("authorizer call = %q (%d more)", call, len(authorized))
	}
	select {
	case event := <-events:
		if event != "inventory.requested" {
			t.Fatalf("event = %s", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event handler not called")
	}
}
