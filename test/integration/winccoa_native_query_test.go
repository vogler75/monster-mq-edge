package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/stores"
)

// fakeOAGraphQL serves the graphql-transport-ws dpQueryConnectSingle
// subscription of the WinCC OA GraphQL server, fed from the simulated OA
// host, so the GraphQL bridge and the native transport see the same data.
type fakeOAGraphQL struct {
	api oahost.API
	srv *httptest.Server
}

var subQueryRe = regexp.MustCompile(`dpQueryConnectSingle\(query: ("(?:[^"\\]|\\.)*"), answer: (true|false)\)`)

func newFakeOAGraphQL(t *testing.T, api oahost.API) *fakeOAGraphQL {
	f := &fakeOAGraphQL{api: api}
	up := websocket.Upgrader{Subprotocols: []string{"graphql-transport-ws"}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{}}`))
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		var wmu sync.Mutex
		send := func(v any) {
			wmu.Lock()
			defer wmu.Unlock()
			_ = conn.WriteJSON(v)
		}
		var refs []uint64
		defer func() {
			for _, ref := range refs {
				_ = api.QueryDisconnect(context.Background(), ref)
			}
			conn.Close()
		}()
		for {
			var msg map[string]any
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			switch msg["type"] {
			case "connection_init":
				send(map[string]any{"type": "connection_ack"})
			case "subscribe":
				id, _ := msg["id"].(string)
				payload, _ := msg["payload"].(map[string]any)
				q, _ := payload["query"].(string)
				m := subQueryRe.FindStringSubmatch(q)
				if m == nil {
					continue
				}
				var query string
				_ = json.Unmarshal([]byte(m[1]), &query)
				ref, err := api.QueryConnect(context.Background(), query, m[2] == "true", func(ev oahost.Message) {
					rows, err := oahost.QueryRows(ev)
					if err != nil {
						return
					}
					table := make([]any, len(rows))
					for i, r := range rows {
						cells := make([]any, len(r))
						for j, v := range r {
							cells[j] = v.JSON()
						}
						table[i] = cells
					}
					send(map[string]any{"id": id, "type": "next", "payload": map[string]any{
						"data": map[string]any{"dpQueryConnectSingle": map[string]any{"values": table, "type": "hotlink", "error": nil}},
					}})
				}, 2*time.Second)
				if err == nil {
					refs = append(refs, ref)
				}
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func deviceConfigJSON(t *testing.T, endpoint, format string) string {
	cfg := map[string]any{
		"graphqlEndpoint": endpoint,
		"messageFormat":   format,
		"reconnectDelay":  1000,
		"addresses": []map[string]any{
			{"query": "SELECT '_online.._value', '_online.._stime' FROM 'Pump*.speed'", "topic": "speeds", "answer": true, "retained": true},
			{"query": "SELECT '_online.._value' FROM 'Pump1.name'", "topic": "names", "answer": false},
			{"query": "SELECT '_online.._value' FROM 'Pump1.blob'", "topic": "blobs", "answer": true, "retained": true},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func startQueryBroker(t *testing.T, port int, deviceJSON string, native *oahost.Client) *broker.Server {
	t.Helper()
	cfg := config.Default()
	cfg.NodeID = "qnode"
	cfg.TCP.Port = port
	cfg.GraphQL.Enabled = false
	cfg.Metrics.Enabled = false
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "q.db")
	cfg.Features.WinCCOa = true
	if native != nil {
		cfg.WinCCOaNative = config.WinCCOaNativeConfig{Enabled: true, Transport: config.WinCCOaTransportNative}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	opts := broker.Options{
		OA: native,
		ConfigureStorage: func(ctx context.Context, s *stores.Storage) error {
			return s.DeviceConfig.Save(ctx, stores.DeviceConfig{
				Name: "oa1", Namespace: "plant", NodeID: "qnode", Type: "WinCCOA-Client", Enabled: true, Config: deviceJSON,
			})
		},
	}
	srv, err := broker.NewWithOptions(cfg, slog.New(slog.DiscardHandler), nil, opts)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	time.Sleep(200 * time.Millisecond)
	return srv
}

type seenMsg struct {
	Topic   string
	Payload string
	Retain  bool
}

func collect(c *rawClient, d time.Duration) []seenMsg {
	var out []seenMsg
	deadline := time.Now().Add(d)
	for {
		pk, ok := c.Next(time.Until(deadline))
		if !ok {
			break
		}
		out = append(out, seenMsg{pk.TopicName, string(pk.Payload), pk.FixedHeader.Retain})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Topic != out[j].Topic {
			return out[i].Topic < out[j].Topic
		}
		return out[i].Payload < out[j].Payload
	})
	return out
}

// AC-10, AC-11: initial answers, live values and output parity with the
// GraphQL bridge for every message format.
func TestNativeQueryParity(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	_ = sim.Set("System1:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: 12.5})
	_ = sim.Set("System1:Pump1.speed", oahost.Value{Kind: oahost.KindFloat, Float: 7})
	_ = sim.Set("System1:Pump1.blob", oahost.Value{Kind: oahost.KindBytes, Bytes: []byte{0, 'A', 0xFF}})
	fake := newFakeOAGraphQL(t, oahost.API{C: client})

	for i, format := range []string{"JSON_ISO", "JSON_MS", "RAW_VALUE"} {
		gqlPort, natPort := 27120+2*i, 27121+2*i
		gql := startQueryBroker(t, gqlPort, deviceConfigJSON(t, fake.srv.URL+"/graphql", format), nil)
		nat := startQueryBroker(t, natPort, deviceConfigJSON(t, "", format), client)

		cg, _ := dialRaw(t, gqlPort, rawConnect{ClientID: "cg", Version: 5, Clean: true})
		cn, _ := dialRaw(t, natPort, rawConnect{ClientID: "cn", Version: 5, Clean: true})
		time.Sleep(1500 * time.Millisecond) // both connectors registered, initial answers retained
		cg.Subscribe(packets.Subscription{Filter: "plant/#", Qos: 0})
		cn.Subscribe(packets.Subscription{Filter: "plant/#", Qos: 0})
		initG, initN := collect(cg, 700*time.Millisecond), collect(cn, 700*time.Millisecond)
		if len(initN) == 0 || fmt.Sprint(initG) != fmt.Sprint(initN) {
			t.Fatalf("%s initial mismatch\ngraphql %v\nnative  %v", format, initG, initN)
		}
		for _, m := range initN {
			if strings.HasPrefix(m.Topic, "plant/names/") {
				t.Fatalf("%s: answer=false address published an initial value: %v", format, m)
			}
		}

		_ = sim.Set("System1:Pump1.name", oahost.Value{Kind: oahost.KindString, Str: fmt.Sprintf("n%d", i)})
		_ = sim.Set("System1:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: float64(100 + i)})
		liveG, liveN := collect(cg, 700*time.Millisecond), collect(cn, 700*time.Millisecond)
		if len(liveN) != 2 || fmt.Sprint(liveG) != fmt.Sprint(liveN) {
			t.Fatalf("%s live mismatch\ngraphql %v\nnative  %v", format, liveG, liveN)
		}
		t.Logf("%s native output: %v", format, append(initN, liveN...))
		cg.Close()
		cn.Close()
		gql.Close()
		nat.Close()
	}
	if n := sim.Queries(); n != 0 {
		t.Fatalf("query registrations leaked: %d", n)
	}
}

// AC-12, AC-13: lifecycle, reload, reconnect without duplicates, and
// registration failures that keep the connector not-connected.
func TestNativeQueryLifecycle(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	device := `{"addresses":[{"query":"SELECT '_online.._value' FROM 'Pump1.speed'","topic":"a","answer":true},` +
		`{"query":"SELECT '_online.._value' FROM 'SubstationB:Feeder*.voltage'","topic":"b"},` +
		`{"query":"not a query","topic":"c"}],"reconnectDelay":1000}`
	srv := startQueryBroker(t, 27130, device, client)
	defer srv.Close()
	time.Sleep(500 * time.Millisecond)

	mgr := srv.WinCCOa()
	conn := mgr.Connector("oa1")
	if conn == nil {
		t.Fatal("connector not started")
	}
	if conn.IsConnected() {
		t.Fatal("connector reported connected with failing registrations")
	}
	if n := sim.Queries(); n != 1 {
		t.Fatalf("registered queries %d, want 1", n)
	}

	// The remote system appears: its query registers on retry, the invalid
	// query still fails.
	sim.SetSystemAvailable("SubstationB", true)
	time.Sleep(1500 * time.Millisecond)
	if n := sim.Queries(); n != 2 || conn.IsConnected() {
		t.Fatalf("after recovery queries=%d connected=%v", n, conn.IsConnected())
	}

	// Local reconnect: one registration per address, no duplicates.
	sim.SetSystemAvailable("System1", false)
	time.Sleep(100 * time.Millisecond)
	sim.SetSystemAvailable("System1", true)
	time.Sleep(1500 * time.Millisecond)
	if n := sim.Queries(); n != 2 {
		t.Fatalf("after reconnect queries=%d, want 2", n)
	}

	// Reload with a valid config replaces the connector and releases the
	// old registrations exactly once.
	ctx := context.Background()
	valid := `{"addresses":[{"query":"SELECT '_online.._value' FROM 'Pump1.speed'","topic":"a","answer":true}]}`
	if err := srv.Storage().DeviceConfig.Save(ctx, stores.DeviceConfig{Name: "oa1", Namespace: "plant", NodeID: "qnode", Type: "WinCCOA-Client", Enabled: true, Config: valid}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := sim.Queries(); n != 1 || !mgr.Connector("oa1").IsConnected() {
		t.Fatalf("after reload queries=%d", n)
	}
	// Disable: the query stops.
	if _, err := srv.Storage().DeviceConfig.Toggle(ctx, "oa1", false); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := sim.Queries(); n != 0 {
		t.Fatalf("after disable queries=%d", n)
	}

	// Output colliding with a reserved native branch is refused.
	bad := `{"addresses":[{"query":"SELECT '_online.._value' FROM 'Pump1.speed'","topic":"this/tags"}]}`
	if err := srv.Storage().DeviceConfig.Save(ctx, stores.DeviceConfig{Name: "oa2", Namespace: "winccoa", NodeID: "qnode", Type: "WinCCOA-Client", Enabled: true, Config: bad}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if mgr.Connector("oa2") != nil || sim.Queries() != 0 {
		t.Fatal("connector publishing into winccoa/this was started")
	}
}
