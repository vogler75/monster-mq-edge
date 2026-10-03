package tlsutil

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"
)

// Node oa-a listens, node oa-b dials it, both with certificates from one CA.
func TestHandshakeMutualTLS(t *testing.T) {
	ca := newCA(t, "peer-ca")
	a := nodeCert(t, ca, "oa-a")
	b := nodeCert(t, ca, "oa-b")
	trust := Trust{Roots: poolOf(ca.cert)}

	srvCfg := mustServer(t, ServerOptions{Certificate: a.tls, Trust: trust, ClientAuth: ClientAuthRequired, Peers: []Peer{{NodeID: "oa-b"}}})
	cliCfg := mustClient(t, ClientOptions{Certificate: &b.tls, Trust: trust, Peer: Peer{NodeID: "oa-a"}})
	srv, cli := handshake(t, srvCfg, cliCfg)
	if srv.err != nil || cli.err != nil {
		t.Fatalf("server %v, client %v", srv.err, cli.err)
	}
	if cli.state.NegotiatedProtocol != ALPNPeer || srv.state.NegotiatedProtocol != ALPNPeer {
		t.Fatalf("ALPN %q/%q", srv.state.NegotiatedProtocol, cli.state.NegotiatedProtocol)
	}
	// Binding after HELLO: the claimed consumer must be the certificate's node.
	p, err := trust.FindPeer(srv.state.PeerCertificates, []Peer{{NodeID: "oa-b"}}, x509.ExtKeyUsageClientAuth)
	if err != nil || p.NodeID != "oa-b" {
		t.Fatalf("FindPeer: %v %v", p, err)
	}
	if err := trust.Verify(srv.state.PeerCertificates, Peer{NodeID: "oa-b"}, x509.ExtKeyUsageClientAuth); err != nil {
		t.Fatalf("HELLO consumer oa-b: %v", err)
	}
	if err := trust.Verify(srv.state.PeerCertificates, Peer{NodeID: "oa-c"}, x509.ExtKeyUsageClientAuth); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("HELLO consumer oa-c with oa-b's certificate: got %v", err)
	}
}

func TestHandshakeWrongNodeID(t *testing.T) {
	ca := newCA(t, "peer-ca")
	trust := Trust{Roots: poolOf(ca.cert)}
	a := nodeCert(t, ca, "oa-a")
	c := nodeCert(t, ca, "oa-c")

	t.Run("dialer rejects server", func(t *testing.T) {
		srvCfg := mustServer(t, ServerOptions{Certificate: c.tls, Trust: trust})
		cliCfg := mustClient(t, ClientOptions{Trust: trust, Peer: Peer{NodeID: "oa-a"}})
		_, cli := handshake(t, srvCfg, cliCfg)
		if !errors.Is(cli.err, ErrIdentityMismatch) {
			t.Fatalf("client: got %v, want ErrIdentityMismatch", cli.err)
		}
	})
	t.Run("listener rejects client", func(t *testing.T) {
		srvCfg := mustServer(t, ServerOptions{Certificate: a.tls, Trust: trust, ClientAuth: ClientAuthRequired, Peers: []Peer{{NodeID: "oa-b"}}})
		cliCfg := mustClient(t, ClientOptions{Certificate: &c.tls, Trust: trust, Peer: Peer{NodeID: "oa-a"}})
		srv, cli := handshake(t, srvCfg, cliCfg)
		if !errors.Is(srv.err, ErrIdentityMismatch) {
			t.Fatalf("server: got %v, want ErrIdentityMismatch", srv.err)
		}
		if cli.err == nil {
			t.Fatal("client did not see the rejection")
		}
	})
	t.Run("REQUEST still checks a presented certificate", func(t *testing.T) {
		srvCfg := mustServer(t, ServerOptions{Certificate: a.tls, Trust: trust, ClientAuth: ClientAuthRequest, Peers: []Peer{{NodeID: "oa-b"}}})
		cliCfg := mustClient(t, ClientOptions{Certificate: &c.tls, Trust: trust, Peer: Peer{NodeID: "oa-a"}})
		srv, _ := handshake(t, srvCfg, cliCfg)
		if !errors.Is(srv.err, ErrIdentityMismatch) {
			t.Fatalf("server: got %v, want ErrIdentityMismatch", srv.err)
		}
	})
	t.Run("REQUEST without client certificate", func(t *testing.T) {
		srvCfg := mustServer(t, ServerOptions{Certificate: a.tls, Trust: trust, ClientAuth: ClientAuthRequest, Peers: []Peer{{NodeID: "oa-b"}}})
		cliCfg := mustClient(t, ClientOptions{Trust: trust, Peer: Peer{NodeID: "oa-a"}})
		srv, cli := handshake(t, srvCfg, cliCfg)
		if srv.err != nil || cli.err != nil {
			t.Fatalf("server %v, client %v", srv.err, cli.err)
		}
		if len(srv.state.PeerCertificates) != 0 {
			t.Fatal("unexpected client certificate")
		}
	})
	t.Run("REQUIRED without client certificate", func(t *testing.T) {
		srvCfg := mustServer(t, ServerOptions{Certificate: a.tls, Trust: trust, ClientAuth: ClientAuthRequired, Peers: []Peer{{NodeID: "oa-b"}}})
		cliCfg := mustClient(t, ClientOptions{Trust: trust, Peer: Peer{NodeID: "oa-a"}})
		srv, cli := handshake(t, srvCfg, cliCfg)
		if srv.err == nil || cli.err == nil {
			t.Fatalf("server %v, client %v", srv.err, cli.err)
		}
	})
}

func TestHandshakeMissingEKU(t *testing.T) {
	ca := newCA(t, "peer-ca")
	trust := Trust{Roots: poolOf(ca.cert)}
	a := nodeCert(t, ca, "oa-a")
	bServerOnly := nodeCert(t, ca, "oa-b", x509.ExtKeyUsageServerAuth)

	t.Run("client certificate without ClientAuth, truststore", func(t *testing.T) {
		srvCfg := mustServer(t, ServerOptions{Certificate: a.tls, Trust: trust, ClientAuth: ClientAuthRequired, Peers: []Peer{{NodeID: "oa-b"}}})
		cliCfg := mustClient(t, ClientOptions{Certificate: &bServerOnly.tls, Trust: trust, Peer: Peer{NodeID: "oa-a"}})
		srv, cli := handshake(t, srvCfg, cliCfg)
		if srv.err == nil || cli.err == nil {
			t.Fatalf("server %v, client %v", srv.err, cli.err)
		}
	})
	t.Run("client certificate without ClientAuth, pinned", func(t *testing.T) {
		pins := []Pin{SPKIPin(bServerOnly.cert)}
		srvCfg := mustServer(t, ServerOptions{Certificate: a.tls, Trust: trust, ClientAuth: ClientAuthRequired, Peers: []Peer{{NodeID: "oa-b", Pins: pins}}})
		cliCfg := mustClient(t, ClientOptions{Certificate: &bServerOnly.tls, Trust: trust, Peer: Peer{NodeID: "oa-a"}})
		srv, _ := handshake(t, srvCfg, cliCfg)
		if !errors.Is(srv.err, ErrKeyUsage) {
			t.Fatalf("server: got %v, want ErrKeyUsage", srv.err)
		}
	})
	t.Run("server certificate without ServerAuth", func(t *testing.T) {
		aClientOnly := nodeCert(t, ca, "oa-a", x509.ExtKeyUsageClientAuth)
		srvCfg := mustServer(t, ServerOptions{Certificate: aClientOnly.tls, Trust: trust})
		cliCfg := mustClient(t, ClientOptions{Trust: trust, Peer: Peer{NodeID: "oa-a"}})
		_, cli := handshake(t, srvCfg, cliCfg)
		if !errors.Is(cli.err, ErrKeyUsage) {
			t.Fatalf("client: got %v, want ErrKeyUsage", cli.err)
		}
	})
}

func TestHandshakePins(t *testing.T) {
	a := nodeCert(t, nil, "oa-a")
	b := nodeCert(t, nil, "oa-b")
	stranger := nodeCert(t, nil, "oa-b")
	run := func(srvPins, cliPins []Pin) (side, side) {
		srvCfg := mustServer(t, ServerOptions{Certificate: a.tls, ClientAuth: ClientAuthRequired, Peers: []Peer{{NodeID: "oa-b", Pins: srvPins}}})
		cliCfg := mustClient(t, ClientOptions{Certificate: &b.tls, Peer: Peer{NodeID: "oa-a", Pins: cliPins}})
		return handshake(t, srvCfg, cliCfg)
	}

	srv, cli := run([]Pin{SPKIPin(b.cert)}, []Pin{CertPin(a.cert)})
	if srv.err != nil || cli.err != nil {
		t.Fatalf("pins match: server %v, client %v", srv.err, cli.err)
	}
	_, cli = run([]Pin{SPKIPin(b.cert)}, []Pin{SPKIPin(stranger.cert)})
	if !errors.Is(cli.err, ErrPinMismatch) {
		t.Fatalf("server pin mismatch: client got %v", cli.err)
	}
	srv, _ = run([]Pin{SPKIPin(stranger.cert)}, []Pin{SPKIPin(a.cert)})
	if !errors.Is(srv.err, ErrPinMismatch) {
		t.Fatalf("client pin mismatch: server got %v", srv.err)
	}
}

func TestHandshakeSelfSignedTruststore(t *testing.T) {
	a := nodeCert(t, nil, "oa-a")
	b := nodeCert(t, nil, "oa-b")
	trust := Trust{Roots: poolOf(a.cert, b.cert)}
	srvCfg := mustServer(t, ServerOptions{Certificate: a.tls, Trust: trust, ClientAuth: ClientAuthRequired, Peers: []Peer{{NodeID: "oa-b"}}})
	cliCfg := mustClient(t, ClientOptions{Certificate: &b.tls, Trust: trust, Peer: Peer{NodeID: "oa-a"}})
	srv, cli := handshake(t, srvCfg, cliCfg)
	if srv.err != nil || cli.err != nil {
		t.Fatalf("server %v, client %v", srv.err, cli.err)
	}
}

func TestClientConfigNoTrust(t *testing.T) {
	empty, _ := LoadCertPool("", "", "")
	for _, trust := range []Trust{{}, {Roots: empty}} {
		if _, err := ClientConfig(ClientOptions{Trust: trust, Peer: Peer{NodeID: "oa-a"}}); !errors.Is(err, ErrNoTrust) {
			t.Fatalf("got %v, want ErrNoTrust", err)
		}
	}
	for _, o := range []ClientOptions{
		{Peer: Peer{NodeID: "oa-a"}, SharedSecret: true},
		{Peer: Peer{NodeID: "oa-a", Pins: []Pin{{1}}}},
		{Peer: Peer{NodeID: "oa-a"}, InsecureSkipVerify: true},
	} {
		if _, err := ClientConfig(o); err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
	}
	if _, err := ClientConfig(ClientOptions{SharedSecret: true}); err == nil {
		t.Fatal("dialer without a source NodeId accepted")
	}
}

func TestHandshakeSharedSecret(t *testing.T) {
	a := nodeCert(t, nil, "oa-a")
	srvCfg := mustServer(t, ServerOptions{Certificate: a.tls, SharedSecret: true, Peers: []Peer{{NodeID: "oa-b"}}})
	if srvCfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("listener MinVersion %x with a shared secret", srvCfg.MinVersion)
	}

	cliCfg := mustClient(t, ClientOptions{Peer: Peer{NodeID: "oa-a"}, SharedSecret: true})
	if cliCfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("dialer MinVersion %x with a shared secret", cliCfg.MinVersion)
	}
	srv, cli := handshake(t, srvCfg, cliCfg)
	if srv.err != nil || cli.err != nil {
		t.Fatalf("unverified certificate with a secret: server %v, client %v", srv.err, cli.err)
	}
	if cli.state.Version != tls.VersionTLS13 {
		t.Fatalf("version %x", cli.state.Version)
	}
	if err := (Trust{}).Verify(cli.state.PeerCertificates, Peer{NodeID: "oa-a"}, x509.ExtKeyUsageServerAuth); err == nil {
		t.Fatal("certificate reported as verified")
	}
	ekmS, err1 := srv.state.ExportKeyingMaterial("monstermq-peer/1", nil, 32)
	ekmC, err2 := cli.state.ExportKeyingMaterial("monstermq-peer/1", nil, 32)
	if err1 != nil || err2 != nil || !bytes.Equal(ekmS, ekmC) {
		t.Fatalf("exporter: %v %v equal=%v", err1, err2, bytes.Equal(ekmS, ekmC))
	}

	// Pins stay enforced next to a secret.
	other := nodeCert(t, nil, "oa-a")
	cliCfg = mustClient(t, ClientOptions{Peer: Peer{NodeID: "oa-a", Pins: []Pin{SPKIPin(other.cert)}}, SharedSecret: true})
	if _, cli = handshake(t, srvCfg, cliCfg); !errors.Is(cli.err, ErrPinMismatch) {
		t.Fatalf("pin with secret: got %v", cli.err)
	}

	// A TLS 1.2 client cannot reach a listener that requires 1.3.
	old := mustClient(t, ClientOptions{Peer: Peer{NodeID: "oa-a"}, InsecureSkipVerify: true})
	old.MaxVersion = tls.VersionTLS12
	if srv, _ = handshake(t, srvCfg, old); srv.err == nil {
		t.Fatal("TLS 1.2 accepted with a shared secret")
	}
	if cfg := mustClient(t, ClientOptions{Peer: Peer{NodeID: "oa-a", Pins: []Pin{{1}}}}); cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion %x without a secret", cfg.MinVersion)
	}
}

func TestHandshakeInsecureSkipVerify(t *testing.T) {
	a := nodeCert(t, nil, "oa-z")
	srvCfg := mustServer(t, ServerOptions{Certificate: a.tls})
	cliCfg := mustClient(t, ClientOptions{Peer: Peer{NodeID: "oa-a"}, InsecureSkipVerify: true})
	if srv, cli := handshake(t, srvCfg, cliCfg); srv.err != nil || cli.err != nil {
		t.Fatalf("server %v, client %v", srv.err, cli.err)
	}
}

func TestHandshakeResumedSessionVerified(t *testing.T) {
	ca := newCA(t, "peer-ca")
	trust := Trust{Roots: poolOf(ca.cert)}
	a := nodeCert(t, ca, "oa-a")
	b := nodeCert(t, ca, "oa-b")

	srvCfg := mustServer(t, ServerOptions{Certificate: a.tls, Trust: trust, ClientAuth: ClientAuthRequired, Peers: []Peer{{NodeID: "oa-b"}}})
	var srvResumed []bool
	baseSrvVerify := srvCfg.VerifyConnection
	srvCfg.VerifyConnection = func(cs tls.ConnectionState) error {
		srvResumed = append(srvResumed, cs.DidResume)
		return baseSrvVerify(cs)
	}
	cache := tls.NewLRUClientSessionCache(4)
	newClient := func(expect string) (*tls.Config, *[]bool) {
		cfg := mustClient(t, ClientOptions{Certificate: &b.tls, Trust: trust, Peer: Peer{NodeID: expect}, ServerName: "peer-a"})
		cfg.ClientSessionCache = cache
		var seen []bool
		base := cfg.VerifyConnection
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			seen = append(seen, cs.DidResume)
			return base(cs)
		}
		return cfg, &seen
	}

	cliCfg, seen := newClient("oa-a")
	if srv, cli := handshake(t, srvCfg, cliCfg); srv.err != nil || cli.err != nil {
		t.Fatalf("first: server %v, client %v", srv.err, cli.err)
	}
	srv, cli := handshake(t, srvCfg, cliCfg)
	if srv.err != nil || cli.err != nil {
		t.Fatalf("resumed: server %v, client %v", srv.err, cli.err)
	}
	if !cli.state.DidResume {
		t.Fatal("session was not resumed")
	}
	if len(*seen) != 2 || !(*seen)[1] || len(srvResumed) != 2 || !srvResumed[1] {
		t.Fatalf("VerifyConnection calls: client %v, server %v", *seen, srvResumed)
	}

	// The listener re-checks the stored client certificate on resumption as well.
	other := mustServer(t, ServerOptions{Certificate: a.tls, Trust: trust, ClientAuth: ClientAuthRequired, Peers: []Peer{{NodeID: "oa-c"}}})
	srvCfg2 := srvCfg.Clone()
	srvResumed = nil
	srvCfg2.VerifyConnection = func(cs tls.ConnectionState) error {
		srvResumed = append(srvResumed, cs.DidResume)
		return other.VerifyConnection(cs)
	}
	cliCfg, _ = newClient("oa-a")
	srv, _ = handshake(t, srvCfg2, cliCfg)
	if !errors.Is(srv.err, ErrIdentityMismatch) {
		t.Fatalf("server on resumption: got %v", srv.err)
	}
	if len(srvResumed) != 1 || !srvResumed[0] {
		t.Fatalf("expected a resumed attempt on the server, saw %v", srvResumed)
	}

	// A resumed session is still bound to the expected NodeId.
	wrong, seenWrong := newClient("oa-x")
	_, cli = handshake(t, srvCfg, wrong)
	if !errors.Is(cli.err, ErrIdentityMismatch) {
		t.Fatalf("resumed with wrong NodeId: got %v", cli.err)
	}
	if len(*seenWrong) != 1 || !(*seenWrong)[0] {
		t.Fatalf("expected a resumed attempt, VerifyConnection saw %v", *seenWrong)
	}
}

func TestALPNStatusEndpoint(t *testing.T) {
	ca := newCA(t, "peer-ca")
	trust := Trust{Roots: poolOf(ca.cert)}
	a := nodeCert(t, ca, "oa-a")
	srvCfg := mustServer(t, ServerOptions{Certificate: a.tls, Trust: trust})
	cliCfg := mustClient(t, ClientOptions{Trust: trust, Peer: Peer{NodeID: "oa-a"}, NextProtos: []string{ALPNHTTP}})
	srv, cli := handshake(t, srvCfg, cliCfg)
	if srv.err != nil || cli.err != nil {
		t.Fatalf("server %v, client %v", srv.err, cli.err)
	}
	if srv.state.NegotiatedProtocol != ALPNHTTP {
		t.Fatalf("ALPN %q", srv.state.NegotiatedProtocol)
	}
}

func TestVerifyConnectionNeverNil(t *testing.T) {
	ca := newCA(t, "peer-ca")
	a := nodeCert(t, ca, "oa-a")
	roots := poolOf(ca.cert)
	for _, o := range []ServerOptions{
		{Certificate: a.tls},
		{Certificate: a.tls, ClientAuth: ClientAuthRequest, Trust: Trust{Roots: roots}},
		{Certificate: a.tls, ClientAuth: ClientAuthRequired, Trust: Trust{Roots: roots}, Peers: []Peer{{NodeID: "b"}}},
		{Certificate: a.tls, ClientAuth: ClientAuthRequired, Peers: []Peer{{NodeID: "b", Pins: []Pin{{1}}}}},
		{Certificate: a.tls, SharedSecret: true},
	} {
		cfg := mustServer(t, o)
		if cfg.VerifyConnection == nil {
			t.Fatalf("server %+v: VerifyConnection is nil", o.ClientAuth)
		}
		if cfg.ClientAuth >= tls.VerifyClientCertIfGiven && cfg.ClientCAs == nil {
			t.Fatal("verifying ClientAuth with nil ClientCAs would use the system roots")
		}
	}
	for _, o := range []ClientOptions{
		{Peer: Peer{NodeID: "a"}, Trust: Trust{Roots: roots}},
		{Peer: Peer{NodeID: "a", Pins: []Pin{{1}}}},
		{Peer: Peer{NodeID: "a"}, SharedSecret: true},
		{Peer: Peer{NodeID: "a"}, InsecureSkipVerify: true},
		{Peer: Peer{NodeID: "a"}, Trust: Trust{Roots: roots}, Certificate: &a.tls, ServerName: "x"},
	} {
		cfg := mustClient(t, o)
		if cfg.VerifyConnection == nil {
			t.Fatal("client: VerifyConnection is nil")
		}
		if !cfg.InsecureSkipVerify {
			t.Fatal("client: InsecureSkipVerify must be set; VerifyConnection does all checks")
		}
		if cfg.RootCAs == nil {
			t.Fatal("client: RootCAs nil")
		}
	}
}

func TestServerConfigModes(t *testing.T) {
	ca := newCA(t, "peer-ca")
	a := nodeCert(t, ca, "oa-a")
	roots := poolOf(ca.cert)
	pinned := []Peer{{NodeID: "b", Pins: []Pin{{1}}}}

	cases := []struct {
		auth    ClientAuth
		trust   Trust
		peers   []Peer
		want    tls.ClientAuthType
		wantCAs bool
	}{
		{"", Trust{}, nil, tls.NoClientCert, false},
		{ClientAuthNone, Trust{Roots: roots}, nil, tls.NoClientCert, false},
		{ClientAuthRequest, Trust{Roots: roots}, []Peer{{NodeID: "b"}}, tls.VerifyClientCertIfGiven, true},
		{ClientAuthRequired, Trust{Roots: roots}, []Peer{{NodeID: "b"}}, tls.RequireAndVerifyClientCert, true},
		{ClientAuthRequest, Trust{}, pinned, tls.RequestClientCert, false},
		{ClientAuthRequired, Trust{Roots: roots}, append([]Peer{{NodeID: "c"}}, pinned...), tls.RequireAnyClientCert, false},
		{"required", Trust{Roots: roots}, nil, tls.RequireAndVerifyClientCert, true},
	}
	for _, tc := range cases {
		cfg := mustServer(t, ServerOptions{Certificate: a.tls, Trust: tc.trust, ClientAuth: tc.auth, Peers: tc.peers})
		if cfg.ClientAuth != tc.want || (cfg.ClientCAs != nil) != tc.wantCAs {
			t.Errorf("%s: ClientAuth %v ClientCAs %v", tc.auth, cfg.ClientAuth, cfg.ClientCAs != nil)
		}
		if cfg.MinVersion != tls.VersionTLS12 || len(cfg.NextProtos) != 2 || cfg.NextProtos[0] != ALPNPeer || cfg.NextProtos[1] != ALPNHTTP {
			t.Errorf("%s: MinVersion %x NextProtos %v", tc.auth, cfg.MinVersion, cfg.NextProtos)
		}
	}

	for _, o := range []ServerOptions{
		{},
		{Certificate: a.tls, ClientAuth: "SOMETIMES"},
		{Certificate: a.tls, ClientAuth: ClientAuthRequired, Peers: []Peer{{NodeID: "b"}}},
		{Certificate: a.tls, ClientAuth: ClientAuthRequest, Peers: append([]Peer{{NodeID: "c"}}, pinned...)},
	} {
		if _, err := ServerConfig(o); err == nil {
			t.Errorf("ServerConfig(%+v) accepted", o.ClientAuth)
		}
	}
}
