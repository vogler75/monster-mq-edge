package integration

import (
	"bytes"
	"crypto/tls"
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

func startWithMCP(t *testing.T, mqttPort, gqlPort int, cfgFns ...func(*config.Config)) (*broker.Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.NodeID = fmt.Sprintf("mcp-node-%d", gqlPort)
	cfg.TCP.Enabled = true
	cfg.TCP.Port = mqttPort
	cfg.WS.Enabled = false
	cfg.GraphQL.Enabled = true
	cfg.GraphQL.Port = gqlPort
	cfg.GraphQL.TLSPort = gqlPort + 1000
	cfg.MCP.Enabled = true
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

	mcpURL := fmt.Sprintf("http://localhost:%d/mcp", gqlPort)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("POST", mcpURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return srv, mcpURL
}

func mcpRequest(t *testing.T, url, body, authorization string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("create MCP request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("MCP request: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read MCP response: %v", err)
	}
	return resp.StatusCode, string(bodyBytes)
}

func TestMCPBearerAuthenticationAndACL(t *testing.T) {
	srv, mcpURL := startWithMCP(t, 23051, 28051, func(c *config.Config) {
		c.UserManagement.Enabled = true
		c.UserManagement.AnonymousEnabled = false
	})
	defer srv.Close()
	gqlURL := "http://localhost:28051/graphql"
	initialize := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-03-26\",\"capabilities\":{},\"clientInfo\":{\"name\":\"test\",\"version\":\"1\"}}}"

	for name, authorization := range map[string]string{
		"missing":   "",
		"unknown":   "Bearer definitely-not-a-valid-token",
		"empty":     "Bearer ",
		"malformed": "Bearer one two",
	} {
		t.Run(name, func(t *testing.T) {
			status, _ := mcpRequest(t, mcpURL, initialize, authorization)
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", status)
			}
		})
	}

	adminToken := loginToken(t, gqlURL, "Admin", "Admin")
	status, body := mcpRequest(t, mcpURL, initialize, "Bearer "+adminToken)
	if status != http.StatusOK || !strings.Contains(body, "serverInfo") {
		t.Fatalf("valid bearer initialize failed: status=%d body=%s", status, body)
	}

	gqlQueryAuth(t, gqlURL, "mutation { user { createUser(input: { username: \"mcp-user\", password: \"pw\", canSubscribe: true, canPublish: true }) { success } } }", nil, adminToken)
	gqlQueryAuth(t, gqlURL, "mutation { user { createAclRule(input: { username: \"mcp-user\", topicPattern: \"private/#\", canSubscribe: false, canPublish: false, priority: 100 }) { success } } }", nil, adminToken)
	gqlQueryAuth(t, gqlURL, "mutation { user { createAclRule(input: { username: \"mcp-user\", topicPattern: \"#\", canSubscribe: true, canPublish: true, priority: 1 }) { success } } }", nil, adminToken)
	userToken := loginToken(t, gqlURL, "mcp-user", "pw")

	callTool := func(topic string) string {
		body := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"set-topic-value\",\"arguments\":{\"topic\":%q,\"payload\":\"value\"}}}", topic)
		status, response := mcpRequest(t, mcpURL, body, "Bearer "+userToken)
		if status != http.StatusOK {
			t.Fatalf("tool status = %d: %s", status, response)
		}
		return response
	}
	if response := callTool("public/value"); !strings.Contains(response, "Published to topic") {
		t.Fatalf("allowed publish failed: %s", response)
	}
	if response := callTool("private/value"); !strings.Contains(response, "Permission denied") || !strings.Contains(response, "\"isError\":true") {
		t.Fatalf("denied publish was not rejected: %s", response)
	}

	gqlQueryAuth(t, gqlURL, "mutation { user { setPassword(input: { username: \"mcp-user\", password: \"changed\" }) { success } } }", nil, adminToken)
	status, _ = mcpRequest(t, mcpURL, initialize, "Bearer "+userToken)
	if status != http.StatusUnauthorized {
		t.Fatalf("revoked bearer status = %d, want 401", status)
	}
}

func TestMCPInvalidBearerRejectedWhenAnonymousEnabled(t *testing.T) {
	srv, mcpURL := startWithMCP(t, 23052, 28052, func(c *config.Config) {
		c.UserManagement.Enabled = true
		c.UserManagement.AnonymousEnabled = true
	})
	defer srv.Close()
	initialize := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-03-26\",\"capabilities\":{},\"clientInfo\":{\"name\":\"test\",\"version\":\"1\"}}}"

	status, _ := mcpRequest(t, mcpURL, initialize, "Bearer invalid")
	if status != http.StatusUnauthorized {
		t.Fatalf("invalid bearer status = %d, want 401", status)
	}
	status, body := mcpRequest(t, mcpURL, initialize, "")
	if status != http.StatusOK || !strings.Contains(body, "serverInfo") {
		t.Fatalf("anonymous initialize failed: status=%d body=%s", status, body)
	}
}

func TestMCPServerTools(t *testing.T) {
	mqttPort := 23050
	gqlPort := 28050

	srv, mcpURL := startWithMCP(t, mqttPort, gqlPort, func(c *config.Config) {
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

func TestMCPServerOverHTTPS(t *testing.T) {
	tempDir := t.TempDir()
	certPath := filepath.Join(tempDir, "auto.crt")
	keyPath := filepath.Join(tempDir, "auto.key")

	srv, _ := startWithMCP(t, 23053, 28053, func(c *config.Config) {
		c.GraphQL.TLSPort = 28153
		c.GraphQL.KeyStorePath = certPath
		c.GraphQL.KeyPath = keyPath
	})
	defer srv.Close()

	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}

	httpsMCPURL := "https://localhost:28153/mcp"
	req, _ := http.NewRequest("POST", httpsMCPURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("MCP HTTPS request error: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 over HTTPS, got %d: %s", resp.StatusCode, string(bodyBytes))
	}
}
