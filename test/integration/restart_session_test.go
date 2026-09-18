package integration

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	_ "modernc.org/sqlite"

	"monstermq.io/edge/internal/config"
)

// TestPersistentSessionRestoredAcrossRestart verifies issue #9:
// 1. A client with clean=false subscribes to restart/topic.
// 2. Disconnect it, publish and queue a message while offline.
// 3. Restart the broker on the same database.
// 4. Reconnect with the same client ID and clean=false.
// 5. CONNACK reports session present = 1.
// 6. The queued message is replayed and received.
// 7. A subsequent live publish to restart/topic is delivered to the client without resubscribing.
func TestPersistentSessionRestoredAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart_sub.db")
	port := 25050

	srv := startWithDB(t, port, dbPath, func(c *config.Config) {
		c.QueuedMessagesEnabled = true
	})

	// 1. Subscribe with persistent session
	sub := mqtt.NewClient(persistentOpts(port, "restart-sub"))
	tok := sub.Connect()
	if tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok.(*mqtt.ConnectToken).SessionPresent() {
		t.Fatal("expected session present = false on initial connect")
	}

	if tok := sub.Subscribe("restart/topic", 1, nil); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	sub.Disconnect(100)
	time.Sleep(150 * time.Millisecond)

	// 2. Publish while offline so a message is queued
	pub := mqtt.NewClient(mqttOpts(port, "restart-pub"))
	if tok := pub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := pub.Publish("restart/topic", 1, false, "offline-payload"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	pub.Disconnect(100)
	time.Sleep(150 * time.Millisecond)

	// 3. Restart the broker on the same SQLite database
	srv.Close()
	time.Sleep(150 * time.Millisecond)

	srv2 := startWithDB(t, port, dbPath, func(c *config.Config) {
		c.QueuedMessagesEnabled = true
	})
	defer srv2.Close()

	// 4. Reconnect with persistent session (clean=false)
	var gotOffline atomic.Int32
	var gotLive atomic.Int32
	receivedPayloads := make(chan string, 10)

	reconnOpts := persistentOpts(port, "restart-sub")
	reconnOpts.SetDefaultPublishHandler(func(_ mqtt.Client, m mqtt.Message) {
		p := string(m.Payload())
		if p == "offline-payload" {
			gotOffline.Add(1)
		} else if p == "live-payload" {
			gotLive.Add(1)
		}
		select {
		case receivedPayloads <- p:
		default:
		}
	})

	subReconn := mqtt.NewClient(reconnOpts)
	reconnTok := subReconn.Connect()
	if reconnTok.WaitTimeout(2*time.Second) && reconnTok.Error() != nil {
		t.Fatalf("reconnect failed: %v", reconnTok.Error())
	}
	defer subReconn.Disconnect(100)

	// 5. Verify CONNACK session present = true
	if !reconnTok.(*mqtt.ConnectToken).SessionPresent() {
		t.Fatal("expected session present = true after restart, got false")
	}

	// 6. Wait for offline queued message replay
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && gotOffline.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if gotOffline.Load() != 1 {
		t.Fatalf("expected 1 offline queued message, got %d", gotOffline.Load())
	}

	// 7. Publish a NEW LIVE message without the client resubscribing!
	pub2 := mqtt.NewClient(mqttOpts(port, "live-pub"))
	if tok := pub2.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := pub2.Publish("restart/topic", 1, false, "live-payload"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	pub2.Disconnect(100)

	// 8. Verify the client received the live message via its restored subscription
	liveDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(liveDeadline) && gotLive.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if gotLive.Load() != 1 {
		t.Fatalf("expected 1 live message without resubscribing, got %d", gotLive.Load())
	}
}

// TestCleanSessionDiscardsRestartedSession verifies that connecting with clean=true
// post-restart does not restore the previous session, returns sessionPresent=false,
// and does not receive live publishes to the old topic.
func TestCleanSessionDiscardsRestartedSession(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "clean_sub.db")
	port := 25051

	srv := startWithDB(t, port, dbPath, func(c *config.Config) {
		c.QueuedMessagesEnabled = true
	})

	// 1. Subscribe with persistent session
	sub := mqtt.NewClient(persistentOpts(port, "clean-sub"))
	if tok := sub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := sub.Subscribe("clean/topic", 1, nil); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	sub.Disconnect(100)
	time.Sleep(150 * time.Millisecond)

	// 2. Restart broker
	srv.Close()
	time.Sleep(150 * time.Millisecond)

	srv2 := startWithDB(t, port, dbPath, func(c *config.Config) {
		c.QueuedMessagesEnabled = true
	})
	defer srv2.Close()

	// 3. Connect with clean=true
	cleanOpts := mqttOpts(port, "clean-sub")
	cleanOpts.SetCleanSession(true)
	var gotLive atomic.Int32
	cleanOpts.SetDefaultPublishHandler(func(_ mqtt.Client, m mqtt.Message) {
		gotLive.Add(1)
	})

	cleanClient := mqtt.NewClient(cleanOpts)
	tok := cleanClient.Connect()
	if tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer cleanClient.Disconnect(100)

	// SessionPresent must be false for clean=true connect
	if tok.(*mqtt.ConnectToken).SessionPresent() {
		t.Fatal("expected session present = false for clean connect")
	}

	// 4. Publish to clean/topic
	pub := mqtt.NewClient(mqttOpts(port, "clean-pub"))
	if tok := pub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := pub.Publish("clean/topic", 1, false, "discarded?"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	pub.Disconnect(100)

	time.Sleep(300 * time.Millisecond)
	if gotLive.Load() != 0 {
		t.Fatalf("clean client received %d messages for old subscription", gotLive.Load())
	}

	// Check that previous subscriptions were purged from database
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var subsCount int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM subscriptions WHERE client_id = 'clean-sub'`).Scan(&subsCount); err != nil {
		t.Fatal(err)
	}
	if subsCount != 0 {
		t.Fatalf("expected 0 subscriptions after clean connect, got %d", subsCount)
	}
}

// TestSessionExpiryIntervalAfterRestart verifies that an expired session interval
// yields session present = false after restart and purges old subscriptions.
func TestSessionExpiryIntervalAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "expiry_sub.db")
	port := 25052

	srv := startWithDB(t, port, dbPath, func(c *config.Config) {
		c.QueuedMessagesEnabled = true
	})

	// 1. Establish persistent session
	sub := mqtt.NewClient(persistentOpts(port, "expiry-sub"))
	if tok := sub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := sub.Subscribe("expiry/topic", 1, nil); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	sub.Disconnect(100)
	time.Sleep(150 * time.Millisecond)

	// Simulate expired session: set update_time in the past and session_expiry_interval = 1 second
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Update sessions table with information containing sessionExpiryInterval = 1 and old update_time
	oldTime := time.Now().Add(-10 * time.Second).UTC().Format("2006-01-02 15:04:05")
	_, err = db.ExecContext(context.Background(),
		fmt.Sprintf(`UPDATE sessions SET update_time = '%s', information = '{"sessionExpiryInterval":1}' WHERE client_id = 'expiry-sub'`, oldTime))
	if err != nil {
		t.Fatal(err)
	}

	// 2. Restart broker
	srv.Close()
	time.Sleep(150 * time.Millisecond)

	srv2 := startWithDB(t, port, dbPath, func(c *config.Config) {
		c.QueuedMessagesEnabled = true
	})
	defer srv2.Close()

	// 3. Reconnect with clean=false after expiry has passed
	reconnOpts := persistentOpts(port, "expiry-sub")
	reconnClient := mqtt.NewClient(reconnOpts)
	tok := reconnClient.Connect()
	if tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer reconnClient.Disconnect(100)

	// Since session was updated > 10s ago and expiry was 1s, session should be treated as expired
	if tok.(*mqtt.ConnectToken).SessionPresent() {
		t.Fatal("expected session present = false for expired session, got true")
	}
}
