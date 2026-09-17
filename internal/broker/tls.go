package broker

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// EnsureCertificate checks if the cert and key files exist; if either is missing,
// it automatically generates a self-signed ECDSA certificate valid for 10 years
// with SANs for localhost, the machine hostname, and all local IP addresses.
func EnsureCertificate(certPath, keyPath string, logger *slog.Logger) error {
	if certPath == "" || keyPath == "" {
		return fmt.Errorf("certificate and key paths must not be empty")
	}

	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	if certErr == nil && keyErr == nil {
		return nil
	}

	if logger != nil {
		logger.Info("generating self-signed TLS certificate", "cert", certPath, "key", keyPath)
	}

	if err := os.MkdirAll(filepath.Dir(certPath), 0755); err != nil {
		return fmt.Errorf("create cert directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0755); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}

	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate ECDSA private key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return fmt.Errorf("generate serial number: %w", err)
	}

	hostname, _ := os.Hostname()
	dnsNames := []string{"localhost"}
	if hostname != "" && hostname != "localhost" {
		dnsNames = append(dnsNames, hostname)
	}

	ipAddresses := []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
				if ipNet.IP.To4() != nil || ipNet.IP.To16() != nil {
					ipAddresses = append(ipAddresses, ipNet.IP)
				}
			}
		}
	}

	commonName := "MonsterMQ Edge"
	if hostname != "" {
		commonName = hostname
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   commonName,
			Organization: []string{"MonsterMQ"},
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0), // 10 years
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privKey.PublicKey, privKey)
	if err != nil {
		return fmt.Errorf("create self-signed certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return fmt.Errorf("write certificate file: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return fmt.Errorf("marshal EC private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("write private key file: %w", err)
	}

	if logger != nil {
		logger.Info("self-signed TLS certificate ready", "cert", certPath, "key", keyPath, "validity", "10 years")
	}

	return nil
}

// LoadTLS reads certificates and creates a *tls.Config supporting server TLS
// and mutual TLS (mTLS) client certificate verification.
func LoadTLS(params TLSParams) (*tls.Config, error) {
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

// loadTLS is an internal alias for LoadTLS.
func loadTLS(params TLSParams) (*tls.Config, error) {
	return LoadTLS(params)
}
