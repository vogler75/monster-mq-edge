package winccoa

import (
	"encoding/json"
	"testing"
)

func TestParseConnectionConfigDefaults(t *testing.T) {
	cfg, err := ParseConnectionConfig(`{}`)
	if err != nil {
		t.Fatalf("ParseConnectionConfig: %v", err)
	}
	if cfg.GraphqlEndpoint != "http://winccoa:4000/graphql" {
		t.Fatalf("unexpected default endpoint: %q", cfg.GraphqlEndpoint)
	}
	if cfg.WebSocketEndpoint() != "ws://winccoa:4000/graphql" {
		t.Fatalf("unexpected websocket endpoint: %q", cfg.WebSocketEndpoint())
	}
	if cfg.MessageFormat != FormatJSONISO {
		t.Fatalf("unexpected message format: %q", cfg.MessageFormat)
	}
	if !cfg.TransformConfig.RemoveSystemName || !cfg.TransformConfig.ConvertDotToSlash {
		t.Fatalf("default transform not applied: %+v", cfg.TransformConfig)
	}
}

func TestParseConnectionConfigKeepsExplicitEmptyTransform(t *testing.T) {
	cfg, err := ParseConnectionConfig(`{"transformConfig":{"removeSystemName":false,"convertDotToSlash":false,"convertUnderscoreToSlash":false}}`)
	if err != nil {
		t.Fatalf("ParseConnectionConfig: %v", err)
	}
	if cfg.TransformConfig.RemoveSystemName || cfg.TransformConfig.ConvertDotToSlash || cfg.TransformConfig.ConvertUnderscoreToSlash {
		t.Fatalf("explicit false transform was overwritten: %+v", cfg.TransformConfig)
	}
}

func TestParseConnectionConfigDefaultsMissingNestedTransformFields(t *testing.T) {
	cfg, err := ParseConnectionConfig(`{"transformConfig":{"convertUnderscoreToSlash":true}}`)
	if err != nil {
		t.Fatalf("ParseConnectionConfig: %v", err)
	}
	if !cfg.TransformConfig.RemoveSystemName || !cfg.TransformConfig.ConvertDotToSlash || !cfg.TransformConfig.ConvertUnderscoreToSlash {
		t.Fatalf("missing nested transform defaults not applied: %+v", cfg.TransformConfig)
	}
}

func TestTransformDatapointName(t *testing.T) {
	tc := TransformConfig{
		RemoveSystemName:         true,
		ConvertDotToSlash:        true,
		ConvertUnderscoreToSlash: true,
		RegexPattern:             `^Plant/`,
		RegexReplacement:         "",
	}
	got := tc.TransformDatapointName("SYS:Plant.Line_1.Motor.")
	if got != "Line/1/Motor" {
		t.Fatalf("unexpected transformed name: %q", got)
	}
}

func TestConnectionConfigValidate(t *testing.T) {
	cfg := &ConnectionConfig{
		GraphqlEndpoint:   "mqtt://bad",
		Username:          "user",
		ReconnectDelay:    10,
		ConnectionTimeout: 10,
		MessageFormat:     "BAD",
		Addresses:         []Address{{Query: "", Topic: ""}},
	}
	errs := cfg.Validate()
	if len(errs) < 5 {
		t.Fatalf("expected validation errors, got %v", errs)
	}
}

func TestPublisherFormatsRows(t *testing.T) {
	pub := newPublisher("ns", TransformConfig{RemoveSystemName: true, ConvertDotToSlash: true}, FormatJSONMS)
	topic := pub.resolveDatapointTopic("oa", "SYS:Plant.Line.Value")
	if topic != "ns/oa/Plant/Line/Value" {
		t.Fatalf("unexpected topic: %q", topic)
	}
	payload := pub.formatDatapointPayload(map[string]any{
		"value": 12.5,
		"stime": "2026-05-03T10:15:30Z",
	}, 12.5)
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if out["stime"] != float64(1777803330000) {
		t.Fatalf("stime was not converted to millis: %v", out["stime"])
	}
}

func TestPublisherFormatsBufferRawValue(t *testing.T) {
	pub := newPublisher("", TransformConfig{}, FormatRawValue)
	payload := pub.formatDatapointPayload(nil, map[string]any{
		"type": "Buffer",
		"data": []any{float64(1), float64(2), float64(255)},
	})
	if string(payload) != string([]byte{1, 2, 255}) {
		t.Fatalf("unexpected raw buffer: %v", payload)
	}
}
