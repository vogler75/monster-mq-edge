package integration

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"monstermq.io/edge/internal/config"
)

func TestLocalhostAuth_Allowed(t *testing.T) {
	hmiDir := t.TempDir()
	dashDir := filepath.Join(hmiDir, "main")
	if err := os.MkdirAll(dashDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dashDir, "index.html"), []byte("<h1>Localhost HMI</h1>"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	mqttPort := 23300
	gqlPort := 28300
	srv, _ := startWithGraphQL(t, mqttPort, gqlPort, func(c *config.Config) {
		c.UserManagement.Enabled = true
		c.UserManagement.AnonymousEnabled = false
		c.UserManagement.AllowAnonymousLocalhost = true
		c.HMI.Enabled = true
		c.HMI.Path = hmiDir
		c.HMI.MountPath = "/hmi"
		c.Features.Hmi = true
		c.RestApi.Enabled = true
	})
	defer srv.Close()

	// 1. MQTT anonymous connect, publish, and subscribe from 127.0.0.1
	mqttOpts := mqtt.NewClientOptions()
	mqttOpts.AddBroker(fmt.Sprintf("tcp://127.0.0.1:%d", mqttPort))
	mqttOpts.SetClientID("anon-localhost-client")
	mqttOpts.SetConnectTimeout(2 * time.Second)
	mqttOpts.SetCleanSession(true)

	client := mqtt.NewClient(mqttOpts)
	if tok := client.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("expected anonymous localhost MQTT connect to succeed, got: %v", tok.Error())
	}
	defer client.Disconnect(100)

	var received atomic.Int32
	receivedMsg := make(chan string, 1)
	if tok := client.Subscribe("test/localauth", 0, func(_ mqtt.Client, m mqtt.Message) {
		received.Add(1)
		receivedMsg <- string(m.Payload())
	}); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("expected anonymous localhost subscribe to succeed, got: %v", tok.Error())
	}

	if tok := client.Publish("test/localauth", 0, false, "hello localhost"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("expected anonymous localhost publish to succeed, got: %v", tok.Error())
	}

	select {
	case msg := <-receivedMsg:
		if msg != "hello localhost" {
			t.Fatalf("expected 'hello localhost', got %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for MQTT message on localhost")
	}

	// 2. GraphQL query without Authorization header on 127.0.0.1
	localGqlURL := fmt.Sprintf("http://127.0.0.1:%d/graphql", gqlPort)
	res := gqlRequest(t, localGqlURL, `query { broker { version } }`, nil, "")
	if errs, ok := res["errors"]; ok {
		t.Fatalf("expected GraphQL query to succeed without auth on localhost, got errors: %v", errs)
	}
	data, ok := res["data"].(map[string]any)
	if !ok || data["broker"] == nil {
		t.Fatalf("unexpected GraphQL response: %v", res)
	}

	// 3. GraphQL login with empty credentials from localhost
	loginRes := gqlRequest(t, localGqlURL, `mutation { login(username: "", password: "") { success message username isAdmin } }`, nil, "")
	if errs, ok := loginRes["errors"]; ok {
		t.Fatalf("login mutation failed with errors: %v", errs)
	}
	loginData := loginRes["data"].(map[string]any)["login"].(map[string]any)
	if !loginData["success"].(bool) {
		t.Fatalf("expected login to succeed for localhost, got: %v", loginData)
	}
	if loginData["username"] != "localhost" {
		t.Fatalf("expected username=localhost, got %v", loginData["username"])
	}
	if loginData["isAdmin"] != true {
		t.Fatalf("expected isAdmin=true, got %v", loginData["isAdmin"])
	}

	// 4. HMI endpoint without auth on 127.0.0.1
	hmiURL := fmt.Sprintf("http://127.0.0.1:%d/hmi/main/index.html", gqlPort)
	resp, err := http.Get(hmiURL)
	if err != nil {
		t.Fatalf("get hmi: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected HMI status 200, got %d: %s", resp.StatusCode, string(body))
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "<h1>Localhost HMI</h1>" {
		t.Fatalf("unexpected HMI content: %s", string(body))
	}

	// 5. REST API endpoint without auth on 127.0.0.1
	restURL := fmt.Sprintf("http://127.0.0.1:%d/api/v1/docs", gqlPort)
	restResp, err := http.Get(restURL)
	if err != nil {
		t.Fatalf("get rest docs: %v", err)
	}
	defer restResp.Body.Close()
	if restResp.StatusCode != http.StatusOK {
		rBody, _ := io.ReadAll(restResp.Body)
		t.Fatalf("expected REST status 200, got %d: %s", restResp.StatusCode, string(rBody))
	}

	// 6. Explicitly invalid credentials provided on localhost should still be rejected
	badMqttOpts := mqtt.NewClientOptions()
	badMqttOpts.AddBroker(fmt.Sprintf("tcp://127.0.0.1:%d", mqttPort))
	badMqttOpts.SetClientID("bad-cred-client")
	badMqttOpts.SetUsername("nonexistent-user")
	badMqttOpts.SetPassword("wrong-pass")
	badMqttOpts.SetConnectTimeout(2 * time.Second)
	badMqttClient := mqtt.NewClient(badMqttOpts)
	badTok := badMqttClient.Connect()
	if badTok.WaitTimeout(2*time.Second) && badTok.Error() == nil {
		badMqttClient.Disconnect(100)
		t.Fatal("expected connection with invalid credentials to fail even on localhost")
	}
}

func TestLocalhostAuth_DeniedByDefault(t *testing.T) {
	hmiDir := t.TempDir()
	dashDir := filepath.Join(hmiDir, "main")
	if err := os.MkdirAll(dashDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dashDir, "index.html"), []byte("<h1>Localhost HMI</h1>"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	mqttPort := 23301
	gqlPort := 28301
	srv, _ := startWithGraphQL(t, mqttPort, gqlPort, func(c *config.Config) {
		c.UserManagement.Enabled = true
		c.UserManagement.AnonymousEnabled = false
		c.UserManagement.AllowAnonymousLocalhost = false // Default!
		c.HMI.Enabled = true
		c.HMI.Path = hmiDir
		c.HMI.MountPath = "/hmi"
		c.Features.Hmi = true
		c.RestApi.Enabled = true
	})
	defer srv.Close()

	// 1. MQTT anonymous connect from 127.0.0.1 should be rejected
	mqttOpts := mqtt.NewClientOptions()
	mqttOpts.AddBroker(fmt.Sprintf("tcp://127.0.0.1:%d", mqttPort))
	mqttOpts.SetClientID("anon-localhost-denied")
	mqttOpts.SetConnectTimeout(2 * time.Second)
	mqttOpts.SetCleanSession(true)

	client := mqtt.NewClient(mqttOpts)
	tok := client.Connect()
	if tok.WaitTimeout(2*time.Second) && tok.Error() == nil {
		client.Disconnect(100)
		t.Fatal("expected anonymous MQTT connect to fail when AllowAnonymousLocalhost is false")
	}

	// 2. GraphQL query without auth from 127.0.0.1 should be rejected
	localGqlURL := fmt.Sprintf("http://127.0.0.1:%d/graphql", gqlPort)
	res := gqlRequest(t, localGqlURL, `query { broker { version } }`, nil, "")
	if _, ok := res["errors"]; !ok {
		t.Fatalf("expected GraphQL query to fail when unauthenticated, got: %v", res)
	}

	// 3. HMI endpoint without auth from 127.0.0.1 should be rejected with 401
	hmiURL := fmt.Sprintf("http://127.0.0.1:%d/hmi/main/index.html", gqlPort)
	resp, err := http.Get(hmiURL)
	if err != nil {
		t.Fatalf("get hmi: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected HMI status 401 Unauthorized, got %d", resp.StatusCode)
	}

	// 4. REST API endpoint without auth from 127.0.0.1 should be rejected with 401
	restURL := fmt.Sprintf("http://127.0.0.1:%d/api/v1/docs", gqlPort)
	restResp, err := http.Get(restURL)
	if err != nil {
		t.Fatalf("get rest docs: %v", err)
	}
	defer restResp.Body.Close()
	if restResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected REST status 401 Unauthorized, got %d", restResp.StatusCode)
	}

	// 5. HMI with invalid token should be rejected with 401
	badTokenResp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/hmi/main/index.html?token=bad-token", gqlPort))
	if err != nil {
		t.Fatalf("get bad token hmi: %v", err)
	}
	defer badTokenResp.Body.Close()
	if badTokenResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected HMI status 401 for bad token, got %d", badTokenResp.StatusCode)
	}

	// 6. HMI with valid token should succeed with 200
	token := loginToken(t, localGqlURL, "Admin", "Admin")
	goodTokenResp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/hmi/main/index.html?token=%s", gqlPort, token))
	if err != nil {
		t.Fatalf("get good token hmi: %v", err)
	}
	defer goodTokenResp.Body.Close()
	if goodTokenResp.StatusCode != http.StatusOK {
		t.Fatalf("expected HMI status 200 for valid token, got %d", goodTokenResp.StatusCode)
	}
}
