package winccua

import (
	"encoding/json"
	"testing"
)

func TestParseConnectionConfig_DefaultsAppliedOnEmpty(t *testing.T) {
	cfg, err := ParseConnectionConfig("")
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if cfg.DataAccessMode != ModeGraphQL {
		t.Errorf("dataAccessMode default: got %q want %q", cfg.DataAccessMode, ModeGraphQL)
	}
	if cfg.MessageFormat != FormatJSONISO {
		t.Errorf("messageFormat default: got %q", cfg.MessageFormat)
	}
	if cfg.ReconnectDelay != 5000 {
		t.Errorf("reconnectDelay default: got %d", cfg.ReconnectDelay)
	}
	if cfg.ConnectionTimeout != 10000 {
		t.Errorf("connectionTimeout default: got %d", cfg.ConnectionTimeout)
	}
}

func TestParseConnectionConfig_RoundTripsJavaShape(t *testing.T) {
	// Shape minted by WinCCUaConnectionConfig.toJsonObject() in the Kotlin
	// broker. Edge must round-trip identical bytes so the same row works in
	// both backends.
	in := `{
        "dataAccessMode":"OPENPIPE",
        "graphqlEndpoint":"http://winccua:4000/graphql",
        "websocketEndpoint":"",
        "username":"hmiuser",
        "password":"secret",
        "pipePath":"/tmp/HmiRuntime",
        "reconnectDelay":5000,
        "connectionTimeout":10000,
        "messageFormat":"JSON_ISO",
        "transformConfig":{"convertDotToSlash":true,"convertUnderscoreToSlash":false},
        "addresses":[
          {"type":"TAG_VALUES","topic":"tags","retained":false,"nameFilters":["HMI_*"],"includeQuality":false,"pipeTagMode":"SINGLE"},
          {"type":"ACTIVE_ALARMS","topic":"alarms","retained":true,"systemNames":["RT_1"],"filterString":"State = 1"}
        ]
    }`
	cfg, err := ParseConnectionConfig(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		t.Fatalf("validate (openpipe should accept blank graphql endpoint): %v", errs)
	}
	if cfg.DataAccessMode != ModeOpenPipe {
		t.Errorf("dataAccessMode: %q", cfg.DataAccessMode)
	}
	if got := len(cfg.Addresses); got != 2 {
		t.Fatalf("addresses len: %d", got)
	}
	if cfg.Addresses[0].PipeTagMode != PipeTagModeSingle {
		t.Errorf("pipeTagMode: %q", cfg.Addresses[0].PipeTagMode)
	}
	if !cfg.Addresses[1].Retained {
		t.Errorf("alarm retained should be true")
	}

	// Re-encode and re-parse to ensure stable shape.
	out, _ := json.Marshal(cfg)
	cfg2, err := ParseConnectionConfig(string(out))
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if cfg2.Addresses[1].FilterString != "State = 1" {
		t.Errorf("filter lost: %q", cfg2.Addresses[1].FilterString)
	}
}

func TestValidate_GraphQLRequiresEndpointAndCreds(t *testing.T) {
	cfg := &ConnectionConfig{DataAccessMode: ModeGraphQL, MessageFormat: FormatJSONISO,
		ReconnectDelay: 5000, ConnectionTimeout: 10000}
	errs := cfg.Validate()
	want := map[string]bool{
		"graphqlEndpoint cannot be blank": true,
		"username cannot be blank":         true,
		"password cannot be blank":         true,
	}
	for _, e := range errs {
		delete(want, e)
	}
	if len(want) > 0 {
		t.Errorf("missing validation errors: %v (got %v)", want, errs)
	}
}

func TestValidate_RejectsInvalidRegex(t *testing.T) {
	cfg := &ConnectionConfig{
		DataAccessMode: ModeOpenPipe, MessageFormat: FormatJSONISO,
		ReconnectDelay: 5000, ConnectionTimeout: 10000,
		TransformConfig: TransformConfig{RegexPattern: "[unterminated"},
	}
	if errs := cfg.Validate(); len(errs) == 0 {
		t.Error("expected regex validation error")
	}
}

func TestTransformTagName(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		cfg    TransformConfig
		want   string
	}{
		{"dot to slash", "Pump.Motor.Speed", TransformConfig{ConvertDotToSlash: true}, "Pump/Motor/Speed"},
		{"trim trailing dot then convert", "A.B.", TransformConfig{ConvertDotToSlash: true}, "A/B"},
		{"underscore to slash", "PLANT_AREA_TAG", TransformConfig{ConvertUnderscoreToSlash: true}, "PLANT/AREA/TAG"},
		{"both off", "Pump.Motor", TransformConfig{}, "Pump.Motor"},
		{"regex extract", "RT_1::Tag_2", TransformConfig{RegexPattern: "::", RegexReplacement: "/"}, "RT_1/Tag_2"},
		{"trim outer slashes after rules", "/Top/", TransformConfig{}, "Top"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tc := c.cfg
			if got := tc.TransformTagName(c.in); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestWebSocketEndpointDerivation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://winccua:4000/graphql", "ws://winccua:4000/graphql"},
		{"https://srv:4001/api", "wss://srv:4001/api"},
	}
	for _, c := range cases {
		cfg := &ConnectionConfig{GraphqlEndpoint: c.in}
		if got := cfg.WebSocketEndpoint(); got != c.want {
			t.Errorf("derive(%s): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestTagErrorIsZero(t *testing.T) {
	cases := []struct {
		v    any
		want bool
	}{
		{nil, true},
		{"", true},
		{"0", true},
		{0, true},
		{int64(0), true},
		{float64(0), true},
		{"5", false},
		{42, false},
		{"abc", false},
	}
	for _, c := range cases {
		if got := tagErrorIsZero(c.v); got != c.want {
			t.Errorf("tagErrorIsZero(%v) = %v, want %v", c.v, got, c.want)
		}
	}
}
