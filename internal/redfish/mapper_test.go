package redfish

import (
	"testing"
)

func TestEvaluateJSONPath(t *testing.T) {
	data := map[string]any{
		"metrics": map[string]any{
			"temperature": 23.5,
			"humidity":    55.0,
		},
		"sensors": []any{
			map[string]any{"id": "s1", "val": 10.1},
			map[string]any{"id": "s2", "val": 20.2},
		},
	}

	val, ok := EvaluateJSONPath(data, "$.metrics.temperature")
	if !ok || val != 23.5 {
		t.Fatalf("expected 23.5, got %v (ok=%v)", val, ok)
	}

	val, ok = EvaluateJSONPath(data, "$.sensors[1].val")
	if !ok || val != 20.2 {
		t.Fatalf("expected 20.2, got %v (ok=%v)", val, ok)
	}

	val, ok = EvaluateJSONPath(data, "$.sensors[*]")
	if !ok {
		t.Fatalf("expected array, got %v", val)
	}
	slice, isSlice := val.([]any)
	if !isSlice || len(slice) != 2 {
		t.Fatalf("expected slice of 2 items, got %v", slice)
	}
}

func TestExtractSensorRecordsFlat(t *testing.T) {
	gw := &GatewayConfig{
		TopicPrefix:        "redfish",
		ChassisID:          "EdgeNode",
		DefaultReadingType: "Temperature",
		DefaultReadingUnits: "Cel",
		Thresholds: &ThresholdsConfig{
			UpperCaution:  floatPtr(70.0),
			UpperCritical: floatPtr(85.0),
		},
		JSONSchema: map[string]any{
			"mapping": map[string]any{
				"reading":  "$.metrics.temp",
				"sensorId": "$.sensor.id",
				"ts":       "$.timestamp",
			},
		},
	}

	payload := []byte(`{
		"timestamp": "2026-08-20T12:00:00Z",
		"sensor": {"id": "temp-cpu"},
		"metrics": {"temp": 72.5}
	}`)

	records, err := ExtractSensorRecords(payload, "factory/node1/telemetry", gw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}

	r := records[0]
	if r.SensorID != "temp-cpu" {
		t.Errorf("expected sensorId 'temp-cpu', got %s", r.SensorID)
	}
	if r.ChassisID != "EdgeNode" {
		t.Errorf("expected chassisId 'EdgeNode', got %s", r.ChassisID)
	}
	if r.Reading != 72.5 {
		t.Errorf("expected reading 72.5, got %f", r.Reading)
	}
	if r.Health != "Warning" {
		t.Errorf("expected health 'Warning', got %s", r.Health)
	}
	if r.Timestamp != "2026-08-20T12:00:00Z" {
		t.Errorf("expected timestamp '2026-08-20T12:00:00Z', got %s", r.Timestamp)
	}
	if r.TopicPrefix != "redfish" {
		t.Errorf("expected topicPrefix 'redfish', got %s", r.TopicPrefix)
	}
}

func TestExtractSensorRecordsArrayExpansion(t *testing.T) {
	gw := &GatewayConfig{
		TopicPrefix: "telemetry/redfish",
		ChassisID:   "Rack-A",
		JSONSchema: map[string]any{
			"arrayPath": "$.sensors[*]",
			"mapping": map[string]any{
				"sensorId":     "$.id",
				"reading":      "$.val",
				"readingType":  "$.type",
				"readingUnits": "$.unit",
				"ts":           "$.timestamp",
			},
		},
	}

	payload := []byte(`{
		"timestamp": "2026-08-20T12:30:00Z",
		"sensors": [
			{"id": "t1", "val": 24.1, "type": "Temperature", "unit": "Cel"},
			{"id": "v1", "val": 230.5, "type": "Voltage", "unit": "V"}
		]
	}`)

	records, err := ExtractSensorRecords(payload, "rack/a/sensors", gw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}

	if records[0].SensorID != "t1" || records[0].Reading != 24.1 || records[0].ReadingType != "Temperature" {
		t.Errorf("record 0 mismatch: %+v", records[0])
	}
	if records[1].SensorID != "v1" || records[1].Reading != 230.5 || records[1].ReadingType != "Voltage" {
		t.Errorf("record 1 mismatch: %+v", records[1])
	}
	if records[0].Timestamp != "2026-08-20T12:30:00Z" {
		t.Errorf("expected inherited root timestamp, got %s", records[0].Timestamp)
	}
	if records[0].TopicPrefix != "telemetry/redfish" {
		t.Errorf("expected topicPrefix 'telemetry/redfish', got %s", records[0].TopicPrefix)
	}
}

func floatPtr(v float64) *float64 {
	return &v
}
