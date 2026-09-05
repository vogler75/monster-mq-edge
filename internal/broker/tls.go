package broker

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/pkcs12"
	"monstermq.io/edge/internal/config"
)

type TLSParams struct {
	CertPath           string
	KeyPath            string
	Password           string
	ClientAuth         config.ClientAuthType
	TrustStorePath     string
	TrustStorePassword string
	TrustStoreType     string
}

// loadTLS reads certificates and creates a *tls.Config supporting server TLS
// and mutual TLS (mTLS) client certificate verification.
func loadTLS(params TLSParams) (*tls.Config, error) {
	if params.CertPath == "" {
		return nil, fmt.Errorf("KeyStorePath is empty")
	}
	certPath := params.CertPath
	keyPath := params.KeyPath
	if keyPath == "" {
		if i := strings.Index(certPath, ":"); i > 0 {
			keyPath = certPath[i+1:]
			certPath = certPath[:i]
		} else {
			keyPath = certPath
		}
	}
	if _, err := os.Stat(certPath); err != nil {
		return nil, fmt.Errorf("cert %s: %w", certPath, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		return nil, fmt.Errorf("key %s: %w", keyPath, err)
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load keypair: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	switch params.ClientAuth {
	case config.ClientAuthRequest:
		tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
	case config.ClientAuthRequired:
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	default:
		tlsConfig.ClientAuth = tls.NoClientCert
	}

	if tlsConfig.ClientAuth != tls.NoClientCert && params.TrustStorePath != "" {
		caData, err := os.ReadFile(params.TrustStorePath)
		if err != nil {
			return nil, fmt.Errorf("read truststore %s: %w", params.TrustStorePath, err)
		}
		certPool := x509.NewCertPool()
		trustType := strings.ToUpper(strings.TrimSpace(params.TrustStoreType))
		if trustType == "PKCS12" || trustType == "PFX" || trustType == "P12" {
			blocks, err := pkcs12.ToPEM(caData, params.TrustStorePassword)
			if err != nil {
				return nil, fmt.Errorf("parse pkcs12 truststore %s: %w", params.TrustStorePath, err)
			}
			var pemData []byte
			for _, b := range blocks {
				if b.Type == "CERTIFICATE" {
					pemData = append(pemData, pem.EncodeToMemory(b)...)
				}
			}
			if !certPool.AppendCertsFromPEM(pemData) {
				return nil, fmt.Errorf("no certificates found in pkcs12 truststore %s", params.TrustStorePath)
			}
		} else {
			if !certPool.AppendCertsFromPEM(caData) {
				return nil, fmt.Errorf("failed to parse CA certificates from %s", params.TrustStorePath)
			}
		}
		tlsConfig.ClientCAs = certPool
	}

	return tlsConfig, nil
}
