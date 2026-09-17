package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateRejectsUnsupportedPersistentQueueOverride(t *testing.T) {
	cfg := Default()
	cfg.DefaultStoreType = StoreSQLite
	cfg.QueueStoreType = StorePostgres

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "QueueStoreType") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAllowsMemoryQueueOverride(t *testing.T) {
	cfg := Default()
	cfg.DefaultStoreType = StoreSQLite
	cfg.QueueStoreType = StoreMemory

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateHostMonitoring(t *testing.T) {
	t.Run("valid settings", func(t *testing.T) {
		cfg := Default()
		cfg.HostMonitoring.Enabled = true
		cfg.HostMonitoring.IntervalSeconds = 10
		cfg.HostMonitoring.QoS = 1
		cfg.HostMonitoring.BaseTopic = "nodes/test/host"
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected valid config, got error: %v", err)
		}
	})

	t.Run("invalid interval", func(t *testing.T) {
		cfg := Default()
		cfg.HostMonitoring.Enabled = true
		cfg.HostMonitoring.IntervalSeconds = 0
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for interval <= 0")
		}
	})

	t.Run("invalid QoS", func(t *testing.T) {
		cfg := Default()
		cfg.HostMonitoring.Enabled = true
		cfg.HostMonitoring.QoS = 3
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for QoS > 2")
		}
	})

	t.Run("empty BaseTopic", func(t *testing.T) {
		cfg := Default()
		cfg.HostMonitoring.Enabled = true
		cfg.HostMonitoring.BaseTopic = ""
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected error for empty BaseTopic")
		}
	})
}

func TestGraphQLDefault(t *testing.T) {
	cfg := Default()
	if !cfg.GraphQL.HTTPEnabled() || cfg.GraphQL.Port != 4000 {
		t.Fatalf("expected HTTPEnabled on port 4000, got %d", cfg.GraphQL.Port)
	}
	if !cfg.GraphQL.TLSEnabled() || cfg.GraphQL.TLSPort != 4443 {
		t.Fatalf("expected TLSEnabled on port 4443, got %d", cfg.GraphQL.TLSPort)
	}
}

func TestGraphQLLoadYAML(t *testing.T) {
	t.Run("only Port defined enables HTTP and disables TLS", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "config.yaml")
		yaml := []byte("GraphQL:\n  Port: 4001\n")
		if err := os.WriteFile(tmp, yaml, 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(tmp)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.GraphQL.HTTPEnabled() || cfg.GraphQL.Port != 4001 {
			t.Fatalf("expected HTTPEnabled on port 4001, got %v (%d)", cfg.GraphQL.HTTPEnabled(), cfg.GraphQL.Port)
		}
		if cfg.GraphQL.TLSEnabled() || cfg.GraphQL.TLSPort != 0 {
			t.Fatalf("expected TLSEnabled to be false when TLSPort is omitted, got %v (%d)", cfg.GraphQL.TLSEnabled(), cfg.GraphQL.TLSPort)
		}
	})

	t.Run("only TLSPort defined enables TLS and disables HTTP", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "config.yaml")
		yaml := []byte("GraphQL:\n  TLSPort: 4443\n")
		if err := os.WriteFile(tmp, yaml, 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(tmp)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.GraphQL.HTTPEnabled() || cfg.GraphQL.Port != 0 {
			t.Fatalf("expected HTTP to be disabled when Port is omitted, got %v (%d)", cfg.GraphQL.HTTPEnabled(), cfg.GraphQL.Port)
		}
		if !cfg.GraphQL.TLSEnabled() || cfg.GraphQL.TLSPort != 4443 {
			t.Fatalf("expected TLSEnabled on port 4443, got %v (%d)", cfg.GraphQL.TLSEnabled(), cfg.GraphQL.TLSPort)
		}
	})

	t.Run("both Port and TLSPort defined enables both", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "config.yaml")
		yaml := []byte("GraphQL:\n  Port: 4001\n  TLSPort: 4443\n")
		if err := os.WriteFile(tmp, yaml, 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(tmp)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.GraphQL.HTTPEnabled() || cfg.GraphQL.Port != 4001 {
			t.Fatalf("expected HTTP enabled on 4001")
		}
		if !cfg.GraphQL.TLSEnabled() || cfg.GraphQL.TLSPort != 4443 {
			t.Fatalf("expected TLS enabled on 4443")
		}
	})

	t.Run("omitted GraphQL section keeps Default ports", func(t *testing.T) {
		tmp := filepath.Join(t.TempDir(), "config.yaml")
		yaml := []byte("Metrics:\n  Enabled: true\n")
		if err := os.WriteFile(tmp, yaml, 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(tmp)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if !cfg.GraphQL.HTTPEnabled() || cfg.GraphQL.Port != 4000 {
			t.Fatalf("expected HTTP port 4000, got %d", cfg.GraphQL.Port)
		}
		if !cfg.GraphQL.TLSEnabled() || cfg.GraphQL.TLSPort != 4443 {
			t.Fatalf("expected TLS port 4443, got %d", cfg.GraphQL.TLSPort)
		}
	})
}
