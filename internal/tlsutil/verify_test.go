package tlsutil

import (
	"crypto/x509"
	"errors"
	"testing"
	"time"
)

func TestVerifyChain(t *testing.T) {
	root := newCA(t, "root")
	inter := newCert(t, certSpec{cn: "intermediate", ca: true, parent: root})
	leaf := nodeCert(t, inter, "oa-a")
	trust := Trust{Roots: poolOf(root.cert)}

	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if err := trust.Verify(leaf.chain, Peer{NodeID: "oa-a"}, usage); err != nil {
			t.Fatalf("usage %v: %v", usage, err)
		}
	}
	if err := trust.Verify(leaf.chain[:1], Peer{NodeID: "oa-a"}, x509.ExtKeyUsageServerAuth); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("missing intermediate: got %v, want ErrUntrusted", err)
	}
	if err := trust.Verify(nil, Peer{NodeID: "oa-a"}, x509.ExtKeyUsageServerAuth); !errors.Is(err, ErrNoCertificate) {
		t.Fatalf("empty chain: got %v", err)
	}
	foreign := nodeCert(t, newCA(t, "foreign"), "oa-a")
	if err := trust.Verify(foreign.chain, Peer{NodeID: "oa-a"}, x509.ExtKeyUsageServerAuth); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("foreign CA: got %v, want ErrUntrusted", err)
	}
	expired := newCert(t, certSpec{cn: "oa-a", uris: []string{NodeURI("oa-a")}, parent: root,
		notBefore: time.Now().Add(-48 * time.Hour), notAfter: time.Now().Add(-24 * time.Hour)})
	if err := trust.Verify(expired.chain, Peer{NodeID: "oa-a"}, x509.ExtKeyUsageServerAuth); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("expired: got %v, want ErrUntrusted", err)
	}
}

func TestVerifyWrongEKU(t *testing.T) {
	ca := newCA(t, "ca")
	trust := Trust{Roots: poolOf(ca.cert)}
	serverOnly := nodeCert(t, ca, "oa-a", x509.ExtKeyUsageServerAuth)
	if err := trust.Verify(serverOnly.chain, Peer{NodeID: "oa-a"}, x509.ExtKeyUsageClientAuth); !errors.Is(err, ErrKeyUsage) {
		t.Fatalf("ServerAuth-only as client: got %v, want ErrKeyUsage", err)
	}
	clientOnly := nodeCert(t, ca, "oa-a", x509.ExtKeyUsageClientAuth)
	if err := trust.Verify(clientOnly.chain, Peer{NodeID: "oa-a"}, x509.ExtKeyUsageServerAuth); !errors.Is(err, ErrKeyUsage) {
		t.Fatalf("ClientAuth-only as server: got %v, want ErrKeyUsage", err)
	}
	pinned := Peer{NodeID: "oa-a", Pins: []Pin{SPKIPin(serverOnly.cert)}}
	if err := (Trust{}).Verify(serverOnly.chain, pinned, x509.ExtKeyUsageClientAuth); !errors.Is(err, ErrKeyUsage) {
		t.Fatalf("pinned ServerAuth-only as client: got %v, want ErrKeyUsage", err)
	}
	noEKU := newCert(t, certSpec{cn: "oa-a", uris: []string{NodeURI("oa-a")}, parent: ca, noEKU: true})
	if err := trust.Verify(noEKU.chain, Peer{NodeID: "oa-a"}, x509.ExtKeyUsageClientAuth); err != nil {
		t.Fatalf("no EKU means any usage (crypto/x509): %v", err)
	}
}

func TestVerifyWrongIdentity(t *testing.T) {
	ca := newCA(t, "ca")
	trust := Trust{Roots: poolOf(ca.cert)}
	c := nodeCert(t, ca, "oa-c")
	err := trust.Verify(c.chain, Peer{NodeID: "oa-b"}, x509.ExtKeyUsageServerAuth)
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("got %v, want ErrIdentityMismatch", err)
	}
	upper := newCert(t, certSpec{cn: "x", uris: []string{"urn:monstermq:node:OA-B"}, parent: ca})
	if err := trust.Verify(upper.chain, Peer{NodeID: "oa-b"}, x509.ExtKeyUsageServerAuth); err != nil {
		t.Fatalf("NodeIds are case-insensitive: %v", err)
	}
	if err := trust.Verify(c.chain, Peer{}, x509.ExtKeyUsageServerAuth); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("empty NodeId: got %v", err)
	}
}

func TestVerifyNoTruststoreNoPin(t *testing.T) {
	ca := newCA(t, "ca")
	c := nodeCert(t, ca, "oa-a")
	self := nodeCert(t, nil, "oa-a")
	empty, err := LoadCertPool("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, trust := range []Trust{{}, {Roots: empty}} {
		for _, chain := range [][]*x509.Certificate{c.chain, self.chain} {
			if err := trust.Verify(chain, Peer{NodeID: "oa-a"}, x509.ExtKeyUsageServerAuth); !errors.Is(err, ErrUntrusted) {
				t.Fatalf("got %v, want ErrUntrusted", err)
			}
		}
		if trust.hasRoots() {
			t.Fatal("empty trust reports roots")
		}
		if trust.roots() == nil {
			t.Fatal("roots() must never be nil (nil means system roots to crypto/x509)")
		}
	}
	if !(Trust{Roots: poolOf(ca.cert)}).hasRoots() {
		t.Fatal("pool with a CA reports no roots")
	}
}

func TestVerifyPins(t *testing.T) {
	self := nodeCert(t, nil, "oa-a")
	other := nodeCert(t, nil, "oa-a")
	for name, pins := range map[string][]Pin{
		"spki":   {SPKIPin(self.cert)},
		"cert":   {CertPin(self.cert)},
		"rotate": {SPKIPin(other.cert), SPKIPin(self.cert)},
	} {
		if err := (Trust{}).Verify(self.chain, Peer{NodeID: "oa-a", Pins: pins}, x509.ExtKeyUsageServerAuth); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	err := (Trust{}).Verify(self.chain, Peer{NodeID: "oa-a", Pins: []Pin{SPKIPin(other.cert)}}, x509.ExtKeyUsageServerAuth)
	if !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("mismatch: got %v", err)
	}
	// A pin replaces chain verification: a trusted chain does not help when the pin differs.
	ca := newCA(t, "ca")
	issued := nodeCert(t, ca, "oa-a")
	err = (Trust{Roots: poolOf(ca.cert)}).Verify(issued.chain, Peer{NodeID: "oa-a", Pins: []Pin{SPKIPin(other.cert)}}, x509.ExtKeyUsageServerAuth)
	if !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("pin with trusted chain: got %v", err)
	}
	// The identity still applies with a pin.
	err = (Trust{}).Verify(self.chain, Peer{NodeID: "oa-b", Pins: []Pin{SPKIPin(self.cert)}}, x509.ExtKeyUsageServerAuth)
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("pinned wrong identity: got %v", err)
	}
}

func TestSelfSignedInTruststore(t *testing.T) {
	self := nodeCert(t, nil, "oa-a")
	trust := Trust{Roots: poolOf(self.cert)}
	if err := trust.Verify(self.chain, Peer{NodeID: "oa-a"}, x509.ExtKeyUsageClientAuth); err != nil {
		t.Fatalf("self-signed in truststore: %v", err)
	}
	serverOnly := nodeCert(t, nil, "oa-a", x509.ExtKeyUsageServerAuth)
	trust = Trust{Roots: poolOf(serverOnly.cert)}
	if err := trust.Verify(serverOnly.chain, Peer{NodeID: "oa-a"}, x509.ExtKeyUsageClientAuth); !errors.Is(err, ErrKeyUsage) {
		t.Fatalf("EKU must apply to a chain of one: got %v", err)
	}
}

func TestIdentityFallback(t *testing.T) {
	ca := newCA(t, "ca")
	roots := poolOf(ca.cert)
	cnOnly := newCert(t, certSpec{cn: "oa-b", parent: ca})
	dnsOnly := newCert(t, certSpec{cn: "something", dns: []string{"OA-B"}, parent: ca})
	otherURI := newCert(t, certSpec{cn: "oa-b", dns: []string{"oa-b"}, uris: []string{NodeURI("oa-x")}, parent: ca})
	unrelatedURI := newCert(t, certSpec{cn: "oa-b", uris: []string{"spiffe://plant/oa-b"}, parent: ca})
	peer := Peer{NodeID: "oa-b"}

	cases := []struct {
		name     string
		cert     *testCert
		fallback IdentityFallback
		ok       bool
	}{
		{"cn-only default", cnOnly, "", false},
		{"cn-only NONE", cnOnly, FallbackNone, false},
		{"cn-only CN", cnOnly, FallbackCN, true},
		{"cn-only DNS", cnOnly, FallbackDNS, false},
		{"dns-only DNS", dnsOnly, FallbackDNS, true},
		{"dns-only CN", dnsOnly, FallbackCN, false},
		{"other node URI CN", otherURI, FallbackCN, false},
		{"other node URI DNS", otherURI, FallbackDNS, false},
		{"unrelated URI CN", unrelatedURI, FallbackCN, true},
	}
	for _, tc := range cases {
		err := Trust{Roots: roots, Fallback: tc.fallback}.Verify(tc.cert.chain, peer, x509.ExtKeyUsageServerAuth)
		if tc.ok && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if !tc.ok && !errors.Is(err, ErrIdentityMismatch) {
			t.Errorf("%s: got %v, want ErrIdentityMismatch", tc.name, err)
		}
	}
}

func TestCertificateIdentityOverride(t *testing.T) {
	ca := newCA(t, "ca")
	trust := Trust{Roots: poolOf(ca.cert), Fallback: FallbackCN}
	c := newCert(t, certSpec{cn: "oa-a", uris: []string{"spiffe://plant/oa-a"}, dns: []string{"oa-a.plant.local"}, parent: ca})
	for _, id := range []string{"spiffe://plant/oa-a", "OA-A.plant.local"} {
		if err := trust.Verify(c.chain, Peer{NodeID: "oa-a", CertificateIdentity: id}, x509.ExtKeyUsageServerAuth); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	// The override replaces the default identity and the fallback.
	std := nodeCert(t, ca, "oa-a")
	if err := trust.Verify(std.chain, Peer{NodeID: "oa-a", CertificateIdentity: "spiffe://plant/oa-a"}, x509.ExtKeyUsageServerAuth); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("override not matched: got %v", err)
	}
	if err := trust.Verify(c.chain, Peer{NodeID: "oa-a", CertificateIdentity: "spiffe://plant/OA-A"}, x509.ExtKeyUsageServerAuth); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("URI override must match exactly: got %v", err)
	}
}

func TestFindPeer(t *testing.T) {
	ca := newCA(t, "ca")
	trust := Trust{Roots: poolOf(ca.cert)}
	b := nodeCert(t, ca, "oa-b")
	peers := []Peer{{NodeID: "oa-a"}, {NodeID: "oa-b"}, {NodeID: "oa-c"}}
	p, err := trust.FindPeer(b.chain, peers, x509.ExtKeyUsageClientAuth)
	if err != nil || p.NodeID != "oa-b" {
		t.Fatalf("got %v, %v", p, err)
	}
	z := nodeCert(t, ca, "oa-z")
	if _, err := trust.FindPeer(z.chain, peers, x509.ExtKeyUsageClientAuth); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("unknown node: got %v", err)
	}
	foreign := nodeCert(t, newCA(t, "foreign"), "oa-b")
	if _, err := trust.FindPeer(foreign.chain, peers, x509.ExtKeyUsageClientAuth); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("identity matched but untrusted: got %v, want ErrUntrusted", err)
	}
	if _, err := trust.FindPeer(nil, peers, x509.ExtKeyUsageClientAuth); !errors.Is(err, ErrNoCertificate) {
		t.Fatalf("no cert: got %v", err)
	}
}
