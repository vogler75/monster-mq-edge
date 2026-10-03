package peerlink

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/hooks/auth"
	"monstermq.io/edge/internal/mqtt/packets"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func intp(v int) *int { return &v }

func boolp(v bool) *bool { return &v }

// freeAddr reserves a loopback port and releases it for the node under test.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// testNode is one in-process engine with a PeerLink manager.
type testNode struct {
	id   string
	srv  *mqtt.Server
	m    *Manager
	addr string
	recv *recorder
}

type nodeOpt func(*config.PeerLinkConfig, *Deps)

// baseConfig is a plaintext config with the unauthenticated waiver on loopback.
func baseConfig(peers ...config.PeerConfig) config.PeerLinkConfig {
	return config.PeerLinkConfig{
		Enabled:                   true,
		AllowUnauthenticatedPeers: true,
		Listener:                  config.PeerLinkListener{AllowedNetworks: []string{"127.0.0.0/8"}},
		KeepAliveSeconds:          intp(2),
		Fetch:                     config.PeerLinkFetch{MaxWaitMs: intp(200), ReconnectMaxMs: intp(500)},
		Peers:                     peers,
	}
}

// startNode builds and starts a node listening on addr ("" = no listener needed is decided by peers).
func startNode(t *testing.T, id, addr string, peers []config.PeerConfig, opts ...nodeOpt) *testNode {
	t.Helper()
	n := newNode(t, id, addr, peers, opts...)
	if err := n.m.Start(); err != nil {
		t.Fatal(err)
	}
	return n
}

func newNode(t testing.TB, id, addr string, peers []config.PeerConfig, opts ...nodeOpt) *testNode {
	t.Helper()
	srv := mqtt.New(&mqtt.Options{InlineClient: true, Logger: quietLogger()})
	if err := srv.AddHook(new(auth.AllowHook), nil); err != nil {
		t.Fatal(err)
	}
	cfg := baseConfig(peers...)
	if addr == "" {
		addr = "127.0.0.1:0" // pull-only nodes bind the loopback status endpoint
	}
	deps := Deps{Server: srv, Logger: quietLogger(), ListenAddress: addr}
	for _, o := range opts {
		o(&cfg, &deps)
	}
	deps.Config = cfg
	deps.Setup = &config.PeerLinkSetup{NodeID: id, Peers: cfg.Peers}
	m, err := New(deps)
	if err != nil {
		t.Fatalf("New(%s): %v", id, err)
	}
	if err := srv.AddHook(m.Hook(), nil); err != nil {
		t.Fatal(err)
	}
	n := &testNode{id: id, srv: srv, m: m, addr: m.Addr(), recv: &recorder{}}
	if err := srv.Subscribe("#", 1, n.recv.handle); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = m.Close()
		_ = srv.Close()
	})
	return n
}

// recorder collects packets delivered to an inline "#" subscription.
type recorder struct {
	mu   sync.Mutex
	pkts []packets.Packet
}

func (r *recorder) handle(_ *mqtt.Client, _ packets.Subscription, pk packets.Packet) {
	r.mu.Lock()
	r.pkts = append(r.pkts, pk.Copy(false))
	r.pkts[len(r.pkts)-1].Forward = pk.Forward
	r.mu.Unlock()
}

func (r *recorder) byTopic(topic string) []packets.Packet {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []packets.Packet
	for _, p := range r.pkts {
		if p.TopicName == topic {
			out = append(out, p)
		}
	}
	return out
}

func (r *recorder) count(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.pkts {
		if len(p.TopicName) >= len(prefix) && p.TopicName[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func sourceStatus(n *testNode, peer string) SourceStatus {
	for _, s := range n.m.Status().Sources {
		if s.NodeID == peer {
			return s
		}
	}
	return SourceStatus{}
}

func consumerStatus(n *testNode, peer string) ConsumerStatus {
	for _, c := range n.m.Status().Consumers {
		if c.NodeID == peer {
			return c
		}
	}
	return ConsumerStatus{}
}

func waitStreaming(t *testing.T, n *testNode, peer string) {
	t.Helper()
	eventually(t, 5*time.Second, n.id+" streaming from "+peer, func() bool {
		return sourceStatus(n, peer).State == "STREAMING"
	})
}

func publishPkt(t *testing.T, n *testNode, pk packets.Packet) {
	t.Helper()
	pk.FixedHeader.Type = packets.Publish
	if pk.FixedHeader.Qos > 0 && pk.PacketID == 0 {
		pk.PacketID = 1
	}
	if err := n.srv.PublishPacket(pk); err != nil {
		t.Fatalf("publish on %s: %v", n.id, err)
	}
}

func ctxTimeout(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// clientPublish publishes as a network client (QoS 0 path, no connection needed).
func clientPublish(t *testing.T, n *testNode, clientID string, pk packets.Packet) {
	t.Helper()
	cl := n.srv.NewClient(nil, "tcp", clientID, false)
	cl.Properties.ProtocolVersion = 5
	cl.State.Inflight.ResetReceiveQuota(1024)
	pk.FixedHeader.Type = packets.Publish
	if err := n.srv.InjectPacket(cl, pk); err != nil {
		t.Fatalf("client publish on %s: %v", n.id, err)
	}
}

type mqttSubscribersForTest struct{}

func (mqttSubscribersForTest) make() *mqtt.Subscribers {
	return &mqtt.Subscribers{
		Shared:         map[string]map[string]packets.Subscription{"$share/g/t": {"c1": {Filter: "t"}}},
		SharedSelected: map[string]packets.Subscription{},
		Subscriptions:  map[string]packets.Subscription{},
	}
}

// httpRaw sends one raw HTTP request to the peer port and returns the status code and body.
func httpRaw(t *testing.T, addr, req string) (int, []byte) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}
