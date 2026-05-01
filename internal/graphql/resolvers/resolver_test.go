package resolvers

import (
	"testing"

	"monstermq.io/edge/internal/graphql/generated"
)

func TestMqttClientConfigUpdatePreservesAddressesWhenOmitted(t *testing.T) {
	existing := `{"brokerUrl":"tcp://old","password":"secret","addresses":[{"mode":"PUBLISH","remoteTopic":"remote/#","localTopic":"local/#","removePath":true}]}`
	clientID := "client"
	cfg := mqttClientConfigInputToMergedMap(&generated.MqttClientConnectionConfigInput{
		BrokerURL: "tcp://new",
		ClientID:  &clientID,
	}, existing)

	addrs, ok := cfg["addresses"].([]any)
	if !ok || len(addrs) != 1 {
		t.Fatalf("addresses = %#v, want one preserved address", cfg["addresses"])
	}
	if cfg["password"] != "secret" {
		t.Fatalf("password = %#v, want preserved secret", cfg["password"])
	}
}

func TestMqttClientConfigUpdateCanClearAddresses(t *testing.T) {
	existing := `{"addresses":[{"mode":"PUBLISH","remoteTopic":"remote/#","localTopic":"local/#","removePath":true}]}`
	cfg := mqttClientConfigInputToMergedMap(&generated.MqttClientConnectionConfigInput{
		BrokerURL: "tcp://new",
		Addresses: []*generated.MqttClientAddressInput{},
	}, existing)

	addrs, ok := cfg["addresses"].([]map[string]any)
	if !ok || len(addrs) != 0 {
		t.Fatalf("addresses = %#v, want explicit empty address list", cfg["addresses"])
	}
}
