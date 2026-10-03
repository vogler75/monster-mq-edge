package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/stores/sqlite"
)

// PL-43: concurrent same-topic retained publishes from several clients end
// with the same retained value on A and B (capture order = apply order).
func TestPeerLinkRetainedCaptureOrder(t *testing.T) {
	for _, tc := range []struct {
		name          string
		mqtt, peer    int
		retainedStore config.StoreType
	}{
		{"sqlite", 27369, 27370, ""},
		{"memory", 27371, 27372, config.StoreMemory},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := func(c *config.Config) {
				if tc.retainedStore != "" {
					c.RetainedStoreType = tc.retainedStore
				}
			}
			a := startPL(t, "pl43a"+tc.name, tc.mqtt, tc.peer, []config.PeerConfig{plPeer("pl43b"+tc.name, 0)}, store)
			b := startPL(t, "pl43b"+tc.name, 0, 0, []config.PeerConfig{plPullOnly("pl43a"+tc.name, tc.peer)}, store)
			b.waitStreaming("pl43a" + tc.name)

			const clients, each = 4, 150
			var wg sync.WaitGroup
			for c := 0; c < clients; c++ {
				cl := plPaho(t, tc.mqtt, fmt.Sprintf("pl43-%s-%d", tc.name, c))
				wg.Add(1)
				go func(c int, cl paho.Client) {
					defer wg.Done()
					for i := 0; i < each; i++ {
						tok := cl.Publish("pl43/t", byte(i%2), true, fmt.Sprintf("c%d-%d", c, i))
						if !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
							t.Errorf("publish: %v", tok.Error())
							return
						}
					}
				}(c, cl)
			}
			wg.Wait()
			leo := a.status().Log.LEO
			plEventually(t, 10*time.Second, "B applied A's log", func() bool { return b.source("pl43a"+tc.name).AppliedNext >= leo })
			va, vb := plRetainedValue(a, "pl43/t", tc.retainedStore), plRetainedValue(b, "pl43/t", tc.retainedStore)
			if va == "" || va != vb {
				t.Fatalf("retained value differs: A %q, B %q", va, vb)
			}
		})
	}
}

func plRetainedValue(n *plNode, topic string, store config.StoreType) string {
	if store == config.StoreMemory {
		pk, ok := n.srv.MQTT().Topics.Retained.Get(topic)
		if !ok {
			return ""
		}
		return string(pk.Payload)
	}
	return string(plRetained(n, topic))
}

// plHoleProxy forwards TCP connections; in blackhole mode it drops every
// byte in both directions and does not pass a close on, so the far end only
// notices through its keepalive.
type plHoleProxy struct {
	ln     net.Listener
	target string
	hole   atomic.Bool
	mu     sync.Mutex
	conns  []net.Conn
}

func startHoleProxy(t *testing.T, port int, target string) *plHoleProxy {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &plHoleProxy{ln: ln, target: target}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go p.pipe(c, up)
			go p.pipe(up, c)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.mu.Unlock()
	})
	return p
}

func (p *plHoleProxy) pipe(from, to net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := from.Read(buf)
		if err != nil {
			if !p.hole.Load() {
				_ = to.Close()
			}
			return
		}
		if p.hole.Load() {
			continue
		}
		if _, err := to.Write(buf[:n]); err != nil {
			return
		}
	}
}

// PL-29: the client loses its path to A (blackhole), reconnects to B and
// publishes a retained "online" birth. A's keepalive expires and the will
// fires there; on B it is superseded, so B keeps "online", and A converges to
// it through the value B sends back.
func TestPeerLinkWillAfterFailover(t *testing.T) {
	a, b := plPair(t, "pl29a", "pl29b", 27373, 27374, 27375, 27376, nil, nil)
	proxy := startHoleProxy(t, 27377, "127.0.0.1:27373")

	o := mqttOpts(27377, "pl29x")
	o.SetKeepAlive(time.Second)
	o.SetAutoReconnect(false)
	o.SetWill("pl29/status/x", "offline", 1, true)
	x := paho.NewClient(o)
	if tok := x.Connect(); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatalf("connect via proxy: %v", tok.Error())
	}
	plPahoPublish(t, x, "pl29/status/x", 1, true, "online")
	plEventually(t, 5*time.Second, "first birth on B", func() bool { return string(plRetained(b, "pl29/status/x")) == "online" })

	proxy.hole.Store(true)
	x2 := plPaho(t, 27375, "pl29x")
	plPahoPublish(t, x2, "pl29/status/x", 1, true, "online-b")
	plEventually(t, 10*time.Second, "will fired on A", func() bool { return a.status().Log.Appended.Will == 1 })
	plEventually(t, 5*time.Second, "will superseded on B", func() bool {
		return b.source("pl29a").Dropped["will_superseded"] == 1
	})
	time.Sleep(200 * time.Millisecond)
	if v := string(plRetained(b, "pl29/status/x")); v != "online-b" {
		t.Fatalf("B retained %q, want the birth published on B", v)
	}
	// A applied its own will; B sends its current value back, so A converges.
	plEventually(t, 5*time.Second, "A converged to B's value", func() bool {
		return string(plRetained(a, "pl29/status/x")) == "online-b"
	})
	if n := b.source("pl29a").SupersededWillResent; n != 1 {
		t.Fatalf("supersededWillResent %d", n)
	}
	x.Disconnect(0)
}

// PL-11 variant: B was connected, then down while A overflowed. The loss is
// reported identically on both sides and B gets the newest records.
func TestPeerLinkOverflowAfterDisconnect(t *testing.T) {
	small := func(c *config.Config) {
		c.PeerLink.Log.MaxMessages = intPtr(1000)
		c.PeerLink.Fetch.MaxRecords = intPtr(500)
	}
	a, b := plOneWay(t, "pl11ca", "pl11cb", 27378, 0, []plOpt{small}, []plOpt{small})
	b.start()
	b.waitStreaming("pl11ca")
	for i := 1; i <= 100; i++ {
		a.publish("pl11c/n", strconv.Itoa(i), 0, false)
	}
	plWaitCount(t, b, "pl11c/n", 100, 5*time.Second, 100*time.Millisecond)
	b.Close()
	plEventually(t, 5*time.Second, "B disconnected on A", func() bool { return a.consumer("pl11cb").State == "DISCONNECTED" })
	for i := 101; i <= 2600; i++ {
		a.publish("pl11c/n", strconv.Itoa(i), 0, false)
	}
	b.start()
	plWaitCount(t, b, "pl11c/n", 1000, 10*time.Second, 300*time.Millisecond)
	plCheckSequence(t, b, "pl11c/n", 1601, 2600)
	lost := a.consumer("pl11cb").LostTotal
	if got := b.source("pl11ca").GapLostTotal; lost != 1500 || got != lost {
		t.Fatalf("A lostTotal %d, B gapLostTotal %d, want 1500 on both", lost, got)
	}
}

// PL-31: an MQTT bridge on B forwards loop/# to A's MQTT port. Replicas
// never reach the bridge (BridgeOutbound false), so the counts stay bounded:
// one extra copy on A for a publish on B, none for a publish on A.
func TestPeerLinkBridgeLoopBounded(t *testing.T) {
	a := startPL(t, "pl31a", 27379, 27380, []config.PeerConfig{plPeer("pl31b", 27381)})
	b := newPL(t, "pl31b", 0, 27381, []config.PeerConfig{plPeer("pl31a", 27380)}, func(c *config.Config) {
		c.Features.MqttClient = true
	})
	db, err := sqlite.Open(b.cfg.SQLite.Path)
	if err != nil {
		t.Fatal(err)
	}
	dcs := sqlite.NewDeviceConfigStore(db)
	if err := dcs.EnsureTable(context.Background()); err != nil {
		t.Fatal(err)
	}
	bridge, _ := json.Marshal(map[string]any{
		"brokerUrl": "tcp://127.0.0.1:27379", "clientId": "pl31-bridge", "cleanSession": true, "keepAlive": 10,
		"addresses": []map[string]any{{"mode": "PUBLISH", "localTopic": "loop/#", "remoteTopic": ""}},
	})
	if err := dcs.Save(context.Background(), stores.DeviceConfig{Name: "to-a", Namespace: "bridge", NodeID: "pl31b",
		Type: "MQTT_CLIENT", Enabled: true, Config: string(bridge)}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	b.start()
	a.waitStreaming("pl31b")
	b.waitStreaming("pl31a")
	plEventually(t, 10*time.Second, "bridge connected to A", func() bool {
		cl, ok := a.srv.MQTT().Clients.Get("pl31-bridge")
		return ok && !cl.Closed()
	})
	time.Sleep(300 * time.Millisecond) // bridge bus subscription

	a.publish("loop/a", "1", 1, false)
	b.publish("loop/b", "1", 1, false)
	time.Sleep(1500 * time.Millisecond)
	counts := map[string]int{
		"A loop/a": a.rec.count("loop/a"), "B loop/a": b.rec.count("loop/a"),
		"A loop/b": a.rec.count("loop/b"), "B loop/b": b.rec.count("loop/b"),
	}
	want := map[string]int{"A loop/a": 1, "B loop/a": 1, "A loop/b": 2, "B loop/b": 2}
	for k, v := range want {
		if counts[k] != v {
			t.Fatalf("message counts %v, want %v (amplification through the bridge)", counts, want)
		}
	}
}

// PL-41: the lossless TLS migration of 17.5, one node restart at a time.
// After every step both directions stream, and what the running node
// published while its peer restarted arrives completely.
func TestPeerLinkTLSMigration(t *testing.T) {
	ca := newPLCA(t)
	certA, keyA := ca.issue(t, "pl41a", "pl41a")
	certB, keyB := ca.issue(t, "pl41b", "pl41b")
	a, b := plPair(t, "pl41a", "pl41b", 0, 27382, 0, 27383, nil, nil)
	nodes := map[*plNode]struct{ cert, key string }{a: {certA, keyA}, b: {certB, keyB}}
	other := map[*plNode]*plNode{a: b, b: a}

	seq := 0
	step := func(name string, n *plNode, change func(c *config.Config)) {
		t.Helper()
		peer := other[n]
		n.Close()
		seq++
		topic := fmt.Sprintf("pl41/%d", seq)
		for i := 0; i < 50; i++ {
			peer.publish(topic, strconv.Itoa(i), 1, false)
		}
		change(n.cfg)
		n.start()
		n.waitStreaming(peer.id)
		peer.waitStreaming(n.id)
		plWaitCount(t, n, topic, 50, 10*time.Second, 100*time.Millisecond)
		if st := n.status(); st.TLS != n.cfg.PeerLink.Tls.Enabled {
			t.Fatalf("%s %s: listener TLS %v", name, n.id, st.TLS)
		}
	}
	for _, n := range []*plNode{a, b} {
		files := nodes[n]
		step("1 listener TLS with AllowPlaintext", n, func(c *config.Config) {
			c.PeerLink.Tls.Enabled = true
			c.PeerLink.Tls.CertPath, c.PeerLink.Tls.KeyPath = files.cert, files.key
			c.PeerLink.Tls.TrustStorePath = ca.path
			c.PeerLink.Listener.AllowPlaintext = true
			c.PeerLink.Peers[0].Tls.Enabled = boolPtr(false)
		})
	}
	for _, n := range []*plNode{a, b} {
		step("2 dialer TLS", n, func(c *config.Config) { c.PeerLink.Peers[0].Tls.Enabled = nil })
	}
	for _, n := range []*plNode{a, b} {
		step("3 AllowPlaintext off", n, func(c *config.Config) { c.PeerLink.Listener.AllowPlaintext = false })
	}
	if a.status().Admission.RefusedPlaintext != 0 || b.status().Admission.RefusedPlaintext != 0 {
		t.Fatal("a plaintext dial was refused during the migration")
	}
}

// PL-34: strings of applied records are not retained beyond the retained
// store: updating the same 10k retained topics nine more times does not grow
// the heap like the first round did.
func TestPeerLinkStringRetention(t *testing.T) {
	memory := func(c *config.Config) { c.RetainedStoreType = config.StoreMemory }
	a, b := plOneWay(t, "pl34a", "pl34b", 27384, 0, []plOpt{memory}, []plOpt{memory})
	b.start()
	b.waitStreaming("pl34a")
	const topics = 10000
	round := func(r int) {
		for i := 0; i < topics; i++ {
			a.publish(fmt.Sprintf("pl34/retained/topic/%05d", i), fmt.Sprintf("value-%02d-%05d", r, i), 0, true)
		}
		leo := a.status().Log.LEO
		plEventually(t, 20*time.Second, "B applied the round", func() bool { return b.source("pl34a").AppliedNext >= leo })
		// The test recorders hold every delivered packet; they are not under test.
		for _, n := range []*plNode{a, b} {
			n.rec.mu.Lock()
			n.rec.pkts = nil
			n.rec.mu.Unlock()
		}
	}
	heap := func() int64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return int64(ms.HeapInuse)
	}
	base := heap()
	round(0)
	first := heap() - base
	for r := 1; r < 10; r++ {
		round(r)
	}
	later := heap() - base - first
	if later > first/2+4<<20 {
		t.Fatalf("heap grew %d bytes over 9 update rounds after %d for the first round", later, first)
	}
}

// PL-25 soak, env-gated (PEERLINK_SOAK=<duration>, e.g. 1h): traffic in both
// directions while the nodes restart alternately; every loss must appear in a
// counter and goroutines must not leak.
func TestPeerLinkSoak(t *testing.T) {
	d, err := time.ParseDuration(os.Getenv("PEERLINK_SOAK"))
	if err != nil || d <= 0 {
		t.Skip("PL-25 soak: set PEERLINK_SOAK=<duration> to run")
	}
	a, b := plPair(t, "pl25a", "pl25b", 0, 27385, 0, 27386, nil, nil)
	goroutines := runtime.NumGoroutine()
	end := time.Now().Add(d)
	restartEvery := min(5*time.Minute, d/4)
	next := time.Now().Add(restartEvery)
	turn := a
	for i := 0; time.Now().Before(end); i++ {
		if !a.closed {
			a.publish("pl25/a", strconv.Itoa(i), 1, false)
		}
		if !b.closed {
			b.publish("pl25/b", strconv.Itoa(i), 1, false)
		}
		if time.Now().After(next) {
			turn.restart()
			turn = map[*plNode]*plNode{a: b, b: a}[turn]
			next = time.Now().Add(restartEvery)
		}
		time.Sleep(time.Millisecond)
	}
	a.waitStreaming("pl25b")
	b.waitStreaming("pl25a")
	for _, n := range []*plNode{a, b} {
		for _, s := range n.status().Sources {
			if s.Dropped["malformed"] != 0 || s.CRCErrors != 0 {
				t.Errorf("%s from %s: %+v", n.id, s.NodeID, s)
			}
		}
	}
	time.Sleep(time.Second)
	if g := runtime.NumGoroutine(); g > goroutines+50 {
		t.Fatalf("goroutines %d after the soak, %d before", g, goroutines)
	}
}

// PL-24 load matrix (G0-G7) and PL-36 slow link need a separate generator
// host and netem; they are run by hand (dev/bench/peerlink) and skipped here.
func TestPeerLinkLoadMatrix(t *testing.T) {
	if os.Getenv("PEERLINK_LOAD_TARGET") == "" {
		t.Skip("PL-24: set PEERLINK_LOAD_TARGET to the generator setup to run the G0-G7 matrix")
	}
	t.Skip("PL-24: the load harness is not part of this repository yet")
}

func TestPeerLinkSlowLink(t *testing.T) {
	if os.Getenv("PEERLINK_NETEM") == "" {
		t.Skip("PL-36: set PEERLINK_NETEM=1 on a host with netem (1 Mbit/s, 50 ms RTT) on loopback to run")
	}
	a, b := plOneWay(t, "pl36a", "pl36b", 27387, 0, nil, nil)
	b.start()
	b.waitStreaming("pl36a")
	payload := make([]byte, 64<<10)
	for i := 0; i < 64; i++ {
		if err := a.srv.MQTT().Publish("pl36/n", payload, false, 1); err != nil {
			t.Fatal(err)
		}
	}
	plWaitCount(t, b, "pl36/n", 64, 5*time.Minute, 0)
}
