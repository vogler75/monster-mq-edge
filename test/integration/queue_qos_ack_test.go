package integration

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	_ "modernc.org/sqlite"

	"monstermq.io/edge/internal/config"
)

func sendRawConnect(conn net.Conn, clientID string) error {
	var payload []byte
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(clientID)))
	payload = append(payload, []byte(clientID)...)

	var vheader []byte
	vheader = append(vheader, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x04) // protocol name & version 3.1.1
	vheader = append(vheader, 0x00)                                 // connect flags: clean=false
	vheader = append(vheader, 0x00, 0x3c)                           // keepalive 60s

	body := append(vheader, payload...)
	var pkt []byte
	pkt = append(pkt, 0x10) // CONNECT
	pkt = encodeMqttLength(pkt, len(body))
	pkt = append(pkt, body...)
	_, err := conn.Write(pkt)
	return err
}

func readRawPacket(conn net.Conn) (byte, []byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	header := make([]byte, 1)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}
	remLen, err := decodeMqttLength(conn)
	if err != nil {
		return 0, nil, err
	}
	body := make([]byte, remLen)
	if _, err := io.ReadFull(conn, body); err != nil {
		return 0, nil, err
	}
	return header[0], body, nil
}

func encodeMqttLength(buf []byte, l int) []byte {
	for {
		digit := byte(l % 128)
		l /= 128
		if l > 0 {
			digit |= 0x80
		}
		buf = append(buf, digit)
		if l == 0 {
			break
		}
	}
	return buf
}

func decodeMqttLength(r io.Reader) (int, error) {
	multiplier := 1
	value := 0
	b := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, b); err != nil {
			return 0, err
		}
		digit := b[0]
		value += int(digit&127) * multiplier
		multiplier *= 128
		if digit&128 == 0 {
			break
		}
	}
	return value, nil
}

func sendRawPubrec(conn net.Conn, packetID uint16) error {
	pkt := []byte{0x50, 0x02, byte(packetID >> 8), byte(packetID & 0xff)}
	_, err := conn.Write(pkt)
	return err
}

func sendRawPubcomp(conn net.Conn, packetID uint16) error {
	pkt := []byte{0x70, 0x02, byte(packetID >> 8), byte(packetID & 0xff)}
	_, err := conn.Write(pkt)
	return err
}

func extractRawPublish(header byte, body []byte) (topic string, packetID uint16, payload string, err error) {
	qos := (header >> 1) & 0x03
	offset := 0
	if len(body) < 2 {
		return "", 0, "", fmt.Errorf("body too short for topic length")
	}
	tlen := int(binary.BigEndian.Uint16(body[:2]))
	offset += 2
	if len(body) < offset+tlen {
		return "", 0, "", fmt.Errorf("body too short for topic string")
	}
	topic = string(body[offset : offset+tlen])
	offset += tlen
	if qos > 0 {
		if len(body) < offset+2 {
			return "", 0, "", fmt.Errorf("body too short for packetID")
		}
		packetID = binary.BigEndian.Uint16(body[offset : offset+2])
		offset += 2
	}
	payload = string(body[offset:])
	return topic, packetID, payload, nil
}

func countQueuedRows(t *testing.T, dbPath, clientID string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	err = db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messagequeue WHERE client_id = ?`, clientID).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// TestQueuedQoS1NotDeletedBeforePuback verifies that:
// 1. A persistent client subscribes to a QoS 1 topic and disconnects.
// 2. A QoS 1 message is published and queued in storage.
// 3. When the client reconnects, the message is written, but remains in storage before PUBACK.
// 4. Disconnecting before sending PUBACK keeps the message in storage.
// 5. When the client reconnects and sends PUBACK, the row is deleted.
func TestQueuedQoS1NotDeletedBeforePuback(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "qos1_ack.db")
	port := 25060

	srv := startWithDB(t, port, dbPath, func(c *config.Config) {
		c.QueuedMessagesEnabled = true
	})
	defer srv.Close()

	clientID := "sub-qos1-ack"

	// 1. Subscribe persistent session (QoS 1)
	sub := mqtt.NewClient(persistentOpts(port, clientID))
	if tok := sub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := sub.Subscribe("qos1/test", 1, nil); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	sub.Disconnect(100)
	time.Sleep(150 * time.Millisecond)

	// 2. Publish while offline
	pub := mqtt.NewClient(mqttOpts(port, "pub-qos1"))
	if tok := pub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := pub.Publish("qos1/test", 1, false, "msg-qos1-data"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	pub.Disconnect(100)
	time.Sleep(150 * time.Millisecond)

	if n := countQueuedRows(t, dbPath, clientID); n != 1 {
		t.Fatalf("expected 1 row in queue, got %d", n)
	}

	// 3. Connect raw TCP client, read PUBLISH, but DO NOT send PUBACK
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	if err := sendRawConnect(conn, clientID); err != nil {
		conn.Close()
		t.Fatal(err)
	}

	// Read CONNACK (0x20)
	pktType, _, err := readRawPacket(conn)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if pktType != 0x20 {
		conn.Close()
		t.Fatalf("expected CONNACK (0x20), got 0x%02x", pktType)
	}

	// Read PUBLISH (0x32: QoS 1)
	pktType, body, err := readRawPacket(conn)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if (pktType & 0xF0) != 0x30 {
		conn.Close()
		t.Fatalf("expected PUBLISH (0x30..), got 0x%02x", pktType)
	}
	topic, _, payload, err := extractRawPublish(pktType, body)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if topic != "qos1/test" || payload != "msg-qos1-data" {
		conn.Close()
		t.Fatalf("unexpected topic/payload: %s / %s", topic, payload)
	}

	// 4. Check DB row count: MUST STILL BE 1 (before PUBACK)
	time.Sleep(100 * time.Millisecond)
	if n := countQueuedRows(t, dbPath, clientID); n != 1 {
		conn.Close()
		t.Fatalf("expected 1 row in queue before PUBACK, got %d", n)
	}

	// 5. Close connection WITHOUT sending PUBACK
	conn.Close()
	time.Sleep(150 * time.Millisecond)

	// Verify message still exists in queue
	if n := countQueuedRows(t, dbPath, clientID); n != 1 {
		t.Fatalf("expected 1 row in queue after disconnect without PUBACK, got %d", n)
	}

	// 6. Reconnect with paho client (which automatically sends PUBACK)
	var gotMsg atomic.Int32
	opts := persistentOpts(port, clientID)
	opts.SetDefaultPublishHandler(func(_ mqtt.Client, m mqtt.Message) {
		if string(m.Payload()) == "msg-qos1-data" {
			gotMsg.Add(1)
		}
	})
	reconnClient := mqtt.NewClient(opts)
	if tok := reconnClient.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer reconnClient.Disconnect(100)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && gotMsg.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if gotMsg.Load() != 1 {
		t.Fatalf("expected redelivered message, got %d", gotMsg.Load())
	}

	// 7. Verify queue row is deleted after PUBACK
	time.Sleep(150 * time.Millisecond)
	if n := countQueuedRows(t, dbPath, clientID); n != 0 {
		t.Fatalf("expected 0 rows in queue after PUBACK, got %d", n)
	}
}

// TestQueuedQoS2NotDeletedBeforePubcomp verifies that:
// 1. A persistent client subscribes to a QoS 2 topic and disconnects.
// 2. A QoS 2 message is published and queued in storage.
// 3. Client reconnects, receives PUBLISH (QoS 2), sends PUBREC, receives PUBREL.
// 4. Message row remains in storage after PUBREC and PUBREL.
// 5. Client disconnects without sending PUBCOMP; row remains in storage.
// 6. Client reconnects and completes the flow with PUBCOMP; row is deleted.
func TestQueuedQoS2NotDeletedBeforePubcomp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "qos2_ack.db")
	port := 25061

	srv := startWithDB(t, port, dbPath, func(c *config.Config) {
		c.QueuedMessagesEnabled = true
	})
	defer srv.Close()

	clientID := "sub-qos2-ack"

	// 1. Subscribe persistent session (QoS 2)
	sub := mqtt.NewClient(persistentOpts(port, clientID))
	if tok := sub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := sub.Subscribe("qos2/test", 2, nil); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	sub.Disconnect(100)
	time.Sleep(150 * time.Millisecond)

	// 2. Publish while offline
	pub := mqtt.NewClient(mqttOpts(port, "pub-qos2"))
	if tok := pub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := pub.Publish("qos2/test", 2, false, "msg-qos2-data"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	pub.Disconnect(100)
	time.Sleep(150 * time.Millisecond)

	if n := countQueuedRows(t, dbPath, clientID); n != 1 {
		t.Fatalf("expected 1 row in queue, got %d", n)
	}

	// 3. Connect raw TCP client, read PUBLISH (QoS 2)
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	if err := sendRawConnect(conn, clientID); err != nil {
		conn.Close()
		t.Fatal(err)
	}

	pktType, _, err := readRawPacket(conn)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if pktType != 0x20 {
		conn.Close()
		t.Fatalf("expected CONNACK (0x20), got 0x%02x", pktType)
	}

	pktType, body, err := readRawPacket(conn)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if (pktType & 0xF0) != 0x30 {
		conn.Close()
		t.Fatalf("expected PUBLISH (0x30..), got 0x%02x", pktType)
	}
	topic, pid, payload, err := extractRawPublish(pktType, body)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if topic != "qos2/test" || payload != "msg-qos2-data" {
		conn.Close()
		t.Fatalf("unexpected topic/payload: %s / %s", topic, payload)
	}

	// Check DB row count: MUST STILL BE 1
	time.Sleep(100 * time.Millisecond)
	if n := countQueuedRows(t, dbPath, clientID); n != 1 {
		conn.Close()
		t.Fatalf("expected 1 row in queue before PUBREC, got %d", n)
	}

	// 4. Send PUBREC (0x50), read PUBREL (0x62)
	if err := sendRawPubrec(conn, pid); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	relType, _, err := readRawPacket(conn)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if (relType & 0xF0) != 0x60 {
		conn.Close()
		t.Fatalf("expected PUBREL (0x60..), got 0x%02x", relType)
	}

	// Check DB row count: MUST STILL BE 1 before PUBCOMP
	time.Sleep(100 * time.Millisecond)
	if n := countQueuedRows(t, dbPath, clientID); n != 1 {
		conn.Close()
		t.Fatalf("expected 1 row in queue after PUBREL (before PUBCOMP), got %d", n)
	}

	// 5. Close connection WITHOUT sending PUBCOMP
	conn.Close()
	time.Sleep(150 * time.Millisecond)

	if n := countQueuedRows(t, dbPath, clientID); n != 1 {
		t.Fatalf("expected 1 row in queue after disconnect before PUBCOMP, got %d", n)
	}

	// 6. Reconnect raw TCP client: broker resends PUBREL
	conn2, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	if err := sendRawConnect(conn2, clientID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readRawPacket(conn2); err != nil { // CONNACK
		t.Fatal(err)
	}

	// Read resent PUBREL (0x62)
	relType2, _, err := readRawPacket(conn2)
	if err != nil {
		t.Fatal(err)
	}
	if (relType2 & 0xF0) != 0x60 {
		t.Fatalf("expected resent PUBREL (0x60..), got 0x%02x", relType2)
	}

	// Check DB row count: MUST STILL BE 1 before PUBCOMP
	time.Sleep(100 * time.Millisecond)
	if n := countQueuedRows(t, dbPath, clientID); n != 1 {
		t.Fatalf("expected 1 row in queue before PUBCOMP on reconnect, got %d", n)
	}

	// 7. Send PUBCOMP (0x70)
	if err := sendRawPubcomp(conn2, pid); err != nil {
		t.Fatal(err)
	}

	// 8. Verify queue row is deleted after PUBCOMP
	time.Sleep(150 * time.Millisecond)
	if n := countQueuedRows(t, dbPath, clientID); n != 0 {
		t.Fatalf("expected 0 rows in queue after PUBCOMP, got %d", n)
	}
}

// TestQueuedQoS0DeletedOnWrite verifies that QoS 0 queued messages are deleted
// immediately upon successful write.
func TestQueuedQoS0DeletedOnWrite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "qos0_ack.db")
	port := 25062

	srv := startWithDB(t, port, dbPath, func(c *config.Config) {
		c.QueuedMessagesEnabled = true
	})
	defer srv.Close()

	clientID := "sub-qos0-ack"

	// 1. Subscribe persistent session (QoS 0)
	sub := mqtt.NewClient(persistentOpts(port, clientID))
	if tok := sub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := sub.Subscribe("qos0/test", 0, nil); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	sub.Disconnect(100)
	time.Sleep(150 * time.Millisecond)

	// 2. Publish while offline
	pub := mqtt.NewClient(mqttOpts(port, "pub-qos0"))
	if tok := pub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := pub.Publish("qos0/test", 0, false, "msg-qos0-data"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	pub.Disconnect(100)
	time.Sleep(150 * time.Millisecond)

	if n := countQueuedRows(t, dbPath, clientID); n != 1 {
		t.Fatalf("expected 1 row in queue, got %d", n)
	}

	// 3. Connect raw TCP client, read PUBLISH (QoS 0)
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := sendRawConnect(conn, clientID); err != nil {
		t.Fatal(err)
	}

	pktType, _, err := readRawPacket(conn)
	if err != nil {
		t.Fatal(err)
	}
	if pktType != 0x20 {
		t.Fatalf("expected CONNACK (0x20), got 0x%02x", pktType)
	}

	pktType, body, err := readRawPacket(conn)
	if err != nil {
		t.Fatal(err)
	}
	if (pktType & 0xF0) != 0x30 {
		t.Fatalf("expected PUBLISH (0x30..), got 0x%02x", pktType)
	}
	topic, _, payload, err := extractRawPublish(pktType, body)
	if err != nil {
		t.Fatal(err)
	}
	if topic != "qos0/test" || payload != "msg-qos0-data" {
		t.Fatalf("unexpected topic/payload: %s / %s", topic, payload)
	}

	// 4. Verify QoS 0 row was removed immediately on write (no PUBACK needed)
	time.Sleep(150 * time.Millisecond)
	if n := countQueuedRows(t, dbPath, clientID); n != 0 {
		t.Fatalf("expected 0 rows in queue for QoS 0 after write, got %d", n)
	}
}

// TestQueuedQoS1RedeliveredAcrossRestart verifies that:
// 1. Client disconnects before PUBACK.
// 2. Broker restarts on the same database.
// 3. Message remains in storage and is redelivered upon reconnect.
// 4. Row is removed when PUBACK is finally received.
func TestQueuedQoS1RedeliveredAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "qos1_restart.db")
	port := 25063

	srv := startWithDB(t, port, dbPath, func(c *config.Config) {
		c.QueuedMessagesEnabled = true
	})

	clientID := "sub-qos1-restart"

	// 1. Subscribe persistent session
	sub := mqtt.NewClient(persistentOpts(port, clientID))
	if tok := sub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := sub.Subscribe("restart/qos1", 1, nil); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	sub.Disconnect(100)
	time.Sleep(150 * time.Millisecond)

	// 2. Publish while offline
	pub := mqtt.NewClient(mqttOpts(port, "pub-restart"))
	if tok := pub.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if tok := pub.Publish("restart/qos1", 1, false, "msg-restart-data"); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	pub.Disconnect(100)
	time.Sleep(150 * time.Millisecond)

	if n := countQueuedRows(t, dbPath, clientID); n != 1 {
		t.Fatalf("expected 1 row in queue, got %d", n)
	}

	// 3. Connect raw TCP client, read PUBLISH, close without PUBACK
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	if err := sendRawConnect(conn, clientID); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	readRawPacket(conn) // CONNACK
	readRawPacket(conn) // PUBLISH
	conn.Close()
	time.Sleep(150 * time.Millisecond)

	if n := countQueuedRows(t, dbPath, clientID); n != 1 {
		t.Fatalf("expected 1 row in queue before restart, got %d", n)
	}

	// 4. Restart broker on same database
	srv.Close()
	time.Sleep(150 * time.Millisecond)

	srv2 := startWithDB(t, port, dbPath, func(c *config.Config) {
		c.QueuedMessagesEnabled = true
	})
	defer srv2.Close()

	if n := countQueuedRows(t, dbPath, clientID); n != 1 {
		t.Fatalf("expected 1 row in queue after restart, got %d", n)
	}

	// 5. Reconnect persistent client, receive redelivered message, send PUBACK
	var gotMsg atomic.Int32
	opts := persistentOpts(port, clientID)
	opts.SetDefaultPublishHandler(func(_ mqtt.Client, m mqtt.Message) {
		if string(m.Payload()) == "msg-restart-data" {
			gotMsg.Add(1)
		}
	})
	reconnClient := mqtt.NewClient(opts)
	if tok := reconnClient.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer reconnClient.Disconnect(100)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && gotMsg.Load() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if gotMsg.Load() != 1 {
		t.Fatalf("expected redelivery after restart, got %d", gotMsg.Load())
	}

	// 6. Verify row is deleted after PUBACK
	time.Sleep(150 * time.Millisecond)
	if n := countQueuedRows(t, dbPath, clientID); n != 0 {
		t.Fatalf("expected 0 rows in queue after PUBACK, got %d", n)
	}
}
