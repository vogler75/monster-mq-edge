package cratedb

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/stores"
)

func TestDSN(t *testing.T) {
	cases := []struct{ url, user, pass, want string }{
		{"jdbc:postgresql://h:5432/doc", "", "", "postgresql://crate@h:5432/doc"},
		{"postgres://h:5432/doc", "u", "p", "postgres://u:p@h:5432/doc"},
		{"postgres://a:b@h:5432/doc", "", "", "postgres://a:b@h:5432/doc"},
		{"postgres://a:b@h:5432/doc", "u", "", "postgres://u@h:5432/doc"},
	}
	for _, c := range cases {
		if got := DSN(c.url, c.user, c.pass); got != c.want {
			t.Errorf("DSN(%q, %q, %q) = %q, want %q", c.url, c.user, c.pass, got, c.want)
		}
	}
}

func TestPayloadColumns(t *testing.T) {
	js := &MessageArchive{fmt: stores.PayloadJSON}
	def := &MessageArchive{fmt: stores.PayloadDefault}
	for _, c := range []struct {
		a       *MessageArchive
		payload string
		obj     bool
	}{
		{js, `{"a":1}`, true},
		{js, ` {"a":{"b":2}}`, true},
		{js, `42`, false},
		{js, `[1,2]`, false},
		{js, `{broken`, false},
		{def, `{"a":1}`, false},
	} {
		b64, obj := c.a.payloadColumns([]byte(c.payload))
		if (obj != nil) != c.obj || (b64 != nil) == c.obj {
			t.Errorf("%s %q: obj=%v b64=%v", c.a.fmt, c.payload, obj != nil, b64 != nil)
		}
		if got := string(decodePayload(b64, obj)); got != c.payload {
			t.Errorf("%s %q: round trip %q", c.a.fmt, c.payload, got)
		}
	}
}

func TestValueExpr(t *testing.T) {
	if got := valueExpr("a.b'c"); got != "TRY_CAST(payload_obj['a']['b''c'] AS DOUBLE)" {
		t.Errorf("valueExpr = %s", got)
	}
}

// TestArchiveLive runs against a CrateDB given by MONSTERMQ_TEST_CRATEDB_URL,
// e.g. postgres://crate@localhost:5432/doc.
func TestArchiveLive(t *testing.T) {
	dsn := os.Getenv("MONSTERMQ_TEST_CRATEDB_URL")
	if dsn == "" {
		t.Skip("MONSTERMQ_TEST_CRATEDB_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := Open(ctx, dsn, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, format := range []stores.PayloadFormat{stores.PayloadDefault, stores.PayloadJSON} {
		t.Run(string(format), func(t *testing.T) {
			name := fmt.Sprintf("mmqedge_test_%s_%d", strings.ToLower(string(format)), time.Now().UnixNano())
			a := NewMessageArchive(name, db, format)
			defer db.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+name)
			if err := a.EnsureTable(ctx); err != nil {
				t.Fatal(err)
			}
			if err := a.EnsureTable(ctx); err != nil {
				t.Fatalf("second EnsureTable: %v", err)
			}
			base := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
			msg := func(topic string, min int, payload string) stores.BrokerMessage {
				return stores.BrokerMessage{TopicName: topic, Time: base.Add(time.Duration(min) * time.Minute), Payload: []byte(payload),
					QoS: 1, ClientID: "c1", MessageUUID: "u"}
			}
			batch := []stores.BrokerMessage{
				msg("plant/a/temp", 0, "10"),
				msg("plant/a/temp", 1, "20"),
				msg("plant/a/temp", 7, "30"),
				msg("plant/b/json", 0, `{"v":{"x":4}}`),
				msg("plant/b/json", 2, `{"v":{"x":6}}`),
				msg("plant/c/bin", 0, "\x00\x01\xff"),
				msg("plant/a/temp", 0, "99"), // duplicate (topic, time): ignored
			}
			if err := a.AddHistory(ctx, batch); err != nil {
				t.Fatal(err)
			}
			if _, err := db.pool.Exec(ctx, "REFRESH TABLE "+name); err != nil {
				t.Fatal(err)
			}

			h, err := a.GetHistory(ctx, "plant/a/temp", nil, nil, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(h) != 3 || string(h[0].Payload) != "30" || string(h[2].Payload) != "10" || h[0].QoS != 1 || h[0].ClientID != "c1" {
				t.Fatalf("history: %+v", h)
			}
			if !h[0].Timestamp.Equal(base.Add(7 * time.Minute)) {
				t.Fatalf("timestamp %v", h[0].Timestamp)
			}
			h, err = a.GetHistory(ctx, "plant/#", nil, nil, 10)
			if err != nil || len(h) != 6 {
				t.Fatalf("wildcard history: %d %v", len(h), err)
			}
			for _, m := range h {
				if m.Topic == "plant/c/bin" && string(m.Payload) != "\x00\x01\xff" {
					t.Fatalf("binary payload %q", m.Payload)
				}
				if m.Topic == "plant/b/json" && !strings.Contains(string(m.Payload), `"x"`) {
					t.Fatalf("json payload %q", m.Payload)
				}
			}
			from := base.Add(time.Minute)
			h, err = a.GetHistory(ctx, "plant/a/temp", &from, nil, 10)
			if err != nil || len(h) != 2 {
				t.Fatalf("history from: %d %v", len(h), err)
			}

			agg, err := a.GetAggregatedHistory(ctx, []string{"plant/a/temp"}, base, base.Add(time.Hour), 5, []string{"avg", "count"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(agg.Rows) != 2 || agg.Rows[0][0] != "2026-10-06T10:00:00Z" || agg.Rows[0][1] != 15.0 || agg.Rows[0][2] != 2.0 ||
				agg.Rows[1][0] != "2026-10-06T10:05:00Z" || agg.Rows[1][1] != 30.0 {
				t.Fatalf("aggregation: %v %v", agg.Columns, agg.Rows)
			}
			if format == stores.PayloadJSON {
				agg, err = a.GetAggregatedHistory(ctx, []string{"plant/b/json"}, base, base.Add(time.Hour), 60, []string{"max"}, []string{"v.x"})
				if err != nil {
					t.Fatal(err)
				}
				if len(agg.Rows) != 1 || agg.Columns[1] != "plant/b/json.v_x_max" || agg.Rows[0][1] != 6.0 {
					t.Fatalf("field aggregation: %v %v", agg.Columns, agg.Rows)
				}
			}

			minTs, days, err := a.GetArchiveStats(ctx, nil, nil)
			if err != nil || minTs == nil || !minTs.Equal(base) || len(days) != 1 || days[0].Date != "2026-10-06" || days[0].Count != 6 {
				t.Fatalf("stats: %v %v %v", minTs, days, err)
			}

			res, err := a.PurgeOlderThan(ctx, base.Add(5*time.Minute))
			if err != nil || res.DeletedRows != 5 {
				t.Fatalf("purge: %+v %v", res, err)
			}
		})
	}
}
