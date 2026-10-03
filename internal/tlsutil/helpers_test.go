package tlsutil

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testCert struct {
	cert  *x509.Certificate
	key   *ecdsa.PrivateKey
	chain []*x509.Certificate // leaf first, without the root
	tls   tls.Certificate
}

type certSpec struct {
	cn        string
	uris      []string
	dns       []string
	eku       []x509.ExtKeyUsage // nil: ServerAuth and ClientAuth
	noEKU     bool
	ca        bool
	parent    *testCert // nil: self-signed
	notBefore time.Time
	notAfter  time.Time
}

var serial int64

func newCert(t *testing.T, s certSpec) *testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial++
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: s.cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		DNSNames:              s.dns,
	}
	if !s.notBefore.IsZero() {
		tmpl.NotBefore, tmpl.NotAfter = s.notBefore, s.notAfter
	}
	for _, u := range s.uris {
		pu, err := url.Parse(u)
		if err != nil {
			t.Fatal(err)
		}
		tmpl.URIs = append(tmpl.URIs, pu)
	}
	switch {
	case s.ca:
		tmpl.IsCA = true
		tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	case s.noEKU:
	case s.eku != nil:
		tmpl.ExtKeyUsage = s.eku
	default:
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}
	}
	parent, signer := tmpl, key
	if s.parent != nil {
		parent, signer = s.parent.cert, s.parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	tc := &testCert{cert: c, key: key, chain: []*x509.Certificate{c}}
	if s.parent != nil && s.parent.cert.Subject.String() != s.parent.cert.Issuer.String() {
		tc.chain = append(tc.chain, s.parent.chain...)
	}
	for _, cc := range tc.chain {
		tc.tls.Certificate = append(tc.tls.Certificate, cc.Raw)
	}
	tc.tls.PrivateKey = key
	tc.tls.Leaf = c
	return tc
}

func newCA(t *testing.T, cn string) *testCert {
	return newCert(t, certSpec{cn: cn, ca: true})
}

// nodeCert issues a peer certificate with the URI SAN of nodeID (CN = nodeID) under ca, or
// self-signed when ca is nil.
func nodeCert(t *testing.T, ca *testCert, nodeID string, eku ...x509.ExtKeyUsage) *testCert {
	return newCert(t, certSpec{cn: nodeID, uris: []string{NodeURI(nodeID)}, parent: ca, eku: eku})
}

func poolOf(certs ...*x509.Certificate) *x509.CertPool {
	p := x509.NewCertPool()
	for _, c := range certs {
		p.AddCert(c)
	}
	return p
}

func writeKeyPair(t *testing.T, dir, name string, c *testCert) (certPath, keyPath string) {
	t.Helper()
	certPath = filepath.Join(dir, name+".pem")
	keyPath = filepath.Join(dir, name+".key")
	var certPEM []byte
	for _, der := range c.tls.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalECPrivateKey(c.key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

type side struct {
	state tls.ConnectionState
	err   error
}

// handshake runs one TLS handshake over loopback TCP. The server writes one byte after a
// successful handshake and the client reads it, so a client certificate rejected by a TLS 1.3
// server (after the client finished its part) also shows up as a client error.
func handshake(t *testing.T, srvCfg, cliCfg *tls.Config) (srv, cli side) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srvDone := make(chan side, 1)
	cliDone := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvDone <- side{err: err}
			return
		}
		defer conn.Close()
		tc := tls.Server(conn, srvCfg)
		err = tc.HandshakeContext(ctx)
		if err == nil {
			_, err = tc.Write([]byte{'x'})
		}
		srvDone <- side{state: tc.ConnectionState(), err: err}
		if err == nil {
			select {
			case <-cliDone:
			case <-ctx.Done():
			}
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(conn, cliCfg)
	err = tc.HandshakeContext(ctx)
	if err == nil {
		_ = tc.SetReadDeadline(time.Now().Add(5 * time.Second))
		var b [1]byte
		_, err = io.ReadFull(tc, b[:])
	}
	cli = side{state: tc.ConnectionState(), err: err}
	close(cliDone)
	conn.Close()
	srv = <-srvDone
	return srv, cli
}

func mustServer(t *testing.T, o ServerOptions) *tls.Config {
	t.Helper()
	cfg, err := ServerConfig(o)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	return cfg
}

func mustClient(t *testing.T, o ClientOptions) *tls.Config {
	t.Helper()
	cfg, err := ClientConfig(o)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}
	return cfg
}
