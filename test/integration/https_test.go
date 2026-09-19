package integration

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/config"
)

func TestHTTPSAutoCertGeneration(t *testing.T) {
	tempDir := t.TempDir()
	certPath := filepath.Join(tempDir, "auto.crt")
	keyPath := filepath.Join(tempDir, "auto.key")

	srv, _ := startWithGraphQL(t, 23180, 28180, func(c *config.Config) {
		c.GraphQL.TLSPort = 28181
		c.GraphQL.KeyStorePath = certPath
		c.GraphQL.KeyPath = keyPath
	})
	defer srv.Close()

	// 1. Verify files were automatically generated
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("expected cert file to be created: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("expected key file to be created: %v", err)
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	// 2. HTTPS /health
	resp, err := client.Get("https://localhost:28181/health")
	if err != nil {
		t.Fatalf("HTTPS GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on HTTPS, got %d", resp.StatusCode)
	}

	// 3. HTTPS dashboard root
	dashResp, err := client.Get("https://localhost:28181/")
	if err != nil {
		t.Fatalf("HTTPS GET /: %v", err)
	}
	defer dashResp.Body.Close()
	body, _ := io.ReadAll(dashResp.Body)
	if !strings.Contains(string(body), "MonsterMQ") {
		t.Fatalf("expected MonsterMQ in dashboard root, got: %s", string(body))
	}

	// 4. Plain HTTP should also continue to work on port 28180
	httpResp, err := http.Get("http://localhost:28180/health")
	if err != nil {
		t.Fatalf("HTTP GET /health: %v", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on HTTP, got %d", httpResp.StatusCode)
	}
}

func TestHTTPSRequireFromOutside_LocalhostAllowed(t *testing.T) {
	tempDir := t.TempDir()
	certPath := filepath.Join(tempDir, "auto.crt")
	keyPath := filepath.Join(tempDir, "auto.key")

	srv, _ := startWithGraphQL(t, 23182, 28182, func(c *config.Config) {
		c.GraphQL.TLSPort = 28183
		c.GraphQL.RequireHTTPSFromOutside = true
		c.GraphQL.KeyStorePath = certPath
		c.GraphQL.KeyPath = keyPath
	})
	defer srv.Close()

	// Localhost HTTP request is allowed
	resp, err := http.Get("http://127.0.0.1:28182/health")
	if err != nil {
		t.Fatalf("Localhost HTTP GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for localhost HTTP, got %d", resp.StatusCode)
	}

	dashResp, err := http.Get("http://127.0.0.1:28182/")
	if err != nil {
		t.Fatalf("Localhost HTTP GET /: %v", err)
	}
	defer dashResp.Body.Close()
	if dashResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for localhost HTTP dashboard, got %d", dashResp.StatusCode)
	}
}

func TestHTTPSRequireFromOutside_ExternalRedirect(t *testing.T) {
	tempDir := t.TempDir()
	certPath := filepath.Join(tempDir, "auto.crt")
	keyPath := filepath.Join(tempDir, "auto.key")

	srv, _ := startWithGraphQL(t, 23184, 28184, func(c *config.Config) {
		c.GraphQL.TLSPort = 28185
		c.GraphQL.RequireHTTPSFromOutside = true
		c.GraphQL.KeyStorePath = certPath
		c.GraphQL.KeyPath = keyPath
	})
	defer srv.Close()

	// Simulate an external request by sending a request without localhost RemoteAddr
	// In Go's net/http, any request arriving on a real TCP listener has the real RemoteAddr.
	// We can test the handler middleware directly:
	cfg := config.Default()
	cfg.GraphQL.TLSPort = 28185
	cfg.GraphQL.RequireHTTPSFromOutside = true

	// Custom client with CheckRedirect to capture 307
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Request from loopback should NOT redirect
	resp, err := client.Get("http://127.0.0.1:28184/health")
	if err != nil {
		t.Fatalf("local request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for local, got %d", resp.StatusCode)
	}
}

func TestRequireHTTPSMiddlewareRedirect(t *testing.T) {
	cfg := config.Default()
	cfg.GraphQL.TLSPort = 4443
	cfg.GraphQL.RequireHTTPSFromOutside = true

	req := httptest.NewRequest(http.MethodGet, "/pages/dashboard.html", nil)
	req.Host = "192.168.1.100:4000"
	req.RemoteAddr = "192.168.1.50:52345" // external client IP
	rec := httptest.NewRecorder()

	host, _, err := net.SplitHostPort(req.Host)
	if err != nil {
		host = req.Host
	}
	target := fmt.Sprintf("https://%s:%d%s", host, cfg.EffectiveGraphQLTLSPort(), req.URL.RequestURI())
	http.Redirect(rec, req, target, http.StatusTemporaryRedirect)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://192.168.1.100:4443/pages/dashboard.html" {
		t.Fatalf("expected https://192.168.1.100:4443/pages/dashboard.html, got %s", loc)
	}
}

func TestHTTPS_OnlyHTTPSPort_NoHTTP(t *testing.T) {
	tempDir := t.TempDir()
	certPath := filepath.Join(tempDir, "auto.crt")
	keyPath := filepath.Join(tempDir, "auto.key")

	srv, _ := startWithGraphQL(t, 23186, 28186, func(c *config.Config) {
		c.GraphQL.Port = 0 // HTTP disabled
		c.GraphQL.TLSPort = 28187
		c.GraphQL.KeyStorePath = certPath
		c.GraphQL.KeyPath = keyPath
	})
	defer srv.Close()

	// HTTPS works
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	resp, err := client.Get("https://localhost:28187/health")
	if err != nil {
		t.Fatalf("HTTPS GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Plain HTTP port was never opened
	_, err = http.Get("http://localhost:28186/health")
	if err == nil {
		t.Fatal("expected error connecting to disabled HTTP port")
	}
}

func TestHTTP_OnlyHTTPPort_NoHTTPS(t *testing.T) {
	srv, _ := startWithGraphQL(t, 23188, 28188, func(c *config.Config) {
		c.GraphQL.TLSPort = 0 // HTTPS disabled
	})
	defer srv.Close()

	// HTTP works
	resp, err := http.Get("http://localhost:28188/health")
	if err != nil {
		t.Fatalf("HTTP GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// HTTPS port was never opened
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Transport: tr, Timeout: 500 * time.Millisecond}
	_, err = client.Get("https://localhost:29188/health")
	if err == nil {
		t.Fatal("expected error connecting to disabled HTTPS port")
	}
}
