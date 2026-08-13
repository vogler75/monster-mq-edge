package integration

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"monstermq.io/edge/internal/config"
)

// TestMQTTBridgeLocalNodeID tests that an MQTT Client bridge created with
// nodeId = "local" is properly recognized as running on the current node
// (isOnCurrentNode = true) and is loaded/executed by the edge broker.
func TestMQTTBridgeLocalNodeID(t *testing.T) {
	dbA := filepath.Join(t.TempDir(), "a.db")
	dbB := filepath.Join(t.TempDir(), "b.db")

	// Remote broker B
	srvB := startWithDB(t, 26201, dbB, func(c *config.Config) {
		c.Features.MqttClient = false
	})
	defer srvB.Close()

	// Edge broker A — NodeID is "edge-node-specific-99", GraphQL on 28201, MQTT on 26200
	cfgA := config.Default()
	cfgA.NodeID = "edge-node-specific-99"
	cfgA.TCP.Port = 26200
	cfgA.GraphQL.Enabled = true
	cfgA.GraphQL.Port = 28201
	cfgA.SQLite.Path = dbA
	cfgA.Features.MqttClient = true
	srvA, urlA := startWithGraphQL(t, 26200, 28201, func(c *config.Config) {
		*c = *cfgA
	})
	_ = srvA
	defer srvA.Close()

	// Subscribe on broker A to verify incoming bridged messages
	subA := mqtt.NewClient(mqttOpts(26200, "subLocalNodeA"))
	if tok := subA.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer subA.Disconnect(100)
	var got atomic.Int32
	rcv := make(chan string, 8)
	if tok := subA.Subscribe("local-in/#", 0, func(_ mqtt.Client, m mqtt.Message) {
		got.Add(1)
		select {
		case rcv <- fmt.Sprintf("%s=%s", m.Topic(), string(m.Payload())):
		default:
		}
	}); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}

	// Create bridge via GraphQL with nodeId = "local"
	res := gqlQuery(t, urlA,
		`mutation C($i:MqttClientInput!){ mqttClient{ create(input:$i){ success client { nodeId isOnCurrentNode } errors } } }`,
		map[string]any{
			"i": map[string]any{
				"name":      "LocalBridgeToB",
				"namespace": "bridge/local",
				"nodeId":    "local",
				"enabled":   true,
				"config": map[string]any{
					"brokerUrl":    fmt.Sprintf("tcp://localhost:%d", 26201),
					"clientId":     "local-node-bridge-client",
					"cleanSession": true,
					"keepAlive":    10,
					"addresses": []map[string]any{
						{
							"mode":        "SUBSCRIBE",
							"remoteTopic": "remote-src/#",
							"localTopic":  "local-in",
							"removePath":  true,
							"qos":         0,
						},
					},
				},
			},
		})

	// Verify GraphQL response confirms nodeId == "local" and isOnCurrentNode == true
	clientRes, ok := res["mqttClient"].(map[string]any)["create"].(map[string]any)["client"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected graphql create response: %v", res)
	}
	if clientRes["nodeId"] != "local" {
		t.Errorf("expected nodeId = 'local', got %v", clientRes["nodeId"])
	}
	if clientRes["isOnCurrentNode"] != true {
		t.Errorf("expected isOnCurrentNode = true, got %v", clientRes["isOnCurrentNode"])
	}

	// Verify brokers query returns single "local" node with isCurrent=true
	brokersRes := gqlQuery(t, urlA, `{ brokers { nodeId isCurrent } }`, nil)
	brokersList, _ := brokersRes["brokers"].([]any)
	if len(brokersList) != 1 {
		t.Fatalf("expected single broker node 'local', got %v", brokersList)
	}
	firstBroker, _ := brokersList[0].(map[string]any)
	if firstBroker["nodeId"] != "local" || firstBroker["isCurrent"] != true {
		t.Fatalf("expected broker {nodeId: 'local', isCurrent: true}, got %v", firstBroker)
	}

	// Also verify MqttClients query with node="local" filter
	queryRes := gqlQuery(t, urlA,
		`query Q($n:String){ mqttClients(node:$n){ name nodeId isOnCurrentNode } }`,
		map[string]any{"n": "local"})
	clients, _ := queryRes["mqttClients"].([]any)
	if len(clients) == 0 {
		t.Fatalf("expected at least 1 client for node='local', got none")
	}

	// Give bridge a moment to connect to B and subscribe
	time.Sleep(500 * time.Millisecond)

	// Publish on B to remote-src/hello
	pubB := mqtt.NewClient(mqttOpts(26201, "pubB"))
	if tok := pubB.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer pubB.Disconnect(100)
	if tok := pubB.Publish("remote-src/hello", 0, false, "world"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}

	// Assert message received on A
	select {
	case msg := <-rcv:
		if msg != "local-in/hello=world" {
			t.Fatalf("expected local-in/hello=world, got %q", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for bridged message on local broker")
	}
}
