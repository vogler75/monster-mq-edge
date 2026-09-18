package integration

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"

	"monstermq.io/edge/internal/broker"
	"monstermq.io/edge/internal/config"
)

func startBrokerWithMaxMessageSize(t *testing.T, port int, wsPort int, maxMsgSize int) (*broker.Server, string, string) {
	t.Helper()
	cfg := config.Default()
	cfg.NodeID = fmt.Sprintf("test-mms-%d", port)
	cfg.TCP.Enabled = true
	cfg.TCP.Port = port
	if wsPort > 0 {
		cfg.WS.Enabled = true
		cfg.WS.Port = wsPort
	} else {
		cfg.WS.Enabled = false
	}
	cfg.GraphQL.Enabled = false
	cfg.Metrics.Enabled = false
	cfg.MaxMessageSize = maxMsgSize
	cfg.SQLite.Path = t.TempDir() + "/test.db"

	logger := slog.New(slog.DiscardHandler)
	srv, err := broker.New(cfg, logger, nil)
	if err != nil {
		t.Fatalf("broker init: %v", err)
	}
	go func() { _ = srv.Serve() }()
	time.Sleep(100 * time.Millisecond)

	tcpAddr := fmt.Sprintf("localhost:%d", port)
	var wsAddr string
	if wsPort > 0 {
		wsAddr = fmt.Sprintf("ws://localhost:%d", wsPort)
	}
	return srv, tcpAddr, wsAddr
}

func sendMqtt5Connect(conn net.Conn, clientID string) error {
	var vheader []byte
	// Protocol Name: "MQTT"
	vheader = append(vheader, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x05)
	// Connect flags: clean start = true (bit 1)
	vheader = append(vheader, 0x02)
	// Keepalive: 60s
	vheader = append(vheader, 0x00, 0x3C)
	// Connect properties length: 0
	vheader = append(vheader, 0x00)

	// Payload: ClientID
	var payload []byte
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(clientID)))
	payload = append(payload, []byte(clientID)...)

	body := append(vheader, payload...)
	var pkt []byte
	pkt = append(pkt, 0x10) // CONNECT
	pkt = encodeMqttLength(pkt, len(body))
	pkt = append(pkt, body...)

	_, err := conn.Write(pkt)
	return err
}

func parseMqtt5ConnackMaxPacketSize(body []byte) (uint32, bool, error) {
	// CONNACK variable header in MQTT 5:
	// Byte 0: Connect Acknowledge Flags (session present)
	// Byte 1: Connect Reason Code (0x00 = success)
	if len(body) < 2 {
		return 0, false, fmt.Errorf("connack body too short: %d", len(body))
	}
	if body[1] != 0x00 {
		return 0, false, fmt.Errorf("connack reason code non-zero: 0x%02x", body[1])
	}
	// Properties length (variable byte integer)
	offset := 2
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
		if propID == 0x27 { // Maximum Packet Size (PropMaximumPacketSize = 39 = 0x27)
			if offset+4 > propEnd {
				return 0, false, fmt.Errorf("truncated MaximumPacketSize property")
			}
			val := binary.BigEndian.Uint32(body[offset : offset+4])
			return val, true, nil
		}
		// Skip other known properties
		switch propID {
		case 0x11: // Session Expiry Interval (uint32)
			offset += 4
		case 0x13, 0x21, 0x22: // Server Keep Alive, Receive Maximum (uint16)
			offset += 2
		case 0x23: // Topic Alias Maximum (uint16)
			offset += 2
		case 0x24: // Maximum QoS byte
			offset += 1
		case 0x25: // Retain Available byte
			offset += 1
		case 0x28: // Wildcard Sub Available byte
			offset += 1
		case 0x29: // Subscription ID Available byte
			offset += 1
		case 0x2A: // Shared Sub Available byte
			offset += 1
		case 0x12, 0x1A, 0x1C, 0x1F: // String
			if offset+2 > propEnd {
				return 0, false, fmt.Errorf("truncated string property")
			}
			sLen := int(binary.BigEndian.Uint16(body[offset : offset+2]))
			offset += 2 + sLen
		default:
			// Unhandled property, abort search
			return 0, false, nil
		}
	}
	return 0, false, nil
}

// Build an MQTT v5 QoS 0 PUBLISH packet with exact total byte size.
func buildMqtt5PublishQoS0(topic string, totalSize int) ([]byte, error) {
	// Variable header:
	// Topic Length: 2 bytes
	// Topic: len(topic) bytes
	// Property length: 1 byte (0x00)
	vhLen := 2 + len(topic) + 1
	// Total packet size = 1 (header 0x30) + bu (len bytes) + vhLen + payloadLen
	// For totalSize <= 128, bu = 1.
	remLen := totalSize - 2
	payloadLen := remLen - vhLen
	if payloadLen < 0 {
		return nil, fmt.Errorf("totalSize %d too small for topic %q (needs at least %d)", totalSize, topic, 2+vhLen)
	}

	var vheader []byte
	vheader = binary.BigEndian.AppendUint16(vheader, uint16(len(topic)))
	vheader = append(vheader, []byte(topic)...)
	vheader = append(vheader, 0x00) // 0 properties

	payload := make([]byte, payloadLen)
	for i := range payload {
		payload[i] = 'A'
	}

	body := append(vheader, payload...)
	var pkt []byte
	pkt = append(pkt, 0x30) // PUBLISH QoS 0
	pkt = encodeMqttLength(pkt, len(body))
	pkt = append(pkt, body...)
	if len(pkt) != totalSize {
		return nil, fmt.Errorf("constructed packet size %d does not match requested %d", len(pkt), totalSize)
	}
	return pkt, nil
}

// Build an MQTT 3.1.1 QoS 1 PUBLISH packet with exact total byte size.
func buildMqtt3PublishQoS1(topic string, packetID uint16, totalSize int) ([]byte, error) {
	// Variable header:
	// Topic Length: 2 bytes
	// Topic: len(topic) bytes
	// Packet ID: 2 bytes
	vhLen := 2 + len(topic) + 2
	// Total size = 1 (0x32) + bu (1) + vhLen + payloadLen
	remLen := totalSize - 2
	payloadLen := remLen - vhLen
	if payloadLen < 0 {
		return nil, fmt.Errorf("totalSize %d too small for topic %q", totalSize, topic)
	}

	var vheader []byte
	vheader = binary.BigEndian.AppendUint16(vheader, uint16(len(topic)))
	vheader = append(vheader, []byte(topic)...)
	vheader = binary.BigEndian.AppendUint16(vheader, packetID)

	payload := make([]byte, payloadLen)
	for i := range payload {
		payload[i] = 'B'
	}

	body := append(vheader, payload...)
	var pkt []byte
	pkt = append(pkt, 0x32) // PUBLISH QoS 1
	pkt = encodeMqttLength(pkt, len(body))
	pkt = append(pkt, body...)
	if len(pkt) != totalSize {
		return nil, fmt.Errorf("constructed packet size %d does not match requested %d", len(pkt), totalSize)
	}
	return pkt, nil
}

func TestMaxMessageSize_ConfigValidation(t *testing.T) {
	cfg := config.Default()
	cfg.MaxMessageSize = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for negative MaxMessageSize, got nil")
	}

	cfg.MaxMessageSize = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error for MaxMessageSize=0: %v", err)
	}

	cfg.MaxMessageSize = 1048576
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error for default MaxMessageSize: %v", err)
	}
}

func TestMaxMessageSize_Mqtt5_ConnackProperty(t *testing.T) {
	limit := 128
	srv, addr, _ := startBrokerWithMaxMessageSize(t, 22180, 0, limit)
	defer srv.Close()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	if err := sendMqtt5Connect(conn, "test-m5-connack"); err != nil {
		t.Fatalf("send connect: %v", err)
	}

	hdr, body, err := readRawPacket(conn)
	if err != nil {
		t.Fatalf("read connack: %v", err)
	}
	if hdr != 0x20 {
		t.Fatalf("expected CONNACK (0x20), got 0x%02x", hdr)
	}

	maxPkt, found, err := parseMqtt5ConnackMaxPacketSize(body)
	if err != nil {
		t.Fatalf("parse connack properties: %v", err)
	}
	if !found {
		t.Fatal("MaximumPacketSize property not found in CONNACK")
	}
	if int(maxPkt) != limit {
		t.Fatalf("expected MaximumPacketSize %d in CONNACK, got %d", limit, maxPkt)
	}
}

func TestMaxMessageSize_Mqtt5_Boundaries(t *testing.T) {
	limit := 128
	srv, addr, _ := startBrokerWithMaxMessageSize(t, 22181, 0, limit)
	defer srv.Close()

	// 1. Below limit (127 bytes)
	t.Run("BelowLimit_127B", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		if err := sendMqtt5Connect(conn, "c-below"); err != nil {
			t.Fatalf("connect: %v", err)
		}
		hdr, _, err := readRawPacket(conn)
		if err != nil || hdr != 0x20 {
			t.Fatalf("connack failed: hdr=0x%02x, err=%v", hdr, err)
		}

		pkt, err := buildMqtt5PublishQoS0("t", 127)
		if err != nil {
			t.Fatalf("build packet: %v", err)
		}
		if _, err := conn.Write(pkt); err != nil {
			t.Fatalf("write publish: %v", err)
		}

		// Ping to ensure connection remains alive and healthy
		ping := []byte{0xC0, 0x00}
		if _, err := conn.Write(ping); err != nil {
			t.Fatalf("write ping: %v", err)
		}
		hdr, _, err = readRawPacket(conn)
		if err != nil {
			t.Fatalf("read pingresp: %v", err)
		}
		if hdr != 0xD0 {
			t.Fatalf("expected PINGRESP (0xD0), got 0x%02x", hdr)
		}
	})

	// 2. At limit (128 bytes)
	t.Run("AtLimit_128B", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		if err := sendMqtt5Connect(conn, "c-at"); err != nil {
			t.Fatalf("connect: %v", err)
		}
		hdr, _, err := readRawPacket(conn)
		if err != nil || hdr != 0x20 {
			t.Fatalf("connack failed: hdr=0x%02x, err=%v", hdr, err)
		}

		pkt, err := buildMqtt5PublishQoS0("t", 128)
		if err != nil {
			t.Fatalf("build packet: %v", err)
		}
		if _, err := conn.Write(pkt); err != nil {
			t.Fatalf("write publish: %v", err)
		}

		// Ping to ensure connection remains alive and healthy
		ping := []byte{0xC0, 0x00}
		if _, err := conn.Write(ping); err != nil {
			t.Fatalf("write ping: %v", err)
		}
		hdr, _, err = readRawPacket(conn)
		if err != nil {
			t.Fatalf("read pingresp: %v", err)
		}
		if hdr != 0xD0 {
			t.Fatalf("expected PINGRESP (0xD0), got 0x%02x", hdr)
		}
	})

	// 3. Above limit (129 bytes) -> DISCONNECT with 0x95
	t.Run("AboveLimit_129B", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		if err := sendMqtt5Connect(conn, "c-above"); err != nil {
			t.Fatalf("connect: %v", err)
		}
		hdr, _, err := readRawPacket(conn)
		if err != nil || hdr != 0x20 {
			t.Fatalf("connack failed: hdr=0x%02x, err=%v", hdr, err)
		}

		pkt, err := buildMqtt5PublishQoS0("t", 129)
		if err != nil {
			t.Fatalf("build packet: %v", err)
		}
		if _, err := conn.Write(pkt); err != nil {
			t.Fatalf("write publish: %v", err)
		}

		// In MQTT 5, server MUST send DISCONNECT (0xE0) with Reason Code 0x95 (Packet too large)
		hdr, body, err := readRawPacket(conn)
		if err != nil {
			t.Fatalf("failed to read disconnect packet: %v", err)
		}
		if hdr != 0xE0 {
			t.Fatalf("expected DISCONNECT (0xE0), got 0x%02x", hdr)
		}
		if len(body) < 1 {
			t.Fatalf("disconnect packet missing reason code")
		}
		if body[0] != 0x95 {
			t.Fatalf("expected DISCONNECT reason code 0x95 (Packet too large), got 0x%02x", body[0])
		}

		// Verify connection is closed by server
		one := make([]byte, 1)
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		_, err = conn.Read(one)
		if err != io.EOF {
			t.Fatalf("expected socket EOF after disconnect, got %v", err)
		}
	})
}

func TestMaxMessageSize_Mqtt3_Boundaries(t *testing.T) {
	limit := 128
	srv, addr, _ := startBrokerWithMaxMessageSize(t, 22182, 0, limit)
	defer srv.Close()

	// 1. Below limit (127 bytes) QoS 1 -> receives PUBACK
	t.Run("BelowLimit_127B", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		if err := sendRawConnect(conn, "c-m3-below"); err != nil {
			t.Fatalf("connect: %v", err)
		}
		hdr, _, err := readRawPacket(conn)
		if err != nil || hdr != 0x20 {
			t.Fatalf("connack: %v", err)
		}

		pkt, err := buildMqtt3PublishQoS1("t", 1, 127)
		if err != nil {
			t.Fatalf("build pkt: %v", err)
		}
		if _, err := conn.Write(pkt); err != nil {
			t.Fatalf("write: %v", err)
		}

		hdr, body, err := readRawPacket(conn)
		if err != nil {
			t.Fatalf("read puback: %v", err)
		}
		if hdr != 0x40 {
			t.Fatalf("expected PUBACK (0x40), got 0x%02x", hdr)
		}
		if len(body) < 2 || binary.BigEndian.Uint16(body[0:2]) != 1 {
			t.Fatalf("expected PUBACK packet ID 1, got %v", body)
		}
	})

	// 2. At limit (128 bytes) QoS 1 -> receives PUBACK
	t.Run("AtLimit_128B", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		if err := sendRawConnect(conn, "c-m3-at"); err != nil {
			t.Fatalf("connect: %v", err)
		}
		hdr, _, err := readRawPacket(conn)
		if err != nil || hdr != 0x20 {
			t.Fatalf("connack: %v", err)
		}

		pkt, err := buildMqtt3PublishQoS1("t", 2, 128)
		if err != nil {
			t.Fatalf("build pkt: %v", err)
		}
		if _, err := conn.Write(pkt); err != nil {
			t.Fatalf("write: %v", err)
		}

		hdr, body, err := readRawPacket(conn)
		if err != nil {
			t.Fatalf("read puback: %v", err)
		}
		if hdr != 0x40 {
			t.Fatalf("expected PUBACK (0x40), got 0x%02x", hdr)
		}
		if len(body) < 2 || binary.BigEndian.Uint16(body[0:2]) != 2 {
			t.Fatalf("expected PUBACK packet ID 2, got %v", body)
		}
	})

	// 3. Above limit (129 bytes) QoS 1 -> connection closed, no PUBACK
	t.Run("AboveLimit_129B", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()

		if err := sendRawConnect(conn, "c-m3-above"); err != nil {
			t.Fatalf("connect: %v", err)
		}
		hdr, _, err := readRawPacket(conn)
		if err != nil || hdr != 0x20 {
			t.Fatalf("connack: %v", err)
		}

		pkt, err := buildMqtt3PublishQoS1("t", 3, 129)
		if err != nil {
			t.Fatalf("build pkt: %v", err)
		}
		if _, err := conn.Write(pkt); err != nil {
			t.Fatalf("write: %v", err)
		}

		// In MQTT 3.1.1, broker closes connection on oversized packet
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		hdr, _, err = readRawPacket(conn)
		if err == nil {
			t.Fatalf("expected connection close / error, got packet 0x%02x", hdr)
		}
	})
}

func TestMaxMessageSize_OversizedConnect(t *testing.T) {
	limit := 128
	srv, addr, _ := startBrokerWithMaxMessageSize(t, 22183, 0, limit)
	defer srv.Close()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Construct CONNECT packet with client ID larger than 128 bytes
	hugeClientID := make([]byte, 150)
	for i := range hugeClientID {
		hugeClientID[i] = 'X'
	}

	if err := sendRawConnect(conn, string(hugeClientID)); err != nil {
		t.Fatalf("write connect: %v", err)
	}

	// Server should close connection without sending CONNACK
	_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	hdr, _, err := readRawPacket(conn)
	if err == nil {
		t.Fatalf("expected connection drop on oversized CONNECT, got packet 0x%02x", hdr)
	}
}

func TestMaxMessageSize_PahoDeliveryAndRejection(t *testing.T) {
	limit := 256
	srv, _, _ := startBrokerWithMaxMessageSize(t, 22184, 0, limit)
	defer srv.Close()

	subOpts := mqttOpts(22184, "paho-sub")
	subClient := mqtt.NewClient(subOpts)
	if tok := subClient.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("sub connect: %v", tok.Error())
	}
	defer subClient.Disconnect(100)

	var receivedCount atomic.Int32
	if tok := subClient.Subscribe("test/size", 0, func(_ mqtt.Client, _ mqtt.Message) {
		receivedCount.Add(1)
	}); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("sub: %v", tok.Error())
	}

	// Publish small message (well within 256 byte limit)
	pubOpts := mqttOpts(22184, "paho-pub")
	pubClient := mqtt.NewClient(pubOpts)
	if tok := pubClient.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatalf("pub connect: %v", tok.Error())
	}
	defer pubClient.Disconnect(100)

	smallPayload := make([]byte, 50)
	tok := pubClient.Publish("test/size", 0, false, smallPayload)
	if !tok.WaitTimeout(2 * time.Second) || tok.Error() != nil {
		t.Fatalf("small publish: %v", tok.Error())
	}

	time.Sleep(100 * time.Millisecond)
	if receivedCount.Load() != 1 {
		t.Fatalf("expected 1 message received, got %d", receivedCount.Load())
	}

	// Publish large message (exceeding 256 byte limit)
	largePayload := make([]byte, 1024)
	tok = pubClient.Publish("test/size", 0, false, largePayload)
	tok.WaitTimeout(1 * time.Second)

	time.Sleep(100 * time.Millisecond)
	// Large message should not be delivered
	if receivedCount.Load() != 1 {
		t.Fatalf("oversized message was unexpectedly delivered, count: %d", receivedCount.Load())
	}
}

func TestMaxMessageSize_WebSocket(t *testing.T) {
	limit := 128
	srv, _, wsAddr := startBrokerWithMaxMessageSize(t, 22185, 22186, limit)
	defer srv.Close()

	u, err := url.Parse(wsAddr)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}

	wsConn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer wsConn.Close()

	// Send MQTT 5 connect over websocket binary frame
	var vheader []byte
	vheader = append(vheader, 0x00, 0x04, 'M', 'Q', 'T', 'T', 0x05, 0x02, 0x00, 0x3C, 0x00)
	var payload []byte
	payload = binary.BigEndian.AppendUint16(payload, uint16(len("ws-client")))
	payload = append(payload, []byte("ws-client")...)
	body := append(vheader, payload...)
	var pkt []byte
	pkt = append(pkt, 0x10)
	pkt = encodeMqttLength(pkt, len(body))
	pkt = append(pkt, body...)

	if err := wsConn.WriteMessage(websocket.BinaryMessage, pkt); err != nil {
		t.Fatalf("ws write connect: %v", err)
	}

	// Read CONNACK
	msgType, data, err := wsConn.ReadMessage()
	if err != nil {
		t.Fatalf("ws read connack: %v", err)
	}
	if msgType != websocket.BinaryMessage || len(data) < 2 || data[0] != 0x20 {
		t.Fatalf("invalid connack over ws: %v", data)
	}

	// Send oversized packet over WS (129 bytes)
	overPkt, err := buildMqtt5PublishQoS0("t", 129)
	if err != nil {
		t.Fatalf("build over pkt: %v", err)
	}
	if err := wsConn.WriteMessage(websocket.BinaryMessage, overPkt); err != nil {
		t.Fatalf("ws write over pkt: %v", err)
	}

	// Expect DISCONNECT (0xE0) with 0x95
	_, data, err = wsConn.ReadMessage()
	if err != nil {
		t.Fatalf("ws read disconnect: %v", err)
	}
	// In WS, data[0] = 0xE0, data[1] = remLen, data[2] = reason code
	if len(data) < 3 || data[0] != 0xE0 || data[2] != 0x95 {
		t.Fatalf("expected DISCONNECT with reason code 0x95 over ws, got %v", data)
	}
}
