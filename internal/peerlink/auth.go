package peerlink

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/tlsutil"
)

// tlsMaterial is this node's TLS identity and trust (plan 17.1, 17.2), loaded once in New.
type tlsMaterial struct {
	cert       *tls.Certificate
	trust      tlsutil.Trust
	clientAuth tlsutil.ClientAuth
	server     *tls.Config // listener TLS; nil when the listener is plaintext
}

// loadTLS loads the key pair, truststore and identity rules, and builds the listener config.
// servePeers are the peers allowed to pull from this node; needDialer tells whether any dialer
// uses TLS (a client certificate is then offered when one is configured).
func loadTLS(cfg *config.PeerLinkConfig, nodeID string, servePeers []*consumerSlot, listenerTLS, needDialer bool, logger *slog.Logger) (*tlsMaterial, error) {
	tm := &tlsMaterial{}
	if !listenerTLS && !needDialer {
		return tm, nil
	}
	t := cfg.Tls
	certPath := tlsutil.ExpandPath(t.GetCertPath(), nodeID)
	keyPath := tlsutil.ExpandPath(t.GetKeyPath(), nodeID)
	if certPath != "" && keyPath != "" {
		if t.AutoGenerate {
			spki, created, err := tlsutil.EnsurePeerCertificate(certPath, keyPath, nodeID)
			if err != nil {
				return nil, fmt.Errorf("peerlink: generate peer certificate: %w", err)
			}
			if created {
				logger.Info("peerlink: generated self-signed peer certificate", "cert", certPath, "spkiSha256", spki)
			} else {
				logger.Info("peerlink: peer certificate", "cert", certPath, "spkiSha256", spki)
			}
		}
		kp, err := tlsutil.LoadKeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("peerlink: load peer key pair: %w", err)
		}
		if kp.Leaf != nil && !t.AutoGenerate {
			logger.Info("peerlink: peer certificate", "cert", certPath, "spkiSha256", tlsutil.SPKIFingerprint(kp.Leaf))
		}
		tm.cert = &kp
	} else if listenerTLS {
		return nil, fmt.Errorf("peerlink: listener TLS needs Tls.CertPath and Tls.KeyPath or Tls.AutoGenerate")
	}

	pool, err := tlsutil.LoadCertPool(tlsutil.ExpandPath(t.TrustStorePath, nodeID), t.GetTrustStoreType(), t.TrustStorePassword)
	if err != nil {
		return nil, fmt.Errorf("peerlink: load peer truststore: %w", err)
	}
	fb, err := tlsutil.ParseIdentityFallback(t.GetIdentityFallback())
	if err != nil {
		return nil, fmt.Errorf("peerlink: %w", err)
	}
	tm.trust = tlsutil.Trust{Roots: pool, Fallback: fb}
	ca, err := tlsutil.ParseClientAuth(string(t.GetClientAuth()))
	if err != nil {
		return nil, fmt.Errorf("peerlink: %w", err)
	}
	tm.clientAuth = ca

	if listenerTLS {
		opts := tlsutil.ServerOptions{Certificate: *tm.cert, Trust: tm.trust, ClientAuth: ca}
		for _, s := range servePeers {
			opts.Peers = append(opts.Peers, s.tlsPeer)
			if len(s.secrets) > 0 {
				opts.SharedSecret = true
			}
		}
		sc, err := tlsutil.ServerConfig(opts)
		if err != nil {
			return nil, fmt.Errorf("peerlink: listener TLS: %w", err)
		}
		tm.server = sc
	}
	return tm, nil
}

// clientConfig builds the dialer TLS config towards one source.
func (tm *tlsMaterial) clientConfig(p *puller) (*tls.Config, error) {
	sni := p.peer.Tls.ServerName
	if sni == "" {
		if host, _, err := net.SplitHostPort(p.address); err == nil && net.ParseIP(host) == nil {
			sni = host
		}
	}
	cc, err := tlsutil.ClientConfig(tlsutil.ClientOptions{
		Certificate:        tm.cert,
		Trust:              tm.trust,
		Peer:               p.tlsPeer,
		ServerName:         sni,
		SharedSecret:       len(p.secrets) > 0,
		InsecureSkipVerify: p.peer.Tls.InsecureSkipVerify,
	})
	if err != nil {
		return nil, fmt.Errorf("peerlink: dialer TLS for %s: %w", p.nodeID, err)
	}
	return cc, nil
}

func tlsPeerOf(peer config.PeerConfig) (tlsutil.Peer, error) {
	pins, err := tlsutil.ParsePins(peer.Tls.PinnedSha256)
	if err != nil {
		return tlsutil.Peer{}, fmt.Errorf("peerlink: peer %s: %w", peer.NodeID, err)
	}
	return tlsutil.Peer{NodeID: peer.NodeID, CertificateIdentity: peer.Tls.CertificateIdentity, Pins: pins}, nil
}
