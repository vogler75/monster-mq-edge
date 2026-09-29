package winccoanative

import (
	"encoding/json"
	"testing"
	"time"

	"monstermq.io/edge/internal/oahost"
)

// The hand-built payload must match what encoding/json produced before.
func TestValuePayloadMatchesJSON(t *testing.T) {
	ts := time.Date(2026, 9, 29, 10, 0, 0, 123e6, time.UTC)
	vals := []oahost.Value{
		{Kind: oahost.KindFloat, Float: 1000123}, {Kind: oahost.KindFloat, Float: 12.5},
		{Kind: oahost.KindFloat, Float: 1e21}, {Kind: oahost.KindFloat, Float: 1e-7},
		{Kind: oahost.KindFloat, Float: -0.000001}, {Kind: oahost.KindFloat, Float: 0},
		{Kind: oahost.KindInt, Int: -5}, {Kind: oahost.KindUint, Uint: 4000000000},
		{Kind: oahost.KindBool, Bool: true}, {Kind: oahost.KindString, Str: `a\"b`},
		{Kind: oahost.KindTime, Time: ts}, {Kind: oahost.KindNull},
	}
	for _, v := range vals {
		want, _ := json.Marshal(map[string]any{"value": v.JSON(), "time": ts.Format("2006-01-02T15:04:05.000Z")})
		if got := ValuePayload(v, ts); string(got) != string(want) {
			t.Errorf("kind %d: got %s want %s", v.Kind, got, want)
		}
	}
}
