package integration

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"monstermq.io/edge/internal/config"
)

func TestDashboardDefault(t *testing.T) {
	srv, gqlURL := startWithGraphQL(t, 23170, 28170)
	defer srv.Close()

	base := strings.TrimSuffix(gqlURL, "/graphql")
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for dashboard root, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "MonsterMQ") {
		t.Fatalf("expected MonsterMQ in dashboard root, got: %s", string(body))
	}
}

func TestDashboardFilesystemOverride(t *testing.T) {
	tempDir := t.TempDir()
	customHTML := "<html><body><h1>Custom Edge Dashboard Test</h1></body></html>"
	if err := os.WriteFile(filepath.Join(tempDir, "index.html"), []byte(customHTML), 0644); err != nil {
		t.Fatal(err)
	}

	srv, gqlURL := startWithGraphQL(t, 23171, 28171, func(c *config.Config) {
		c.Dashboard.Path = tempDir
	})
	defer srv.Close()

	base := strings.TrimSuffix(gqlURL, "/graphql")
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != customHTML {
		t.Fatalf("unexpected content: %s", string(body))
	}
}

func TestDashboardDisabled(t *testing.T) {
	srv, gqlURL := startWithGraphQL(t, 23172, 28172, func(c *config.Config) {
		c.Dashboard.Enabled = false
	})
	defer srv.Close()

	base := strings.TrimSuffix(gqlURL, "/graphql")
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 when dashboard disabled, got %d", resp.StatusCode)
	}

	healthResp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /health, got %d", healthResp.StatusCode)
	}
}
