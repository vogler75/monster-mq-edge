package integration

import (
	"context"
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
