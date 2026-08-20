package winccua

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
)

func TestGraphQLPublishTagValueOmitsQualityWhenIncludeQualityDisabled(t *testing.T) {
	pub := newPublisher("ns", TransformConfig{}, FormatJSONISO)
	var captured []byte

	conn := &graphqlConnector{
		cfg:     &ConnectionConfig{},
		pub:     pub,
		publish: func(_ string, payload []byte, _ bool, _ byte) error { captured = append([]byte(nil), payload...); return nil },
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	conn.publishTagValue(Address{
		Topic:          "tags",
		Retained:       false,
		IncludeQuality: false,
	}, "Pump.Speed", 12, "2026-01-01T00:00:00Z", map[string]any{"quality": "Unknown"}, map[string]any{"Name": "Pump.Speed", "Value": 12})

	out := map[string]any{}
	if err := json.Unmarshal(captured, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := out["quality"]; ok {
		t.Fatalf("expected quality to be omitted, got payload %s", string(captured))
	}
}

func TestPipePublishTagValueOmitsQualityWhenIncludeQualityDisabled(t *testing.T) {
	pub := newPublisher("ns", TransformConfig{}, FormatJSONISO)
	var captured []byte

	conn := &pipeConnector{
		cfg:     &ConnectionConfig{},
		pub:     pub,
		publish: func(_ string, payload []byte, _ bool, _ byte) error { captured = append([]byte(nil), payload...); return nil },
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	conn.publishTagValue(Address{
		Topic:          "tags",
		Retained:       false,
		IncludeQuality: false,
	}, "Pump.Speed", 12, "2026-01-01T00:00:00Z", map[string]any{"quality": "Unknown"}, map[string]any{"Name": "Pump.Speed", "Value": 12})

	out := map[string]any{}
	if err := json.Unmarshal(captured, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := out["quality"]; ok {
		t.Fatalf("expected quality to be omitted, got payload %s", string(captured))
	}
}

func TestPipePublishTagValueIncludesFlatQualityWhenIncludeQualityEnabled(t *testing.T) {
	pub := newPublisher("ns", TransformConfig{}, FormatJSONISO)
	var captured []byte

	conn := &pipeConnector{
		cfg:     &ConnectionConfig{},
		pub:     pub,
		publish: func(_ string, payload []byte, _ bool, _ byte) error { captured = append([]byte(nil), payload...); return nil },
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	rawTag := map[string]any{
		"Name":             "Pump.Speed",
		"Quality":          "Good",
		"QualityCode":      "192",
		"TimeStamp":        "2026-08-20 11:54:49.3160000",
		"Value":            "2179.176758",
		"ErrorCode":        0,
		"ErrorDescription": "",
	}

	conn.publishTagValue(Address{
		Topic:          "tags",
		Retained:       false,
		IncludeQuality: true,
	}, "Pump.Speed", "2179.176758", "2026-08-20 11:54:49.3160000", map[string]any{"quality": "Good", "qualityCode": "192"}, rawTag)

	out := map[string]any{}
	if err := json.Unmarshal(captured, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if out["quality"] != "Good" {
		t.Fatalf("expected quality='Good', got %v (payload: %s)", out["quality"], string(captured))
	}
	if out["qualityCode"] != "192" {
		t.Fatalf("expected qualityCode='192', got %v (payload: %s)", out["qualityCode"], string(captured))
	}
	if out["value"] != "2179.176758" {
		t.Fatalf("expected value='2179.176758', got %v", out["value"])
	}
	if out["time"] != "2026-08-20 11:54:49.3160000" {
		t.Fatalf("expected time='2026-08-20 11:54:49.3160000', got %v", out["time"])
	}
}

func TestPipePublishTagValueFormatRawJSON(t *testing.T) {
	pub := newPublisher("ns", TransformConfig{}, FormatRawJSON)
	var captured []byte

	conn := &pipeConnector{
		cfg:     &ConnectionConfig{},
		pub:     pub,
		publish: func(_ string, payload []byte, _ bool, _ byte) error { captured = append([]byte(nil), payload...); return nil },
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	rawTag := map[string]any{
		"Name":             "Tag_0",
		"Quality":          "Good",
		"QualityCode":      "192",
		"TimeStamp":        "2019-01-30T11:25:35Z",
		"Value":            "16",
		"ErrorCode":        float64(0),
		"ErrorDescription": "",
	}

	conn.publishTagValue(Address{
		Topic:          "tags",
		Retained:       false,
		IncludeQuality: true,
	}, "Tag_0", "16", "2019-01-30T11:25:35Z", map[string]any{"quality": "Good", "qualityCode": "192"}, rawTag)

	out := map[string]any{}
	if err := json.Unmarshal(captured, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if out["Name"] != "Tag_0" || out["Value"] != "16" || out["Quality"] != "Good" || out["QualityCode"] != "192" {
		t.Fatalf("expected raw tag 1:1, got: %s", string(captured))
	}
}

