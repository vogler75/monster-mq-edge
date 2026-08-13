package integration

import (
	"sort"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

func TestSearchTopicsWildcards(t *testing.T) {
	srv, url := startWithGraphQL(t, 23021, 28021)
	defer srv.Close()

	pub := mqtt.NewClient(mqttOpts(23021, "search-pub"))
	if tok := pub.Connect(); tok.WaitTimeout(2 * time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	topics := []string{
		"sensors/Watt/power",
		"building1/WattMeter",
		"sensors/temperature",
		"voltage/Watt",
	}
	for _, topic := range topics {
		if tok := pub.Publish(topic, 0, false, "v"); tok.WaitTimeout(2 * time.Second) && tok.Error() != nil {
			t.Fatal(tok.Error())
		}
	}
	pub.Disconnect(100)
	time.Sleep(500 * time.Millisecond) // archive group flush

	search := func(pattern string) []string {
		data := gqlQuery(t, url, `query Q($p:String!){ searchTopics(pattern:$p, archiveGroup:"Default") }`,
			map[string]any{"p": pattern})
		raw := data["searchTopics"].([]any)
		out := make([]string, len(raw))
		for i, r := range raw {
			out[i] = r.(string)
		}
		sort.Strings(out)
		return out
	}

	// 1. Wildcard *Watt* should match all topics containing "Watt"
	gotWattWildcard := search("*Watt*")
	expectedWatt := []string{
		"building1/WattMeter",
		"sensors/Watt/power",
		"voltage/Watt",
	}
	if len(gotWattWildcard) != len(expectedWatt) {
		t.Fatalf("*Watt*: expected %v, got %v", expectedWatt, gotWattWildcard)
	}
	for i := range expectedWatt {
		if gotWattWildcard[i] != expectedWatt[i] {
			t.Fatalf("*Watt*[%d]: expected %s, got %s", i, expectedWatt[i], gotWattWildcard[i])
		}
	}

	// 2. Substring "Watt" without wildcards should also match all topics containing "Watt"
	gotWattSubstring := search("Watt")
	if len(gotWattSubstring) != len(expectedWatt) {
		t.Fatalf("Watt: expected %v, got %v", expectedWatt, gotWattSubstring)
	}

	// 3. Case-insensitive search "watt"
	gotLowerWatt := search("watt")
	if len(gotLowerWatt) != len(expectedWatt) {
		t.Fatalf("watt: expected %v, got %v", expectedWatt, gotLowerWatt)
	}

	// 4. Prefix search "sensors/*"
	gotSensorsPrefix := search("sensors/*")
	expectedSensors := []string{
		"sensors/Watt/power",
		"sensors/temperature",
	}
	if len(gotSensorsPrefix) != len(expectedSensors) {
		t.Fatalf("sensors/*: expected %v, got %v", expectedSensors, gotSensorsPrefix)
	}
	for i := range expectedSensors {
		if gotSensorsPrefix[i] != expectedSensors[i] {
			t.Fatalf("sensors/*[%d]: expected %s, got %s", i, expectedSensors[i], gotSensorsPrefix[i])
		}
	}
}
