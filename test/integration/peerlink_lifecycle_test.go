package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/stores"
)

// plOneWay starts source a (serving b) and consumer b (pulling from a only).
// b is returned unstarted so the caller can run it later.
func plOneWay(t *testing.T, a, b string, aPeer, bMQTT int, aOpts, bOpts []plOpt) (*plNode, *plNode) {
	t.Helper()
	na := startPL(t, a, 0, aPeer, []config.PeerConfig{plPeer(b, 0)}, aOpts...)
	nb := newPL(t, b, bMQTT, 0, []config.PeerConfig{plPullOnly(a, aPeer)}, bOpts...)
	return na, nb
}

// plCheckSequence checks that n received exactly the payloads first..last
// under topic, in order.
func plCheckSequence(t *testing.T, n *plNode, topic string, first, last int) {
	t.Helper()
	got := n.rec.prefix(topic)
	if len(got) != last-first+1 {
		t.Fatalf("%s: %d messages on %s, want %d", n.id, len(got), topic, last-first+1)
	}
	for i, pk := range got {
		if v, _ := strconv.Atoi(string(pk.Payload)); v != first+i {
			t.Fatalf("%s: message %d has payload %q, want %d (order or duplicate)", n.id, i, pk.Payload, first+i)
		}
	}
}

// PL-08: the consumer restarts and gets everything published meanwhile, in
// order, from the commit the source kept.
func TestPeerLinkConsumerRestart(t *testing.T) {
	a, b := plOneWay(t, "pl08a", "pl08b", 27320, 0, nil, nil)
	b.start()
	b.waitStreaming("pl08a")
	a.publish("pl08/n", "0", 1, false)
	plWaitCount(t, b, "pl08/n", 1, 5*time.Second, 0)

	b.Close()
	const n = 3000
	for i := 1; i <= n; i++ {
		a.publish("pl08/n", strconv.Itoa(i), 1, false)
	}
	b.start()
	plWaitCount(t, b, "pl08/n", n, 15*time.Second, 300*time.Millisecond)
	plCheckSequence(t, b, "pl08/n", 1, n)
	if c := a.consumer("pl08b"); c.LostTotal != 0 || c.Committed != c.Served {
		t.Fatalf("A consumer status %+v", c)
	}
}

// plProxy forwards TCP connections to target; Kill drops every connection
// and Block refuses new ones.
type plProxy struct {
	ln      net.Listener
	target  string
	blocked atomic.Bool
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	accepts atomic.Int64
}

func startProxy(t *testing.T, port int, target string) *plProxy {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &plProxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	go p.loop()
	t.Cleanup(func() { _ = ln.Close(); p.Kill() })
	return p
}

func (p *plProxy) loop() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.blocked.Load() {
			_ = c.Close()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.accepts.Add(1)
		p.mu.Lock()
		p.conns[c], p.conns[up] = struct{}{}, struct{}{}
		p.mu.Unlock()
		go func() { _, _ = io.Copy(up, c); _ = up.Close(); _ = c.Close() }()
		go func() { _, _ = io.Copy(c, up); _ = up.Close(); _ = c.Close() }()
	}
}

func (p *plProxy) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
	clear(p.conns)
}

// PL-09: dropped connections mid-stream lose nothing and duplicate nothing.
func TestPeerLinkConnectionDrop(t *testing.T) {
	a := startPL(t, "pl09a", 0, 27321, []config.PeerConfig{plPeer("pl09b", 0)})
	proxy := startProxy(t, 27322, "127.0.0.1:27321")
	b := startPL(t, "pl09b", 0, 0, []config.PeerConfig{plPullOnly("pl09a", 27322)})
	b.waitStreaming("pl09a")

	const n = 6000
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= n; i++ {
			if err := a.srv.MQTT().Publish("pl09/n", []byte(strconv.Itoa(i)), false, 1); err != nil {
				return
			}
			if i%500 == 0 {
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()
	for k := 0; k < 3; k++ {
		time.Sleep(80 * time.Millisecond)
		proxy.Kill()
	}
	<-done
	plWaitCount(t, b, "pl09/n", n, 15*time.Second, 300*time.Millisecond)
	plCheckSequence(t, b, "pl09/n", 1, n)
	if proxy.accepts.Load() < 2 {
		t.Fatalf("the link was never re-established (accepts %d)", proxy.accepts.Load())
	}
	if s := b.source("pl09a"); s.GapLostTotal != 0 || s.SourceResets != 0 {
		t.Fatalf("B source status %+v", s)
	}
}

// PL-10: a source restart is a new epoch; the consumer counts the reset,
// resumes at offset 1 and keeps receiving.
func TestPeerLinkSourceRestart(t *testing.T) {
	a, b := plOneWay(t, "pl10a", "pl10b", 27323, 0, nil, nil)
	b.start()
	b.waitStreaming("pl10a")
	a.publish("pl10/n", "1", 1, false)
	plWaitCount(t, b, "pl10/n", 1, 5*time.Second, 0)
	epoch := b.source("pl10a").Epoch

	a.restart()
	plEventually(t, 10*time.Second, "source reset seen by B", func() bool {
		s := b.source("pl10a")
		return s.SourceResets == 1 && s.State == "STREAMING" && s.Epoch != epoch
	})
	a.publish("pl10/n", "2", 1, false)
	plWaitCount(t, b, "pl10/n", 2, 5*time.Second, 200*time.Millisecond)
	plCheckSequence(t, b, "pl10/n", 1, 2)
	if s := b.source("pl10a"); s.AppliedNext != 2 {
		t.Fatalf("B appliedNext %d after the reset, want 2 (resumed at offset 1)", s.AppliedNext)
	}
}

// PL-11: overflow with a consumer that never connected; the newest records
// survive and the loss is counted on both sides.
func TestPeerLinkOverflow(t *testing.T) {
	small := func(c *config.Config) {
		c.PeerLink.Log.MaxMessages = intPtr(1000)
		c.PeerLink.Fetch.MaxRecords = intPtr(500)
	}
	a, b := plOneWay(t, "pl11a", "pl11b", 27324, 0, []plOpt{small}, []plOpt{small})
	for i := 1; i <= 2500; i++ {
		a.publish("pl11/n", strconv.Itoa(i), 0, false)
	}
	b.start()
	plWaitCount(t, b, "pl11/n", 1000, 10*time.Second, 300*time.Millisecond)
	plCheckSequence(t, b, "pl11/n", 1501, 2500)
	if s := b.source("pl11a"); s.GapLostTotal != 1500 {
		t.Fatalf("B gapLostTotal %d, want 1500", s.GapLostTotal)
	}
	if c := a.consumer("pl11b"); c.LostTotal != 1500 {
		t.Fatalf("A lostTotal %d, want 1500", c.LostTotal)
	}
	if l := a.status().Log; l.EvictedBy["count"] != 1500 {
		t.Fatalf("A evictedBy %+v, want count 1500", l.EvictedBy)
	}
}

// PL-30: a MEMORY-retained consumer restarts; the snapshot fills absent
// topics and keeps values the consumer already has.
func TestPeerLinkSnapshotFill(t *testing.T) {
	memory := func(c *config.Config) { c.RetainedStoreType = config.StoreMemory }
	a, b := plOneWay(t, "pl30a", "pl30b", 27325, 27326, nil, []plOpt{memory})
	b.start()
	b.waitStreaming("pl30a")
	a.publish("pl30/x", "a", 1, true)
	a.publish("pl30/y", "a", 1, true)
	a.publish("$pl30/z", "a", 1, true)
	plWaitCount(t, b, "pl30/", 2, 5*time.Second, 0)
	plEventually(t, 5*time.Second, "B committed", func() bool { return a.consumer("pl30b").Lag == 0 })

	// B restarts with an empty MEMORY retained store and sets x locally
	// before its puller starts.
	b.Close()
	b.beforeServe = func(n *plNode) { n.publish("pl30/x", "b-local", 1, true) }
	b.start()
	plEventually(t, 5*time.Second, "snapshot applied on B", func() bool {
		s := b.source("pl30a")
		return s.Snapshots >= 1 && s.State == "STREAMING"
	})
	s := b.source("pl30a")
	if s.SnapshotFilled != 1 || s.SnapshotSkippedPresent != 1 {
		t.Fatalf("B snapshot filled %d skippedPresent %d, want 1 and 1", s.SnapshotFilled, s.SnapshotSkippedPresent)
	}
	sub, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "pl30-late", Clean: true})
	defer sub.Close()
	sub.Subscribe(packets.Subscription{Filter: "pl30/#", Qos: 1})
	got := sub.NextOnAll(3*time.Second, "pl30/x", "pl30/y")
	if string(got["pl30/x"].Payload) != "b-local" || string(got["pl30/y"].Payload) != "a" {
		t.Fatalf("retained on B: x=%q y=%q, want b-local and a", got["pl30/x"].Payload, got["pl30/y"].Payload)
	}
}

// PL-19: a graceful source shutdown drains everything captured before it
// to the connected consumer.
func TestPeerLinkDrainOnShutdown(t *testing.T) {
	a, b := plOneWay(t, "pl19a", "pl19b", 27327, 0, nil, nil)
	b.start()
	b.waitStreaming("pl19a")
	const n = 5000
	for i := 1; i <= n; i++ {
		a.publish("pl19/n", strconv.Itoa(i), 1, false)
	}
	a.Close()
	plWaitCount(t, b, "pl19/n", n, 10*time.Second, 200*time.Millisecond)
	plCheckSequence(t, b, "pl19/n", 1, n)
	st := a.srv.PeerLink().Status()
	if !st.Log.Sealed {
		t.Fatal("A log not sealed after Close")
	}
	for _, c := range st.Consumers {
		if c.ShutdownUnserved != 0 {
			t.Fatalf("A consumer %s shutdownUnserved %d, want 0", c.NodeID, c.ShutdownUnserved)
		}
	}
}

// rawWillConn connects an MQTT 5 client with a will and returns the raw
// connection; closing it without DISCONNECT fires the will.
func rawWillConn(t *testing.T, port int, clientID, willTopic, willPayload string, retain bool) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pk := packets.Packet{
		FixedHeader:     packets.FixedHeader{Type: packets.Connect},
		ProtocolVersion: 5,
		Connect: packets.ConnectParams{
			ProtocolName:     []byte("MQTT"),
			ClientIdentifier: clientID,
			Clean:            true,
			Keepalive:        60,
			WillFlag:         true,
			WillTopic:        willTopic,
			WillPayload:      []byte(willPayload),
			WillQos:          1,
			WillRetain:       retain,
		},
	}
	var buf bytes.Buffer
	if err := pk.ConnectEncode(&buf); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	hdr := make([]byte, 1)
	if _, err := conn.Read(hdr); err != nil || hdr[0]>>4 != packets.Connack {
		t.Fatalf("no CONNACK for %s: %v", clientID, err)
	}
	n, err := decodeMqttLength(conn)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, n)
	if _, err := readFull(conn, body); err != nil {
		t.Fatal(err)
	}
	if len(body) > 1 && body[1] != 0 {
		t.Fatalf("CONNACK reason 0x%02x for %s", body[1], clientID)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn
}

// PL-15 and the will supersession of 12.2: a will is forwarded unless the
// client is connected on the receiver.
func TestPeerLinkWills(t *testing.T) {
	a := startPL(t, "pl15a", 27328, 27329, []config.PeerConfig{plPeer("pl15b", 0)})
	b := startPL(t, "pl15b", 27330, 0, []config.PeerConfig{plPullOnly("pl15a", 27329)})
	b.waitStreaming("pl15a")
	busID, bus := b.srv.Bus().Subscribe([]string{"pl15/#"}, 16)
	defer b.srv.Bus().Unsubscribe(busID)

	c1 := rawWillConn(t, a.mqttPort, "pl15-w1", "pl15/w1", "offline", false)
	_ = c1.Close()
	plWaitCount(t, b, "pl15/w1", 1, 5*time.Second, 0)
	if pk := b.rec.prefix("pl15/w1")[0]; pk.Forward == nil || !pk.Forward.Will || string(pk.Payload) != "offline" {
		t.Fatalf("will replica %+v forward %+v", pk, pk.Forward)
	}
	if a.status().Log.Appended.Will != 1 {
		t.Fatalf("A appended %+v, want one will", a.status().Log.Appended)
	}

	// The same client id is connected on B: the will is superseded there.
	live, _ := dialRaw(t, b.mqttPort, rawConnect{ClientID: "pl15-w2", Clean: true})
	defer live.Close()
	c2 := rawWillConn(t, a.mqttPort, "pl15-w2", "pl15/w2", "offline", false)
	_ = c2.Close()
	plEventually(t, 5*time.Second, "will superseded on B", func() bool {
		return b.source("pl15a").Dropped["will_superseded"] == 1
	})
	if n := b.rec.count("pl15/w2"); n != 0 {
		t.Fatalf("superseded will delivered %d times on B", n)
	}
	select {
	case m := <-bus:
		t.Fatalf("will replica reached B's bus: %+v", m.TopicName)
	case <-time.After(200 * time.Millisecond):
	}
}

// PL-28: wills fired by the source's own shutdown are not forwarded.
func TestPeerLinkShutdownWillsNotForwarded(t *testing.T) {
	a := startPL(t, "pl28a", 27365, 27366, []config.PeerConfig{plPeer("pl28b", 0)})
	b := startPL(t, "pl28b", 0, 0, []config.PeerConfig{plPullOnly("pl28a", 27366)})
	b.waitStreaming("pl28a")
	c := rawWillConn(t, a.mqttPort, "pl28-w", "pl28/will", "offline", false)
	defer c.Close()
	a.publish("pl28/marker", "m", 1, false)
	plWaitCount(t, b, "pl28/marker", 1, 5*time.Second, 0)

	a.Close()
	time.Sleep(300 * time.Millisecond)
	if n := b.rec.count("pl28/will"); n != 0 {
		t.Fatalf("a shutdown will of A reached B %d times", n)
	}
	if st := a.srv.PeerLink().Status(); st.Log.SkipWill < 1 || st.Log.Appended.Will != 0 {
		t.Fatalf("A skipWill %d appended %+v", st.Log.SkipWill, st.Log.Appended)
	}
}

// warnRecorder is a slog handler that keeps WARN messages.
type warnRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (w *warnRecorder) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (w *warnRecorder) Handle(_ context.Context, r slog.Record) error {
	w.mu.Lock()
	w.msgs = append(w.msgs, r.Message)
	w.mu.Unlock()
	return nil
}
func (w *warnRecorder) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnRecorder) WithGroup(string) slog.Handler      { return w }

func (w *warnRecorder) has(sub string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, m := range w.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// PL-07 startup WARNs: an MQTT bridge pointing at a peer, a Redfish gateway
// and a host monitoring topic without {NodeId}.
func TestPeerLinkDeviceWarnings(t *testing.T) {
	dir := t.TempDir()
	peers := []config.PeerConfig{plPullOnly("pl07wb", 27367)}
	cfg := plConfig("pl07wa", 0, 27368, dir, peers)
	srv, err := broker.New(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	devs := []stores.DeviceConfig{
		{Name: "bridge-to-peer", NodeID: "pl07wa", Type: "MQTT_CLIENT", Enabled: true,
			Config: `{"brokerUrl":"tcp://127.0.0.1:1883","addresses":[{"mode":"PUBLISH","remoteTopic":"x/#","localTopic":"x/#"}]}`},
		{Name: "gateway", NodeID: "pl07wa", Type: "Redfish", Enabled: true, Config: `{}`},
	}
	for _, d := range devs {
		if err := srv.Storage().DeviceConfig.Save(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	_ = srv.Close()

	rec := &warnRecorder{}
	cfg = plConfig("pl07wa", 0, 27368, dir, peers, func(c *config.Config) {
		c.HostMonitoring.Enabled = true
		c.HostMonitoring.BaseTopic = "hosts/shared"
	})
	srv, err = broker.New(cfg, slog.New(rec), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	for _, want := range []string{"MQTT bridge connects to a PeerLink peer", "Redfish gateways ignore NodeId", "HostMonitoring.BaseTopic has no {NodeId}"} {
		if !rec.has(want) {
			t.Fatalf("missing startup WARN %q; got %q", want, rec.msgs)
		}
	}
}
