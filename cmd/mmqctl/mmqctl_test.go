package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	tempDir := t.TempDir()
	envPath := filepath.Join(tempDir, ".env")

	content := `
# Sample config
MQ_URL=http://127.0.0.1:4000/graphql
MQ_USER=testuser
MQ_PASS="testpass"
`
	if err := os.WriteFile(envPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp env file: %v", err)
	}

	LoadDotEnv(envPath)

	if got := os.Getenv("MQ_URL"); got != "http://127.0.0.1:4000/graphql" {
		t.Errorf("expected MQ_URL=http://127.0.0.1:4000/graphql, got %s", got)
	}
	if got := os.Getenv("MQ_USER"); got != "testuser" {
		t.Errorf("expected MQ_USER=testuser, got %s", got)
	}
	if got := os.Getenv("MQ_PASS"); got != "testpass" {
		t.Errorf("expected MQ_PASS=testpass, got %s", got)
	}
}

func TestResolveClientConfig(t *testing.T) {
	cfg := ResolveClientConfig("http://custom:4000/graphql", "admin", "secret", "tok123", "", true)

	if cfg.URL != "http://custom:4000/graphql" {
		t.Errorf("unexpected URL: %s", cfg.URL)
	}
	if cfg.Username != "admin" {
		t.Errorf("unexpected Username: %s", cfg.Username)
	}
	if cfg.Password != "secret" {
		t.Errorf("unexpected Password: %s", cfg.Password)
	}
	if cfg.Token != "tok123" {
		t.Errorf("unexpected Token: %s", cfg.Token)
	}
	if !cfg.JSONMode {
		t.Errorf("expected JSONMode to be true")
	}
}
