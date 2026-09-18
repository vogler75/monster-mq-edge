package integration

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
)

func startBrokerWithRetainedConfig(t *testing.T, port int, dbPath string, storeType config.StoreType) (*broker.Server, string) {
	t.Helper()
	cfg := config.Default()
	cfg.NodeID = fmt.Sprintf("test-ret-%d", port)
	cfg.TCP.Enabled = true
	cfg.TCP.Port = port
	cfg.WS.Enabled = false
	cfg.GraphQL.Enabled = false
	cfg.Metrics.Enabled = false
	cfg.RetainedStoreType = storeType
	if dbPath != "" {
		cfg.SQLite.Path = dbPath
	} else {
		cfg.SQLite.Path = t.TempDir() + "/test.db"
	}

	logger := slog.New(slog.DiscardHandler)
	srv, err := broker.New(cfg, logger, nil)
	if err != nil {
		t.Fatalf("broker init: %v", err)
	}
	go func() { _ = srv.Serve() }()
	time.Sleep(100 * time.Millisecond)

	return srv, fmt.Sprintf("localhost:%d", port)
}

func sendMqtt5Publish(conn net.Conn, topic string, payload []byte, retain bool, expirySec uint32) error {
	var vheader []byte
	vheader = binary.BigEndian.AppendUint16(vheader, uint16(len(topic)))
	vheader = append(vheader, []byte(topic)...)

	// Properties
	var props []byte
	if expirySec > 0 {
		props = append(props, 0x02) // PropMessageExpiryInterval
		props = binary.BigEndian.AppendUint32(props, expirySec)
	}
	vheader = encodeMqttLength(vheader, len(props))
	vheader = append(vheader, props...)

	body := append(vheader, payload...)
	var pkt []byte
	hdr := byte(0x30) // PUBLISH QoS 0
	if retain {
		hdr |= 0x01 // Retain flag
	}
	pkt = append(pkt, hdr)
	pkt = encodeMqttLength(pkt, len(body))
	pkt = append(pkt, body...)

	_, err := conn.Write(pkt)
	return err
}

func sendMqtt5Subscribe(conn net.Conn, packetID uint16, topicFilter string) error {
	var vheader []byte
	vheader = binary.BigEndian.AppendUint16(vheader, packetID)
	// Subscribe properties length: 0
	vheader = append(vheader, 0x00)

	// Payload: topic filter + options (QoS 0)
	var payload []byte
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(topicFilter)))
	payload = append(payload, []byte(topicFilter)...)
	payload = append(payload, 0x00) // Options: QoS 0, NoLocal 0, RetainAsPublished 0, RetainHandling 0

	body := append(vheader, payload...)
	var pkt []byte
	pkt = append(pkt, 0x82) // SUBSCRIBE
	pkt = encodeMqttLength(pkt, len(body))
	pkt = append(pkt, body...)

	_, err := conn.Write(pkt)
	return err
}

func parseMqtt5PublishMessageExpiry(body []byte) (uint32, bool, error) {
	if len(body) < 2 {
		return 0, false, fmt.Errorf("publish body too short")
	}
	tLen := int(binary.BigEndian.Uint16(body[0:2]))
	offset := 2 + tLen
	if offset > len(body) {
		return 0, false, fmt.Errorf("truncated topic name")
	}

	// Properties length (variable byte integer)
	var propLen uint32
	var multiplier uint32
	for {
		if offset >= len(body) {
			return 0, false, fmt.Errorf("malformed property length")
		}
		b := body[offset]
		offset++
		propLen |= uint32(b&0x7F) << multiplier
		if (b & 0x80) == 0 {
			break
		}
		multiplier += 7
	}

	propEnd := offset + int(propLen)
	if propEnd > len(body) {
		return 0, false, fmt.Errorf("property length exceeds packet body")
	}

	for offset < propEnd {
		propID := body[offset]
		offset++
		if propID == 0x02 { // PropMessageExpiryInterval
			if offset+4 > propEnd {
				return 0, false, fmt.Errorf("truncated message expiry interval property")
			}
			val := binary.BigEndian.Uint32(body[offset : offset+4])
			return val, true, nil
		}
		// Skip other properties
		switch propID {
		case 0x01: // Payload Format Indicator
			offset += 1
		case 0x23: // Topic Alias
			offset += 2
		case 0x08, 0x1C: // Response Topic, Content Type (string)
			if offset+2 > propEnd {
				return 0, false, fmt.Errorf("truncated string property")
			}
			sLen := int(binary.BigEndian.Uint16(body[offset : offset+2]))
			offset += 2 + sLen
		case 0x09: // Correlation Data (binary)
			if offset+2 > propEnd {
				return 0, false, fmt.Errorf("truncated binary property")
			}
			bLen := int(binary.BigEndian.Uint16(body[offset : offset+2]))
			offset += 2 + bLen
		case 0x26: // User Property (pair of strings)
			for i := 0; i < 2; i++ {
				if offset+2 > propEnd {
					return 0, false, fmt.Errorf("truncated user property string")
				}
				sLen := int(binary.BigEndian.Uint16(body[offset : offset+2]))
				offset += 2 + sLen
			}
		case 0x0B: // Subscription Identifier (varint)
			for {
				if offset >= propEnd {
					return 0, false, fmt.Errorf("truncated sub id")
				}
				b := body[offset]
				offset++
				if (b & 0x80) == 0 {
					break
				}
			}
		default:
			return 0, false, nil
		}
	}
	return 0, false, nil
}

func TestRetainedExpiry_LiveExpiry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "retained.db")
	srv, addr := startBrokerWithRetainedConfig(t, 22280, dbPath, config.StoreSQLite)
	defer srv.Close()

	// 1. Publisher connects and publishes retained message with 1s expiry
	pubConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial publisher: %v", err)
	}
	defer pubConn.Close()

	if err := sendMqtt5Connect(pubConn, "pub-live-exp"); err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	hdr, _, err := readRawPacket(pubConn)
	if err != nil || hdr != 0x20 {
		t.Fatalf("pub connack: %v, hdr: 0x%02x", err, hdr)
	}

	if err := sendMqtt5Publish(pubConn, "test/live-exp", []byte("hello-expiring"), true, 1); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Verify the message is in the DB immediately
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	time.Sleep(100 * time.Millisecond)
	var count int
	err = db.QueryRow("SELECT count(*) FROM retainedmessages WHERE topic = 'test/live-exp'").Scan(&count)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 retained row in DB initially, got %d", count)
	}

	// Wait 1.5 seconds for message to expire and background retention loop to purge it
	time.Sleep(1500 * time.Millisecond)

	// 2. New subscriber connects and subscribes
	subConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial subscriber: %v", err)
	}
	defer subConn.Close()

	if err := sendMqtt5Connect(subConn, "sub-live-exp"); err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	hdr, _, err = readRawPacket(subConn)
	if err != nil || hdr != 0x20 {
		t.Fatalf("sub connack: %v", err)
	}

	if err := sendMqtt5Subscribe(subConn, 1, "test/live-exp"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	hdr, _, err = readRawPacket(subConn)
	if err != nil || hdr != 0x90 { // SUBACK
		t.Fatalf("suback: %v, hdr: 0x%02x", err, hdr)
	}

	// Attempt to read next packet — subscriber should NOT receive expired message
	_ = subConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	hdr, _, err = readRawPacket(subConn)
	if err == nil {
		t.Fatalf("expected no packet for expired retained message, but received packet type 0x%02x", hdr)
	}

	// Verify the row was purged from SQLite DB
	err = db.QueryRow("SELECT count(*) FROM retainedmessages WHERE topic = 'test/live-exp'").Scan(&count)
	if err != nil {
		t.Fatalf("count query after expiry: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 retained rows in DB after expiry purge, got %d", count)
	}
}

func TestRetainedExpiry_BrokerRestart_Expired(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "retained_restart.db")
	port := 22281

	// 1. Start broker instance 1
	srv1, addr1 := startBrokerWithRetainedConfig(t, port, dbPath, config.StoreSQLite)

	conn1, err := net.Dial("tcp", addr1)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := sendMqtt5Connect(conn1, "p-restart-1"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	hdr, _, err := readRawPacket(conn1)
	if err != nil || hdr != 0x20 {
		t.Fatalf("connack: %v", err)
	}

	// Publish retained message with 2 seconds expiry
	if err := sendMqtt5Publish(conn1, "test/restart-exp", []byte("restart-payload"), true, 2); err != nil {
		t.Fatalf("publish: %v", err)
	}
	conn1.Close()

	// Stop broker instance 1 after 300ms
	time.Sleep(300 * time.Millisecond)
	_ = srv1.Close()

	// Sleep 2.2 seconds — message expires while broker is stopped
	time.Sleep(2200 * time.Millisecond)

	// 2. Start broker instance 2 on the same database
	srv2, addr2 := startBrokerWithRetainedConfig(t, port, dbPath, config.StoreSQLite)
	defer srv2.Close()

	conn2, err := net.Dial("tcp", addr2)
	if err != nil {
		t.Fatalf("dial srv2: %v", err)
	}
	defer conn2.Close()

	if err := sendMqtt5Connect(conn2, "sub-restart-2"); err != nil {
		t.Fatalf("connect srv2: %v", err)
	}
	hdr, _, err = readRawPacket(conn2)
	if err != nil || hdr != 0x20 {
		t.Fatalf("connack srv2: %v", err)
	}

	if err := sendMqtt5Subscribe(conn2, 1, "test/restart-exp"); err != nil {
		t.Fatalf("subscribe srv2: %v", err)
	}
	hdr, _, err = readRawPacket(conn2)
	if err != nil || hdr != 0x90 {
		t.Fatalf("suback: %v", err)
	}

	// Attempt to read next packet — should NOT receive expired message
	_ = conn2.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	hdr, _, err = readRawPacket(conn2)
	if err == nil {
		t.Fatalf("expected no packet for message expired during restart, got packet 0x%02x", hdr)
	}
}

func TestRetainedExpiry_BrokerRestart_RemainingTTL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "retained_ttl.db")
	port := 22282

	// 1. Start broker instance 1
	srv1, addr1 := startBrokerWithRetainedConfig(t, port, dbPath, config.StoreSQLite)

	conn1, err := net.Dial("tcp", addr1)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := sendMqtt5Connect(conn1, "p-ttl-1"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	hdr, _, err := readRawPacket(conn1)
	if err != nil || hdr != 0x20 {
		t.Fatalf("connack: %v", err)
	}

	// Publish retained message with 60 seconds expiry
	if err := sendMqtt5Publish(conn1, "test/remaining-ttl", []byte("ttl-payload"), true, 60); err != nil {
		t.Fatalf("publish: %v", err)
	}
	conn1.Close()

	// Stop broker instance 1 after 500ms
	time.Sleep(500 * time.Millisecond)
	_ = srv1.Close()

	// Sleep 1.5 seconds (total elapsed ~2s)
	time.Sleep(1500 * time.Millisecond)

	// 2. Start broker instance 2 on the same database
	srv2, addr2 := startBrokerWithRetainedConfig(t, port, dbPath, config.StoreSQLite)
	defer srv2.Close()

	conn2, err := net.Dial("tcp", addr2)
	if err != nil {
		t.Fatalf("dial srv2: %v", err)
	}
	defer conn2.Close()

	if err := sendMqtt5Connect(conn2, "sub-ttl-2"); err != nil {
		t.Fatalf("connect srv2: %v", err)
	}
	hdr, _, err = readRawPacket(conn2)
	if err != nil || hdr != 0x20 {
		t.Fatalf("connack srv2: %v", err)
	}

	if err := sendMqtt5Subscribe(conn2, 1, "test/remaining-ttl"); err != nil {
		t.Fatalf("subscribe srv2: %v", err)
	}
	hdr, _, err = readRawPacket(conn2)
	if err != nil || hdr != 0x90 {
		t.Fatalf("suback: %v", err)
	}

	// Read retained PUBLISH packet
	_ = conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	hdr, body, err := readRawPacket(conn2)
	if err != nil {
		t.Fatalf("expected retained PUBLISH packet, got err: %v", err)
	}
	if (hdr & 0xF0) != 0x30 {
		t.Fatalf("expected PUBLISH packet (0x30..0x3F), got 0x%02x", hdr)
	}

	// Parse remaining MessageExpiryInterval from packet
	remainingTTL, found, err := parseMqtt5PublishMessageExpiry(body)
	if err != nil {
		t.Fatalf("parse message expiry: %v", err)
	}
	if !found {
		t.Fatal("MessageExpiryInterval property not found in delivered retained message")
	}

	// Since ~2 seconds have passed out of 60, remaining TTL must be between 56 and 59 seconds
	if remainingTTL < 56 || remainingTTL > 59 {
		t.Fatalf("expected remaining TTL between 56 and 59 seconds, got %d", remainingTTL)
	}
}

func TestRetainedExpiry_MemoryMode(t *testing.T) {
	srv, addr := startBrokerWithRetainedConfig(t, 22283, "", config.StoreMemory)
	defer srv.Close()

	pubConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial pub: %v", err)
	}
	defer pubConn.Close()

	if err := sendMqtt5Connect(pubConn, "pub-mem-exp"); err != nil {
		t.Fatalf("pub connect: %v", err)
	}
	hdr, _, err := readRawPacket(pubConn)
	if err != nil || hdr != 0x20 {
		t.Fatalf("pub connack: %v", err)
	}

	// Publish with 1 second expiry
	if err := sendMqtt5Publish(pubConn, "test/mem-exp", []byte("mem-payload"), true, 1); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Wait 1.5 seconds for memory retention expiry
	time.Sleep(1500 * time.Millisecond)

	subConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial sub: %v", err)
	}
	defer subConn.Close()

	if err := sendMqtt5Connect(subConn, "sub-mem-exp"); err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	hdr, _, err = readRawPacket(subConn)
	if err != nil || hdr != 0x20 {
		t.Fatalf("sub connack: %v", err)
	}

	if err := sendMqtt5Subscribe(subConn, 1, "test/mem-exp"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	hdr, _, err = readRawPacket(subConn)
	if err != nil || hdr != 0x90 {
		t.Fatalf("suback: %v", err)
	}

	// Should not receive expired message
	_ = subConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	hdr, _, err = readRawPacket(subConn)
	if err == nil {
		t.Fatalf("expected no packet for expired in-memory retained message, got 0x%02x", hdr)
	}
}

func TestRetainedExpiry_ClearRetained(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "retained_clear.db")
	srv, addr := startBrokerWithRetainedConfig(t, 22284, dbPath, config.StoreSQLite)
	defer srv.Close()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := sendMqtt5Connect(conn, "c-clear"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	hdr, _, err := readRawPacket(conn)
	if err != nil || hdr != 0x20 {
		t.Fatalf("connack: %v", err)
	}

	// 1. Set retained message with 60s expiry
	if err := sendMqtt5Publish(conn, "test/clear", []byte("initial"), true, 60); err != nil {
		t.Fatalf("pub initial: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// 2. Clear retained message by publishing empty payload with retain=true
	if err := sendMqtt5Publish(conn, "test/clear", []byte{}, true, 0); err != nil {
		t.Fatalf("pub clear: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	// 3. Subscribe from new client
	subConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial sub: %v", err)
	}
	defer subConn.Close()

	if err := sendMqtt5Connect(subConn, "c-clear-sub"); err != nil {
		t.Fatalf("sub connect: %v", err)
	}
	hdr, _, err = readRawPacket(subConn)
	if err != nil || hdr != 0x20 {
		t.Fatalf("sub connack: %v", err)
	}

	if err := sendMqtt5Subscribe(subConn, 1, "test/clear"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	hdr, _, err = readRawPacket(subConn)
	if err != nil || hdr != 0x90 {
		t.Fatalf("suback: %v", err)
	}

	// Should not receive any retained message
	_ = subConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	hdr, _, err = readRawPacket(subConn)
	if err == nil {
		t.Fatalf("expected no packet after clearing retained, got 0x%02x", hdr)
	}

	// Verify 0 rows in DB
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var count int
	err = db.QueryRow("SELECT count(*) FROM retainedmessages WHERE topic = 'test/clear'").Scan(&count)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 retained rows in DB after clear, got %d", count)
	}
}
