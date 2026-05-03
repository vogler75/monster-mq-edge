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

// TestMetricsCollectorAndPersistence publishes a few messages and confirms
// the broker records broker-level metrics in SQLite and surfaces them via GraphQL.
func TestMetricsCollectorAndPersistence(t *testing.T) {
	srv, url := startWithGraphQL(t, 23010, 28010)
	defer srv.Close()

	pub := mqtt.NewClient(mqttOpts(23010, "metrics-pub"))
	if tok := pub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	for i := 0; i < 10; i++ {
		_ = pub.Publish("m/x", 0, false, "x").WaitTimeout(2 * time.Second)
	}
	pub.Disconnect(100)

	// One full collector tick (default 1s) plus margin.
	time.Sleep(1500 * time.Millisecond)

	data := gqlQuery(t, url, `{ broker { metrics { messagesIn nodeSessionCount } metricsHistory(lastMinutes: 1) { messagesIn timestamp } } }`, nil)
	br := data["broker"].(map[string]any)
	metrics := br["metrics"].([]any)
	if len(metrics) == 0 {
		t.Fatal("expected current metrics")
	}
	hist := br["metricsHistory"].([]any)
	if len(hist) == 0 {
		t.Fatal("expected at least one metrics history row in SQLite")
	}

	data = gqlQuery(t, url, `{ archiveGroup(name: "Default") { metrics { messagesOut bufferSize timestamp } metricsHistory(lastMinutes: 1) { messagesOut bufferSize timestamp } } }`, nil)
	ag := data["archiveGroup"].(map[string]any)
	archiveMetrics := ag["metrics"].([]any)
	if len(archiveMetrics) == 0 {
		t.Fatal("expected current archive metrics")
	}
	if archiveMetrics[0].(map[string]any)["bufferSize"] == nil {
		t.Fatal("expected archive buffer size")
	}
	archiveHist := ag["metricsHistory"].([]any)
	if len(archiveHist) == 0 {
		t.Fatal("expected at least one archive metrics history row in SQLite")
	}
	foundOut := false
	for _, row := range archiveHist {
		if row.(map[string]any)["messagesOut"].(float64) > 0 {
			foundOut = true
			break
		}
	}
	if !foundOut {
		t.Fatal("expected archive messagesOut to be recorded")
	}
}

func TestMqttClientMetricsCollectorAndPersistence(t *testing.T) {
	remoteCfg := config.Default()
	remoteCfg.NodeID = "mqtt-metrics-remote"
	remoteCfg.TCP.Port = 23011
	remoteCfg.GraphQL.Enabled = false
	remoteCfg.Metrics.Enabled = false
	remoteCfg.SQLite.Path = filepath.Join(t.TempDir(), "remote.db")
	remote, err := broker.New(remoteCfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = remote.Serve() }()
	defer remote.Close()
	time.Sleep(200 * time.Millisecond)

	bridgeDB := filepath.Join(t.TempDir(), "bridge.db")
	bootDB, err := sqlite.Open(bridgeDB)
	if err != nil {
		t.Fatal(err)
	}
	dcs := sqlite.NewDeviceConfigStore(bootDB)
	if err := dcs.EnsureTable(context.Background()); err != nil {
		t.Fatal(err)
	}
	bridgeCfg := map[string]any{
		"brokerUrl":    "tcp://localhost:23011",
		"clientId":     "metrics-bridge",
		"cleanSession": true,
		"keepAlive":    10,
		"addresses": []map[string]any{
			{"mode": "PUBLISH", "localTopic": "bridge-out/+", "remoteTopic": "remote-out"},
			{"mode": "SUBSCRIBE", "remoteTopic": "remote-in/+", "localTopic": "bridge-in", "qos": 0},
		},
	}
	cfgJSON, _ := json.Marshal(bridgeCfg)
	if err := dcs.Save(context.Background(), stores.DeviceConfig{
		Name: "metrics-to-remote", Namespace: "bridge", NodeID: "mqtt-metrics-local", Type: "MQTT_CLIENT",
		Enabled: true, Config: string(cfgJSON),
	}); err != nil {
		t.Fatal(err)
	}
	_ = bootDB.Close()

	local, url := startWithGraphQL(t, 23012, 28012, func(cfg *config.Config) {
		cfg.NodeID = "mqtt-metrics-local"
		cfg.SQLite.Path = bridgeDB
		cfg.Features.MqttClient = true
	})
	defer local.Close()
	time.Sleep(800 * time.Millisecond)

	remoteSub := mqtt.NewClient(mqttOpts(23011, "mqtt-metrics-remote-sub"))
	if tok := remoteSub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer remoteSub.Disconnect(100)
	gotRemote := make(chan string, 1)
	if tok := remoteSub.Subscribe("remote-out/#", 0, func(_ mqtt.Client, m mqtt.Message) {
		gotRemote <- fmt.Sprintf("%s=%s", m.Topic(), string(m.Payload()))
	}); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}

	localPub := mqtt.NewClient(mqttOpts(23012, "mqtt-metrics-local-pub"))
	if tok := localPub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer localPub.Disconnect(100)
	if tok := localPub.Publish("bridge-out/x", 0, false, "out"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	select {
	case <-gotRemote:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge outbound did not deliver")
	}

	localSub := mqtt.NewClient(mqttOpts(23012, "mqtt-metrics-local-sub"))
	if tok := localSub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer localSub.Disconnect(100)
	gotLocal := make(chan string, 1)
	if tok := localSub.Subscribe("bridge-in/#", 0, func(_ mqtt.Client, m mqtt.Message) {
		gotLocal <- fmt.Sprintf("%s=%s", m.Topic(), string(m.Payload()))
	}); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}

	remotePub := mqtt.NewClient(mqttOpts(23011, "mqtt-metrics-remote-pub"))
	if tok := remotePub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer remotePub.Disconnect(100)
	if tok := remotePub.Publish("remote-in/x", 0, false, "in"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	select {
	case <-gotLocal:
	case <-time.After(3 * time.Second):
		t.Fatal("bridge inbound did not deliver")
	}

	time.Sleep(1500 * time.Millisecond)

	data := gqlQuery(t, url, `{ mqttClients(name: "metrics-to-remote") { metrics { messagesIn messagesOut timestamp } metricsHistory(lastMinutes: 1) { messagesIn messagesOut timestamp } } broker { metrics { mqttClientIn mqttClientOut } } }`, nil)
	clients := data["mqttClients"].([]any)
	if len(clients) != 1 {
		t.Fatalf("expected one mqtt client, got %d", len(clients))
	}
	client := clients[0].(map[string]any)
	current := client["metrics"].([]any)
	if len(current) == 0 {
		t.Fatal("expected current mqtt client metrics")
	}
	history := client["metricsHistory"].([]any)
	if len(history) == 0 {
		t.Fatal("expected mqtt client metrics history")
	}
	var foundIn, foundOut bool
	for _, row := range history {
		m := row.(map[string]any)
		foundIn = foundIn || m["messagesIn"].(float64) > 0
		foundOut = foundOut || m["messagesOut"].(float64) > 0
	}
	if !foundIn || !foundOut {
		t.Fatalf("expected mqtt client messagesIn and messagesOut history, foundIn=%v foundOut=%v", foundIn, foundOut)
	}
}
