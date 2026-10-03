package integration

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/peerlink/wire"
	"monstermq.io/edge/internal/tlsutil"
)

// plCA issues PeerLink node certificates with the NodeId URI SAN.
type plCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	path string
	dir  string
}

func newPLCA(t *testing.T) *plCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "PeerLink test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return &plCA{cert: cert, key: key, path: path, dir: dir}
}

// issue writes a certificate for nodeID (URI SAN urn:monstermq:node:<id>,
// both EKUs) and returns the certificate and key paths.
func (ca *plCA) issue(t *testing.T, file, nodeID string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(tlsutil.NodeURI(nodeID))
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: nodeID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{u},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(key)
	certPath := filepath.Join(ca.dir, file+".pem")
	keyPath := filepath.Join(ca.dir, file+".key")
	_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
	_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600)
	return certPath, keyPath
}

func plSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// plTLS switches a node to TLS without the unauthenticated waiver.
func plTLS(certPath, keyPath string) plOpt {
	return func(c *config.Config) {
		c.PeerLink.AllowUnauthenticatedPeers = false
		c.PeerLink.Tls.Enabled = true
		c.PeerLink.Tls.CertPath = certPath
		c.PeerLink.Tls.KeyPath = keyPath
	}
}

// PL-16: TLS with a group shared secret in the middle of a rotation (each
// side dials with a different first secret) and a wrong secret.
func TestPeerLinkTLSSharedSecret(t *testing.T) {
	dir := t.TempDir()
	old, next, wrong := plSecret(), plSecret(), plSecret()
	secrets := func(list ...string) plOpt {
		return func(c *config.Config) {
			c.PeerLink.Tls.AutoGenerate = true
			c.PeerLink.SharedSecrets = list
		}
	}
	a, b := plPair(t, "pl16sa", "pl16sb", 0, 27350, 0, 27351,
		[]plOpt{plTLS(filepath.Join(dir, "a.pem"), filepath.Join(dir, "a.key")), secrets(next, old)},
		[]plOpt{plTLS(filepath.Join(dir, "b.pem"), filepath.Join(dir, "b.key")), secrets(old, next)})
	if st := a.status(); !st.TLS {
		t.Fatal("A listener is not TLS")
	}
	a.publish("pl16s/x", "1", 1, false)
	b.publish("pl16s/y", "1", 1, false)
	plWaitCount(t, b, "pl16s/x", 1, 5*time.Second, 0)
	plWaitCount(t, a, "pl16s/y", 1, 5*time.Second, 0)

	b.Close()
	b.cfg.PeerLink.SharedSecrets = []string{wrong}
	b.start()
	plEventually(t, 5*time.Second, "wrong secret refused by A", func() bool {
		return a.consumer("pl16sb").AuthFailures["auth_failed"] >= 1
	})
	time.Sleep(300 * time.Millisecond)
	if s := plSourceState(b, "pl16sa"); s == "STREAMING" {
		t.Fatal("B streams from A with a wrong secret")
	}
	if s := plSourceState(a, "pl16sb"); s == "STREAMING" {
		t.Fatal("A streams from B with a wrong secret")
	}
}

// PL-16: mTLS against a CA with the NodeId URI SAN; a certificate naming
// another node is refused in both directions.
func TestPeerLinkMTLSIdentity(t *testing.T) {
	ca := newPLCA(t)
	mtls := func(file, node string) plOpt {
		cert, key := ca.issue(t, file, node)
		return func(c *config.Config) {
			plTLS(cert, key)(c)
			c.PeerLink.Tls.TrustStorePath = ca.path
			c.PeerLink.Tls.ClientAuth = config.ClientAuthRequired
		}
	}
	a, b := plPair(t, "pl16ma", "pl16mb", 0, 27352, 0, 27353,
		[]plOpt{mtls("a", "pl16ma")}, []plOpt{mtls("b", "pl16mb")})
	a.publish("pl16m/x", "1", 1, false)
	plWaitCount(t, b, "pl16m/x", 1, 5*time.Second, 0)

	// B restarts with a CA-signed certificate that names another node.
	b.Close()
	cert, key := ca.issue(t, "x", "pl16mx")
	b.cfg.PeerLink.Tls.CertPath, b.cfg.PeerLink.Tls.KeyPath = cert, key
	b.start()
	plEventually(t, 5*time.Second, "A refuses B's certificate", func() bool {
		st := a.srv.PeerLink().Status()
		var refused uint64
		for _, c := range st.Consumers {
			refused += c.AuthFailures["identity_mismatch"]
		}
		return refused+st.Admission.TLSFailures >= 1
	})
	plEventually(t, 5*time.Second, "A's dialer refuses B's certificate", func() bool {
		return a.source("pl16mb").LastError != ""
	})
	time.Sleep(300 * time.Millisecond)
	if s := plSourceState(b, "pl16ma"); s == "STREAMING" {
		t.Fatal("B streams from A with a certificate for another NodeId")
	}
	if s := plSourceState(a, "pl16mb"); s == "STREAMING" {
		t.Fatal("A streams from B whose certificate names another NodeId")
	}
}

// PL-17/PL-22: an unauthenticated link fails closed at startup.
func TestPeerLinkFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  plOpt
		want string
	}{
		{"no auth and no waiver", func(c *config.Config) { c.PeerLink.AllowUnauthenticatedPeers = false }, "not authenticated"},
		{"waiver without networks", func(c *config.Config) { c.PeerLink.Listener.AllowedNetworks = nil }, "AllowedNetworks"},
		{"waiver with user management", func(c *config.Config) { c.UserManagement.Enabled = true }, "UserManagement"},
		{"secret without TLS", func(c *config.Config) { c.PeerLink.SharedSecrets = []string{plSecret()} }, "Tls.Enabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := plConfig("pl17f", 0, 27354, t.TempDir(), []config.PeerConfig{plPeer("pl17g", 27355)}, tc.mod)
			srv, err := broker.New(cfg, slog.New(slog.DiscardHandler), nil)
			if err == nil {
				_ = srv.Close()
				t.Fatal("broker started with an unauthenticated or invalid PeerLink config")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
	// The peer port is free again: a failed start leaves nothing bound.
	ln, err := net.Listen("tcp", "127.0.0.1:27354")
	if err != nil {
		t.Fatalf("peer port still bound after failed starts: %v", err)
	}
	_ = ln.Close()
}

// PL-17: AllowedNetworks refuses a disallowed source address before any
// handshake.
func TestPeerLinkAllowedNetworks(t *testing.T) {
	a := startPL(t, "pl17na", 0, 27356, []config.PeerConfig{plPeer("pl17nb", 0)},
		func(c *config.Config) { c.PeerLink.Listener.AllowedNetworks = []string{"10.0.0.0/8"} })
	b := startPL(t, "pl17nb", 0, 0, []config.PeerConfig{plPullOnly("pl17na", 27356)})
	plEventually(t, 5*time.Second, "loopback refused by AllowedNetworks", func() bool {
		return a.srv.PeerLink().Status().Admission.RefusedNetwork >= 1
	})
	if s := plSourceState(b, "pl17na"); s == "STREAMING" {
		t.Fatal("B streams although its address is not allowed")
	}
}

// plRawPeer is a scripted mmq-peer/1 consumer.
type plRawPeer struct {
	t  *testing.T
	c  net.Conn
	fr *wire.FrameReader
}

func dialRawPeer(t *testing.T, port int, major uint16) *plRawPeer {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial peer port: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(wire.AppendPreambleVersion(nil, major, 0)); err != nil {
		t.Fatal(err)
	}
	return &plRawPeer{t: t, c: c, fr: wire.NewFrameReader(bufio.NewReader(c), wire.DefaultMaxFrameBytes)}
}

func (r *plRawPeer) read() wire.Frame {
	r.t.Helper()
	typ, body, err := r.fr.ReadFrame()
	if err != nil {
		r.t.Fatalf("read frame: %v", err)
	}
	f, err := wire.DecodeFrame(typ, body)
	if err != nil {
		r.t.Fatalf("decode %s: %v", typ, err)
	}
	return f
}

func (r *plRawPeer) write(f wire.Frame) {
	r.t.Helper()
	if err := wire.WriteFrame(r.c, f); err != nil {
		r.t.Fatalf("write %s: %v", f.Type(), err)
	}
}

func (r *plRawPeer) hello(consumer, expected string) wire.Frame {
	r.t.Helper()
	if _, ok := r.read().(*wire.ServerHello); !ok {
		r.t.Fatal("no SERVER_HELLO")
	}
	r.write(&wire.Hello{Capabilities: wire.CapsV1, InstanceID: 42, MaxRecordBytes: 1 << 20,
		ConsumerNodeID: consumer, ExpectedSourceNodeID: expected})
	return r.read()
}

func expectGoAway(t *testing.T, f wire.Frame, code wire.GoAwayCode) {
	t.Helper()
	g, ok := f.(*wire.GoAway)
	if !ok {
		t.Fatalf("got %s, want GOAWAY(%s)", f.Type(), code)
	}
	if g.Code != code {
		t.Fatalf("GOAWAY %s (%q), want %s", g.Code, g.Reason, code)
	}
}

// PL-17 and PL-20: handshake guards and protocol violations against the
// broker's real listener, plus a crossed address in a real config.
func TestPeerLinkHandshakeGoAway(t *testing.T) {
	a := startPL(t, "pl17a", 0, 27357, []config.PeerConfig{
		plPeer("pl17b", 0),
		plPullOnly("pl17c", 27358), // nothing listens there
	})
	a.publish("pl17/x", "1", 1, false)

	expectGoAway(t, dialRawPeer(t, 27357, 2).read(), wire.GoAwayVersion)
	for _, tc := range []struct {
		consumer, expected string
		code               wire.GoAwayCode
	}{
		{"PL17A", "pl17a", wire.GoAwaySelfConnection},
		{"pl17z", "pl17a", wire.GoAwayUnknownPeer},
		{"pl17c", "pl17a", wire.GoAwayNotAllowed},
		{"pl17b", "pl17q", wire.GoAwayWrongNode},
	} {
		expectGoAway(t, dialRawPeer(t, 27357, wire.VersionMajor).hello(tc.consumer, tc.expected), tc.code)
	}

	// A valid session; a FETCH beyond the log end and a commit beyond it
	// are protocol errors.
	r := dialRawPeer(t, 27357, wire.VersionMajor)
	ok, isOK := r.hello("pl17b", "pl17a").(*wire.HelloOK)
	if !isOK || ok.SourceNodeID != "pl17a" || ok.Leo != 2 || ok.TopicRoot != "" {
		t.Fatalf("HELLO_OK %+v", ok)
	}
	r.write(&wire.Fetch{FetchID: 1, Offset: ok.Leo + 5, MaxRecords: 10, MaxBytes: 1 << 20})
	expectGoAway(t, r.read(), wire.GoAwayOffsetOutOfRange)

	r = dialRawPeer(t, 27357, wire.VersionMajor)
	if _, isOK := r.hello("pl17b", "pl17a").(*wire.HelloOK); !isOK {
		t.Fatal("no HELLO_OK")
	}
	r.write(&wire.Commit{Commit: 1 << 40})
	expectGoAway(t, r.read(), wire.GoAwayProtocol)

	// A real node whose peer entry points at A under another NodeId.
	b := startPL(t, "pl17b", 0, 0, []config.PeerConfig{plPullOnly("pl17q", 27357)})
	plEventually(t, 5*time.Second, "wrong_node seen by B", func() bool {
		return strings.Contains(b.source("pl17q").LastError, "wrong_node")
	})
}

// PL-42: pre-authentication connections are limited per source IP.
func TestPeerLinkAdmission(t *testing.T) {
	a := startPL(t, "pl42a", 0, 27359, []config.PeerConfig{plPeer("pl42b", 0)})
	var conns []net.Conn
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", "127.0.0.1:27359")
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		defer c.Close()
		time.Sleep(50 * time.Millisecond)
	}
	plEventually(t, 3*time.Second, "third pre-auth connection refused", func() bool {
		return a.srv.PeerLink().Status().Admission.RefusedBusy >= 1
	})
	_ = conns[2].SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conns[2].Read(make([]byte, 1)); err == nil {
		t.Fatal("third connection was not closed")
	}
}

// PL-21 and PL-40: the status endpoint and the operator resync that
// restores newer values after an overflow while keeping newer local ones.
func TestPeerLinkStatusAndResync(t *testing.T) {
	small := func(c *config.Config) {
		c.PeerLink.Log.MaxMessages = intPtr(100)
		c.PeerLink.Fetch.MaxRecords = intPtr(100)
	}
	a := startPL(t, "pl40a", 0, 27360, []config.PeerConfig{plPeer("pl40b", 0)}, small)
	// B serves an idle peer only so that it has a status listener.
	b := startPL(t, "pl40b", 27361, 27362, []config.PeerConfig{plPullOnly("pl40a", 27360), plPeer("pl40idle", 0)}, small)
	b.waitStreaming("pl40a")

	code, body := plHTTP(t, 27362, "GET", "/peerlink/v1/status")
	var st map[string]any
	if code != http.StatusOK || json.Unmarshal(body, &st) != nil || st["nodeId"] != "pl40b" {
		t.Fatalf("status: %d %s", code, body)
	}
	if code, _ := plHTTP(t, 27362, "GET", "/peerlink/v1/nothing"); code != http.StatusNotFound {
		t.Fatalf("unknown path: HTTP %d", code)
	}
	if code, _ := plHTTP(t, 27362, "POST", "/peerlink/v1/resync?source=nobody"); code != http.StatusBadRequest {
		t.Fatalf("resync of an unknown source: HTTP %d", code)
	}

	a.publish("pl40/y", "a", 1, true)
	plEventually(t, 5*time.Second, "y on B", func() bool { return string(plRetained(b, "pl40/y")) == "a" })
	b.publish("pl40/y", "b-newer", 1, true)
	b.publish("pl40/x", "old", 1, true)
	plEventually(t, 5*time.Second, "local values on B", func() bool {
		return string(plRetained(b, "pl40/x")) == "old" && string(plRetained(b, "pl40/y")) == "b-newer"
	})
	b.Close()

	time.Sleep(2100 * time.Millisecond)
	a.publish("pl40/x", "new", 1, true)
	for i := 0; i < 150; i++ {
		a.publish("pl40/fill", fmt.Sprint(i), 0, false)
	}
	b.start()
	b.waitStreaming("pl40a")
	if s := b.source("pl40a"); s.GapLostTotal == 0 {
		t.Fatalf("no gap counted after the overflow: %+v", s)
	}
	if v := string(plRetained(b, "pl40/x")); v != "old" {
		t.Fatalf("x on B is %q before the resync, want old (fill keeps present values)", v)
	}

	snaps := b.source("pl40a").Snapshots
	if code, body := plHTTP(t, 27362, "POST", "/peerlink/v1/resync?source=pl40a"); code != http.StatusAccepted {
		t.Fatalf("resync: HTTP %d %s", code, body)
	}
	plEventually(t, 10*time.Second, "resync applied", func() bool {
		s := b.source("pl40a")
		return s.Snapshots > snaps && s.State == "STREAMING"
	})
	plEventually(t, 5*time.Second, "newer x on B", func() bool { return string(plRetained(b, "pl40/x")) == "new" })
	if v := string(plRetained(b, "pl40/y")); v != "b-newer" {
		t.Fatalf("y on B is %q after the resync, want the newer local value", v)
	}
	if s := b.source("pl40a"); s.SnapshotNewer < 1 {
		t.Fatalf("snapshotNewer %d", s.SnapshotNewer)
	}
}

// PL-39: NodeIds are compared in canonical (lower) case; an own entry in a
// shared Peers list is ignored.
func TestPeerLinkNodeIDCanonical(t *testing.T) {
	peers := []config.PeerConfig{plPeer("PL39-Node-A", 27363), plPeer("pl39-node-b", 27364)}
	a := startPL(t, "PL39-NODE-A", 0, 27363, peers)
	b := startPL(t, "pl39-node-b", 0, 27364, peers)
	a.waitStreaming("pl39-node-b")
	b.waitStreaming("pl39-node-a")
	a.publish("pl39/x", "1", 1, false)
	plWaitCount(t, b, "pl39/x", 1, 5*time.Second, 0)
	if id := a.srv.PeerLink().NodeID(); id != "pl39-node-a" {
		t.Fatalf("canonical NodeId %q", id)
	}
}
