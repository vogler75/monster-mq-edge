package integration

import (
	"fmt"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"monstermq.io/edge/internal/config"
)

// TestAnonymousUserRecordRejectedWhenAnonymousDisabled: the broker creates a
// passwordless Anonymous user; with AnonymousEnabled=false nobody may log in
// as that user, over MQTT or GraphQL.
func TestAnonymousUserRecordRejectedWhenAnonymousDisabled(t *testing.T) {
	mqttPort, gqlPort := 23008, 28008
	srv, url := startWithGraphQL(t, mqttPort, gqlPort, func(c *config.Config) {
		c.UserManagement.Enabled = true
		c.UserManagement.AnonymousEnabled = false
		c.UserManagement.AllowAnonymousLocalhost = false
	})
	defer srv.Close()

	opts := mqtt.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://127.0.0.1:%d", mqttPort)).
		SetClientID("anonymous-by-name").
		SetUsername("Anonymous").
		SetConnectTimeout(2 * time.Second)
	client := mqtt.NewClient(opts)
	if tok := client.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() == nil {
		client.Disconnect(100)
		t.Error("MQTT CONNECT as Anonymous should fail when AnonymousEnabled is false")
	}

	data := gqlQuery(t, url, `mutation { login(username: "Anonymous", password: "") { success token } }`, nil)
	if login := data["login"].(map[string]any); login["success"].(bool) {
		t.Errorf("GraphQL login as Anonymous should fail when AnonymousEnabled is false, got %v", login)
	}
}
