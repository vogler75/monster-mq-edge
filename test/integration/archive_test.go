package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"monstermq.io/edge/internal/stores"
)

// TestDefaultArchiveGroupUsesMemoryLastValue confirms the Default archive group
// is created on startup with in-memory last-value storage and no history archive.
func TestDefaultArchiveGroupUsesMemoryLastValue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	port := 22003
	srv := startWithDB(t, port, dbPath, nil)
	defer srv.Close()

	client := mqtt.NewClient(mqttOpts(port, "ar-pub"))
	if tok := client.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	for i := 0; i < 5; i++ {
		if tok := client.Publish("sensor/temp", 0, false, "21.5"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
			t.Fatal(tok.Error())
		}
	}
	client.Disconnect(100)

	// Wait long enough for the archive group ticker (250ms) to flush.
	time.Sleep(600 * time.Millisecond)

	var def stores.MessageStore
	for _, group := range srv.Archives().Snapshot() {
		if group.Name() == "Default" {
			if group.Config().LastValType != stores.MessageStoreMemory {
				t.Fatalf("Default lastValType = %s, want MEMORY", group.Config().LastValType)
			}
			if group.Config().ArchiveType != stores.ArchiveNone {
				t.Fatalf("Default archiveType = %s, want NONE", group.Config().ArchiveType)
			}
			if group.Archive() != nil {
				t.Fatal("Default archive store should be nil")
			}
			def = group.LastValue()
			break
		}
	}
	if def == nil {
		t.Fatal("Default last-value store missing")
	}
	msg, err := def.Get(context.Background(), "sensor/temp")
	if err != nil {
		t.Fatal(err)
	}
	if msg == nil || string(msg.Payload) != "21.5" {
		t.Fatalf("Default last-value message = %#v", msg)
	}
}

func TestDefaultArchiveGroupEmptyMessageDeletesLastValue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	port := 22004
	srv := startWithDB(t, port, dbPath, nil)
	defer srv.Close()

	client := mqtt.NewClient(mqttOpts(port, "ar-empty-pub"))
	if tok := client.Connect(); tok.WaitTimeout(2 * time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}

	// 1. Publish a normal message
	if tok := client.Publish("sensor/humidity", 0, false, "65%"); tok.WaitTimeout(2 * time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	time.Sleep(200 * time.Millisecond)

	var def stores.MessageStore
	for _, group := range srv.Archives().Snapshot() {
		if group.Name() == "Default" {
			def = group.LastValue()
			break
		}
	}
	if def == nil {
		t.Fatal("Default last-value store missing")
	}

	msg, err := def.Get(context.Background(), "sensor/humidity")
	if err != nil {
		t.Fatal(err)
	}
	if msg == nil || string(msg.Payload) != "65%" {
		t.Fatalf("Expected LastValue '65%%', got %#v", msg)
	}

	// 2. Publish an empty message (tombstone)
	if tok := client.Publish("sensor/humidity", 0, false, ""); tok.WaitTimeout(2 * time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	time.Sleep(200 * time.Millisecond)
	client.Disconnect(100)

	msgDeleted, err := def.Get(context.Background(), "sensor/humidity")
	if err != nil {
		t.Fatal(err)
	}
	if msgDeleted != nil {
		t.Fatalf("Expected LastValue entry for 'sensor/humidity' to be deleted, got %#v", msgDeleted)
	}
}

func TestAggregatedMessagesGraphQLQuery(t *testing.T) {
	mqttPort := 22005
	gqlPort := 24005
	srv, gqlURL := startWithGraphQL(t, mqttPort, gqlPort)
	defer srv.Close()

	ctx := context.Background()
	err := srv.Storage().ArchiveConfig.Save(ctx, stores.ArchiveGroupConfig{
		Name:         "Industrial",
		Enabled:      true,
		TopicFilters: []string{"factory/#"},
		LastValType:  stores.MessageStoreSQLite,
		ArchiveType:  stores.ArchiveSQLite,
	})
	if err != nil {
		t.Fatalf("Save ArchiveGroupConfig failed: %v", err)
	}
	if err := srv.Archives().Reload(ctx); err != nil {
		t.Fatalf("Reload Archives failed: %v", err)
	}

	client := mqtt.NewClient(mqttOpts(mqttPort, "agg-pub"))
	if tok := client.Connect(); tok.WaitTimeout(2 * time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}

	for i := 0; i < 5; i++ {
		payload := fmt.Sprintf(`{"temp": %d}`, 20+i)
		if tok := client.Publish("factory/temp", 0, false, payload); tok.WaitTimeout(2 * time.Second) && tok.Error() != nil {
			t.Fatal(tok.Error())
		}
	}
	client.Disconnect(100)

	time.Sleep(600 * time.Millisecond)

	now := time.Now().UTC()
	start := now.Add(-1 * time.Hour).Format(time.RFC3339)
	end := now.Add(1 * time.Hour).Format(time.RFC3339)

	query := fmt.Sprintf(`{
		aggregatedMessages(
			topics: ["factory/temp"],
			interval: FIVE_MINUTES,
			startTime: "%s",
			endTime: "%s",
			functions: [AVG, MAX],
			fields: ["temp"],
			archiveGroup: "Industrial"
		) {
			columns
			rows
			rowCount
		}
	}`, start, end)

	resp := gqlQuery(t, gqlURL, query, nil)
	aggRes, ok := resp["aggregatedMessages"].(map[string]any)
	if !ok {
		t.Fatalf("invalid aggregatedMessages result: %#v", resp)
	}

	columns, _ := aggRes["columns"].([]any)
	rowCount := int(aggRes["rowCount"].(float64))

	if len(columns) != 3 {
		t.Fatalf("expected 3 columns, got %#v", columns)
	}
	if rowCount == 0 {
		t.Fatalf("expected rowCount > 0, got %d", rowCount)
	}
}

