package tlsutil

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// EnsurePeerCertificate makes certPath and keyPath hold a peer identity for nodeID and returns the
// SPKI SHA-256 (hex) of the certificate, which the caller logs at startup so it can be configured
// as a pin on the other nodes. created reports whether a file was written.
//
//   - Both files exist: they are loaded and left unchanged.
//   - Neither exists: a new ECDSA P-256 key and a self-signed certificate are written.
//   - Only the key exists (e.g. an interrupted earlier run): a certificate is created for it, so
//     the SPKI pin stays the same.
//   - Only the certificate exists: error, the certificate is never overwritten.
//
// The certificate has CN=nodeID, the URI SAN urn:monstermq:node:<nodeID>, the ServerAuth and
// ClientAuth extended key usages and a validity of 10 years.
func EnsurePeerCertificate(certPath, keyPath, nodeID string) (spki string, created bool, err error) {
	if certPath == "" || keyPath == "" {
		return "", false, errors.New("tlsutil: certificate and key paths must both be set")
	}
	if nodeID == "" {
		return "", false, errors.New("tlsutil: NodeId must not be empty")
	}
	certExists, err := fileExists(certPath)
	if err != nil {
		return "", false, err
	}
	keyExists, err := fileExists(keyPath)
	if err != nil {
		return "", false, err
	}

	switch {
	case certExists && keyExists:
		pair, err := LoadKeyPair(certPath, keyPath)
		if err != nil {
			return "", false, err
		}
		return SPKIFingerprint(pair.Leaf), false, nil
	case certExists:
		return "", false, fmt.Errorf("tlsutil: certificate %s exists but key %s is missing; refusing to overwrite it", certPath, keyPath)
	}

	var key crypto.Signer
	if keyExists {
		if key, err = readPrivateKey(keyPath); err != nil {
			return "", false, err
		}
	} else {
		ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return "", false, fmt.Errorf("tlsutil: generate key: %w", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(ec)
		if err != nil {
			return "", false, fmt.Errorf("tlsutil: marshal key: %w", err)
		}
		if err := writeFileAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return "", false, err
		}
		key = ec
	}

	der, err := selfSignedPeerCert(key, nodeID)
	if err != nil {
		return "", false, err
	}
	if err := writeFileAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", false, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", false, fmt.Errorf("tlsutil: parse generated certificate: %w", err)
	}
	return SPKIFingerprint(cert), true, nil
}

func selfSignedPeerCert(key crypto.Signer, nodeID string) ([]byte, error) {
	uri, err := url.Parse(NodeURI(nodeID))
	if err != nil {
		return nil, fmt.Errorf("tlsutil: NodeId %q does not form a URI SAN: %w", nodeID, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("tlsutil: serial number: %w", err)
	}
	usage := x509.KeyUsageDigitalSignature
	if _, ok := key.Public().(*rsa.PublicKey); ok {
		usage |= x509.KeyUsageKeyEncipherment
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: nodeID, Organization: []string{"MonsterMQ"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              usage,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{uri},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("tlsutil: create certificate: %w", err)
	}
	return der, nil
}

func readPrivateKey(path string) (crypto.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tlsutil: read key %s: %w", path, err)
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("tlsutil: no PEM private key in %s", path)
		}
		var key any
		switch block.Type {
		case "PRIVATE KEY":
			key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		case "EC PRIVATE KEY":
			key, err = x509.ParseECPrivateKey(block.Bytes)
		case "RSA PRIVATE KEY":
			key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		default:
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("tlsutil: parse key %s: %w", path, err)
		}
		if signer, ok := key.(crypto.Signer); ok {
			return signer, nil
		}
		return nil, fmt.Errorf("tlsutil: unsupported key type %T in %s", key, path)
	}
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("tlsutil: %w", err)
}

// writeFileAtomic writes through a temporary file and a rename, so a crash never leaves a
// truncated key or certificate behind.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("tlsutil: create directory %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("tlsutil: write %s: %w", path, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(perm); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		f.Close()
		return fmt.Errorf("tlsutil: write %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("tlsutil: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("tlsutil: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("tlsutil: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("tlsutil: write %s: %w", path, err)
	}
	return nil
}
