package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/stores/sqlite"
)

// A retained message without an expiry that an MQTT 5 bridge forwards live is
// stored on the bridging broker without an expiry, so it is never purged.
func TestMqttBridgeLiveRetainedKeepsNoExpiry(t *testing.T) {
	cfgA := config.Default()
	cfgA.NodeID = "broker-a-exp"
	cfgA.TCP.Port = 24021
	cfgA.GraphQL.Enabled = false
	cfgA.Metrics.Enabled = false
	cfgA.SQLite.Path = filepath.Join(t.TempDir(), "a_exp.db")
	srvA, err := broker.New(cfgA, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srvA.Serve() }()
	defer srvA.Close()

	dbB := filepath.Join(t.TempDir(), "b_exp.db")
	bootDB, err := sqlite.Open(dbB)
	if err != nil {
		t.Fatal(err)
	}
	dcs := sqlite.NewDeviceConfigStore(bootDB)
	if err := dcs.EnsureTable(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfgJSON, _ := json.Marshal(map[string]any{
		"brokerUrl":       "tcp://localhost:24021",
		"clientId":        "bridge-exp-b-to-a",
		"cleanSession":    true,
		"protocolVersion": 5,
		"addresses": []map[string]any{
			{"mode": "SUBSCRIBE", "remoteTopic": "uns/#", "localTopic": "uns/#", "removePath": true, "qos": 1},
		},
	})
	if err := dcs.Save(context.Background(), stores.DeviceConfig{
		Name: "bridge-exp", Namespace: "bridge", NodeID: "broker-b-exp", Type: "MQTT_CLIENT",
		Enabled: true, Config: string(cfgJSON),
	}); err != nil {
		t.Fatal(err)
	}
	bootDB.Close()

	cfgB := config.Default()
	cfgB.NodeID = "broker-b-exp"
	cfgB.TCP.Port = 24022
	cfgB.GraphQL.Enabled = false
	cfgB.Metrics.Enabled = false
	cfgB.SQLite.Path = dbB
	cfgB.Features.MqttClient = true
	srvB, err := broker.New(cfgB, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srvB.Serve() }()
	defer srvB.Close()

	readDB, err := sqlite.Open(dbB)
	if err != nil {
		t.Fatal(err)
	}
	defer readDB.Close()
	retainedB := sqlite.NewMessageStore("retainedmessages", readDB)

	pub := mqtt.NewClient(mqttOpts(24021, "pub-exp"))
	if tok := pub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer pub.Disconnect(100)

	waitRow := func(payload string) *stores.BrokerMessage {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if m, err := retainedB.Get(context.Background(), "uns/dfm/model"); err == nil && m != nil && string(m.Payload) == payload {
				return m
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("retained %q never reached broker B", payload)
		return nil
	}

	// v1 may arrive as the retained message sent at subscribe; once it is
	// there the bridge is subscribed, so v2 is forwarded live.
	pub.Publish("uns/dfm/model", 1, true, "v1").WaitTimeout(2 * time.Second)
	waitRow("v1")
	pub.Publish("uns/dfm/model", 1, true, "v2").WaitTimeout(2 * time.Second)
	if m := waitRow("v2"); m.MessageExpiryInterval != nil {
		t.Fatalf("stored MessageExpiryInterval = %d, want none", *m.MessageExpiryInterval)
	}
}
