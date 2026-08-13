package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
)

func startWithMCP(t *testing.T, mqttPort, gqlPort, mcpPort int, cfgFns ...func(*config.Config)) (*broker.Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.NodeID = fmt.Sprintf("mcp-node-%d", mcpPort)
	cfg.TCP.Enabled = true
	cfg.TCP.Port = mqttPort
	cfg.WS.Enabled = false
	cfg.GraphQL.Enabled = true
	cfg.GraphQL.Port = gqlPort
	cfg.MCP.Enabled = true
	cfg.MCP.Port = mcpPort
	cfg.Features.Mcp = true
	cfg.SQLite.Path = filepath.Join(t.TempDir(), "mcp.db")
	for _, fn := range cfgFns {
		fn(cfg)
	}
	srv, err := broker.New(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("broker init: %v", err)
	}
	go func() { _ = srv.Serve() }()

	mcpURL := fmt.Sprintf("http://localhost:%d/mcp", mcpPort)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("POST", mcpURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnsupportedMediaType || resp.StatusCode == http.StatusBadRequest {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return srv, mcpURL
}

func TestMCPServerTools(t *testing.T) {
	mqttPort := 23050
	gqlPort := 28050
	mcpPort := 23000

	srv, mcpURL := startWithMCP(t, mqttPort, gqlPort, mcpPort, func(c *config.Config) {
		c.UserManagement.Enabled = false
	})
	defer srv.Close()

	// 1. List tools via JSON-RPC request
	reqBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req, _ := http.NewRequest("POST", mcpURL, bytes.NewBufferString(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mcp tools/list request error: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)

	var res map[string]any
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		t.Fatalf("failed to parse json response: %v, raw: %s", err, string(bodyBytes))
	}
	result, ok := res["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object in response, got: %s", string(bodyBytes))
	}
	tools, ok := result["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("expected tools array in result, got: %v", result)
	}

	toolNames := make(map[string]bool)
	for _, toolObj := range tools {
		tmap, ok := toolObj.(map[string]any)
		if ok {
			if name, ok := tmap["name"].(string); ok {
				toolNames[name] = true
			}
		}
	}

	expectedTools := []string{
		"list-archive-groups",
		"find-topics-by-name",
		"find-topics-by-description",
		"get-topic-value",
		"set-topic-value",
		"query-message-archive",
		"query-message-archive-by-sql",
		"query-message-archive-aggregated",
	}

	for _, expected := range expectedTools {
		if !toolNames[expected] {
			t.Errorf("missing expected MCP tool: %s", expected)
		}
	}

	// 2. Call set-topic-value tool
	setReqBody := `{
		"jsonrpc": "2.0",
		"id": 2,
		"method": "tools/call",
		"params": {
			"name": "set-topic-value",
			"arguments": {
				"topic": "sensors/temp/room1",
				"payload": "{\"temperature\": 22.5}",
				"retained": true
			}
		}
	}`
	req, _ = http.NewRequest("POST", mcpURL, bytes.NewBufferString(setReqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call set-topic-value error: %v", err)
	}
	bodyBytes, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(bodyBytes), "Published to topic 'sensors/temp/room1'") {
		t.Fatalf("set-topic-value response invalid: %s", string(bodyBytes))
	}

	// 3. Call get-topic-value tool
	getReqBody := `{
		"jsonrpc": "2.0",
		"id": 3,
		"method": "tools/call",
		"params": {
			"name": "get-topic-value",
			"arguments": {
				"topics": ["sensors/temp/room1"]
			}
		}
	}`
	req, _ = http.NewRequest("POST", mcpURL, bytes.NewBufferString(getReqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call get-topic-value error: %v", err)
	}
	bodyBytes, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(bodyBytes), "sensors/temp/room1") || !strings.Contains(string(bodyBytes), "22.5") {
		t.Fatalf("get-topic-value response invalid: %s", string(bodyBytes))
	}

	// 4. Call find-topics-by-name tool
	findReqBody := `{
		"jsonrpc": "2.0",
		"id": 4,
		"method": "tools/call",
		"params": {
			"name": "find-topics-by-name",
			"arguments": {
				"name": "room1"
			}
		}
	}`
	req, _ = http.NewRequest("POST", mcpURL, bytes.NewBufferString(findReqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call find-topics-by-name error: %v", err)
	}
	bodyBytes, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(bodyBytes), "sensors/temp/room1") {
		t.Fatalf("find-topics-by-name response invalid: %s", string(bodyBytes))
	}
}
