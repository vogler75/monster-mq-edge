package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink"
)

// PeerLink integration tests use ports 27300-27399. Each test owns a block
// of ports; a node uses mqttPort for MQTT and peerPort for PeerLink.

type plOpt func(*config.Config)

// plNode is one in-process broker with PeerLink enabled.
type plNode struct {
	t        *testing.T
	id       string
	mqttPort int
	peerPort int
	cfg      *config.Config
	srv      *broker.Server
	rec      *plRecorder
	closed   bool
	// beforeServe runs between broker.New and Serve on every start, while
	// no puller has started yet.
	beforeServe func(n *plNode)
}

func intPtr(v int) *int            { return &v }
func boolPtr(v bool) *bool         { return &v }
func int64Ptr(v int64) *int64      { return &v }
func strsPtr(v []string) *[]string { return &v }

// plPeer is a peer entry that pulls from addrPort (0 = serve only) and may
// pull from this node.
func plPeer(id string, addrPort int) config.PeerConfig {
	p := config.PeerConfig{NodeID: id}
	if addrPort > 0 {
		p.Address = fmt.Sprintf("127.0.0.1:%d", addrPort)
	}
	return p
}

// plPullOnly is a peer this node pulls from but that may not pull from it.
func plPullOnly(id string, addrPort int) config.PeerConfig {
	p := plPeer(id, addrPort)
	p.Serve = boolPtr(false)
	return p
}

// plConfig is a plaintext PeerLink node with the unauthenticated waiver on
// loopback and short timeouts.
func plConfig(id string, mqttPort, peerPort int, dir string, peers []config.PeerConfig, opts ...plOpt) *config.Config {
	cfg := config.Default()
	cfg.NodeID = id
	cfg.TCP.Enabled = mqttPort > 0
	cfg.TCP.Port = mqttPort
	cfg.WS.Enabled = false
	cfg.GraphQL.Enabled = false
	cfg.Metrics.Enabled = false
	cfg.SQLite.Path = filepath.Join(dir, id+".db")
	cfg.PeerLink = config.PeerLinkConfig{
		Enabled:                   true,
		AllowUnauthenticatedPeers: true,
		Listener: config.PeerLinkListener{
			Address:         "127.0.0.1",
			Port:            peerPort,
			AllowedNetworks: []string{"127.0.0.0/8"},
		},
		KeepAliveSeconds: intPtr(2),
		Fetch:            config.PeerLinkFetch{MaxWaitMs: intPtr(200), ReconnectMaxMs: intPtr(300)},
		Log:              config.PeerLinkLog{DrainOnShutdownMs: intPtr(3000)},
		Peers:            peers,
	}
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

func startPL(t *testing.T, id string, mqttPort, peerPort int, peers []config.PeerConfig, opts ...plOpt) *plNode {
	t.Helper()
	n := newPL(t, id, mqttPort, peerPort, peers, opts...)
	n.start()
	return n
}

// newPL prepares a node without starting it.
func newPL(t *testing.T, id string, mqttPort, peerPort int, peers []config.PeerConfig, opts ...plOpt) *plNode {
	t.Helper()
	n := &plNode{t: t, id: id, mqttPort: mqttPort, peerPort: peerPort,
		cfg: plConfig(id, mqttPort, peerPort, t.TempDir(), peers, opts...)}
	t.Cleanup(n.Close)
	return n
}

func (n *plNode) start() {
	n.t.Helper()
	srv, err := broker.New(n.cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		n.t.Fatalf("broker %s: %v", n.id, err)
	}
	n.srv = srv
	n.closed = false
	n.rec = &plRecorder{}
	if err := srv.MQTT().Subscribe("#", 9001, n.rec.handle); err != nil {
		n.t.Fatalf("inline subscribe on %s: %v", n.id, err)
	}
	if n.beforeServe != nil {
		n.beforeServe(n)
	}
	if err := srv.Serve(); err != nil {
		n.t.Fatalf("serve %s: %v", n.id, err)
	}
	if n.mqttPort > 0 {
		waitPort(n.t, n.mqttPort)
	}
}

// Close stops the broker (graceful PeerLink drain); safe to call twice.
func (n *plNode) Close() {
	if n.closed || n.srv == nil {
		return
	}
	n.closed = true
	_ = n.srv.Close()
}

// restart closes the node and starts it again on the same ports and database.
func (n *plNode) restart() {
	n.t.Helper()
	n.Close()
	n.start()
}

func waitPort(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("port %d not listening", port)
}

// status reads the node's PeerLink status through GET /peerlink/v1/status
// when it has a listener (pull-only nodes bind it on loopback), otherwise
// from the manager.
func (n *plNode) status() peerlink.Status {
	n.t.Helper()
	pl := n.srv.PeerLink()
	if pl == nil {
		n.t.Fatalf("%s: PeerLink disabled", n.id)
	}
	if n.closed || pl.Addr() == "" {
		return pl.Status()
	}
	code, body := plHTTPAt(n.t, pl.Addr(), "GET", "/peerlink/v1/status")
	if code != http.StatusOK {
		n.t.Fatalf("%s status: HTTP %d %s", n.id, code, body)
	}
	var st peerlink.Status
	if err := json.Unmarshal(body, &st); err != nil {
		n.t.Fatalf("%s status json: %v", n.id, err)
	}
	return st
}

func (n *plNode) source(peer string) peerlink.SourceStatus {
	n.t.Helper()
	for _, s := range n.status().Sources {
		if s.NodeID == peer {
			return s
		}
	}
	return peerlink.SourceStatus{}
}

func (n *plNode) consumer(peer string) peerlink.ConsumerStatus {
	n.t.Helper()
	for _, c := range n.status().Consumers {
		if c.NodeID == peer {
			return c
		}
	}
	return peerlink.ConsumerStatus{}
}

func (n *plNode) waitStreaming(peers ...string) {
	n.t.Helper()
	for _, p := range peers {
		plEventually(n.t, 10*time.Second, n.id+" streaming from "+p, func() bool {
			return n.srv.PeerLink().Status().Sources != nil && plSourceState(n, p) == "STREAMING"
		})
	}
}

func plSourceState(n *plNode, peer string) string {
	for _, s := range n.srv.PeerLink().Status().Sources {
		if s.NodeID == peer {
			return s.State
		}
	}
	return ""
}

// publish publishes through the broker's inline client (an internal publish).
func (n *plNode) publish(topic, payload string, qos byte, retain bool) {
	n.t.Helper()
	if err := n.srv.MQTT().Publish(topic, []byte(payload), retain, qos); err != nil {
		n.t.Fatalf("publish on %s: %v", n.id, err)
	}
}

func plHTTP(t *testing.T, port int, method, path string) (int, []byte) {
	t.Helper()
	return plHTTPAt(t, fmt.Sprintf("127.0.0.1:%d", port), method, path)
}

func plHTTPAt(t *testing.T, addr, method, path string) (int, []byte) {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	req, err := http.NewRequest(method, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func plEventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// plRecorder records what an inline "#" subscription on a node receives.
type plRecorder struct {
	mu   sync.Mutex
	pkts []packets.Packet
}

func (r *plRecorder) handle(_ *mqtt.Client, _ packets.Subscription, pk packets.Packet) {
	cp := pk.Copy(false)
	cp.Payload = append([]byte(nil), pk.Payload...)
	cp.Forward = pk.Forward
	if pk.Forward != nil {
		f := *pk.Forward
		cp.Forward = &f
	}
	r.mu.Lock()
	r.pkts = append(r.pkts, cp)
	r.mu.Unlock()
}

func (r *plRecorder) prefix(p string) []packets.Packet {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []packets.Packet
	for _, pk := range r.pkts {
		if strings.HasPrefix(pk.TopicName, p) {
			out = append(out, pk)
		}
	}
	return out
}

func (r *plRecorder) count(p string) int { return len(r.prefix(p)) }

// plWaitCount waits until the recorder of n holds want packets under
// prefix, then checks that no more arrive for settle.
func plWaitCount(t *testing.T, n *plNode, prefix string, want int, timeout, settle time.Duration) {
	t.Helper()
	plEventually(t, timeout, fmt.Sprintf("%d messages under %q on %s (have %d)", want, prefix, n.id, n.rec.count(prefix)), func() bool {
		return n.rec.count(prefix) >= want
	})
	if settle > 0 {
		time.Sleep(settle)
	}
	if got := n.rec.count(prefix); got != want {
		t.Fatalf("%s: %d messages under %q, want exactly %d", n.id, got, prefix, want)
	}
}
