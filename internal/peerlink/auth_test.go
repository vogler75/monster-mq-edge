package peerlink

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink/wire"
	"monstermq.io/edge/internal/tlsutil"
)

type certSet struct {
	dir  string
	spki map[string]string
}

func genCerts(t *testing.T, ids ...string) certSet {
	t.Helper()
	cs := certSet{dir: t.TempDir(), spki: map[string]string{}}
	for _, id := range ids {
		spki, _, err := tlsutil.EnsurePeerCertificate(cs.cert(id), cs.key(id), id)
		if err != nil {
			t.Fatal(err)
		}
		cs.spki[id] = spki
	}
	return cs
}

func (c certSet) cert(id string) string { return filepath.Join(c.dir, "peer-"+id+".pem") }
func (c certSet) key(id string) string  { return filepath.Join(c.dir, "peer-"+id+".key") }

// mtls configures node TLS with the generated certificate, REQUIRED client certificates and pins.
func (c certSet) mtls(pins map[string]string) nodeOpt {
	return func(cfg *config.PeerLinkConfig, _ *Deps) {
		cfg.AllowUnauthenticatedPeers = false
		cfg.Tls = config.PeerLinkTLS{Enabled: true, CertPath: filepath.Join(c.dir, "peer-{NodeId}.pem"),
			KeyPath: filepath.Join(c.dir, "peer-{NodeId}.key"), ClientAuth: config.ClientAuthRequired}
		for i := range cfg.Peers {
			if pin, ok := pins[cfg.Peers[i].NodeID]; ok {
				cfg.Peers[i].Tls.PinnedSha256 = []string{pin}
			}
		}
	}
}

func TestMutualTLSWithPins(t *testing.T) {
	cs := genCerts(t, "node-a", "node-b", "node-c")
	a := startNode(t, "node-a", "127.0.0.1:0", []config.PeerConfig{{NodeID: "node-b"}, {NodeID: "node-c"}},
		cs.mtls(map[string]string{"node-b": cs.spki["node-b"], "node-c": cs.spki["node-c"]}))
	b := startNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: a.addr, Serve: boolp(false)}},
		cs.mtls(map[string]string{"node-a": cs.spki["node-a"]}))
	waitStreaming(t, b, "node-a")
	publishPkt(t, a, packets.Packet{TopicName: "tls/x", Payload: []byte("1")})
	eventually(t, 5*time.Second, "delivered over TLS", func() bool { return b.recv.count("tls/x") == 1 })
	if !a.m.Status().TLS {
		t.Fatal("status does not report listener TLS")
	}

	// A valid certificate of node-b claiming to be node-c is refused, with no reason disclosed.
	kp, err := tls.LoadX509KeyPair(cs.cert("node-b"), cs.key("node-b"))
	if err != nil {
		t.Fatal(err)
	}
	pinA, _ := tlsutil.ParsePin(cs.spki["node-a"])
	cc, err := tlsutil.ClientConfig(tlsutil.ClientOptions{Certificate: &kp, Peer: tlsutil.Peer{NodeID: "node-a", Pins: []tlsutil.Pin{pinA}}})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", a.addr, cc)
	if err != nil {
		t.Fatal(err)
	}
	r := &rawConsumer{t: t, c: conn, fr: wire.NewFrameReader(bufio.NewReader(conn), wire.MaxPreAuthFrame)}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := wire.WritePreamble(conn); err != nil {
		t.Fatal(err)
	}
	sh := r.read().(*wire.ServerHello)
	if sh.AuthModes&wire.AuthClientCertRequested == 0 {
		t.Fatal("authModes does not announce client certificates")
	}
	g := expectGoAway(t, r.hello("node-c", "node-a", nil), wire.GoAwayAuthFailed)
	if g.Reason != "" {
		t.Fatalf("reason disclosed: %q", g.Reason)
	}
	if consumerStatus(a, "node-c").AuthFailures["identity_mismatch"] != 1 {
		t.Fatalf("auth failures %+v", consumerStatus(a, "node-c").AuthFailures)
	}

	// Status over TLS (ALPN http/1.1) for an mTLS peer; resync stays loopback-plaintext only.
	hc := cc.Clone()
	hc.NextProtos = []string{tlsutil.ALPNHTTP}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: hc, ForceAttemptHTTP2: false,
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}, Timeout: 5 * time.Second}
	resp, err := client.Get("https://" + a.addr + "/peerlink/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"nodeId":"node-a"`) {
		t.Fatalf("status over TLS %d %s", resp.StatusCode, body)
	}
	resp, err = client.Post("https://"+a.addr+"/peerlink/v1/resync?source=x", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("remote resync %d", resp.StatusCode)
	}
}

func TestTLSWrongPinRefused(t *testing.T) {
	cs := genCerts(t, "node-a", "node-b", "node-x")
	a := startNode(t, "node-a", "127.0.0.1:0", []config.PeerConfig{{NodeID: "node-b"}},
		cs.mtls(map[string]string{"node-b": cs.spki["node-b"]}))
	b := startNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: a.addr, Serve: boolp(false)}},
		cs.mtls(map[string]string{"node-a": cs.spki["node-x"]}))
	eventually(t, 5*time.Second, "handshake failure", func() bool {
		return strings.Contains(sourceStatus(b, "node-a").LastError, "tls handshake")
	})
	if sourceStatus(b, "node-a").Sessions != 0 || consumerStatus(a, "node-b").Sessions != 0 {
		t.Fatal("session established with a wrong pin")
	}
}

func secretOpt(dir, secret string) nodeOpt {
	return func(cfg *config.PeerLinkConfig, _ *Deps) {
		cfg.AllowUnauthenticatedPeers = false
		cfg.Tls = config.PeerLinkTLS{Enabled: true, AutoGenerate: true,
			CertPath: filepath.Join(dir, "peer-{NodeId}.pem"), KeyPath: filepath.Join(dir, "peer-{NodeId}.key")}
		for i := range cfg.Peers {
			cfg.Peers[i].SharedSecrets = []string{secret}
		}
	}
}

func TestSharedSecretOverTLS(t *testing.T) {
	const s1 = "c2VjcmV0LXNlY3JldC1zZWNyZXQtMQ=="
	const s2 = "c2VjcmV0LXNlY3JldC1zZWNyZXQtMg=="
	dir := t.TempDir()
	a := startNode(t, "node-a", "127.0.0.1:0", []config.PeerConfig{{NodeID: "node-b"}}, secretOpt(dir, s1),
		func(cfg *config.PeerLinkConfig, _ *Deps) { cfg.Peers[0].SharedSecrets = []string{s2, s1} })
	b := startNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: a.addr, Serve: boolp(false)}}, secretOpt(dir, s1))
	waitStreaming(t, b, "node-a")
	publishPkt(t, a, packets.Packet{TopicName: "psk/x", Payload: []byte("1")})
	eventually(t, 5*time.Second, "delivered", func() bool { return b.recv.count("psk/x") == 1 })

	// A consumer with the wrong secret is refused.
	c := startNode(t, "node-b", "", []config.PeerConfig{{NodeID: "node-a", Address: a.addr, Serve: boolp(false)}},
		secretOpt(dir, "d3Jvbmctc2VjcmV0LXdyb25nLXNlY3JldA=="))
	eventually(t, 5*time.Second, "auth failure", func() bool {
		return consumerStatus(a, "node-b").AuthFailures["auth_failed"] >= 1
	})
	if got := sourceStatus(c, "node-a").Sessions; got != 0 {
		t.Fatalf("wrong secret got a session")
	}
}

func TestTLSListenerPlaintextPolicy(t *testing.T) {
	dir := t.TempDir()
	a := startNode(t, "node-a", "127.0.0.1:0", []config.PeerConfig{{NodeID: "node-b"}}, secretOpt(dir, "c2VjcmV0LXNlY3JldC1zZWNyZXQtMQ=="))
	c, err := net.Dial("tcp", a.addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write(wire.AppendPreamble(nil))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("plaintext preamble served by a TLS listener")
	}
	_ = c.Close()
	eventually(t, 2*time.Second, "refusedPlaintext", func() bool { return a.m.Status().Admission.RefusedPlaintext == 1 })

	// Migration: AllowPlaintext with the waiver accepts the plaintext preamble of a peer without a
	// secret, but never of a peer that has one (review finding 16).
	m := startNode(t, "node-a", "127.0.0.1:0", []config.PeerConfig{{NodeID: "node-b"}, {NodeID: "node-c"}},
		secretOpt(t.TempDir(), "c2VjcmV0LXNlY3JldC1zZWNyZXQtMQ=="), func(cfg *config.PeerLinkConfig, _ *Deps) {
			cfg.Listener.AllowPlaintext = true
			cfg.AllowUnauthenticatedPeers = true
			cfg.Peers[1].SharedSecrets = nil
		})
	if _, ok := dialRaw(t, m.addr).hello("node-c", "node-a", nil).(*wire.HelloOK); !ok {
		t.Fatal("plaintext migration session refused")
	}
	g := expectGoAway(t, dialRaw(t, m.addr).hello("node-b", "node-a", nil), wire.GoAwayAuthFailed)
	if g.Reason != "" {
		t.Fatalf("reason %q leaked", g.Reason)
	}
}
