package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
)

func TestRedfishEndpoints(t *testing.T) {
	mqttPort := 21910
	gqlPort := 21911

	cfg := config.Default()
	cfg.NodeID = "test-redfish-node"
	cfg.TCP.Enabled = true
	cfg.TCP.Port = mqttPort
	cfg.WS.Enabled = false
	cfg.GraphQL.Enabled = true
	cfg.GraphQL.Port = gqlPort
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "redfish.db")

	cfg.Features.Redfish = true
	cfg.Redfish.Enabled = true
	cfg.Redfish.MountPath = "/redfish/v1"
	cfg.Redfish.DefaultChassisId = "EdgeNode"

	srv, err := broker.New(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	go func() { _ = srv.Serve() }()
	defer srv.Close()

	// Wait for server ready
	baseURL := fmt.Sprintf("http://localhost:%d/redfish/v1", gqlPort)
	waitForHTTP(t, fmt.Sprintf("http://localhost:%d/health", gqlPort))

	// 1. ServiceRoot
	resp := httpGet(t, baseURL)
	if resp["Id"] != "RootService" || resp["RedfishVersion"] != "1.18.0" {
		t.Fatalf("unexpected ServiceRoot: %+v", resp)
	}
	if resp["@odata.id"] != "/redfish/v1" {
		t.Fatalf("unexpected @odata.id: %v", resp["@odata.id"])
	}

	// 2. Chassis Collection
	chassisCol := httpGet(t, baseURL+"/Chassis")
	if chassisCol["@odata.type"] != "#ChassisCollection.ChassisCollection" {
		t.Fatalf("unexpected Chassis collection: %+v", chassisCol)
	}

	// 3. Default Chassis
	chassis := httpGet(t, baseURL+"/Chassis/EdgeNode")
	if chassis["Id"] != "EdgeNode" || chassis["@odata.type"] != "#Chassis.v1_22_0.Chassis" {
		t.Fatalf("unexpected Chassis: %+v", chassis)
	}

	// 4. Systems Collection & System
	sysCol := httpGet(t, baseURL+"/Systems")
	if sysCol["Members@odata.count"].(float64) < 1 {
		t.Fatalf("expected at least 1 system: %+v", sysCol)
	}

	// 5. Managers Collection & Manager
	mgrCol := httpGet(t, baseURL+"/Managers")
	if mgrCol["Members@odata.count"].(float64) < 1 {
		t.Fatalf("expected at least 1 manager: %+v", mgrCol)
	}

	// 6. TelemetryService
	ts := httpGet(t, baseURL+"/TelemetryService")
	if ts["Id"] != "TelemetryService" {
		t.Fatalf("unexpected TelemetryService: %+v", ts)
	}
}

func TestRedfishMQTTPayloadExtractionAndSensors(t *testing.T) {
	mqttPort := 21920
	gqlPort := 21921

	cfg := config.Default()
	cfg.NodeID = "test-redfish-mqtt"
	cfg.TCP.Enabled = true
	cfg.TCP.Port = mqttPort
	cfg.WS.Enabled = false
	cfg.GraphQL.Enabled = true
	cfg.GraphQL.Port = gqlPort
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "redfish_mqtt.db")

	cfg.Features.Redfish = true
	cfg.Redfish.Enabled = true
	cfg.Redfish.MountPath = "/redfish/v1"
	cfg.Redfish.DefaultChassisId = "EdgeNode"

	srv, err := broker.New(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	go func() { _ = srv.Serve() }()
	defer srv.Close()

	gqlURL := fmt.Sprintf("http://localhost:%d/graphql", gqlPort)
	redfishURL := fmt.Sprintf("http://localhost:%d/redfish/v1", gqlPort)
	waitForHTTP(t, fmt.Sprintf("http://localhost:%d/health", gqlPort))

	// 1. Configure Gateway via GraphQL Mutation
	saveMutation := `
		mutation SaveGw($name: String!, $cfg: RedfishMappingConfigInput!) {
			saveRedfishMapping(name: $name, config: $cfg, enabled: true) {
				success
				redfish {
					name
					enabled
					config {
						topicPrefix
						topicFilters
					}
				}
			}
		}
	`
	saveVars := map[string]any{
		"name": "EnvSensors",
		"cfg": map[string]any{
			"topicPrefix":         "redfish",
			"topicFilters":        []string{"sensors/+/telemetry"},
			"chassisId":           "EdgeNode",
			"defaultReadingType":  "Temperature",
			"defaultReadingUnits": "Cel",
			"thresholds": map[string]any{
				"upperCaution":  70.0,
				"upperCritical": 85.0,
			},
			"jsonSchema": map[string]any{
				"arrayPath": "$.readings[*]",
				"mapping": map[string]any{
					"sensorId":     "$.id",
					"name":         "$.name",
					"reading":      "$.value",
					"readingType":  "$.type",
					"readingUnits": "$.unit",
					"ts":           "$.timestamp",
				},
			},
		},
	}
	gqlRes := gqlQuery(t, gqlURL, saveMutation, saveVars)
	data, ok := gqlRes["saveRedfishMapping"].(map[string]any)
	if !ok || data["success"] != true {
		t.Fatalf("saveRedfishMapping failed: %+v", gqlRes)
	}

	time.Sleep(100 * time.Millisecond)

	// 2. Publish MQTT Message
	pubClient := mqtt.NewClient(mqttOpts(mqttPort, "pub-redfish-test"))
	if tok := pubClient.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("pub connect: %v", tok.Error())
	}
	defer pubClient.Disconnect(100)

	payload := `{
		"timestamp": "2026-08-20T12:30:00Z",
		"readings": [
			{"id": "temp-cpu", "name": "CPU Temp", "value": 45.5, "type": "Temperature", "unit": "Cel"},
			{"id": "volt-main", "name": "Main Voltage", "value": 230.1, "type": "Voltage", "unit": "V"}
		]
	}`

	if tok := pubClient.Publish("sensors/rack1/telemetry", 0, false, []byte(payload)); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("publish: %v", tok.Error())
	}

	time.Sleep(200 * time.Millisecond)

	// 3. Query Redfish Sensors Collection
	sensorsCol := httpGet(t, redfishURL+"/Chassis/EdgeNode/Sensors")
	count := sensorsCol["Members@odata.count"].(float64)
	if count != 2 {
		t.Fatalf("expected 2 sensors in collection, got %v: %+v", count, sensorsCol)
	}

	// 4. Query Individual Sensor Resource
	tempSensor := httpGet(t, redfishURL+"/Chassis/EdgeNode/Sensors/temp-cpu")
	if tempSensor["Id"] != "temp-cpu" || tempSensor["Reading"].(float64) != 45.5 {
		t.Fatalf("unexpected temp sensor: %+v", tempSensor)
	}
	if tempSensor["ReadingType"] != "Temperature" || tempSensor["ReadingUnits"] != "Cel" {
		t.Fatalf("unexpected reading metadata: %+v", tempSensor)
	}
	status := tempSensor["Status"].(map[string]any)
	if status["Health"] != "OK" {
		t.Fatalf("expected Health OK, got %v", status["Health"])
	}

	// 5. Query Legacy Thermal Resource
	thermal := httpGet(t, redfishURL+"/Chassis/EdgeNode/Thermal")
	temps := thermal["Temperatures"].([]any)
	if len(temps) != 1 {
		t.Fatalf("expected 1 temperature member in /Thermal, got %d", len(temps))
	}
	temp0 := temps[0].(map[string]any)
	if temp0["ReadingCelsius"].(float64) != 45.5 {
		t.Fatalf("unexpected ReadingCelsius: %v", temp0["ReadingCelsius"])
	}

	// 6. Query Legacy Power Resource
	power := httpGet(t, redfishURL+"/Chassis/EdgeNode/Power")
	volts := power["Voltages"].([]any)
	if len(volts) != 1 {
		t.Fatalf("expected 1 voltage member in /Power, got %d", len(volts))
	}
	volt0 := volts[0].(map[string]any)
	if volt0["ReadingVolts"].(float64) != 230.1 {
		t.Fatalf("unexpected ReadingVolts: %v", volt0["ReadingVolts"])
	}

	// 7. Query GraphQL Live Sensors
	liveSensorsQuery := `
		query {
			redfishLiveSensors(chassisId: "EdgeNode") {
				id
				name
				chassisId
				reading
				health
			}
		}
	`
	gqlLiveRes := gqlQuery(t, gqlURL, liveSensorsQuery, nil)
	liveSensors := gqlLiveRes["redfishLiveSensors"].([]any)
	if len(liveSensors) != 2 {
		t.Fatalf("expected 2 live sensors in GraphQL query, got %d", len(liveSensors))
	}
}

func TestRedfishThresholdHealth(t *testing.T) {
	mqttPort := 21930
	gqlPort := 21931

	cfg := config.Default()
	cfg.NodeID = "test-redfish-threshold"
	cfg.TCP.Enabled = true
	cfg.TCP.Port = mqttPort
	cfg.WS.Enabled = false
	cfg.GraphQL.Enabled = true
	cfg.GraphQL.Port = gqlPort
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "redfish_thresh.db")

	cfg.Features.Redfish = true
	cfg.Redfish.Enabled = true
	cfg.Redfish.MountPath = "/redfish/v1"
	cfg.Redfish.DefaultChassisId = "EdgeNode"

	srv, err := broker.New(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	go func() { _ = srv.Serve() }()
	defer srv.Close()

	gqlURL := fmt.Sprintf("http://localhost:%d/graphql", gqlPort)
	redfishURL := fmt.Sprintf("http://localhost:%d/redfish/v1", gqlPort)
	waitForHTTP(t, fmt.Sprintf("http://localhost:%d/health", gqlPort))

	// Save mapping with thresholds: Caution=70, Critical=85
	saveMutation := `
		mutation SaveGw($name: String!, $cfg: RedfishMappingConfigInput!) {
			saveRedfishMapping(name: $name, config: $cfg, enabled: true) {
				success
			}
		}
	`
	saveVars := map[string]any{
		"name": "ThresholdGw",
		"cfg": map[string]any{
			"topicPrefix":         "redfish",
			"topicFilters":        []string{"telemetry/temperature"},
			"chassisId":           "EdgeNode",
			"defaultReadingType":  "Temperature",
			"defaultReadingUnits": "Cel",
			"thresholds": map[string]any{
				"upperCaution":  70.0,
				"upperCritical": 85.0,
			},
			"jsonSchema": map[string]any{
				"mapping": map[string]any{
					"sensorId": "$.id",
					"reading":  "$.temp",
				},
			},
		},
	}
	gqlQuery(t, gqlURL, saveMutation, saveVars)

	time.Sleep(100 * time.Millisecond)

	pubClient := mqtt.NewClient(mqttOpts(mqttPort, "pub-thresh"))
	if tok := pubClient.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("pub connect: %v", tok.Error())
	}
	defer pubClient.Disconnect(100)

	// Publish Critical temperature (92.0 >= 85.0)
	pubClient.Publish("telemetry/temperature", 0, false, []byte(`{"id": "cpu-temp", "temp": 92.0}`))
	time.Sleep(200 * time.Millisecond)

	sensor := httpGet(t, redfishURL+"/Chassis/EdgeNode/Sensors/cpu-temp")
	status := sensor["Status"].(map[string]any)
	if status["Health"] != "Critical" {
		t.Fatalf("expected Health 'Critical', got %v", status["Health"])
	}

	// Chassis health rollup should also be Critical
	chassis := httpGet(t, redfishURL+"/Chassis/EdgeNode")
	chStatus := chassis["Status"].(map[string]any)
	if chStatus["Health"] != "Critical" {
		t.Fatalf("expected Chassis Health 'Critical', got %v", chStatus["Health"])
	}

	// Now publish normal temperature (45.0)
	pubClient.Publish("telemetry/temperature", 0, false, []byte(`{"id": "cpu-temp", "temp": 45.0}`))
	time.Sleep(200 * time.Millisecond)

	sensorOK := httpGet(t, redfishURL+"/Chassis/EdgeNode/Sensors/cpu-temp")
	statusOK := sensorOK["Status"].(map[string]any)
	if statusOK["Health"] != "OK" {
		t.Fatalf("expected Health 'OK', got %v", statusOK["Health"])
	}
}

func TestRedfishGraphQLMutationsAndQueries(t *testing.T) {
	mqttPort := 21940
	gqlPort := 21941

	cfg := config.Default()
	cfg.NodeID = "test-redfish-gql"
	cfg.TCP.Enabled = true
	cfg.TCP.Port = mqttPort
	cfg.WS.Enabled = false
	cfg.GraphQL.Enabled = true
	cfg.GraphQL.Port = gqlPort
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "redfish_gql.db")

	cfg.Features.Redfish = true
	cfg.Redfish.Enabled = true

	srv, err := broker.New(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	go func() { _ = srv.Serve() }()
	defer srv.Close()

	gqlURL := fmt.Sprintf("http://localhost:%d/graphql", gqlPort)
	waitForHTTP(t, fmt.Sprintf("http://localhost:%d/health", gqlPort))

	// 1. Create mapping
	saveMutation := `
		mutation Save($name: String!, $cfg: RedfishMappingConfigInput!) {
			saveRedfishMapping(name: $name, config: $cfg, enabled: true) {
				success
				redfish {
					name
					enabled
					config {
						topicPrefix
						topicFilters
						chassisId
					}
				}
			}
		}
	`
	saveVars := map[string]any{
		"name": "RackSensors",
		"cfg": map[string]any{
			"topicPrefix":  "redfish",
			"topicFilters": []string{"rack/+/sensors"},
			"chassisId":    "Rack-1",
			"jsonSchema": map[string]any{
				"mapping": map[string]any{
					"reading":  "$.val",
					"sensorId": "$.id",
				},
			},
		},
	}
	res := gqlQuery(t, gqlURL, saveMutation, saveVars)
	saveData := res["saveRedfishMapping"].(map[string]any)
	if saveData["success"] != true {
		t.Fatalf("saveRedfishMapping failed: %+v", res)
	}

	// 2. Query all mappings
	listQuery := `
		query {
			redfishMappings {
				name
				enabled
				config {
					topicPrefix
					chassisId
				}
			}
		}
	`
	listRes := gqlQuery(t, gqlURL, listQuery, nil)
	mappings := listRes["redfishMappings"].([]any)
	if len(mappings) != 1 {
		t.Fatalf("expected 1 mapping in list, got %d", len(mappings))
	}
	m0 := mappings[0].(map[string]any)
	if m0["name"] != "RackSensors" || m0["enabled"] != true {
		t.Fatalf("unexpected mapping in list: %+v", m0)
	}

	// 3. Query single mapping
	singleQuery := `
		query GetOne($name: String!) {
			redfishMapping(name: $name) {
				name
				enabled
			}
		}
	`
	singleRes := gqlQuery(t, gqlURL, singleQuery, map[string]any{"name": "RackSensors"})
	singleData := singleRes["redfishMapping"].(map[string]any)
	if singleData["name"] != "RackSensors" {
		t.Fatalf("unexpected single mapping: %+v", singleData)
	}

	// 4. Toggle mapping (disable)
	toggleMutation := `
		mutation Toggle($name: String!, $enabled: Boolean!) {
			toggleRedfishMapping(name: $name, enabled: $enabled) {
				success
				redfish {
					enabled
				}
			}
		}
	`
	toggleRes := gqlQuery(t, gqlURL, toggleMutation, map[string]any{"name": "RackSensors", "enabled": false})
	toggleData := toggleRes["toggleRedfishMapping"].(map[string]any)
	if toggleData["success"] != true || toggleData["redfish"].(map[string]any)["enabled"] != false {
		t.Fatalf("toggle failed: %+v", toggleRes)
	}

	// 5. Delete mapping
	deleteMutation := `
		mutation Delete($name: String!) {
			deleteRedfishMapping(name: $name)
		}
	`
	delRes := gqlQuery(t, gqlURL, deleteMutation, map[string]any{"name": "RackSensors"})
	if delRes["deleteRedfishMapping"] != true {
		t.Fatalf("delete failed: %+v", delRes)
	}

	// 6. Verify empty list
	listAfter := gqlQuery(t, gqlURL, listQuery, nil)
	mappingsAfter := listAfter["redfishMappings"].([]any)
	if len(mappingsAfter) != 0 {
		t.Fatalf("expected 0 mappings after deletion, got %d", len(mappingsAfter))
	}
}

func waitForHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", url)
}

func httpGet(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s error: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("GET %s status %d: %s", url, resp.StatusCode, string(body))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal json from %s: %v (raw: %s)", url, err, string(body))
	}
	return out
}
