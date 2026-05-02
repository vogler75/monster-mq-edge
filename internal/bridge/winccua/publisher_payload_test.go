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
	}, "Pump.Speed", 12, "2026-01-01T00:00:00Z", map[string]any{"quality": "Unknown"})

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
	}, "Pump.Speed", 12, "2026-01-01T00:00:00Z", map[string]any{"quality": "Unknown"})

	out := map[string]any{}
	if err := json.Unmarshal(captured, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, ok := out["quality"]; ok {
		t.Fatalf("expected quality to be omitted, got payload %s", string(captured))
	}
}
