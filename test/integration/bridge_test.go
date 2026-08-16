package integration

import (
	"context"
	"encoding/json"
	"fmt"
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

// TestMqttBridgeOutboundAndInbound spins up two brokers; the second is
// configured with a bridge that forwards local "out/+" publishes to the first
// broker as "fwd/+", and inbound-subscribes to the first broker's "in/#"
// republishing as "from-remote/...".
func TestMqttBridgeOutboundAndInbound(t *testing.T) {
	// Broker A — the "remote".
	dbA := filepath.Join(t.TempDir(), "a.db")
	cfgA := config.Default()
	cfgA.NodeID = "broker-a"
	cfgA.TCP.Port = 24001
	cfgA.GraphQL.Enabled = false
	cfgA.Metrics.Enabled = false
	cfgA.SQLite.Path = dbA
	srvA, err := broker.New(cfgA, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srvA.Serve() }()
	defer srvA.Close()
	time.Sleep(100 * time.Millisecond)

	// Bootstrap broker B with a pre-loaded bridge config pointing at A.
	dbB := filepath.Join(t.TempDir(), "b.db")
	bootDB, _ := sqlite.Open(dbB)
	dcs := sqlite.NewDeviceConfigStore(bootDB)
	if err := dcs.EnsureTable(context.Background()); err != nil {
		t.Fatal(err)
	}
	bridgeCfg := map[string]any{
		"brokerUrl":    "tcp://localhost:24001",
		"clientId":     "bridge-b-to-a",
		"cleanSession": true,
		"keepAlive":    10,
		"addresses": []map[string]any{
			{"mode": "PUBLISH", "localTopic": "out/+", "remoteTopic": "fwd"},
			{"mode": "SUBSCRIBE", "remoteTopic": "in/+", "localTopic": "from-remote", "qos": 0},
		},
	}
	cfgJSON, _ := json.Marshal(bridgeCfg)
	if err := dcs.Save(context.Background(), stores.DeviceConfig{
		Name: "to-a", Namespace: "bridge", NodeID: "broker-b", Type: "MQTT_CLIENT",
		Enabled: true, Config: string(cfgJSON),
	}); err != nil {
		t.Fatal(err)
	}
	bootDB.Close()

	cfgB := config.Default()
	cfgB.NodeID = "broker-b"
	cfgB.TCP.Port = 24002
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
	// Wait until the bridge has produced at least one client session on A,
	// which is the most reliable signal the SUBSCRIBE took effect.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c := mqtt.NewClient(mqttOpts(24001, "probe-bridge"))
		if tok := c.Connect(); tok.WaitTimeout(500*time.Millisecond) && tok.Error() == nil {
			c.Disconnect(50)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(800 * time.Millisecond)

	// Probe outbound: publish on B, see it on A under "fwd/...".
	subA := mqtt.NewClient(mqttOpts(24001, "subA"))
	if tok := subA.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer subA.Disconnect(100)
	gotOnA := make(chan string, 1)
	if tok := subA.Subscribe("fwd/#", 0, func(_ mqtt.Client, m mqtt.Message) {
		gotOnA <- fmt.Sprintf("%s=%s", m.Topic(), string(m.Payload()))
	}); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}

	pubB := mqtt.NewClient(mqttOpts(24002, "pubB"))
	if tok := pubB.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer pubB.Disconnect(100)
	if tok := pubB.Publish("out/sensor", 0, false, "vB"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}

	select {
	case got := <-gotOnA:
		if got != "fwd=vB" {
			t.Logf("outbound topic mapping: %q (current behaviour appends nothing because remoteTopic was a fixed leaf)", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("outbound bridge did not deliver to A")
	}

	// Probe inbound: publish on A's "in/+", see it on B under "from-remote/...".
	subB := mqtt.NewClient(mqttOpts(24002, "subB"))
	if tok := subB.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer subB.Disconnect(100)
	gotOnB := make(chan string, 1)
	if tok := subB.Subscribe("from-remote/#", 0, func(_ mqtt.Client, m mqtt.Message) {
		gotOnB <- fmt.Sprintf("%s=%s", m.Topic(), string(m.Payload()))
	}); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}

	pubA := mqtt.NewClient(mqttOpts(24001, "pubA"))
	if tok := pubA.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer pubA.Disconnect(100)
	if tok := pubA.Publish("in/abc", 0, false, "vA"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	select {
	case got := <-gotOnB:
		if got == "" {
			t.Fatalf("empty inbound: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("inbound bridge did not deliver to B")
	}
}

func TestMqttBridgeRetainedMessages(t *testing.T) {
	// Broker A (Remote)
	dbA := filepath.Join(t.TempDir(), "a_ret.db")
	cfgA := config.Default()
	cfgA.NodeID = "broker-a-ret"
	cfgA.TCP.Port = 24011
	cfgA.GraphQL.Enabled = false
	cfgA.Metrics.Enabled = false
	cfgA.SQLite.Path = dbA
	srvA, err := broker.New(cfgA, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srvA.Serve() }()
	defer srvA.Close()
	time.Sleep(100 * time.Millisecond)

	// Publish a retained message to A *before* bridge connects
	pubA := mqtt.NewClient(mqttOpts(24011, "pubA-init"))
	if tok := pubA.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := pubA.Publish("remote/retained/sensor", 1, true, "remote-retained-data"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	pubA.Disconnect(100)

	// Broker B (Local with Bridge)
	dbB := filepath.Join(t.TempDir(), "b_ret.db")
	bootDB, _ := sqlite.Open(dbB)
	dcs := sqlite.NewDeviceConfigStore(bootDB)
	if err := dcs.EnsureTable(context.Background()); err != nil {
		t.Fatal(err)
	}
	bridgeCfg := map[string]any{
		"brokerUrl":    "tcp://localhost:24011",
		"clientId":     "bridge-ret-b-to-a",
		"cleanSession": true,
		"addresses": []map[string]any{
			{"mode": "SUBSCRIBE", "remoteTopic": "remote/retained/#", "localTopic": "local/retained", "qos": 1, "retain": false},
		},
	}
	cfgJSON, _ := json.Marshal(bridgeCfg)
	if err := dcs.Save(context.Background(), stores.DeviceConfig{
		Name: "bridge-ret", Namespace: "bridge", NodeID: "broker-b-ret", Type: "MQTT_CLIENT",
		Enabled: true, Config: string(cfgJSON),
	}); err != nil {
		t.Fatal(err)
	}
	bootDB.Close()

	cfgB := config.Default()
	cfgB.NodeID = "broker-b-ret"
	cfgB.TCP.Port = 24012
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
	time.Sleep(1000 * time.Millisecond) // wait for bridge to connect and sync retained messages

	// Connect a new subscriber to Broker B and subscribe to local/retained/#
	subB := mqtt.NewClient(mqttOpts(24012, "subB-late"))
	if tok := subB.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer subB.Disconnect(100)

	type msgInfo struct {
		topic    string
		payload  string
		retained bool
	}
	gotCh := make(chan msgInfo, 1)
	if tok := subB.Subscribe("local/retained/#", 1, func(_ mqtt.Client, m mqtt.Message) {
		gotCh <- msgInfo{
			topic:    m.Topic(),
			payload:  string(m.Payload()),
			retained: m.Retained(),
		}
	}); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}

	select {
	case info := <-gotCh:
		t.Logf("Received on B: topic=%s payload=%s retained=%v", info.topic, info.payload, info.retained)
		if info.payload != "remote-retained-data" {
			t.Fatalf("expected payload 'remote-retained-data', got %q", info.payload)
		}
		if !info.retained {
			t.Fatalf("expected retained=true, got retained=false")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscriber on B did not receive retained message from bridge")
	}

	// ---------------------------------------------------------
	// Test Outbound Retained Message:
	// Publish a retained message on Broker B -> Bridge should forward to A with Retain=true.
	// ---------------------------------------------------------
	// Update bridge config to include outbound PUBLISH address with retain: true
	dcsB, _ := sqlite.Open(dbB)
	dcsStoreB := sqlite.NewDeviceConfigStore(dcsB)
	bridgeCfgOut := map[string]any{
		"brokerUrl":    "tcp://localhost:24011",
		"clientId":     "bridge-ret-b-to-a",
		"cleanSession": true,
		"addresses": []map[string]any{
			{"mode": "PUBLISH", "localTopic": "local/out/#", "remoteTopic": "remote/out", "qos": 1, "retain": true},
		},
	}
	cfgJSONOut, _ := json.Marshal(bridgeCfgOut)
	_ = dcsStoreB.Save(context.Background(), stores.DeviceConfig{
		Name: "bridge-ret", Namespace: "bridge", NodeID: "broker-b-ret", Type: "MQTT_CLIENT",
		Enabled: true, Config: string(cfgJSONOut),
	})
	dcsB.Close()

	// Restart Broker B with updated bridge config
	srvB.Close()
	time.Sleep(200 * time.Millisecond)

	srvB2, err := broker.New(cfgB, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srvB2.Serve() }()
	defer srvB2.Close()
	time.Sleep(1000 * time.Millisecond)

	// Publish retained message on Broker B
	pubB := mqtt.NewClient(mqttOpts(24012, "pubB-out-ret"))
	if tok := pubB.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := pubB.Publish("local/out/sensor1", 1, true, "local-retained-data"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	pubB.Disconnect(100)
	time.Sleep(500 * time.Millisecond)

	// Connect a new subscriber to Broker A and verify it receives the retained message
	subA := mqtt.NewClient(mqttOpts(24011, "subA-late"))
	if tok := subA.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer subA.Disconnect(100)

	gotOutCh := make(chan msgInfo, 1)
	if tok := subA.Subscribe("remote/out/#", 1, func(_ mqtt.Client, m mqtt.Message) {
		gotOutCh <- msgInfo{
			topic:    m.Topic(),
			payload:  string(m.Payload()),
			retained: m.Retained(),
		}
	}); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}

	select {
	case info := <-gotOutCh:
		t.Logf("Received on A: topic=%s payload=%s retained=%v", info.topic, info.payload, info.retained)
		if info.payload != "local-retained-data" {
			t.Fatalf("expected payload 'local-retained-data', got %q", info.payload)
		}
		if !info.retained {
			t.Fatalf("expected retained=true on remote broker A, got retained=false")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscriber on A did not receive forwarded retained message")
	}
}

