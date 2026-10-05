package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/stores/sqlite"
	"monstermq.io/edge/internal/version"
)

// The broker's inline client is not a session: it is neither listed nor
// persisted (records of older versions are dropped at start), cannot be
// removed or taken over; $SYS/broker/version is the build version.
func TestInlineClientNotASession(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "g.db")
	seedLegacyInline(t, db)

	srv, url := startWithGraphQL(t, 23060, 28060, func(c *config.Config) {
		c.SQLite.Path = db
		c.HMI.Enabled = true // subscribes through the inline client
		c.HMI.Path = filepath.Join(dir, "hmi")
	})
	defer srv.Close()

	data := gqlQuery(t, url, `{ sessions { clientId } }`, nil)
	for _, s := range data["sessions"].([]any) {
		if id := s.(map[string]any)["clientId"]; id == mqtt.InlineClientId {
			t.Fatalf("inline client listed as a session")
		}
	}

	present, err := srv.Storage().Sessions.IsPresent(context.Background(), mqtt.InlineClientId)
	if err != nil || present {
		t.Fatalf("inline session persisted: %v %v", present, err)
	}
	if n, _ := srv.Storage().Subscriptions.GetSubscriptionsForClient(context.Background(), mqtt.InlineClientId); len(n) != 0 {
		t.Fatalf("inline subscriptions persisted: %v", n)
	}

	res := gqlQuery(t, url, `mutation { session { removeSessions(clientIds: ["inline"]) { success removedCount } } }`, nil)
	if r := res["session"].(map[string]any)["removeSessions"].(map[string]any); r["success"] != false || r["removedCount"] != float64(0) {
		t.Fatalf("inline client removed: %v", r)
	}

	if _, ok := srv.MQTT().Clients.Get(mqtt.InlineClientId); !ok {
		t.Fatal("inline client gone after removeSessions")
	}
	opts := paho.NewClientOptions().AddBroker("tcp://127.0.0.1:23060").SetClientID(mqtt.InlineClientId).SetConnectRetry(false)
	pc := paho.NewClient(opts)
	if tok := pc.Connect(); tok.WaitTimeout(3*time.Second) && tok.Error() == nil {
		pc.Disconnect(0)
		t.Fatal("network client took the inline client id")
	}

	c, _ := dialRaw(t, 23060, rawConnect{ClientID: "ver", Version: 5, Clean: true})
	defer c.Close()
	c.Subscribe(sub("$SYS/broker/version", 0))
	pk, ok := c.NextOn("$SYS/broker/version", 3*time.Second)
	if !ok || string(pk.Payload) != version.Version {
		t.Fatalf("$SYS/broker/version %q, want %q", pk.Payload, version.Version)
	}
}

func seedLegacyInline(t *testing.T, path string) {
	t.Helper()
	d, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := sqlite.NewSessionStore(d)
	ctx := context.Background()
	if err := s.EnsureTable(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSubscriptions(ctx, []stores.MqttSubscription{{ClientID: "inline", TopicFilter: "monstermq/hmi/sync/+/upstream"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetClient(ctx, stores.SessionInfo{ClientID: "inline"}); err != nil {
		t.Fatal(err)
	}
}
