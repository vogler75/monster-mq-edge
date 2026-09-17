package integration

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
)

func restCall(t *testing.T, method, endpoint string, body []byte, authorization string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == 204 || len(data) == 0 {
		return resp.StatusCode, nil
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return resp.StatusCode, result
}

func TestRestPublishReadAndBulk(t *testing.T) {
	srv, gqlURL := startWithGraphQL(t, 23160, 28160)
	defer srv.Close()
	base := strings.TrimSuffix(gqlURL, "/graphql") + "/api/v1"
	status, login := restCall(t, "POST", base+"/login", nil, "")
	if status != 200 || login["success"] != true || login["username"] != "anonymous" {
		t.Fatalf("login: %d %v", status, login)
	}
	cl := mqtt.NewClient(mqttOpts(23160, "rest-sub"))
	token := cl.Connect()
	if !token.WaitTimeout(3*time.Second) || token.Error() != nil {
		t.Fatalf("MQTT connect: %v", token.Error())
	}
	defer cl.Disconnect(100)
	received := make(chan []byte, 1)
	sub := cl.Subscribe("rest/raw", 1, func(_ mqtt.Client, m mqtt.Message) { received <- append([]byte(nil), m.Payload()...) })
	if !sub.WaitTimeout(3*time.Second) || sub.Error() != nil {
		t.Fatalf("MQTT subscribe: %v", sub.Error())
	}
	raw := []byte{0, 0xff, 0x10}
	status, result := restCall(t, "POST", base+"/topics/rest/raw?retain=true&qos=1", raw, "")
	if status != 200 || result["topic"] != "rest/raw" {
		t.Fatalf("publish: %d %v", status, result)
	}
	select {
	case got := <-received:
		if !bytes.Equal(got, raw) {
			t.Fatalf("payload %v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no MQTT delivery")
	}
	status, result = restCall(t, "GET", base+"/topics/rest/raw?retained", nil, "")
	if status != 200 || len(result["messages"].([]any)) != 1 {
		t.Fatalf("retained: %d %v", status, result)
	}
	retained := result["messages"].([]any)[0].(map[string]any)
	if retained["retain"] != true || retained["qos"] != float64(1) {
		t.Fatalf("retained flags: %v", retained)
	}
	if retained["value"] != "AP8Q" {
		t.Fatalf("binary live value: %v", retained)
	}
	status, result = restCall(t, "PUT", base+"/topics/rest/current?payload=23.5", nil, "")
	if status != 200 {
		t.Fatalf("inline: %d %v", status, result)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		status, result = restCall(t, "GET", base+"/topics/rest/current", nil, "")
		if status == 200 && len(result["messages"].([]any)) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("current: %d %v", status, result)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if result["messages"].([]any)[0].(map[string]any)["value"] != float64(23.5) {
		t.Fatalf("current value: %v", result)
	}
	if result["messages"].([]any)[0].(map[string]any)["retain"] != false {
		t.Fatalf("current retain flag: %v", result)
	}
	status, result = restCall(t, "POST", base+"/write", []byte(`{"messages":[{"topic":"rest/a","value":{"x":1}},{"topic":"bad/#","value":"x"},5],"records":[["rest/b",true,1,true],false]}`), "")
	if status != 200 || result["count"] != float64(2) || result["success"] != false {
		t.Fatalf("bulk: %d %v", status, result)
	}
	if len(result["errors"].([]any)) != 3 {
		t.Fatalf("bulk item errors: %v", result)
	}
	status, result = restCall(t, "POST", base+"/write/influx?base=plant&format=json", []byte("temp,room=A value=3i 1700000000000000000"), "")
	if status != 204 {
		t.Fatalf("influx: %d %v", status, result)
	}
	status, result = restCall(t, "POST", base+"/write/influx?base=plant&format=simple&retain=true", []byte("temp,room=B value=7i,label=\"hi\""), "")
	if status != 204 {
		t.Fatalf("influx simple: %d %v", status, result)
	}
	status, result = restCall(t, "GET", base+"/topics/plant/temp/B/label?retained", nil, "")
	if status != 200 || result["messages"].([]any)[0].(map[string]any)["value"] != "hi" {
		t.Fatalf("influx string: %d %v", status, result)
	}
	for _, path := range []string{"/docs", "/openapi.yaml"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
	}
	for _, suffix := range []string{"?group=", "?group=Default"} {
		status, result = restCall(t, "GET", base+"/topics/rest/current"+suffix, nil, "")
		if status != 200 || len(result["messages"].([]any)) != 1 {
			t.Fatalf("default group %s: %d %v", suffix, status, result)
		}
	}
	status, result = restCall(t, "GET", base+"/topics/rest/missing", nil, "")
	if status != 200 || len(result["messages"].([]any)) != 0 {
		t.Fatalf("missing current value: %d %v", status, result)
	}
}

func TestRestWildcardAndRawReads(t *testing.T) {
	srv, gqlURL := startWithGraphQL(t, 23168, 28168)
	defer srv.Close()
	base := strings.TrimSuffix(gqlURL, "/graphql") + "/api/v1/topics/"
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 0x4a, 0x46, 0x49, 0x46, 0xff, 0xd9}
	for _, item := range []struct {
		topic   string
		payload []byte
	}{{"multi/a", []byte("first")}, {"multi/b", jpeg}} {
		status, result := restCall(t, "POST", base+item.topic+"?retain=true", item.payload, "")
		if status != 200 {
			t.Fatalf("publish %s: %d %v", item.topic, status, result)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for _, filter := range []string{"multi/%23", "multi/%2B"} {
		for {
			status, result := restCall(t, "GET", base+filter, nil, "")
			if status == 200 && len(result["messages"].([]any)) == 2 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("wildcard %s: %d %v", filter, status, result)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	status, retained := restCall(t, "GET", base+"multi/%23?retained", nil, "")
	if status != 200 || len(retained["messages"].([]any)) != 2 {
		t.Fatalf("retained wildcard: %d %v", status, retained)
	}
	for _, suffix := range []string{"?raw", "?retained&raw", "?group=&raw"} {
		resp, err := http.Get(base + "multi/b" + suffix)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || !bytes.Equal(body, jpeg) || resp.Header.Get("Content-Type") != "image/jpeg" {
			t.Fatalf("raw picture %s: %d %s %v", suffix, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
	}
	resp, err := http.Get(base + "multi/a?raw")
	if err != nil {
		t.Fatal(err)
	}
	textBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || string(textBody) != "first" || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("raw text bytes: %d %s %q", resp.StatusCode, resp.Header.Get("Content-Type"), textBody)
	}
	for _, suffix := range []string{"multi/%23?raw", "multi/b?raw&start=2020-01-01T00:00:00Z"} {
		status, result := restCall(t, "GET", base+suffix, nil, "")
		if status != 400 {
			t.Fatalf("invalid raw %s: %d %v", suffix, status, result)
		}
	}
	status, result := restCall(t, "GET", base+"multi/missing?raw", nil, "")
	if status != 404 {
		t.Fatalf("missing raw topic: %d %v", status, result)
	}
	docsURL := strings.TrimSuffix(base, "topics/") + "docs"
	resp, err = http.Get(docsURL)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !bytes.Contains(docs, []byte("cameras/%23")) || !bytes.Contains(docs, []byte("snapshot?raw")) {
		t.Fatalf("docs missing wildcard/raw examples: %d", resp.StatusCode)
	}
}

func TestRestAuthAndACL(t *testing.T) {
	srv, gqlURL := startWithGraphQL(t, 23161, 28161, func(c *config.Config) { c.UserManagement.Enabled = true; c.UserManagement.AnonymousEnabled = false })
	defer srv.Close()
	base := strings.TrimSuffix(gqlURL, "/graphql") + "/api/v1"
	status, _ := restCall(t, "GET", base+"/topics/a?retained", nil, "")
	if status != 401 {
		t.Fatalf("missing auth: %d", status)
	}
	status, login := restCall(t, "POST", base+"/login", []byte(`{"username":"Admin","password":"Admin"}`), "")
	if status != 200 || login["token"] == nil {
		t.Fatalf("login: %d %v", status, login)
	}
	bearer := "Bearer " + login["token"].(string)
	if gqlQueryAuth(t, gqlURL, `{ currentUser { username } }`, nil, login["token"].(string))["currentUser"].(map[string]any)["username"] != "Admin" {
		t.Fatal("REST token failed in GraphQL")
	}
	status, _ = restCall(t, "PUT", base+"/topics/auth/value?payload=ok", nil, bearer)
	if status != 200 {
		t.Fatalf("bearer publish: %d", status)
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("Admin:Admin"))
	status, _ = restCall(t, "GET", base+"/topics/auth/value", nil, basic)
	if status != 200 {
		t.Fatalf("basic read: %d", status)
	}
	admin := login["token"].(string)
	gqlQueryAuth(t, gqlURL, `mutation { user { createUser(input: { username: "limited", password: "pw", canSubscribe: true, canPublish: true }) { success } } }`, nil, admin)
	gqlQueryAuth(t, gqlURL, `mutation { user { createAclRule(input: { username: "limited", topicPattern: "private/#", canSubscribe: false, canPublish: false, priority: 100 }) { success } } }`, nil, admin)
	gqlQueryAuth(t, gqlURL, `mutation { user { createAclRule(input: { username: "limited", topicPattern: "#", canSubscribe: true, canPublish: true, priority: 1 }) { success } } }`, nil, admin)
	status, limited := restCall(t, "POST", base+"/login", []byte(`{"username":"limited","password":"pw"}`), "")
	if status != 200 {
		t.Fatalf("limited login: %d %v", status, limited)
	}
	userAuth := "Bearer " + limited["token"].(string)
	status, _ = restCall(t, "POST", base+"/topics/private/a", []byte("secret"), userAuth)
	if status != 403 {
		t.Fatalf("denied write: %d", status)
	}
	_, _ = restCall(t, "POST", base+"/topics/private/a?retain=true", []byte("secret"), bearer)
	status, _ = restCall(t, "GET", base+"/topics/private/a?retained&raw", nil, userAuth)
	if status != 403 {
		t.Fatalf("denied raw read: %d", status)
	}
	_, _ = restCall(t, "POST", base+"/topics/public/a?retain=true", []byte("hello"), bearer)
	status, result := restCall(t, "GET", base+"/topics/%23?retained", nil, userAuth)
	if status != 200 || len(result["messages"].([]any)) != 1 {
		t.Fatalf("ACL wildcard: %d %v", status, result)
	}
}

func TestRestSSE(t *testing.T) {
	srv, gqlURL := startWithGraphQL(t, 23162, 28162)
	defer srv.Close()
	base := strings.TrimSuffix(gqlURL, "/graphql") + "/api/v1"
	status, _ := restCall(t, "GET", base+"/subscribe", nil, "")
	if status != 400 {
		t.Fatalf("missing filters: %d", status)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(base + "/subscribe?topic=sse/%23")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "connected") {
		t.Fatalf("connect: %q %v", line, err)
	}
	_, _ = restCall(t, "POST", base+"/topics/sse/value", []byte("42"), "")
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			break
		}
	}
	if !strings.Contains(line, `"topic":"sse/value"`) {
		t.Fatalf("SSE: %s", line)
	}
}

func TestRestHistoryAndLimits(t *testing.T) {
	srv, gqlURL := startWithGraphQL(t, 23164, 28164)
	defer srv.Close()
	base := strings.TrimSuffix(gqlURL, "/graphql") + "/api/v1"
	created := gqlQuery(t, gqlURL, `mutation Create($input: CreateArchiveGroupInput!) { archiveGroup { create(input: $input) { success } } }`, map[string]any{"input": map[string]any{"name": "RestHistory", "topicFilter": []string{"history/#"}, "lastValType": "SQLITE", "archiveType": "SQLITE"}})
	if created["archiveGroup"].(map[string]any)["create"].(map[string]any)["success"] != true {
		t.Fatalf("create: %v", created)
	}
	for _, payload := range [][]byte{[]byte(`{"n":1}`), []byte("text"), []byte("42"), {0xff, 0x00}} {
		status, result := restCall(t, "POST", base+"/topics/history/a", payload, "")
		if status != 200 {
			t.Fatalf("publish: %d %v", status, result)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	var result map[string]any
	for {
		_, result = restCall(t, "GET", base+"/topics/history/a?group=RestHistory&start=2020-01-01T00:00:00Z", nil, "")
		if len(result["messages"].([]any)) == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("history never flushed: %v", result)
		}
		time.Sleep(30 * time.Millisecond)
	}
	rows := result["messages"].([]any)
	if rows[0].(map[string]any)["payload_base64"] != base64.StdEncoding.EncodeToString([]byte{0xff, 0x00}) {
		t.Fatalf("binary history: %v", rows[0])
	}
	if rows[1].(map[string]any)["payload"] != "42" {
		t.Fatalf("scalar history should remain text: %v", rows[1])
	}
	if _, ok := rows[3].(map[string]any)["payload"].(map[string]any); !ok {
		t.Fatalf("JSON history: %v", rows[3])
	}
	if _, ok := rows[0].(map[string]any)["timestamp"].(float64); !ok {
		t.Fatalf("history timestamp: %v", rows[0])
	}
	status, result := restCall(t, "GET", base+"/topics/history/a?group=RestHistory&end=bad", nil, "")
	if status != 400 {
		t.Fatalf("bad end: %d %v", status, result)
	}
	status, result = restCall(t, "GET", base+"/topics/history/a?group=RestHistory&start=2020-01-01T00:00:00Z&limit=1", nil, "")
	if status != 200 || len(result["messages"].([]any)) != 1 {
		t.Fatalf("limit: %d %v", status, result)
	}
	status, result = restCall(t, "GET", base+"/topics/history/a?group=missing", nil, "")
	if status != 404 {
		t.Fatalf("missing group: %d %v", status, result)
	}
	status, result = restCall(t, "GET", base+"/topics/history/a?group=Default&start=2020-01-01T00:00:00Z", nil, "")
	if status != 404 {
		t.Fatalf("unavailable archive: %d %v", status, result)
	}
	for _, suffix := range []string{"?start=2020-01-01T00:00:00Z", "?group=&start=2020-01-01T00:00:00Z"} {
		status, result = restCall(t, "GET", base+"/topics/history/a"+suffix, nil, "")
		if status != 404 || !strings.Contains(result["error"].(string), "Default") {
			t.Fatalf("default archive %s: %d %v", suffix, status, result)
		}
	}
	status, result = restCall(t, "POST", base+"/topics/history/oversized", bytes.Repeat([]byte("x"), 4<<20+1), "")
	if status != 413 {
		t.Fatalf("oversized: %d %v", status, result)
	}
}

func TestRestDisabled(t *testing.T) {
	srv, gqlURL := startWithGraphQL(t, 23163, 28163, func(c *config.Config) { c.RestApi.Enabled = false })
	defer srv.Close()
	base := strings.TrimSuffix(gqlURL, "/graphql")
	resp, err := http.Get(base + "/api/v1/docs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("disabled REST: %d", resp.StatusCode)
	}
	resp, err = http.Get(base + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health: %d", resp.StatusCode)
	}
}

func TestRestSQLiteRestart(t *testing.T) {
	srv, gqlURL := startWithGraphQL(t, 23165, 28165)
	base := strings.TrimSuffix(gqlURL, "/graphql")
	api := base + "/api/v1"
	sqlitePath := gqlQuery(t, gqlURL, `{ brokerConfig { sqlitePath } }`, nil)["brokerConfig"].(map[string]any)["sqlitePath"].(string)
	created := gqlQuery(t, gqlURL, `mutation { archiveGroup { create(input: { name: "RestPersist", topicFilter: ["persist/#"], lastValType: SQLITE, archiveType: SQLITE }) { success } } }`, nil)
	if created["archiveGroup"].(map[string]any)["create"].(map[string]any)["success"] != true {
		t.Fatalf("create: %v", created)
	}
	status, result := restCall(t, "POST", api+"/topics/persist/a?retain=true", []byte(`{"ok":true}`), "")
	if status != 200 {
		t.Fatalf("publish: %d %v", status, result)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, result = restCall(t, "GET", api+"/topics/persist/a?group=RestPersist&start=2020-01-01T00:00:00Z", nil, "")
		if len(result["messages"].([]any)) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("history not flushed: %v", result)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.NodeID = "g-28165"
	cfg.TCP.Port = 23165
	cfg.WS.Enabled = false
	cfg.GraphQL.Port = 28165
	cfg.SQLite.Path = sqlitePath
	restarted, err := broker.New(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer restarted.Close()
	go func() { _ = restarted.Serve() }()
	for {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline.Add(3 * time.Second)) {
			t.Fatal("restart listener unavailable")
		}
		time.Sleep(30 * time.Millisecond)
	}
	for _, suffix := range []string{"?retained", "?group=RestPersist", "?group=RestPersist&start=2020-01-01T00:00:00Z"} {
		status, result := restCall(t, "GET", api+"/topics/persist/a"+suffix, nil, "")
		if status != 200 || len(result["messages"].([]any)) != 1 {
			t.Fatalf("after restart %s: %d %v", suffix, status, result)
		}
	}
}

func TestRestSSEClientLimit(t *testing.T) {
	srv, gqlURL := startWithGraphQL(t, 23166, 28166)
	defer srv.Close()
	url := strings.TrimSuffix(gqlURL, "/graphql") + "/api/v1/subscribe?topic=limit/%23"
	client := &http.Client{Timeout: 5 * time.Second}
	connections := make([]*http.Response, 0, 128)
	defer func() {
		for _, resp := range connections {
			resp.Body.Close()
		}
	}()
	for i := 0; i < 128; i++ {
		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("SSE connection %d: %v", i, err)
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			t.Fatalf("SSE connection %d: %d", i, resp.StatusCode)
		}
		connections = append(connections, resp)
	}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("SSE cap: %d", resp.StatusCode)
	}
}

func TestRestSSESlowReader(t *testing.T) {
	srv, _ := startWithGraphQL(t, 23167, 28167)
	defer srv.Close()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:28167", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(1024)
	}
	if _, err := io.WriteString(conn, "GET /api/v1/subscribe?topic=slow/%23 HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(statusLine, "200") {
		t.Fatalf("SSE status: %q %v", statusLine, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	cl := mqtt.NewClient(mqttOpts(23167, "sse-flood"))
	connected := cl.Connect()
	if !connected.WaitTimeout(3*time.Second) || connected.Error() != nil {
		t.Fatalf("MQTT connect: %v", connected.Error())
	}
	defer cl.Disconnect(100)
	payload := bytes.Repeat([]byte("x"), 128<<10)
	start := time.Now()
	for i := 0; i < 300; i++ {
		tok := cl.Publish("slow/data", 0, false, payload)
		if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
			t.Fatalf("MQTT publish %d: %v", i, tok.Error())
		}
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("MQTT publish blocked by slow SSE reader: %s", elapsed)
	}
	resp, err := http.Get("http://127.0.0.1:28167/health")
	if err != nil {
		t.Fatalf("broker became unresponsive: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("broker health after flood: %d", resp.StatusCode)
	}
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	if n, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("slow SSE stream did not terminate: %v", err)
	} else if n == 0 {
		t.Fatal("slow SSE stream closed without any content")
	}
}
