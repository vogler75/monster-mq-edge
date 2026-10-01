package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"monstermq.io/edge/internal/config"
)

// An unauthenticated websocket subscription gets one error and ends. The
// auth middleware used to return a handler that answered every call with the
// error, which gqlgen sent in a tight loop (100% CPU, even after the client
// left). The dashboard's log viewer subscribes without a token.
func TestGraphQLWebsocketUnauthenticatedSubscription(t *testing.T) {
	srv, url := startWithGraphQL(t, 23006, 28006, func(c *config.Config) {
		c.UserManagement.Enabled = true
		c.UserManagement.AnonymousEnabled = false
	})
	defer srv.Close()

	dialer := websocket.Dialer{Subprotocols: []string{"graphql-transport-ws"}, HandshakeTimeout: 2 * time.Second}
	ws, _, err := dialer.Dial(strings.Replace(url, "http://", "ws://", 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if err := ws.WriteJSON(map[string]any{"type": "connection_init", "payload": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := ws.ReadJSON(&ack); err != nil || ack["type"] != "connection_ack" {
		t.Fatalf("ack %v %v", ack, err)
	}
	if err := ws.WriteJSON(map[string]any{"id": "1", "type": "subscribe", "payload": map[string]any{
		"query": `subscription { topicUpdates(topicFilters: ["#"]) { topic } }`,
	}}); err != nil {
		t.Fatal(err)
	}

	var types []string
	_ = ws.SetReadDeadline(time.Now().Add(time.Second))
	for len(types) < 50 {
		var m map[string]any
		if err := ws.ReadJSON(&m); err != nil {
			break // deadline: nothing more is sent
		}
		types = append(types, m["type"].(string))
	}
	if len(types) == 0 || len(types) > 2 {
		t.Fatalf("got %d messages %v, want one error and the end of the operation", len(types), types)
	}
	if !strings.Contains(strings.Join(types, " "), "error") && !strings.Contains(strings.Join(types, " "), "next") {
		t.Fatalf("no error reported: %v", types)
	}
}
