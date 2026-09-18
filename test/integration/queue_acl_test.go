package integration

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	_ "modernc.org/sqlite"

	"monstermq.io/edge/internal/config"
)

// TestDeniedSubscriptionNotQueued verifies issue #8:
// 1. A client with restricted ACL (cannot subscribe to secret/#) connects with a persistent session (clean=false).
// 2. Client requests a persistent subscription to secret/topic.
// 3. Broker denies the subscription.
// 4. Broker must NOT persist the denied subscription in SQLite.
// 5. Client disconnects.
// 6. Publisher sends a message to secret/topic while client is offline.
// 7. Message must NOT be enqueued for the client in SQLite messagequeue.
// 8. On reconnect, the client receives 0 messages.
func TestDeniedSubscriptionNotQueued(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "queue_acl.db")
	mqttPort := 23410
	gqlPort := 28410

	srv, gqlURL := startWithGraphQL(t, mqttPort, gqlPort, func(c *config.Config) {
		c.SQLite.Path = dbPath
		c.QueuedMessagesEnabled = true
		c.UserManagement.Enabled = true
		c.UserManagement.AnonymousEnabled = false
		c.UserManagement.AllowAnonymousLocalhost = false
	})
	defer srv.Close()

	// 1. Set up users via GraphQL
	adminToken := loginToken(t, gqlURL, "Admin", "Admin")

	// Create restricted user
	gqlQueryAuth(t, gqlURL, `mutation { user {
		createUser(input: { username: "alice", password: "pw", canSubscribe: true, canPublish: true }) { success }
	} }`, nil, adminToken)

	// Allow alice on "allowed/#"
	gqlQueryAuth(t, gqlURL, `mutation { user {
		createAclRule(input: { username: "alice", topicPattern: "allowed/#", canSubscribe: true, canPublish: true, priority: 10 }) { success }
	} }`, nil, adminToken)

	// Deny alice on "secret/#"
	gqlQueryAuth(t, gqlURL, `mutation { user {
		createAclRule(input: { username: "alice", topicPattern: "secret/#", canSubscribe: false, canPublish: false, priority: 100 }) { success }
	} }`, nil, adminToken)

	// 2. Alice connects with persistent session (clean=false)
	aliceOpts := persistentOpts(mqttPort, "alice-client")
	aliceOpts.SetUsername("alice")
	aliceOpts.SetPassword("pw")

	alice := mqtt.NewClient(aliceOpts)
	if tok := alice.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("alice connect failed: %v", tok.Error())
	}

	// 3. Alice attempts to subscribe to secret/topic
	subTok := alice.Subscribe("secret/topic", 1, nil)
	subTok.WaitTimeout(2 * time.Second)

	// Also subscribe to allowed/topic (which should succeed)
	if tok := alice.Subscribe("allowed/topic", 1, nil); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("alice subscribe to allowed/topic failed: %v", tok.Error())
	}

	alice.Disconnect(100)
	time.Sleep(200 * time.Millisecond)

	// 4. Verify SQLite subscriptions table:
	// secret/topic must NOT be persisted; allowed/topic MUST be persisted.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	var secretSubs int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM subscriptions WHERE client_id = 'alice-client' AND topic = 'secret/topic'`).Scan(&secretSubs); err != nil {
		t.Fatalf("query secret subscriptions: %v", err)
	}
	if secretSubs != 0 {
		t.Fatalf("rejected subscription secret/topic was persisted! count=%d", secretSubs)
	}

	var allowedSubs int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM subscriptions WHERE client_id = 'alice-client' AND topic = 'allowed/topic'`).Scan(&allowedSubs); err != nil {
		t.Fatalf("query allowed subscriptions: %v", err)
	}
	if allowedSubs != 1 {
		t.Fatalf("expected 1 allowed subscription, got %d", allowedSubs)
	}

	// 5. Publisher (Admin) publishes while Alice is offline
	adminOpts := mqttOpts(mqttPort, "admin-pub")
	adminOpts.SetUsername("Admin")
	adminOpts.SetPassword("Admin")
	adminPub := mqtt.NewClient(adminOpts)
	if tok := adminPub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("admin connect failed: %v", tok.Error())
	}
	if tok := adminPub.Publish("secret/topic", 1, false, "confidential"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("admin publish secret failed: %v", tok.Error())
	}
	if tok := adminPub.Publish("allowed/topic", 1, false, "welcome"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("admin publish allowed failed: %v", tok.Error())
	}
	adminPub.Disconnect(100)
	time.Sleep(200 * time.Millisecond)

	// 6. Verify SQLite messagequeue table:
	// secret/topic must NOT be queued; allowed/topic SHOULD be queued.
	var queuedSecret int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messagequeue WHERE client_id = 'alice-client' AND topic = 'secret/topic'`).Scan(&queuedSecret); err != nil {
		t.Fatalf("query queued secret: %v", err)
	}
	if queuedSecret != 0 {
		t.Fatalf("message for secret/topic was queued for alice! count=%d", queuedSecret)
	}

	var queuedAllowed int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messagequeue WHERE client_id = 'alice-client' AND topic = 'allowed/topic'`).Scan(&queuedAllowed); err != nil {
		t.Fatalf("query queued allowed: %v", err)
	}
	if queuedAllowed != 1 {
		t.Fatalf("expected 1 queued message for allowed/topic, got %d", queuedAllowed)
	}

	// 7. Alice reconnects and should receive only the allowed message, NEVER the secret message
	var receivedSecret atomic.Int32
	var receivedAllowed atomic.Int32
	reconnOpts := persistentOpts(mqttPort, "alice-client")
	reconnOpts.SetUsername("alice")
	reconnOpts.SetPassword("pw")
	reconnOpts.SetDefaultPublishHandler(func(_ mqtt.Client, m mqtt.Message) {
		if m.Topic() == "secret/topic" {
			receivedSecret.Add(1)
		} else if m.Topic() == "allowed/topic" {
			receivedAllowed.Add(1)
		}
	})

	aliceReconn := mqtt.NewClient(reconnOpts)
	if tok := aliceReconn.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("alice reconnect failed: %v", tok.Error())
	}
	defer aliceReconn.Disconnect(100)

	// Wait up to 1 second for queue delivery
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && receivedAllowed.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}

	if receivedSecret.Load() != 0 {
		t.Fatalf("alice received %d secret messages on reconnect!", receivedSecret.Load())
	}
	if receivedAllowed.Load() != 1 {
		t.Fatalf("alice expected 1 allowed message, got %d", receivedAllowed.Load())
	}
}

// TestQueuedReplayAclDefenseInDepth verifies defense in depth:
// If an unauthorized message is queued (e.g. injected or permissions revoked while offline),
// QueueHook.OnSessionEstablished drops the message on replay, acks it from the DB queue,
// and does not deliver it to the reconnected client.
func TestQueuedReplayAclDefenseInDepth(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "queue_defense.db")
	mqttPort := 23411
	gqlPort := 28411

	srv, gqlURL := startWithGraphQL(t, mqttPort, gqlPort, func(c *config.Config) {
		c.SQLite.Path = dbPath
		c.QueuedMessagesEnabled = true
		c.UserManagement.Enabled = true
		c.UserManagement.AnonymousEnabled = false
		c.UserManagement.AllowAnonymousLocalhost = false
	})
	defer srv.Close()

	adminToken := loginToken(t, gqlURL, "Admin", "Admin")

	// Create user "bob"
	gqlQueryAuth(t, gqlURL, `mutation { user {
		createUser(input: { username: "bob", password: "pw", canSubscribe: true, canPublish: true }) { success }
	} }`, nil, adminToken)

	// Bob initially has permission on "data/#"
	gqlQueryAuth(t, gqlURL, `mutation { user {
		createAclRule(input: { username: "bob", topicPattern: "data/#", canSubscribe: true, canPublish: true, priority: 10 }) { success }
	} }`, nil, adminToken)

	// Bob connects with clean=false and subscribes to data/metric
	bobOpts := persistentOpts(mqttPort, "bob-client")
	bobOpts.SetUsername("bob")
	bobOpts.SetPassword("pw")

	bob := mqtt.NewClient(bobOpts)
	if tok := bob.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("bob connect failed: %v", tok.Error())
	}
	if tok := bob.Subscribe("data/metric", 1, nil); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("bob subscribe failed: %v", tok.Error())
	}
	bob.Disconnect(100)
	time.Sleep(200 * time.Millisecond)

	// Admin publishes to data/metric while Bob is offline -> message gets queued
	adminOpts := mqttOpts(mqttPort, "admin-pub-defense")
	adminOpts.SetUsername("Admin")
	adminOpts.SetPassword("Admin")
	adminPub := mqtt.NewClient(adminOpts)
	if tok := adminPub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("admin connect failed: %v", tok.Error())
	}
	if tok := adminPub.Publish("data/metric", 1, false, "secret-metric"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("admin publish failed: %v", tok.Error())
	}
	adminPub.Disconnect(100)
	time.Sleep(200 * time.Millisecond)

	// Verify message landed in queue
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var queuedCount int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messagequeue WHERE client_id = 'bob-client'`).Scan(&queuedCount); err != nil {
		t.Fatalf("query queued: %v", err)
	}
	if queuedCount != 1 {
		t.Fatalf("expected 1 queued message, got %d", queuedCount)
	}

	// Now revoke Bob's permission on data/# while he is still offline!
	gqlQueryAuth(t, gqlURL, `mutation { user {
		createAclRule(input: { username: "bob", topicPattern: "data/#", canSubscribe: false, canPublish: false, priority: 100 }) { success }
	} }`, nil, adminToken)

	// Bob reconnects
	var received atomic.Int32
	reconnOpts := persistentOpts(mqttPort, "bob-client")
	reconnOpts.SetUsername("bob")
	reconnOpts.SetPassword("pw")
	reconnOpts.SetDefaultPublishHandler(func(_ mqtt.Client, m mqtt.Message) {
		received.Add(1)
	})

	bobReconn := mqtt.NewClient(reconnOpts)
	if tok := bobReconn.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("bob reconnect failed: %v", tok.Error())
	}
	defer bobReconn.Disconnect(100)

	time.Sleep(600 * time.Millisecond)

	// Defense in depth: Bob must NOT have received the message
	if received.Load() != 0 {
		t.Fatalf("bob received %d messages after permission was revoked!", received.Load())
	}

	// And the unauthorized message was acked (removed) from the queue so it doesn't linger
	var remainingCount int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messagequeue WHERE client_id = 'bob-client'`).Scan(&remainingCount); err != nil {
		t.Fatalf("query remaining: %v", err)
	}
	if remainingCount != 0 {
		t.Fatalf("expected message to be acked and removed from queue, but %d remain", remainingCount)
	}
}

// TestExistingRejectedSubscriptionPrunedOnConnect verifies acceptance criterion 3:
// Existing rejected/unauthorized subscriptions in the database are identified and removed on connect.
func TestExistingRejectedSubscriptionPrunedOnConnect(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "queue_prune.db")
	mqttPort := 23412
	gqlPort := 28412

	srv, gqlURL := startWithGraphQL(t, mqttPort, gqlPort, func(c *config.Config) {
		c.SQLite.Path = dbPath
		c.QueuedMessagesEnabled = true
		c.UserManagement.Enabled = true
		c.UserManagement.AnonymousEnabled = false
		c.UserManagement.AllowAnonymousLocalhost = false
	})
	defer srv.Close()

	adminToken := loginToken(t, gqlURL, "Admin", "Admin")

	// Create user "charlie"
	gqlQueryAuth(t, gqlURL, `mutation { user {
		createUser(input: { username: "charlie", password: "pw", canSubscribe: true, canPublish: true }) { success }
	} }`, nil, adminToken)

	// Charlie allowed only on allowed/#
	gqlQueryAuth(t, gqlURL, `mutation { user {
		createAclRule(input: { username: "charlie", topicPattern: "allowed/#", canSubscribe: true, canPublish: true, priority: 10 }) { success }
	} }`, nil, adminToken)
	gqlQueryAuth(t, gqlURL, `mutation { user {
		createAclRule(input: { username: "charlie", topicPattern: "forbidden/#", canSubscribe: false, canPublish: false, priority: 100 }) { success }
	} }`, nil, adminToken)

	// Directly insert a legacy unauthorized subscription into SQLite subscriptions table
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	_, err = db.ExecContext(context.Background(),
		`INSERT INTO subscriptions (client_id, topic, qos) VALUES ('charlie-client', 'forbidden/topic', 1)`)
	if err != nil {
		t.Fatalf("insert legacy subscription: %v", err)
	}
	_, err = db.ExecContext(context.Background(),
		`INSERT INTO subscriptions (client_id, topic, qos) VALUES ('charlie-client', 'allowed/topic', 1)`)
	if err != nil {
		t.Fatalf("insert allowed subscription: %v", err)
	}

	// Charlie connects with clean=false
	charlieOpts := persistentOpts(mqttPort, "charlie-client")
	charlieOpts.SetUsername("charlie")
	charlieOpts.SetPassword("pw")

	charlie := mqtt.NewClient(charlieOpts)
	if tok := charlie.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("charlie connect failed: %v", tok.Error())
	}
	defer charlie.Disconnect(100)

	time.Sleep(300 * time.Millisecond)

	// Verify that forbidden/topic was pruned from the database!
	var forbiddenCount int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM subscriptions WHERE client_id = 'charlie-client' AND topic = 'forbidden/topic'`).Scan(&forbiddenCount); err != nil {
		t.Fatalf("query forbidden: %v", err)
	}
	if forbiddenCount != 0 {
		t.Fatalf("expected legacy forbidden subscription to be pruned, got %d", forbiddenCount)
	}

	var allowedCount int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM subscriptions WHERE client_id = 'charlie-client' AND topic = 'allowed/topic'`).Scan(&allowedCount); err != nil {
		t.Fatalf("query allowed: %v", err)
	}
	if allowedCount != 1 {
		t.Fatalf("expected allowed subscription to remain, got %d", allowedCount)
	}
}
