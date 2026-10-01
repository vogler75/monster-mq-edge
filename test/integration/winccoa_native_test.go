package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/oahost/simhost"
	"monstermq.io/edge/internal/stores"
)

// These tests drive the broker through its real MQTT listener with the
// simulated WinCC OA host (internal/oahost/simhost). They verify the Go
// side of the native namespace; live OA acceptance needs the developer-built
// manager and a real project.

type nativeEnv struct {
	srv    *broker.Server
	sim    *simhost.Host
	client *oahost.Client
	port   int
	db     string
}

func newSim(queue int) (*simhost.Host, *oahost.Client) {
	sim := simhost.New("System1", queue)
	client := oahost.NewClient(sim, oahost.Limits{MaxPending: 256, EventQueue: 4096, DefaultTimeout: 2 * time.Second})
	sim.Attach(client)
	sim.AddType(simhost.Type{Name: "AnalogDrive", Elements: map[string]uint32{
		"":         simhost.ElemStruct,
		"speed":    uint32(oahost.KindFloat),
		"running":  uint32(oahost.KindBool),
		"name":     uint32(oahost.KindString),
		"count":    uint32(oahost.KindInt),
		"unsigned": uint32(oahost.KindUint),
		"ts":       uint32(oahost.KindTime),
		"blob":     uint32(oahost.KindBytes),
		"set":      uint32(oahost.KindFloat),
		"nested":   simhost.ElemStruct,
		"nested.a": uint32(oahost.KindFloat),
	}})
	sim.AddType(simhost.Type{Name: "Scalar", Elements: map[string]uint32{"": uint32(oahost.KindFloat)}})
	sim.AddType(simhost.Type{Name: "Feeder", Elements: map[string]uint32{"": simhost.ElemStruct, "voltage": uint32(oahost.KindFloat)}})
	addStoreTypes(sim)
	must := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	must(sim.CreateDP("System1", "Pump101", "AnalogDrive"))
	must(sim.CreateDP("System1", "Pump1", "AnalogDrive"))
	must(sim.CreateDP("System1", "ScalarTag", "Scalar"))
	must(sim.CreateDP("System1", "_Users", "Scalar"))
	must(sim.CreateDP("System1", "MMQConfigs_k1", "MMQConfigs"))
	sim.AddSystem("SubstationA", true)
	must(sim.CreateDP("SubstationA", "Feeder1", "Feeder"))
	must(sim.CreateDP("SubstationA", "Pump101", "AnalogDrive"))
	sim.AddSystem("SubstationB", false)
	return sim, client
}

func addStoreTypes(sim *simhost.Host) {
	str, tim := uint32(oahost.KindString), uint32(oahost.KindTime)
	sim.AddType(simhost.Type{Name: "MMQConfigs", Elements: map[string]uint32{"": simhost.ElemStruct, "config": str, "type": str, "updated": tim}})
	sim.AddType(simhost.Type{Name: "MMQSessions", Elements: map[string]uint32{"": simhost.ElemStruct, "session": str, "subs": str, "connected": uint32(oahost.KindBool), "nodeId": str, "updated": tim}})
	u := uint32(oahost.KindUint)
	sim.AddType(simhost.Type{Name: "MMQRetained", Elements: map[string]uint32{"": simhost.ElemStruct, "value": uint32(oahost.KindBytes), "topic": str, "user": str, "qos": u, "expiry": u, "updated": tim}})
}

func startNative(t *testing.T, port int, dbPath string, sim *simhost.Host, client *oahost.Client, cfgFn func(*config.Config), opts broker.Options) *nativeEnv {
	t.Helper()
	cfg := config.Default()
	cfg.NodeID = fmt.Sprintf("n-%d", port)
	cfg.TCP.Enabled = true
	cfg.TCP.Port = port
	cfg.WS.Enabled = false
	cfg.GraphQL.Enabled = false
	cfg.Metrics.Enabled = false
	cfg.SQLite.Path = dbPath
	cfg.WinCCOaNative = config.WinCCOaNativeConfig{Enabled: true, Namespace: true}
	if cfgFn != nil {
		cfgFn(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	opts.OA = client
	if opts.NativeReconcile == 0 {
		opts.NativeReconcile = 200 * time.Millisecond
	}
	srv, err := broker.NewWithOptions(cfg, slog.New(slog.DiscardHandler), nil, opts)
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve() }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
	}
	return &nativeEnv{srv: srv, sim: sim, client: client, port: port, db: dbPath}
}

func sub(filter string, qos byte) packets.Subscription {
	return packets.Subscription{Filter: filter, Qos: qos}
}

func payloadValue(t *testing.T, pk packets.Packet) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(pk.Payload, &m); err != nil {
		t.Fatalf("payload %q: %v", pk.Payload, err)
	}
	return m["value"]
}

func countSubRows(t *testing.T, dbPath, clientID string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM subscriptions WHERE client_id = ?`, clientID).Scan(&n); err != nil {
		t.Fatalf("count subscriptions: %v", err)
	}
	return n
}

// AC-20: one SUBACK result per filter, in order, for MQTT 5 and 3.1.1.
func TestNativeSubackPerFilter(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	db := filepath.Join(t.TempDir(), "n.db")
	env := startNative(t, 27101, db, sim, client, nil, broker.Options{})
	defer env.srv.Close()

	filters := []packets.Subscription{
		sub("winccoa/systems/System1/tags/Pump101/speed", 1),          // valid
		sub("winccoa/systems/System1/tags/Nope/speed", 1),             // missing DP
		sub("winccoa/systems/System1/tags/_Users", 1),                 // internal DP: denied
		sub("winccoa/systems/SubstationB/tags/Feeder1/voltage", 1),    // unavailable remote system
		sub("winccoa/systems/System1/tags/Pump101/+/x", 1),            // wildcard below a leaf: accepted, matches nothing
		sub("$share/g/winccoa/systems/System1/tags/Pump101/speed", 1), // shared
		sub("winccoa/systems/System1", 1),                             // status topic
		sub("winccoa/systems/System1/cns/View/node", 1),               // reserved CNS
		sub("winccoa/systems/System1/tags/Pump101", 1),                // struct root: not a value element
		sub("winccoa/systems/System1/types/Feeder/Pump101/speed", 1),  // DPT mismatch
		sub("winccoa/systems/Unknown/tags/Pump101/speed", 1),          // unknown system
		sub("winccoa/System1/tags/Pump101/speed", 1),                  // system without systems/
		sub("winccoa/systems/System1/tags/ScalarTag", 0),              // scalar root
		sub("winccoa/systems/System1/tags/MMQConfigs_k1/config", 1),   // native store DP: denied
	}
	want5 := []byte{0x01, 0x8F, 0x87, 0x83, 0x01, 0x9E, 0x01, 0x83, 0x8F, 0x8F, 0x83, 0x8F, 0x00, 0x87}
	want3 := make([]byte, len(want5))
	for i, c := range want5 {
		if c > 2 {
			c = 0x80
		}
		want3[i] = c
	}

	for _, v := range []struct {
		version byte
		want    []byte
	}{{5, want5}, {4, want3}} {
		id := fmt.Sprintf("suback-v%d", v.version)
		c, _ := dialRaw(t, env.port, rawConnect{ClientID: id, Version: v.version, Clean: false, SessionExpiry: 3600})
		got := c.Subscribe(filters...)
		if string(got) != string(v.want) {
			t.Errorf("v%d SUBACK\n got % x\nwant % x", v.version, got, v.want)
		}
		time.Sleep(200 * time.Millisecond)
		// Only the accepted filters are persisted.
		accepted := 0
		for _, code := range v.want {
			if code <= 2 {
				accepted++
			}
		}
		if n := countSubRows(t, db, id); n != accepted {
			t.Errorf("v%d persisted %d subscriptions, want %d", v.version, n, accepted)
		}
		c.Close()
	}
	// Two clients hold the same two native DPEs (Pump101.speed and
	// ScalarTag.): the rejected filters must not have created OA interests.
	names := sim.ConnectedNames()
	for _, n := range names {
		if !strings.HasPrefix(n, "System1:Pump101.speed:") && !strings.HasPrefix(n, "System1:ScalarTag.:") {
			t.Errorf("unexpected OA connection %q", n)
		}
	}
	if st := env.srv.Native().Stats(); st.DPEs != 2 {
		t.Errorf("native DPEs = %d, want 2 (%+v)", st.DPEs, st)
	}
}

// AC-21 and AC-22: shared connections, initial value, live values,
// reference counting.
func TestNativeInterestsAndInitialValue(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27102, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()

	if err := sim.Set("System1:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: 42.5}); err != nil {
		t.Fatal(err)
	}
	tags := "winccoa/systems/System1/tags/Pump101/speed"
	types := "winccoa/systems/System1/types/AnalogDrive/Pump101/speed"
	explicit := "winccoa/systems/System1/tags/Pump101/speed/_online.._value"

	a, _ := dialRaw(t, env.port, rawConnect{ClientID: "ia", Version: 5, Clean: true})
	defer a.Close()
	if got := a.Subscribe(sub(tags, 1)); got[0] != 1 {
		t.Fatalf("suback %x", got)
	}
	pk, ok := a.NextOn(tags, 3*time.Second)
	if !ok {
		t.Fatal("no initial value for an unchanged DPE")
	}
	if !pk.FixedHeader.Retain || payloadValue(t, pk) != 42.5 {
		t.Fatalf("initial value retain=%v payload=%s", pk.FixedHeader.Retain, pk.Payload)
	}

	b, _ := dialRaw(t, env.port, rawConnect{ClientID: "ib", Version: 5, Clean: true})
	defer b.Close()
	b.Subscribe(sub(types, 1), sub(explicit, 1))
	got := b.NextOnAll(3*time.Second, types, explicit)
	for _, topic := range []string{types, explicit} {
		if pk, ok := got[topic]; !ok || payloadValue(t, pk) != 42.5 {
			t.Fatalf("alias %s initial value missing", topic)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := sim.Connections(); n != 1 {
		t.Fatalf("OA connections = %d, want 1 shared", n)
	}

	// Repeated SUBSCRIBE replaces options without a second reference.
	a.Subscribe(sub(tags, 0))
	a.Drain(300 * time.Millisecond)
	if st := env.srv.Native().Stats(); st.Interests != 3 || st.DPEs != 1 {
		t.Fatalf("stats after resubscribe %+v", st)
	}
	// Retain handling 2: no current value on subscribe.
	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "ic", Version: 5, Clean: true})
	defer c.Close()
	c.Subscribe(packets.Subscription{Filter: tags, Qos: 1, RetainHandling: 2})
	if _, ok := c.NextOn(tags, 500*time.Millisecond); ok {
		t.Fatal("retain handling 2 must suppress the current value")
	}

	// Live value reaches every alias, not retained.
	if err := sim.Set("System1:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: 7}); err != nil {
		t.Fatal(err)
	}
	if pk, ok := a.NextOn(tags, 3*time.Second); !ok || pk.FixedHeader.Retain || payloadValue(t, pk) != float64(7) {
		t.Fatalf("live value on tags: ok=%v retain=%v %s", ok, pk.FixedHeader.Retain, pk.Payload)
	}
	got = b.NextOnAll(3*time.Second, types, explicit)
	for _, topic := range []string{types, explicit} {
		if pk, ok := got[topic]; !ok || payloadValue(t, pk) != float64(7) {
			t.Fatalf("live value on %s missing", topic)
		}
	}

	// Unsubscribe removes only the caller's interests.
	a.Unsubscribe(tags)
	c.Unsubscribe(tags)
	time.Sleep(200 * time.Millisecond)
	if n := sim.Connections(); n != 1 {
		t.Fatalf("connection dropped while b still subscribed: %d", n)
	}
	b.Unsubscribe(types, explicit)
	time.Sleep(300 * time.Millisecond)
	if n := sim.Connections(); n != 0 {
		t.Fatalf("last interest must disconnect exactly once; connections=%d", n)
	}
	if st := env.srv.Native().Stats(); st.Disconnects != 1 || st.DPEs != 0 {
		t.Fatalf("stats %+v", st)
	}
}

// AC-22: an offline persistent subscriber receives queued changes.
func TestNativeOfflinePersistentSubscriber(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27103, filepath.Join(t.TempDir(), "n.db"), sim, client, func(c *config.Config) {
		c.QueuedMessagesEnabled = true
	}, broker.Options{})
	defer env.srv.Close()

	topic := "winccoa/systems/System1/tags/Pump1/count"
	a, _ := dialRaw(t, env.port, rawConnect{ClientID: "off", Version: 5, Clean: true, SessionExpiry: 3600})
	a.Subscribe(sub(topic, 1))
	a.NextOn(topic, 2*time.Second)
	a.Close()
	time.Sleep(200 * time.Millisecond)

	for i := 1; i <= 3; i++ {
		if err := sim.Set("System1:Pump1.count", oahost.Value{Kind: oahost.KindInt, Int: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n := sim.Connections(); n != 1 {
		t.Fatalf("offline persistent session must keep its interest; connections=%d", n)
	}
	b, ack := dialRaw(t, env.port, rawConnect{ClientID: "off", Version: 5, Clean: false, SessionExpiry: 3600})
	defer b.Close()
	if !ack.SessionPresent {
		t.Fatal("session present expected")
	}
	var got []any
	for len(got) < 3 {
		pk, ok := b.NextOn(topic, 3*time.Second)
		if !ok {
			break
		}
		got = append(got, payloadValue(t, pk))
	}
	if fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("queued values %v", got)
	}
}

// AC-24 and AC-26: typed writes, validation, results and duplicates.
func TestNativeTypedWrites(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27104, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()

	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "writer", Version: 5, Clean: true})
	defer c.Close()
	c.Subscribe(sub("results/#", 1))

	base := "winccoa/systems/System1/tags/Pump1/"
	cases := []struct {
		elem    string
		payload string
		code    byte
		want    oahost.Value
	}{
		{"speed", `{"value":1500.5,"id":"w1"}`, 0x00, oahost.Value{Kind: oahost.KindFloat, Float: 1500.5}},
		{"speed", `12`, 0x00, oahost.Value{Kind: oahost.KindFloat, Float: 12}},
		{"running", `{"value":true}`, 0x00, oahost.Value{Kind: oahost.KindBool, Bool: true}},
		{"name", `{"value":"über"}`, 0x00, oahost.Value{Kind: oahost.KindString, Str: "über"}},
		{"count", `{"value":-2147483648}`, 0x00, oahost.Value{Kind: oahost.KindInt, Int: -2147483648}},
		{"unsigned", `{"value":4294967295}`, 0x00, oahost.Value{Kind: oahost.KindUint, Uint: 4294967295}},
		{"ts", `{"value":"2026-09-29T10:00:00.123Z"}`, 0x00, oahost.Value{Kind: oahost.KindTime, Time: time.Date(2026, 9, 29, 10, 0, 0, 123e6, time.UTC)}},
		{"blob", `{"value":"AAEC"}`, 0x00, oahost.Value{Kind: oahost.KindBytes, Bytes: []byte{0, 1, 2}}},
		{"%73et", `{"value":3}`, 0x00, oahost.Value{Kind: oahost.KindFloat, Float: 3}},
		{"count", `{"value":2147483648}`, 0x99, oahost.Value{}},
		{"count", `{"value":1.5}`, 0x99, oahost.Value{}},
		{"unsigned", `{"value":-1}`, 0x99, oahost.Value{}},
		{"speed", `{"value":"12"}`, 0x99, oahost.Value{}},
		{"speed", `{"value":null}`, 0x99, oahost.Value{}},
		{"speed", `{"valu":1}`, 0x99, oahost.Value{}},
		{"speed", `not json`, 0x99, oahost.Value{}},
		{"running", `{"value":1}`, 0x99, oahost.Value{}},
		{"nope", `{"value":1}`, 0x90, oahost.Value{}},
		{"nested", `{"value":1}`, 0x90, oahost.Value{}},
	}
	for _, tc := range cases {
		addr := "System1:Pump1." + strings.Replace(tc.elem, "%73et", "set", 1) + ":_original.._value"
		before, _ := sim.Get(addr)
		code := c.Publish(rawPub{Topic: base + tc.elem + "/set", Payload: []byte(tc.payload), QoS: 1})
		if code != tc.code {
			t.Errorf("%s %s: PUBACK 0x%02x want 0x%02x", tc.elem, tc.payload, code, tc.code)
			continue
		}
		time.Sleep(100 * time.Millisecond)
		after, err := sim.Get(addr)
		if tc.code == 0 {
			if err != nil || fmt.Sprint(after) != fmt.Sprint(tc.want) {
				t.Errorf("%s: value %+v want %+v (err %v)", tc.elem, after, tc.want, err)
			}
		} else if err == nil && fmt.Sprint(after) != fmt.Sprint(before) {
			t.Errorf("%s: rejected write changed OA value to %+v", tc.elem, after)
		}
	}

	// Oversized payload.
	big := `{"value":"` + strings.Repeat("x", 70<<10) + `"}`
	if code := c.Publish(rawPub{Topic: base + "name/set", Payload: []byte(big), QoS: 1}); code != 0x99 {
		t.Errorf("oversized payload PUBACK 0x%02x", code)
	}
	// Retained commands are rejected.
	if code := c.Publish(rawPub{Topic: base + "speed/set", Payload: []byte(`1`), QoS: 1, Retain: true}); code != 0x83 {
		t.Errorf("retained command PUBACK 0x%02x", code)
	}
	// Value topics are broker-owned.
	if code := c.Publish(rawPub{Topic: base + "speed", Payload: []byte(`1`), QoS: 1}); code != 0x87 {
		t.Errorf("publish to value topic PUBACK 0x%02x", code)
	}
	if code := c.Publish(rawPub{Topic: "winccoa/systems/System1", Payload: []byte(`{}`), QoS: 1}); code != 0x87 {
		t.Errorf("forged status PUBACK 0x%02x", code)
	}
	if code := c.Publish(rawPub{Topic: "winccoa/systems/System1/cns/x", Payload: []byte(`1`), QoS: 1}); code != 0x87 {
		t.Errorf("publish into CNS PUBACK 0x%02x", code)
	}

	// Result via MQTT 5 response topic and correlation data.
	code := c.Publish(rawPub{Topic: base + "speed/set", Payload: []byte(`{"value":99,"id":"r1"}`), QoS: 1, ResponseTopic: "results/a", Correlation: []byte("corr-1")})
	if code != 0 {
		t.Fatalf("PUBACK 0x%02x", code)
	}
	pk, ok := c.NextOn("results/a", 3*time.Second)
	if !ok {
		t.Fatal("no command result")
	}
	var res map[string]any
	_ = json.Unmarshal(pk.Payload, &res)
	if res["status"] != "confirmed" || res["id"] != "r1" || string(pk.Properties.CorrelationData) != "corr-1" {
		t.Fatalf("result %s corr=%q", pk.Payload, pk.Properties.CorrelationData)
	}

	// Duplicate command id: not executed again, earlier result re-sent.
	sets := sim.Sets()
	c.Publish(rawPub{Topic: base + "speed/set", Payload: []byte(`{"value":100,"id":"r1"}`), QoS: 1, ResponseTopic: "results/a"})
	if pk, ok := c.NextOn("results/a", 3*time.Second); !ok || !strings.Contains(string(pk.Payload), `"confirmed"`) {
		t.Fatal("duplicate did not re-send the earlier result")
	}
	if sim.Sets() != sets {
		t.Fatal("duplicate command id was executed again")
	}
	if v, _ := sim.Get("System1:Pump1.speed"); v.Float != 99 {
		t.Fatalf("value %v", v.Float)
	}

	// MQTT 3.1.1: QoS 0 rejection is visible through replyTo.
	v3, _ := dialRaw(t, env.port, rawConnect{ClientID: "writer3", Version: 4, Clean: true})
	defer v3.Close()
	v3.Subscribe(sub("results/#", 1))
	v3.Publish(rawPub{Topic: base + "running/set", Payload: []byte(`{"value":"yes","replyTo":"results/v3"}`), QoS: 0})
	if pk, ok := v3.NextOn("results/v3", 3*time.Second); !ok || !strings.Contains(string(pk.Payload), `"rejected"`) {
		t.Fatalf("v3 QoS0 rejection result missing: %s", pk.Payload)
	}
	v3.Publish(rawPub{Topic: base + "running/set", Payload: []byte(`{"value":false,"replyTo":"results/v3"}`), QoS: 0})
	if pk, ok := v3.NextOn("results/v3", 3*time.Second); !ok || !strings.Contains(string(pk.Payload), `"confirmed"`) {
		t.Fatalf("v3 QoS0 confirmation missing: %s", pk.Payload)
	}
	// MQTT 3.1.1 QoS 1 rejection closes the connection (no negative PUBACK).
	if code := v3.Publish(rawPub{Topic: base + "running/set", Payload: []byte(`{"value":"no"}`), QoS: 1}); code != 0xFF || !v3.Closed() {
		t.Fatalf("v3 QoS1 rejection: code 0x%02x", code)
	}
}

// AC-19, AC-23: remote scope, identically named DPEs, outage and recovery.
func TestNativeRemoteSystems(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27105, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()

	_ = sim.Set("System1:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: 1})
	_ = sim.Set("SubstationA:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: 2})
	local := "winccoa/systems/System1/tags/Pump101/speed"
	remote := "winccoa/systems/SubstationA/tags/Pump101/speed"

	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "rs", Version: 5, Clean: true})
	defer c.Close()
	if got := c.Subscribe(sub(local, 1), sub(remote, 1)); string(got) != "\x01\x01" {
		t.Fatalf("suback % x", got)
	}
	init := c.NextOnAll(2*time.Second, local, remote)
	if pk, ok := init[local]; !ok || payloadValue(t, pk) != float64(1) {
		t.Fatal("local initial value")
	}
	if pk, ok := init[remote]; !ok || payloadValue(t, pk) != float64(2) {
		t.Fatal("remote initial value")
	}

	// Remote outage: no fallback to local, no stale replay.
	sim.SetSystemAvailable("SubstationA", false)
	time.Sleep(200 * time.Millisecond)
	_ = sim.Set("System1:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: 10})
	if pk, ok := c.NextOn(local, 2*time.Second); !ok || payloadValue(t, pk) != float64(10) {
		t.Fatal("local value during remote outage")
	}
	if _, ok := c.NextOn(remote, 300*time.Millisecond); ok {
		t.Fatal("remote topic published during outage")
	}
	d, _ := dialRaw(t, env.port, rawConnect{ClientID: "rs2", Version: 5, Clean: true})
	defer d.Close()
	if got := d.Subscribe(sub("winccoa/systems/SubstationA/tags/Feeder1/voltage", 1)); got[0] != 0x83 {
		t.Fatalf("subscribe during outage: 0x%02x", got[0])
	}
	if code := d.Publish(rawPub{Topic: "winccoa/systems/SubstationA/tags/Pump101/speed/set", Payload: []byte(`5`), QoS: 1}); code != 0x83 {
		t.Fatalf("write during outage: 0x%02x", code)
	}

	// Recovery re-registers once and delivers the current value.
	sim.SetSystemAvailable("SubstationA", true)
	if pk, ok := c.NextOn(remote, 3*time.Second); !ok || payloadValue(t, pk) != float64(2) {
		t.Fatal("remote value after recovery")
	}
	_ = sim.Set("SubstationA:Pump101.speed", oahost.Value{Kind: oahost.KindFloat, Float: 3})
	if pk, ok := c.NextOn(remote, 2*time.Second); !ok || payloadValue(t, pk) != float64(3) {
		t.Fatal("remote live value after recovery")
	}
	if n := len(sim.ConnectedNames()); n != 2 {
		t.Fatalf("registrations after recovery: %v", sim.ConnectedNames())
	}
}

// AC-23: a deleted DP is invalidated, never replayed, and its interest is
// restored when the DP is recreated.
func TestNativeDeletedDatapoint(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27106, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()

	_ = sim.Set("System1:Pump1.speed", oahost.Value{Kind: oahost.KindFloat, Float: 5})
	topic := "winccoa/systems/System1/tags/Pump1/speed"
	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "del", Version: 5, Clean: true})
	defer c.Close()
	c.Subscribe(sub(topic, 1))
	c.NextOn(topic, 2*time.Second)

	sim.DeleteDP("System1", "Pump1")
	time.Sleep(300 * time.Millisecond)
	if n := sim.Connections(); n != 0 {
		t.Fatalf("deleted DP still connected: %d", n)
	}
	d, _ := dialRaw(t, env.port, rawConnect{ClientID: "del2", Version: 5, Clean: true})
	defer d.Close()
	if got := d.Subscribe(sub(topic, 1)); got[0] != 0x8F {
		t.Fatalf("subscribe to deleted DP: 0x%02x", got[0])
	}
	if code := d.Publish(rawPub{Topic: topic + "/set", Payload: []byte(`1`), QoS: 1}); code != 0x90 {
		t.Fatalf("write to deleted DP: 0x%02x", code)
	}

	if err := sim.CreateDP("System1", "Pump1", "AnalogDrive"); err != nil {
		t.Fatal(err)
	}
	pk, ok := c.NextOn(topic, 3*time.Second)
	if !ok {
		t.Fatal("interest not restored after recreation")
	}
	if payloadValue(t, pk) != float64(0) {
		t.Fatalf("stale value replayed after recreation: %s", pk.Payload)
	}
}

// AC-21: restoring more interests than the connect batch limit registers
// all of them in batches the host accepts.
func TestNativeRestoreBatches(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	for i := 0; i < 250; i++ {
		if err := sim.CreateDP("System1", fmt.Sprintf("Bulk%03d", i), "Scalar"); err != nil {
			t.Fatal(err)
		}
	}
	db := filepath.Join(t.TempDir(), "n.db")
	env := startNative(t, 27107, db, sim, client, nil, broker.Options{})
	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "bulk", Version: 4, Clean: false})
	var subs []packets.Subscription
	for i := 0; i < 250; i++ {
		subs = append(subs, sub(fmt.Sprintf("winccoa/systems/System1/tags/Bulk%03d", i), 0))
	}
	for i := 0; i < len(subs); i += 50 {
		for _, code := range c.Subscribe(subs[i : i+50]...) {
			if code != 0 {
				t.Fatalf("suback 0x%02x", code)
			}
		}
	}
	c.Close()
	time.Sleep(300 * time.Millisecond)
	env.srv.Close()
	if n := sim.Connections(); n != 0 {
		t.Fatalf("stop left %d registrations", n)
	}
	// One persisted filter now fails revalidation.
	sim.DeleteDP("System1", "Bulk000")

	connectsBefore := sim.Calls(oahost.OpDpConnect)
	env2 := startNative(t, 27107, db, sim, client, nil, broker.Options{})
	defer env2.srv.Close()
	deadline := time.Now().Add(5 * time.Second)
	for len(sim.ConnectedNames()) < 249 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := len(sim.ConnectedNames()); n != 249 {
		t.Fatalf("restored registrations %d, want 249", n)
	}
	if calls := sim.Calls(oahost.OpDpConnect) - connectsBefore; calls < 3 || calls > 5 {
		t.Fatalf("restore used %d dpConnect batches", calls)
	}
	if n := countSubRows(t, db, "bulk"); n != 249 {
		t.Fatalf("invalid persisted subscription not removed: %d rows", n)
	}
}

// AC-06, AC-07: bounded overload, deadlines, no late execution, and
// thread confinement in the simulated host.
func TestNativeOverload(t *testing.T) {
	sim, client := newSim(8)
	defer sim.Close()
	env := startNative(t, 27108, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()

	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "ov", Version: 5, Clean: true})
	defer c.Close()
	// Warm the catalog so writes only need the DP_SET round trip.
	if code := c.Publish(rawPub{Topic: "winccoa/systems/System1/tags/Pump1/count/set", Payload: []byte(`0`), QoS: 1}); code != 0 {
		t.Fatalf("warm-up PUBACK 0x%02x", code)
	}
	time.Sleep(100 * time.Millisecond)
	setsBefore := sim.Sets()

	c.Subscribe(sub("ov/res", 1))
	sim.Pause()
	var accepted int
	for i := 1; i <= 40; i++ {
		code := c.Publish(rawPub{Topic: "winccoa/systems/System1/tags/Pump1/count/set", Payload: []byte(fmt.Sprintf(`{"value":%d,"id":"o%d","replyTo":"ov/res"}`, i, i)), QoS: 1})
		if code == 0 {
			accepted++
		}
	}
	// Bounded by the host queue plus the one request the paused manager
	// thread holds.
	if env.client.Pending() > 9 || sim.QueueLen() > 8 {
		t.Fatalf("unbounded: pending=%d queue=%d", env.client.Pending(), sim.QueueLen())
	}
	// Every accepted command reports failed/timeout within the deadline.
	results := map[string]int{}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) && len(results) < accepted {
		pk, ok := c.NextOn("ov/res", time.Until(deadline))
		if !ok {
			break
		}
		var r map[string]any
		_ = json.Unmarshal(pk.Payload, &r)
		results[r["id"].(string)] = 1
		if r["status"] == "confirmed" {
			t.Fatalf("command confirmed while OA paused: %s", pk.Payload)
		}
	}
	if len(results) != accepted {
		t.Fatalf("results %d for %d accepted commands", len(results), accepted)
	}
	sim.Resume()
	time.Sleep(500 * time.Millisecond)
	if sim.Sets() != setsBefore {
		t.Fatalf("expired commands executed after resume: %d", sim.Sets()-setsBefore)
	}
	st := env.client.Stats()
	if st.Overloaded == 0 || st.TimedOut == 0 {
		t.Fatalf("expected overload and timeout counters: %+v", st)
	}
	if sim.ThreadViolations() != 0 {
		t.Fatal("operations ran off the manager goroutine")
	}
	// Processing resumes normally.
	if code := c.Publish(rawPub{Topic: "winccoa/systems/System1/tags/Pump1/count/set", Payload: []byte(`77`), QoS: 1}); code != 0 {
		t.Fatalf("after resume PUBACK 0x%02x", code)
	}
	time.Sleep(200 * time.Millisecond)
	if v, _ := sim.Get("System1:Pump1.count"); v.Int != 77 {
		t.Fatalf("value after resume %d", v.Int)
	}
}

// Status topic: retained, broker-owned, readable without DPE lookup.
func TestNativeStatusTopic(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27109, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()
	resolves := sim.Calls(oahost.OpResolve)
	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "st", Version: 5, Clean: true})
	defer c.Close()
	c.Subscribe(sub("winccoa/systems/System1", 1))
	pk, ok := c.NextOn("winccoa/systems/System1", 2*time.Second)
	if !ok || !pk.FixedHeader.Retain {
		t.Fatal("retained status missing")
	}
	var st map[string]any
	_ = json.Unmarshal(pk.Payload, &st)
	if st["ready"] != true || st["system"] != "System1" || st["nodeId"] != "n-27109" {
		t.Fatalf("status %s", pk.Payload)
	}
	if sim.Calls(oahost.OpResolve) != resolves {
		t.Fatal("status subscription caused a DPE lookup")
	}
	sim.SetSystemAvailable("System1", false)
	pk, ok = c.NextOn("winccoa/systems/System1", 2*time.Second)
	if !ok || !strings.Contains(string(pk.Payload), `"disconnected"`) {
		t.Fatal("status transition not published")
	}
}

// A stale status of another system (e.g. after the project's system name
// changed) can be cleared with a retained empty publish; the broker's own
// status topic stays protected.
func TestNativeStaleStatusClear(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27110, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	defer env.srv.Close()
	const own, stale = "winccoa/systems/System1", "winccoa/systems/OldSystem"
	if err := env.srv.MQTT().Publish(stale, []byte(`{"system":"OldSystem"}`), true, 1); err != nil {
		t.Fatal(err)
	}
	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "clr", Version: 5, Clean: true})
	defer c.Close()

	if code := c.Publish(rawPub{Topic: own, QoS: 1, Retain: true}); code != 0x87 {
		t.Errorf("clear of own status PUBACK 0x%02x", code)
	}
	// Another system: only a retained empty payload is accepted.
	if code := c.Publish(rawPub{Topic: stale, Payload: []byte(`{}`), QoS: 1, Retain: true}); code != 0x87 {
		t.Errorf("forged stale status PUBACK 0x%02x", code)
	}
	if code := c.Publish(rawPub{Topic: stale, QoS: 1}); code != 0x87 {
		t.Errorf("non-retained empty status PUBACK 0x%02x", code)
	}
	if code := c.Publish(rawPub{Topic: stale, QoS: 1, Retain: true}); code != 0 {
		t.Fatalf("stale status clear PUBACK 0x%02x", code)
	}
	time.Sleep(100 * time.Millisecond)

	s, _ := dialRaw(t, env.port, rawConnect{ClientID: "clr-sub", Version: 5, Clean: true})
	defer s.Close()
	s.Subscribe(sub(own, 1), sub(stale, 1))
	got := s.NextOnAll(time.Second, own, stale)
	if _, ok := got[stale]; ok {
		t.Fatal("stale status still retained")
	}
	if _, ok := got[own]; !ok {
		t.Fatal("own status missing")
	}
}

// Configured topic names replace winccoa, tags and types everywhere.
func TestNativeCustomTopicNames(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27111, filepath.Join(t.TempDir(), "n.db"), sim, client, func(c *config.Config) {
		c.WinCCOaNative.TopicRoot = "plant/oa"
		c.WinCCOaNative.TagsName = "t"
		c.WinCCOaNative.TypesName = "dpt"
		c.WinCCOaNative.SystemsName = "sys"
	}, broker.Options{})
	defer env.srv.Close()
	_ = sim.Set("System1:Pump1.speed", oahost.Value{Kind: oahost.KindFloat, Float: 5})
	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "names", Version: 5, Clean: true})
	defer c.Close()

	const exact, typed, status = "plant/oa/sys/System1/t/Pump1/speed", "plant/oa/sys/System1/dpt/AnalogDrive/Pump1/speed", "plant/oa/sys/System1"
	queries := sim.Queries()
	got := c.Subscribe(sub(exact, 1), sub(typed, 1), sub(status, 1), sub("plant/oa/sys/System1/t/+/speed", 1),
		sub("winccoa/systems/System1/tags/Pump1/speed", 1)) // default names: an ordinary topic now
	if string(got) != "\x01\x01\x01\x01\x01" {
		t.Fatalf("SUBACK % x", got)
	}
	init := c.NextOnAll(2*time.Second, exact, typed, status)
	if len(init) != 3 {
		t.Fatalf("initial publications %v", len(init))
	}
	for deadline := time.Now().Add(2 * time.Second); sim.Queries() != queries+1 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if sim.Queries() != queries+1 {
		t.Fatalf("wildcard queries %d, want %d", sim.Queries(), queries+1)
	}
	if code := c.Publish(rawPub{Topic: exact + "/set", Payload: []byte(`{"value":42}`), QoS: 1}); code != 0 {
		t.Fatalf("write PUBACK 0x%02x", code)
	}
	time.Sleep(200 * time.Millisecond)
	if v, _ := sim.Get("System1:Pump1.speed"); v.Float != 42 {
		t.Fatalf("write not applied: %v", v.Float)
	}
	// The configured root is reserved; the default one is not any more.
	if code := c.Publish(rawPub{Topic: "plant/oa/sys/System1/t/Pump1/speed", Payload: []byte(`1`), QoS: 1}); code != 0x87 {
		t.Errorf("publish to value topic PUBACK 0x%02x", code)
	}
	if code := c.Publish(rawPub{Topic: "winccoa/systems/System1/tags/Pump1/speed", Payload: []byte(`1`), QoS: 1}); code != 0 {
		t.Errorf("publish under the default root PUBACK 0x%02x", code)
	}
}

// Every system under winccoa/systems/<system>; the local system also via
// the shortcut winccoa/tags|types, unless LocalShortcut is false.
func TestNativeLocalShortcut(t *testing.T) {
	sim, client := newSim(0)
	defer sim.Close()
	env := startNative(t, 27112, filepath.Join(t.TempDir(), "n.db"), sim, client, nil, broker.Options{})
	_ = sim.Set("System1:Pump1.speed", oahost.Value{Kind: oahost.KindFloat, Float: 5})
	_ = sim.Set("SubstationA:Feeder1.voltage", oahost.Value{Kind: oahost.KindFloat, Float: 230})
	c, _ := dialRaw(t, env.port, rawConnect{ClientID: "short", Version: 5, Clean: true})

	const short, typed, explicit, remote = "winccoa/tags/Pump1/speed", "winccoa/types/AnalogDrive/Pump1/speed", "winccoa/systems/System1/tags/Pump1/speed", "winccoa/systems/SubstationA/tags/Feeder1/voltage"
	const status, statusExplicit = "winccoa", "winccoa/systems/System1"
	got := c.Subscribe(sub(short, 1), sub(typed, 1), sub(explicit, 1), sub(remote, 1), sub(status, 1), sub(statusExplicit, 1),
		sub("winccoa/System1/tags/Pump1/speed", 1), // system without systems/: no such form
		sub("winccoa/tags/+/speed", 1), sub("winccoa/systems/System1/tags/+/speed", 1))
	if string(got) != "\x01\x01\x01\x01\x01\x01\x8f\x01\x01" {
		t.Fatalf("SUBACK % x", got)
	}
	init := c.NextOnAll(2*time.Second, short, typed, explicit, remote, status, statusExplicit)
	if len(init) != 6 {
		t.Fatalf("initial publications: %d of 6", len(init))
	}
	// One change reaches every subscribed form of the element.
	_ = sim.Set("System1:Pump1.speed", oahost.Value{Kind: oahost.KindFloat, Float: 6})
	if live := c.NextOnAll(2*time.Second, short, typed, explicit); len(live) != 3 {
		t.Fatalf("live publications: %d of 3", len(live))
	}
	for topic, v := range map[string]float64{short: 42, explicit: 43, remote: 231} {
		if code := c.Publish(rawPub{Topic: topic + "/set", Payload: []byte(fmt.Sprintf(`{"value":%v}`, v)), QoS: 1}); code != 0 {
			t.Fatalf("write %s PUBACK 0x%02x", topic, code)
		}
		time.Sleep(150 * time.Millisecond)
	}
	if v, _ := sim.Get("System1:Pump1.speed"); v.Float != 43 {
		t.Fatalf("local writes not applied: %v", v.Float)
	}
	if v, _ := sim.Get("SubstationA:Feeder1.voltage"); v.Float != 231 {
		t.Fatalf("remote write not applied: %v", v.Float)
	}
	c.Close()
	env.srv.Close()

	// LocalShortcut: false - only the explicit form.
	no := false
	env = startNative(t, 27113, filepath.Join(t.TempDir(), "n2.db"), sim, client, func(c *config.Config) {
		c.WinCCOaNative.LocalShortcut = &no
	}, broker.Options{})
	defer env.srv.Close()
	c, _ = dialRaw(t, env.port, rawConnect{ClientID: "noshort", Version: 5, Clean: true})
	defer c.Close()
	got = c.Subscribe(sub(short, 1), sub(explicit, 1), sub(status, 1), sub(statusExplicit, 1))
	if string(got) != "\x8f\x01\x8f\x01" {
		t.Fatalf("SUBACK without shortcut % x", got)
	}
}

var _ = context.Background
var _ stores.MqttSubscription
