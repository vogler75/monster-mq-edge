// Package tlsutil builds the TLS configurations of the PeerLink listener and dialer: PEM key pairs,
// truststores that never fall back to the system roots, certificate pins and the binding of a
// peer certificate to a NodeId (plan-peerlink.md 17.1, 17.2, 17.5).
package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/pkcs12"
)

const (
	ALPNPeer = "mmq-peer/1"
	ALPNHTTP = "http/1.1"

	// NodeURIPrefix starts the URI SAN that names a node: urn:monstermq:node:<NodeId>.
	NodeURIPrefix = "urn:monstermq:node:"

	// NodeIDPlaceholder is replaced by the own NodeId in configured certificate paths.
	NodeIDPlaceholder = "{NodeId}"
)

// ClientAuth is the listener's request for client certificates.
type ClientAuth string

const (
	ClientAuthNone     ClientAuth = "NONE"
	ClientAuthRequest  ClientAuth = "REQUEST"
	ClientAuthRequired ClientAuth = "REQUIRED"
)

// ParseClientAuth accepts NONE, REQUEST and REQUIRED in any case; empty means NONE.
func ParseClientAuth(s string) (ClientAuth, error) {
	switch v := ClientAuth(strings.ToUpper(strings.TrimSpace(s))); v {
	case "":
		return ClientAuthNone, nil
	case ClientAuthNone, ClientAuthRequest, ClientAuthRequired:
		return v, nil
	}
	return "", fmt.Errorf("tlsutil: unknown ClientAuth %q (NONE, REQUEST, REQUIRED)", s)
}

// IdentityFallback names the certificate field accepted as a NodeId when the certificate carries
// no urn:monstermq:node: URI SAN. It is an opt-in for PKIs that cannot issue URI SANs.
type IdentityFallback string

const (
	FallbackNone IdentityFallback = "NONE"
	FallbackDNS  IdentityFallback = "DNS"
	FallbackCN   IdentityFallback = "CN"
)

// ParseIdentityFallback accepts NONE, DNS and CN in any case; empty means NONE.
func ParseIdentityFallback(s string) (IdentityFallback, error) {
	switch v := IdentityFallback(strings.ToUpper(strings.TrimSpace(s))); v {
	case "":
		return FallbackNone, nil
	case FallbackNone, FallbackDNS, FallbackCN:
		return v, nil
	}
	return "", fmt.Errorf("tlsutil: unknown IdentityFallback %q (NONE, DNS, CN)", s)
}

// NodeURI returns the URI SAN that identifies nodeID.
func NodeURI(nodeID string) string { return NodeURIPrefix + nodeID }

// ExpandPath replaces every {NodeId} in path with nodeID, so one config file fits both hosts.
func ExpandPath(path, nodeID string) string {
	return strings.ReplaceAll(path, NodeIDPlaceholder, nodeID)
}

// LoadKeyPair loads a PEM certificate chain and its unencrypted PEM private key. Both paths are
// required; unlike broker.LoadTLS there is no "cert:key" split, which breaks Windows paths.
func LoadKeyPair(certPath, keyPath string) (tls.Certificate, error) {
	if certPath == "" || keyPath == "" {
		return tls.Certificate{}, errors.New("tlsutil: certificate and key paths must both be set")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlsutil: load key pair %s, %s: %w", certPath, keyPath, err)
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return tls.Certificate{}, fmt.Errorf("tlsutil: parse certificate %s: %w", certPath, err)
		}
	}
	return cert, nil
}

// LoadCertPool reads a truststore. storeType is PEM (default) or PKCS12 (also PFX, P12; legacy
// ciphers only, parsed like broker.LoadTLS). An empty path yields an empty pool: the system roots
// are never used, so chain verification then fails unless a pin or a shared secret applies.
func LoadCertPool(path, storeType, password string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if path == "" {
		return pool, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tlsutil: read truststore %s: %w", path, err)
	}
	switch strings.ToUpper(strings.TrimSpace(storeType)) {
	case "", "PEM":
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("tlsutil: no PEM certificates in truststore %s", path)
		}
	case "PKCS12", "PFX", "P12":
		// golang.org/x/crypto/pkcs12 decodes only files holding exactly one key and its
		// certificates; certificate-only PKCS12 truststores are rejected by it.
		blocks, err := pkcs12.ToPEM(data, password)
		if err != nil {
			return nil, fmt.Errorf("tlsutil: parse pkcs12 truststore %s: %w", path, err)
		}
		// The blocks carry bag attributes as PEM headers, which AppendCertsFromPEM skips, so the
		// certificates are added directly.
		n := 0
		for _, b := range blocks {
			if b.Type != "CERTIFICATE" {
				continue
			}
			c, err := x509.ParseCertificate(b.Bytes)
			if err != nil {
				return nil, fmt.Errorf("tlsutil: pkcs12 truststore %s: %w", path, err)
			}
			pool.AddCert(c)
			n++
		}
		if n == 0 {
			return nil, fmt.Errorf("tlsutil: no certificates in pkcs12 truststore %s", path)
		}
	default:
		return nil, fmt.Errorf("tlsutil: unknown truststore type %q (PEM, PKCS12)", storeType)
	}
	return pool, nil
}

func minVersion(sharedSecret bool) uint16 {
	// The shared-secret MAC binds to ExportKeyingMaterial, which needs TLS 1.3 (or 1.2 with EMS).
	if sharedSecret {
		return tls.VersionTLS13
	}
	return tls.VersionTLS12
}

// ServerOptions configures the peer listener.
type ServerOptions struct {
	// Certificate is this node's identity (ServerAuth and ClientAuth EKU).
	Certificate tls.Certificate
	Trust       Trust
	ClientAuth  ClientAuth
	// Peers are the nodes allowed to pull from this node (Serve: true). A presented client
	// certificate must authenticate one of them.
	Peers []Peer
	// SharedSecret raises MinVersion to TLS 1.3 for the exporter-bound MAC.
	SharedSecret bool
}

// ServerConfig returns the listener configuration. A presented client certificate is accepted
// only when it authenticates one of opts.Peers (chain or pin, ClientAuth EKU, identity). After
// HELLO the session must still check the claimed consumer with Trust.Verify.
func ServerConfig(opts ServerOptions) (*tls.Config, error) {
	if len(opts.Certificate.Certificate) == 0 {
		return nil, errors.New("tlsutil: listener needs a certificate")
	}
	auth, err := ParseClientAuth(string(opts.ClientAuth))
	if err != nil {
		return nil, err
	}
	anyPins, allPinned := false, true
	for _, p := range opts.Peers {
		if len(p.Pins) > 0 {
			anyPins = true
		} else {
			allPinned = false
		}
	}
	if auth != ClientAuthNone && !opts.Trust.hasRoots() && !allPinned {
		return nil, fmt.Errorf("tlsutil: ClientAuth %s needs a truststore or pins for every peer", auth)
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{opts.Certificate},
		MinVersion:   minVersion(opts.SharedSecret),
		NextProtos:   []string{ALPNPeer, ALPNHTTP},
	}
	// With pins, a pinned self-signed certificate does not chain to ClientCAs, so the stack must
	// not verify it, and ClientCAs stays nil so that clients are not told to send only
	// certificates issued by the truststore. VerifyConnection checks every certificate either way.
	switch {
	case auth == ClientAuthNone:
		cfg.ClientAuth = tls.NoClientCert
	case anyPins && auth == ClientAuthRequest:
		cfg.ClientAuth = tls.RequestClientCert
	case anyPins:
		cfg.ClientAuth = tls.RequireAnyClientCert
	case auth == ClientAuthRequest:
		cfg.ClientAuth = tls.VerifyClientCertIfGiven
		cfg.ClientCAs = opts.Trust.roots()
	default:
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = opts.Trust.roots()
	}

	trust := opts.Trust
	peers := append([]Peer(nil), opts.Peers...)
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			if auth == ClientAuthRequired {
				return ErrNoCertificate
			}
			return nil
		}
		_, err := trust.FindPeer(cs.PeerCertificates, peers, x509.ExtKeyUsageClientAuth)
		return err
	}
	return cfg, nil
}

// ClientOptions configures the dialer towards one source.
type ClientOptions struct {
	// Certificate is sent when the listener requests a client certificate. Optional.
	Certificate *tls.Certificate
	Trust       Trust
	// Peer is the source being dialed; its NodeId is bound to the server certificate.
	Peer Peer
	// ServerName is sent as SNI only; it is never used for hostname verification.
	ServerName string
	// SharedSecret raises MinVersion to TLS 1.3. The exporter-bound MAC then authenticates the
	// source, so the server certificate is enforced only when Peer.Pins is set: the node
	// truststore need not cover a peer that authenticates by secret. Trust.Verify on the
	// connection state tells afterwards whether the certificate verified.
	SharedSecret bool
	// InsecureSkipVerify disables every certificate check; the direction then counts as
	// unauthenticated unless a shared secret applies.
	InsecureSkipVerify bool
	// NextProtos defaults to [ALPNPeer].
	NextProtos []string
}

// ErrNoTrust is returned by ClientConfig for a source that nothing can authenticate.
var ErrNoTrust = errors.New("tlsutil: no truststore, pin or shared secret to authenticate the peer")

// ClientConfig returns the dialer configuration. It sets InsecureSkipVerify, which only turns off
// the stack's hostname verification; all checks run in VerifyConnection, which is never nil and
// also runs on resumed sessions.
func ClientConfig(opts ClientOptions) (*tls.Config, error) {
	if opts.Peer.NodeID == "" && opts.Peer.CertificateIdentity == "" && !opts.InsecureSkipVerify {
		return nil, errors.New("tlsutil: dialer needs the source NodeId")
	}
	if !opts.InsecureSkipVerify && !opts.SharedSecret && len(opts.Peer.Pins) == 0 && !opts.Trust.hasRoots() {
		return nil, ErrNoTrust
	}
	protos := opts.NextProtos
	if len(protos) == 0 {
		protos = []string{ALPNPeer}
	}
	cfg := &tls.Config{
		MinVersion:         minVersion(opts.SharedSecret),
		NextProtos:         append([]string(nil), protos...),
		ServerName:         opts.ServerName,
		InsecureSkipVerify: true,
		RootCAs:            opts.Trust.roots(),
	}
	if opts.Certificate != nil {
		cfg.Certificates = []tls.Certificate{*opts.Certificate}
	}

	trust := opts.Trust
	peer := opts.Peer
	peer.Pins = append([]Pin(nil), opts.Peer.Pins...)
	insecure := opts.InsecureSkipVerify
	optional := opts.SharedSecret && len(peer.Pins) == 0
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if insecure {
			return nil
		}
		err := trust.Verify(cs.PeerCertificates, peer, x509.ExtKeyUsageServerAuth)
		if err != nil && optional {
			return nil
		}
		return err
	}
	return cfg, nil
}
