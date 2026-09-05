package integration

import (
	"context"
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
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
)

type testCerts struct {
	caPath        string
	caCertPool    *x509.CertPool
	serverCert    string
	serverKey     string
	clientCert    string
	clientKey     string
	clientCertCN  string
	unknownCert   string
	unknownKey    string
	unknownCertCN string
	blankCert     string
	blankKey      string
}

func createTestCertificates(t *testing.T) testCerts {
	t.Helper()
	dir := t.TempDir()

	// 1. CA
	caPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "Test MonsterMQ CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caBytes, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caPriv.PublicKey, caPriv)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caBytes})
	if err := os.WriteFile(caPath, caPEM, 0644); err != nil {
		t.Fatalf("write ca.crt: %v", err)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caPEM)

	// 2. Server cert
	serverPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	serverTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		NotBefore:   time.Now().Add(-1 * time.Hour),
		NotAfter:    time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverBytes, err := x509.CreateCertificate(rand.Reader, serverTmpl, caTmpl, &serverPriv.PublicKey, caPriv)
	if err != nil {
		t.Fatalf("create server cert: %v", err)
	}
	serverCertPath := filepath.Join(dir, "server.crt")
	serverKeyPath := filepath.Join(dir, "server.key")
	if err := os.WriteFile(serverCertPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverBytes}), 0644); err != nil {
		t.Fatalf("write server.crt: %v", err)
	}
	serverKeyBytes, err := x509.MarshalECPrivateKey(serverPriv)
	if err != nil {
		t.Fatalf("marshal server key: %v", err)
	}
	if err := os.WriteFile(serverKeyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyBytes}), 0600); err != nil {
		t.Fatalf("write server.key: %v", err)
	}

	createClientCert := func(cn string, certFileName, keyFileName string) (string, string) {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate client key: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject: pkix.Name{
				CommonName: cn,
			},
			NotBefore:   time.Now().Add(-1 * time.Hour),
			NotAfter:    time.Now().Add(24 * time.Hour),
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		certDer, err := x509.CreateCertificate(rand.Reader, tmpl, caTmpl, &priv.PublicKey, caPriv)
		if err != nil {
			t.Fatalf("create client cert: %v", err)
		}
		cPath := filepath.Join(dir, certFileName)
		kPath := filepath.Join(dir, keyFileName)
		if err := os.WriteFile(cPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDer}), 0644); err != nil {
			t.Fatalf("write client cert: %v", err)
		}
		kb, err := x509.MarshalECPrivateKey(priv)
		if err != nil {
			t.Fatalf("marshal client key: %v", err)
		}
		if err := os.WriteFile(kPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0600); err != nil {
			t.Fatalf("write client key: %v", err)
		}
		return cPath, kPath
	}

	clientCert, clientKey := createClientCert("cert-client-user", "client.crt", "client.key")
	unknownCert, unknownKey := createClientCert("cert-unknown-user", "unknown.crt", "unknown.key")
	blankCert, blankKey := createClientCert("   ", "blank.crt", "blank.key")

	return testCerts{
		caPath:        caPath,
		caCertPool:    caPool,
		serverCert:    serverCertPath,
		serverKey:     serverKeyPath,
		clientCert:    clientCert,
		clientKey:     clientKey,
		clientCertCN:  "cert-client-user",
		unknownCert:   unknownCert,
		unknownKey:    unknownKey,
		unknownCertCN: "cert-unknown-user",
		blankCert:     blankCert,
		blankKey:      blankKey,
	}
}

func startMTLSBroker(t *testing.T, port int, certs testCerts, cfgFns ...func(*config.Config)) (*broker.Server, *stores.Storage) {
	t.Helper()
	cfg := config.Default()
	cfg.NodeID = fmt.Sprintf("mtls-%d", port)
	cfg.TCP.Enabled = false
	cfg.WS.Enabled = false
	cfg.TCPS.Enabled = true
	cfg.TCPS.Port = port
	cfg.TCPS.KeyStorePath = certs.serverCert
	cfg.SSL.KeyPath = certs.serverKey
	cfg.TCPS.ClientAuth = config.ClientAuthRequest
	cfg.TCPS.TrustStorePath = certs.caPath
	cfg.TCPS.TrustStoreType = "PEM"
	useIdent := true
	cfg.TCPS.UseIdentityAsUsername = &useIdent
	autoCreate := false
	cfg.TCPS.AutoCreateUser = &autoCreate

	cfg.UserManagement.Enabled = true
	cfg.UserManagement.AnonymousEnabled = false
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "mtls.db")

	for _, fn := range cfgFns {
		fn(cfg)
	}

	srv, err := broker.New(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("new broker: %v", err)
	}
	go func() { _ = srv.Serve() }()
	time.Sleep(100 * time.Millisecond)
	return srv, srv.Storage()
}

func mtlsClientOptions(port int, clientID string, certs testCerts, certFile, keyFile string) *mqtt.ClientOptions {
	o := mqtt.NewClientOptions()
	o.AddBroker(fmt.Sprintf("ssl://localhost:%d", port))
	o.SetClientID(clientID)
	o.SetConnectTimeout(3 * time.Second)
	o.SetCleanSession(true)

	tlsCfg := &tls.Config{
		RootCAs: certs.caCertPool,
	}
	if certFile != "" && keyFile != "" {
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			panic(fmt.Sprintf("load client keypair: %v", err))
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}
	o.SetTLSConfig(tlsCfg)
	return o
}

func TestMTLS_ConnectWithClientCertificate(t *testing.T) {
	certs := createTestCertificates(t)
	port := 28883

	srv, storage := startMTLSBroker(t, port, certs)
	defer srv.Close()

	// Provision account for certificate Common Name
	hash, _ := storesqlite.HashPassword("secret")
	err := storage.Users.CreateUser(context.Background(), stores.User{
		Username:     certs.clientCertCN,
		PasswordHash: hash,
		Enabled:      true,
		CanSubscribe: true,
		CanPublish:   true,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	_ = srv.AuthCache().Refresh(context.Background())

	// Connect with client certificate, without username or password
	opts := mtlsClientOptions(port, "mtls-connect-client", certs, certs.clientCert, certs.clientKey)
	client := mqtt.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(3 * time.Second) {
		t.Fatal("connect timed out")
	}
	if token.Error() != nil {
		t.Fatalf("expected successful connection with client certificate, got: %v", token.Error())
	}
	client.Disconnect(100)
}

func TestMTLS_PublishAndSubscribe(t *testing.T) {
	certs := createTestCertificates(t)
	port := 28884

	srv, storage := startMTLSBroker(t, port, certs)
	defer srv.Close()

	hash, _ := storesqlite.HashPassword("secret")
	_ = storage.Users.CreateUser(context.Background(), stores.User{
		Username:     certs.clientCertCN,
		PasswordHash: hash,
		Enabled:      true,
		CanSubscribe: true,
		CanPublish:   true,
	})
	_ = srv.AuthCache().Refresh(context.Background())

	subOpts := mtlsClientOptions(port, "mtls-sub", certs, certs.clientCert, certs.clientKey)
	subClient := mqtt.NewClient(subOpts)
	if token := subClient.Connect(); !token.WaitTimeout(3*time.Second) || token.Error() != nil {
		t.Fatalf("sub connect failed: %v", token.Error())
	}
	defer subClient.Disconnect(100)

	var received atomic.Int32
	subClient.Subscribe("test/mtls/demo", 1, func(_ mqtt.Client, m mqtt.Message) {
		if string(m.Payload()) == "hello mtls" {
			received.Add(1)
		}
	})

	pubOpts := mtlsClientOptions(port, "mtls-pub", certs, certs.clientCert, certs.clientKey)
	pubClient := mqtt.NewClient(pubOpts)
	if token := pubClient.Connect(); !token.WaitTimeout(3*time.Second) || token.Error() != nil {
		t.Fatalf("pub connect failed: %v", token.Error())
	}
	defer pubClient.Disconnect(100)

	token := pubClient.Publish("test/mtls/demo", 1, false, []byte("hello mtls"))
	if !token.WaitTimeout(3 * time.Second) || token.Error() != nil {
		t.Fatalf("publish failed: %v", token.Error())
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && received.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if received.Load() == 0 {
		t.Fatal("subscriber did not receive message published over mTLS")
	}
}

func TestMTLS_DisabledUserRejected(t *testing.T) {
	certs := createTestCertificates(t)
	port := 28885

	srv, storage := startMTLSBroker(t, port, certs)
	defer srv.Close()

	hash, _ := storesqlite.HashPassword("secret")
	_ = storage.Users.CreateUser(context.Background(), stores.User{
		Username:     certs.clientCertCN,
		PasswordHash: hash,
		Enabled:      false, // disabled!
		CanSubscribe: true,
		CanPublish:   true,
	})
	_ = srv.AuthCache().Refresh(context.Background())

	opts := mtlsClientOptions(port, "mtls-disabled-client", certs, certs.clientCert, certs.clientKey)
	client := mqtt.NewClient(opts)
	token := client.Connect()
	token.WaitTimeout(3 * time.Second)
	if token.Error() == nil {
		client.Disconnect(100)
		t.Fatal("expected connection to be rejected for disabled certificate user, but it succeeded")
	}
}

func TestMTLS_UnknownCommonNameRejectedWhenAutoCreateDisabled(t *testing.T) {
	certs := createTestCertificates(t)
	port := 28886

	srv, _ := startMTLSBroker(t, port, certs, func(c *config.Config) {
		autoCreate := false
		c.TCPS.AutoCreateUser = &autoCreate
	})
	defer srv.Close()

	// cert-unknown-user is not in the user store
	opts := mtlsClientOptions(port, "mtls-unknown-client", certs, certs.unknownCert, certs.unknownKey)
	client := mqtt.NewClient(opts)
	token := client.Connect()
	token.WaitTimeout(3 * time.Second)
	if token.Error() == nil {
		client.Disconnect(100)
		t.Fatal("expected unknown certificate Common Name to be rejected when AutoCreateUser=false")
	}
}

func TestMTLS_AccountCreatedOnFirstConnect(t *testing.T) {
	certs := createTestCertificates(t)
	port := 28887

	srv, storage := startMTLSBroker(t, port, certs, func(c *config.Config) {
		autoCreate := true
		c.TCPS.AutoCreateUser = &autoCreate
	})
	defer srv.Close()

	// Verify user does not exist yet
	u, err := storage.Users.GetUser(context.Background(), certs.unknownCertCN)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if u != nil {
		t.Fatal("user should not exist before test")
	}

	opts := mtlsClientOptions(port, "mtls-autocreate-client", certs, certs.unknownCert, certs.unknownKey)
	client := mqtt.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(3*time.Second) || token.Error() != nil {
		t.Fatalf("expected connect to succeed and auto-create user, got: %v", token.Error())
	}
	client.Disconnect(100)

	// Verify user now exists and has default non-admin permissions
	created, err := storage.Users.GetUser(context.Background(), certs.unknownCertCN)
	if err != nil {
		t.Fatalf("get created user: %v", err)
	}
	if created == nil {
		t.Fatal("expected user to be created in store")
	}
	if !created.Enabled {
		t.Fatal("expected created user to be enabled")
	}
	if created.IsAdmin {
		t.Fatal("auto-created user must not be an admin")
	}
	if !created.CanPublish || !created.CanSubscribe {
		t.Fatalf("expected CanPublish and CanSubscribe to be true, got publish=%v subscribe=%v", created.CanPublish, created.CanSubscribe)
	}
}

func TestMTLS_PasswordFallbackWithoutCertificate(t *testing.T) {
	certs := createTestCertificates(t)
	port := 28888

	srv, storage := startMTLSBroker(t, port, certs)
	defer srv.Close()

	hash, _ := storesqlite.HashPassword("userpass")
	_ = storage.Users.CreateUser(context.Background(), stores.User{
		Username:     "password-user",
		PasswordHash: hash,
		Enabled:      true,
		CanSubscribe: true,
		CanPublish:   true,
	})
	_ = srv.AuthCache().Refresh(context.Background())

	// Client presents NO certificate, but provides username and password
	opts := mtlsClientOptions(port, "mtls-password-client", certs, "", "")
	opts.SetUsername("password-user")
	opts.SetPassword("userpass")

	client := mqtt.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(3*time.Second) || token.Error() != nil {
		t.Fatalf("expected password client without certificate to connect on REQUEST port, got: %v", token.Error())
	}
	client.Disconnect(100)
}

func TestMTLS_BlankCommonNameFallsBack(t *testing.T) {
	certs := createTestCertificates(t)
	port := 28889

	srv, _ := startMTLSBroker(t, port, certs)
	defer srv.Close()

	// Client presents certificate with whitespace CN and NO username/password; anonymous is disabled
	opts := mtlsClientOptions(port, "mtls-blank-client", certs, certs.blankCert, certs.blankKey)
	client := mqtt.NewClient(opts)
	token := client.Connect()
	token.WaitTimeout(3 * time.Second)
	if token.Error() == nil {
		client.Disconnect(100)
		t.Fatal("expected blank CN certificate without credentials to be rejected")
	}
}

func TestMTLS_ConfiguredViaSSLBlock(t *testing.T) {
	certs := createTestCertificates(t)
	port := 28890

	srv, storage := startMTLSBroker(t, port, certs, func(c *config.Config) {
		// Clear TCPS fields and set them in SSL block
		c.TCPS.KeyStorePath = ""
		c.TCPS.ClientAuth = ""
		c.TCPS.TrustStorePath = ""
		c.TCPS.UseIdentityAsUsername = nil
		c.TCPS.AutoCreateUser = nil

		c.SSL.KeyStorePath = certs.serverCert
		c.SSL.KeyPath = certs.serverKey
		c.SSL.ClientAuth = config.ClientAuthRequest
		c.SSL.TrustStorePath = certs.caPath
		c.SSL.TrustStoreType = "PEM"
		c.SSL.UseIdentityAsUsername = true
		c.SSL.AutoCreateUser = true
	})
	defer srv.Close()

	opts := mtlsClientOptions(port, "mtls-ssl-block-client", certs, certs.unknownCert, certs.unknownKey)
	client := mqtt.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(3*time.Second) || token.Error() != nil {
		t.Fatalf("expected connect to succeed via SSL block configuration, got: %v", token.Error())
	}
	client.Disconnect(100)

	created, err := storage.Users.GetUser(context.Background(), certs.unknownCertCN)
	if err != nil || created == nil {
		t.Fatalf("expected user to be auto-created: %v", err)
	}
}
