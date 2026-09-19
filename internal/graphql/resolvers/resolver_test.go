package resolvers

import (
	"context"
	"testing"

	"monstermq.io/edge/internal/config"
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

func TestDatabaseConnectionTypeSQLiteMapping(t *testing.T) {
	gt := toDatabaseConnectionType("SQLITE")
	if gt != generated.DatabaseConnectionTypeSQLIte {
		t.Fatalf("toDatabaseConnectionType(SQLITE) = %v, want SQLITE", gt)
	}

	st := fromDatabaseConnectionType(generated.DatabaseConnectionTypeSQLIte)
	if st != "SQLITE" {
		t.Fatalf("fromDatabaseConnectionType(SQLITE) = %v, want SQLITE", st)
	}
}

func TestScriptDocumentationAndSkill(t *testing.T) {
	r := &Resolver{
		Cfg: &config.Config{
			Features: config.FeaturesConfig{
				PythonScripts: true,
			},
		},
	}
	qr := &queryResolver{r}
	ctx := context.Background()

	langs, err := qr.ScriptLanguages(ctx)
	if err != nil {
		t.Fatalf("ScriptLanguages error: %v", err)
	}
	if len(langs) == 0 {
		t.Fatalf("expected at least 1 script language")
	}
	star := langs[0]
	if star.Name != "starlark" {
		t.Errorf("expected language starlark, got %s", star.Name)
	}
	if len(star.Documentation) == 0 {
		t.Errorf("expected non-empty documentation for starlark")
	}
	if len(star.Skill) == 0 {
		t.Errorf("expected non-empty skill for starlark")
	}

	doc, err := qr.ScriptDocumentation(ctx, nil)
	if err != nil {
		t.Fatalf("ScriptDocumentation error: %v", err)
	}
	if len(doc) == 0 {
		t.Errorf("expected non-empty default ScriptDocumentation")
	}

	skill, err := qr.ScriptSkill(ctx, nil)
	if err != nil {
		t.Fatalf("ScriptSkill error: %v", err)
	}
	if len(skill) == 0 {
		t.Errorf("expected non-empty default ScriptSkill")
	}
}


