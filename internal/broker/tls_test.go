package broker

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCertificate(t *testing.T) {
	tempDir := t.TempDir()
	certPath := filepath.Join(tempDir, "certs", "server.crt")
	keyPath := filepath.Join(tempDir, "certs", "server.key")

	// 1. First run generates certificate and private key
	if err := EnsureCertificate(certPath, keyPath, nil); err != nil {
		t.Fatalf("EnsureCertificate failed: %v", err)
	}

	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat certPath: %v", err)
	}
	if certInfo.Size() == 0 {
		t.Fatal("certPath is empty")
	}

	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat keyPath: %v", err)
	}
	if keyInfo.Size() == 0 {
		t.Fatal("keyPath is empty")
	}

	// Verify key has 0600 permissions
	if mode := keyInfo.Mode().Perm(); mode != 0600 {
		t.Fatalf("expected 0600 permissions on key, got %v", mode)
	}

	// 2. Load and verify certificate parses properly
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	parsed, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	foundLocalhost := false
	for _, dns := range parsed.DNSNames {
		if dns == "localhost" {
			foundLocalhost = true
			break
		}
	}
	if !foundLocalhost {
		t.Fatalf("expected localhost in DNSNames, got %v", parsed.DNSNames)
	}

	// 3. Second run should be a no-op (files exist, modification time unchanged)
	modTime := certInfo.ModTime()
	if err := EnsureCertificate(certPath, keyPath, nil); err != nil {
		t.Fatalf("EnsureCertificate second run failed: %v", err)
	}
	newCertInfo, _ := os.Stat(certPath)
	if !newCertInfo.ModTime().Equal(modTime) {
		t.Fatal("EnsureCertificate regenerated existing certificate files")
	}
}
