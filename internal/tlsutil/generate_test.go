package tlsutil

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestEnsurePeerCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath := ExpandPath(filepath.Join(dir, "certs", "peer-{NodeId}.pem"), "oa-a")
	keyPath := ExpandPath(filepath.Join(dir, "keys", "peer-{NodeId}.key"), "oa-a")

	spki, created, err := EnsurePeerCertificate(certPath, keyPath, "oa-a")
	if err != nil || !created {
		t.Fatalf("create: %v created=%v", err, created)
	}
	pair, err := LoadKeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	c := pair.Leaf
	if spki != SPKIFingerprint(c) {
		t.Fatalf("returned SPKI %s, certificate has %s", spki, SPKIFingerprint(c))
	}
	if c.Subject.CommonName != "oa-a" {
		t.Errorf("CN %q", c.Subject.CommonName)
	}
	if len(c.URIs) != 1 || c.URIs[0].String() != "urn:monstermq:node:oa-a" {
		t.Errorf("URIs %v", c.URIs)
	}
	if !slices.Contains(c.ExtKeyUsage, x509.ExtKeyUsageServerAuth) || !slices.Contains(c.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		t.Errorf("EKU %v", c.ExtKeyUsage)
	}
	if years := c.NotAfter.Sub(time.Now()).Hours() / 24 / 365; years < 9.9 || years > 10.1 {
		t.Errorf("validity %.2f years", years)
	}
	if pub, ok := c.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Errorf("key %T", c.PublicKey)
	}
	if err := c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature); err != nil || !bytes.Equal(c.RawIssuer, c.RawSubject) {
		t.Errorf("not self-signed: %v", err)
	}
	if c.IsCA {
		t.Error("peer certificate must not be a CA")
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("key mode %v %v", fi.Mode().Perm(), err)
		}
	}
	if err := (Trust{}).Verify([]*x509.Certificate{c}, Peer{NodeID: "oa-a", Pins: []Pin{SPKIPin(c)}}, x509.ExtKeyUsageClientAuth); err != nil {
		t.Errorf("generated certificate does not verify as a pinned peer: %v", err)
	}
	if err := (Trust{Roots: poolOf(c)}).Verify([]*x509.Certificate{c}, Peer{NodeID: "oa-a"}, x509.ExtKeyUsageServerAuth); err != nil {
		t.Errorf("generated certificate does not verify from a truststore: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(certPath))
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestEnsurePeerCertificateIdempotent(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "peer.pem"), filepath.Join(dir, "peer.key")
	spki1, created, err := EnsurePeerCertificate(certPath, keyPath, "oa-a")
	if err != nil || !created {
		t.Fatal(err, created)
	}
	certBytes, _ := os.ReadFile(certPath)
	keyBytes, _ := os.ReadFile(keyPath)

	spki2, created, err := EnsurePeerCertificate(certPath, keyPath, "oa-a")
	if err != nil || created || spki2 != spki1 {
		t.Fatalf("second call: %v created=%v spki %s vs %s", err, created, spki2, spki1)
	}
	certAfter, _ := os.ReadFile(certPath)
	keyAfter, _ := os.ReadFile(keyPath)
	if !bytes.Equal(certBytes, certAfter) || !bytes.Equal(keyBytes, keyAfter) {
		t.Fatal("existing files were rewritten")
	}

	// Only the key left (interrupted run): the new certificate keeps the SPKI pin.
	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}
	spki3, created, err := EnsurePeerCertificate(certPath, keyPath, "oa-a")
	if err != nil || !created || spki3 != spki1 {
		t.Fatalf("key only: %v created=%v spki %s vs %s", err, created, spki3, spki1)
	}
	if after, _ := os.ReadFile(keyPath); !bytes.Equal(after, keyBytes) {
		t.Fatal("key was rewritten")
	}

	// Only the certificate left: never overwritten.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	certBefore, _ := os.ReadFile(certPath)
	if _, _, err := EnsurePeerCertificate(certPath, keyPath, "oa-a"); err == nil {
		t.Fatal("certificate without key accepted")
	}
	if after, _ := os.ReadFile(certPath); !bytes.Equal(after, certBefore) {
		t.Fatal("certificate was overwritten")
	}

	if _, _, err := EnsurePeerCertificate(certPath, keyPath, ""); err == nil {
		t.Fatal("empty NodeId accepted")
	}
	if _, _, err := EnsurePeerCertificate("", keyPath, "oa-a"); err == nil {
		t.Fatal("empty path accepted")
	}
}

func TestEnsurePeerCertificateExistingECKey(t *testing.T) {
	dir := t.TempDir()
	c := nodeCert(t, nil, "old")
	_, keyPath := writeKeyPair(t, dir, "peer", c)
	certPath := filepath.Join(dir, "fresh.pem")
	spki, created, err := EnsurePeerCertificate(certPath, keyPath, "oa-b")
	if err != nil || !created || spki != SPKIFingerprint(c.cert) {
		t.Fatalf("%v created=%v spki %s want %s", err, created, spki, SPKIFingerprint(c.cert))
	}
}

// Two nodes with generated certificates pin each other and complete mTLS in both roles.
func TestGeneratedCertificatesHandshake(t *testing.T) {
	dir := t.TempDir()
	type node struct {
		id   string
		pair *testCert
		pin  Pin
	}
	var nodes []node
	for _, id := range []string{"oa-a", "oa-b"} {
		certPath := ExpandPath(filepath.Join(dir, "peer-{NodeId}.pem"), id)
		keyPath := ExpandPath(filepath.Join(dir, "peer-{NodeId}.key"), id)
		spki, _, err := EnsurePeerCertificate(certPath, keyPath, id)
		if err != nil {
			t.Fatal(err)
		}
		pin, err := ParsePin(spki)
		if err != nil {
			t.Fatal(err)
		}
		pair, err := LoadKeyPair(certPath, keyPath)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node{id: id, pair: &testCert{cert: pair.Leaf, tls: pair}, pin: pin})
	}
	a, b := nodes[0], nodes[1]
	srvCfg := mustServer(t, ServerOptions{Certificate: a.pair.tls, ClientAuth: ClientAuthRequired, Peers: []Peer{{NodeID: b.id, Pins: []Pin{b.pin}}}})
	cliCfg := mustClient(t, ClientOptions{Certificate: &b.pair.tls, Peer: Peer{NodeID: a.id, Pins: []Pin{a.pin}}})
	srv, cli := handshake(t, srvCfg, cliCfg)
	if srv.err != nil || cli.err != nil {
		t.Fatalf("server %v, client %v", srv.err, cli.err)
	}
	if err := (Trust{}).Verify(srv.state.PeerCertificates, Peer{NodeID: b.id, Pins: []Pin{b.pin}}, x509.ExtKeyUsageClientAuth); err != nil {
		t.Fatalf("HELLO binding: %v", err)
	}
}
