package tlsutil

import (
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrNoCertificate    = errors.New("tlsutil: no peer certificate")
	ErrUntrusted        = errors.New("tlsutil: certificate not trusted")
	ErrKeyUsage         = errors.New("tlsutil: certificate lacks the required extended key usage")
	ErrPinMismatch      = errors.New("tlsutil: certificate matches no pin")
	ErrIdentityMismatch = errors.New("tlsutil: certificate identity mismatch")
)

// Peer says how a remote node is identified and, optionally, pinned.
type Peer struct {
	// NodeID is the canonical NodeId. By default the only accepted identity is the URI SAN
	// urn:monstermq:node:<NodeID>.
	NodeID string
	// CertificateIdentity, when set, replaces the default identity: an exact URI SAN or a DNS SAN.
	CertificateIdentity string
	// Pins replace chain verification; any match (SPKI or certificate SHA-256) is accepted.
	Pins []Pin
}

// Trust holds the node-wide trust settings.
type Trust struct {
	// Roots is the peer truststore. Nil means an empty pool, never the system roots.
	Roots    *x509.CertPool
	Fallback IdentityFallback
}

func (t Trust) roots() *x509.CertPool {
	if t.Roots == nil {
		return x509.NewCertPool()
	}
	return t.Roots
}

func (t Trust) hasRoots() bool {
	return t.Roots != nil && !t.Roots.Equal(x509.NewCertPool())
}

// Verify checks a peer certificate chain (leaf first) for peer p: a pin match, or a chain to
// Roots, with the given extended key usage; then the identity. No hostname is checked.
func (t Trust) Verify(chain []*x509.Certificate, p Peer, usage x509.ExtKeyUsage) error {
	if len(chain) == 0 {
		return ErrNoCertificate
	}
	if err := t.verifyTrust(chain, p, usage); err != nil {
		return err
	}
	return t.MatchIdentity(chain[0], p)
}

// FindPeer returns the first of peers that the chain authenticates (see Verify).
func (t Trust) FindPeer(chain []*x509.Certificate, peers []Peer, usage x509.ExtKeyUsage) (Peer, error) {
	if len(chain) == 0 {
		return Peer{}, ErrNoCertificate
	}
	var trustErr error
	for _, p := range peers {
		if t.MatchIdentity(chain[0], p) != nil {
			continue
		}
		err := t.verifyTrust(chain, p, usage)
		if err == nil {
			return p, nil
		}
		if trustErr == nil {
			trustErr = err
		}
	}
	if trustErr != nil {
		return Peer{}, trustErr
	}
	return Peer{}, fmt.Errorf("%w: %s matches no configured peer", ErrIdentityMismatch, describe(chain[0]))
}

func (t Trust) verifyTrust(chain []*x509.Certificate, p Peer, usage x509.ExtKeyUsage) error {
	leaf := chain[0]
	if len(p.Pins) > 0 {
		if !MatchPins(leaf, p.Pins) {
			return fmt.Errorf("%w: spki %s", ErrPinMismatch, SPKIFingerprint(leaf))
		}
		if !hasUsage(leaf, usage) {
			return ErrKeyUsage
		}
		return nil
	}
	opts := x509.VerifyOptions{
		Roots:         t.roots(),
		Intermediates: x509.NewCertPool(),
		KeyUsages:     []x509.ExtKeyUsage{usage},
	}
	for _, c := range chain[1:] {
		opts.Intermediates.AddCert(c)
	}
	if _, err := leaf.Verify(opts); err != nil {
		var invalid x509.CertificateInvalidError
		if errors.As(err, &invalid) && invalid.Reason == x509.IncompatibleUsage {
			return fmt.Errorf("%w: %w", ErrKeyUsage, err)
		}
		return fmt.Errorf("%w: %w", ErrUntrusted, err)
	}
	return nil
}

// hasUsage mirrors crypto/x509: a certificate without extended key usages is valid for any.
func hasUsage(c *x509.Certificate, usage x509.ExtKeyUsage) bool {
	if len(c.ExtKeyUsage) == 0 && len(c.UnknownExtKeyUsage) == 0 {
		return true
	}
	for _, u := range c.ExtKeyUsage {
		if u == usage || u == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

// MatchIdentity checks that leaf names peer p: the per-peer CertificateIdentity (exact URI SAN or
// DNS SAN) when set, otherwise the URI SAN urn:monstermq:node:<NodeID>. Only a certificate without
// any urn:monstermq:node: URI SAN may match through the configured fallback (DNS SAN or CN equal to
// the NodeId), so a certificate that names another node is never accepted through its CN.
func (t Trust) MatchIdentity(leaf *x509.Certificate, p Peer) error {
	if p.CertificateIdentity != "" {
		for _, u := range leaf.URIs {
			if u.String() == p.CertificateIdentity {
				return nil
			}
		}
		for _, d := range leaf.DNSNames {
			if strings.EqualFold(d, p.CertificateIdentity) {
				return nil
			}
		}
		return fmt.Errorf("%w: want %s, have %s", ErrIdentityMismatch, p.CertificateIdentity, describe(leaf))
	}
	if p.NodeID == "" {
		return fmt.Errorf("%w: no NodeId to bind %s to", ErrIdentityMismatch, describe(leaf))
	}
	want := NodeURI(p.NodeID)
	hasNodeURI := false
	for _, u := range leaf.URIs {
		s := u.String()
		if strings.EqualFold(s, want) {
			return nil
		}
		if len(s) >= len(NodeURIPrefix) && strings.EqualFold(s[:len(NodeURIPrefix)], NodeURIPrefix) {
			hasNodeURI = true
		}
	}
	if !hasNodeURI {
		switch t.Fallback {
		case FallbackDNS:
			for _, d := range leaf.DNSNames {
				if strings.EqualFold(d, p.NodeID) {
					return nil
				}
			}
		case FallbackCN:
			if strings.EqualFold(leaf.Subject.CommonName, p.NodeID) {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: want %s, have %s", ErrIdentityMismatch, want, describe(leaf))
}

func describe(c *x509.Certificate) string {
	var b strings.Builder
	b.WriteString("CN=")
	b.WriteString(c.Subject.CommonName)
	for _, u := range c.URIs {
		b.WriteString(" URI:")
		b.WriteString(u.String())
	}
	for _, d := range c.DNSNames {
		b.WriteString(" DNS:")
		b.WriteString(d)
	}
	return b.String()
}
