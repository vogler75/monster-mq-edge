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
// host.
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
		cfg.WinCCOaNative = config.WinCCOaNativeConfig{Enabled: true}
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

// A WinCCOA-Client device in the embedded broker uses the GraphQL server
// (there is no native transport): initial answers and live values for every
// message format.
func TestWinCCOaClientGraphQL(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	_ = sim.Set("System1:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: 12.5})
	_ = sim.Set("System1:Pump1.speed", oahost.Value{Kind: oahost.KindFloat, Float: 7})
	_ = sim.Set("System1:Pump1.blob", oahost.Value{Kind: oahost.KindBytes, Bytes: []byte{0, 'A', 0xFF}})
	fake := newFakeOAGraphQL(t, oahost.API{C: client})

	for i, format := range []string{"JSON_ISO", "JSON_MS", "RAW_VALUE"} {
		port := 27120 + i
		srv := startQueryBroker(t, port, deviceConfigJSON(t, fake.srv.URL+"/graphql", format), client)
		c, _ := dialRaw(t, port, rawConnect{ClientID: "cg", Version: 5, Clean: true})
		time.Sleep(1500 * time.Millisecond) // connector subscribed, initial answers retained
		c.Subscribe(packets.Subscription{Filter: "plant/#", Qos: 0})
		initial := collect(c, 700*time.Millisecond)
		if len(initial) != 3 {
			t.Fatalf("%s initial %v", format, initial)
		}
		for _, m := range initial {
			if strings.HasPrefix(m.Topic, "plant/names/") {
				t.Fatalf("%s: answer=false address published an initial value: %v", format, m)
			}
		}
		_ = sim.Set("System1:Pump1.name", oahost.Value{Kind: oahost.KindString, Str: fmt.Sprintf("n%d", i)})
		_ = sim.Set("System1:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: float64(100 + i)})
		if live := collect(c, 700*time.Millisecond); len(live) != 2 {
			t.Fatalf("%s live %v", format, live)
		}
		c.Close()
		srv.Close()
	}
}

// Device output into the native namespace is refused in the embedded broker.
func TestWinCCOaClientReservedOutput(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	bad := `{"graphqlEndpoint":"http://127.0.0.1:1/graphql","addresses":[{"query":"SELECT '_online.._value' FROM 'Pump1.speed'","topic":"q1"}]}`
	srv := startQueryBroker(t, 27130, bad, client)
	defer srv.Close()
	ctx := context.Background()
	mgr := srv.WinCCOa()
	if mgr.Connector("oa1") == nil {
		t.Fatal("device outside the native namespace not started")
	}
	if err := srv.Storage().DeviceConfig.Save(ctx, stores.DeviceConfig{Name: "oa2", Namespace: "winccoa", NodeID: "qnode", Type: "WinCCOA-Client", Enabled: true, Config: bad}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if mgr.Connector("oa2") != nil {
		t.Fatal("connector publishing into winccoa/ was started")
	}
}
