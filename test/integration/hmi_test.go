package integration

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/hmi"
)

func startWithHMI(t *testing.T, mqttPort, gqlPort int, hmiDir string) (*broker.Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.NodeID = fmt.Sprintf("test-hmi-%d", mqttPort)
	cfg.TCP.Enabled = true
	cfg.TCP.Port = mqttPort
	cfg.WS.Enabled = false
	cfg.GraphQL.Enabled = true
	cfg.GraphQL.Port = gqlPort
	cfg.Metrics.Enabled = false
	cfg.HMI.Enabled = true
	cfg.HMI.Path = hmiDir
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "broker.db")

	srv, err := broker.New(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("broker init: %v", err)
	}
	go func() { _ = srv.Serve() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", gqlPort))
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	return srv, fmt.Sprintf("http://localhost:%d/graphql", gqlPort)
}

func TestHmiExportZip_DirectoryTraversalRejected(t *testing.T) {
	tempDir := t.TempDir()
	hmiDir := filepath.Join(tempDir, "hmi_storage")
	if err := os.MkdirAll(hmiDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a sibling directory fixture outside HMI root
	outsideDir := filepath.Join(tempDir, "outside-fixture")
	if err := os.MkdirAll(outsideDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("confidential-data"), 0644)

	// Create a legitimate dashboard inside HMI root
	validDir := filepath.Join(hmiDir, "legit-dashboard")
	if err := os.MkdirAll(validDir, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(validDir, "index.html"), []byte("<h1>Legit</h1>"), 0644)

	srv, gqlURL := startWithHMI(t, 22007, 24007, hmiDir)
	defer srv.Close()

	// 1. Attempt traversal via exportHmiZip(name: "../outside-fixture") - Issue #14 vulnerability
	traversalQuery := `{
		exportHmiZip(name: "../outside-fixture")
	}`
	respTraversal := gqlRequest(t, gqlURL, traversalQuery, nil, "")
	errs, _ := respTraversal["errors"].([]any)
	if len(errs) == 0 {
		t.Fatalf("expected GraphQL errors for traversal exportHmiZip, but got none: %#v", respTraversal)
	}

	// 2. Attempt traversal via hmiFiles(name: "../outside-fixture")
	hmiFilesQuery := `{
		hmiFiles(name: "../outside-fixture") {
			path
			sizeBytes
		}
	}`
	respFiles := gqlRequest(t, gqlURL, hmiFilesQuery, nil, "")
	errs, _ = respFiles["errors"].([]any)
	if len(errs) == 0 {
		t.Fatalf("expected GraphQL errors for traversal hmiFiles, but got none: %#v", respFiles)
	}

	// 3. Verify legitimate dashboard export succeeds
	legitQuery := `{
		exportHmiZip(name: "legit-dashboard")
	}`
	respLegit := gqlQuery(t, gqlURL, legitQuery, nil)
	zipB64, ok := respLegit["exportHmiZip"].(string)
	if !ok || len(zipB64) == 0 {
		t.Fatalf("expected valid base64 export, got: %#v", respLegit)
	}

	zipBytes, err := base64.StdEncoding.DecodeString(zipB64)
	if err != nil {
		t.Fatal(err)
	}
	r, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatal(err)
	}
	foundIndex := false
	for _, f := range r.File {
		if f.Name == "index.html" {
			foundIndex = true
		}
	}
	if !foundIndex {
		t.Fatal("expected index.html in exported legit-dashboard zip")
	}
}

func TestHmiValidateDashboardName_Coverage(t *testing.T) {
	// Verify ValidateDashboardName behavior
	if err := hmi.ValidateDashboardName("main"); err != nil {
		t.Errorf("expected 'main' to be valid, got: %v", err)
	}
	if err := hmi.ValidateDashboardName("../outside"); err == nil {
		t.Errorf("expected '../outside' to be invalid")
	}
}
