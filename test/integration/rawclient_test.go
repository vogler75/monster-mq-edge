package integration

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"monstermq.io/edge/internal/mqtt/packets"
)

// rawClient is a minimal MQTT 3.1.1 / 5 client built on the engine's packet
// codec, for tests that need exact reason codes. A reader goroutine routes
// acknowledgements to waiters and PUBLISH packets to a channel; QoS 1
// publishes are acknowledged automatically.
type rawClient struct {
	t       testing.TB
	conn    net.Conn
	version byte
	nextID  uint16

	mu      sync.Mutex
	waiters map[uint16]chan packets.Packet
	msgs    chan packets.Packet
	closed  chan struct{}
	done    chan struct{} // closed by Close so a blocked reader gives up
	connack chan packets.Packet
}

type rawConnect struct {
	ClientID      string
	Version       byte
	Clean         bool
	Username      string
	Password      string
	SessionExpiry uint32
}

func dialRaw(t testing.TB, port int, c rawConnect) (*rawClient, packets.Packet) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if c.Version == 0 {
		c.Version = 5
	}
	rc := &rawClient{
		t: t, conn: conn, version: c.Version,
		waiters: map[uint16]chan packets.Packet{},
		msgs:    make(chan packets.Packet, 1024),
		closed:  make(chan struct{}),
		done:    make(chan struct{}),
		connack: make(chan packets.Packet, 1),
	}
	pk := packets.Packet{
		FixedHeader:     packets.FixedHeader{Type: packets.Connect},
		ProtocolVersion: c.Version,
		Connect: packets.ConnectParams{
			ProtocolName:     []byte("MQTT"),
			ClientIdentifier: c.ClientID,
			Clean:            c.Clean,
			Keepalive:        60,
		},
	}
	if c.Version == 3 {
		pk.Connect.ProtocolName = []byte("MQIsdp")
	}
	if c.Version == 4 {
		pk.ProtocolVersion = 4
	}
	if c.Username != "" {
		pk.Connect.UsernameFlag = true
		pk.Connect.Username = []byte(c.Username)
		pk.Connect.PasswordFlag = true
		pk.Connect.Password = []byte(c.Password)
	}
	if c.Version == 5 && c.SessionExpiry > 0 {
		pk.Properties.SessionExpiryInterval = c.SessionExpiry
		pk.Properties.SessionExpiryIntervalFlag = true
	}
	var buf bytes.Buffer
	if err := pk.ConnectEncode(&buf); err != nil {
		t.Fatalf("encode connect: %v", err)
	}
	if _, err := conn.Write(buf.Bytes()); err != nil {
		t.Fatalf("write connect: %v", err)
	}
	go rc.readLoop()
	select {
	case ack := <-rc.connack:
		if ack.ReasonCode != 0 {
			t.Fatalf("connack reason 0x%02x", ack.ReasonCode)
		}
		return rc, ack
	case <-time.After(3 * time.Second):
		t.Fatal("no CONNACK")
	}
	return nil, packets.Packet{}
}

func (c *rawClient) readLoop() {
	defer close(c.closed)
	for {
		_ = c.conn.SetReadDeadline(time.Time{})
		hb := make([]byte, 1)
		if _, err := c.conn.Read(hb); err != nil {
			return
		}
		n, err := decodeMqttLength(c.conn)
		if err != nil {
			return
		}
		body := make([]byte, n)
		if _, err := readFull(c.conn, body); err != nil {
			return
		}
		pk := packets.Packet{ProtocolVersion: c.version}
		if err := pk.FixedHeader.Decode(hb[0]); err != nil {
			return
		}
		pk.FixedHeader.Remaining = n
		switch pk.FixedHeader.Type {
		case packets.Connack:
			_ = pk.ConnackDecode(body)
			c.connack <- pk
		case packets.Suback:
			_ = pk.SubackDecode(body)
			c.route(pk)
		case packets.Unsuback:
			_ = pk.UnsubackDecode(body)
			c.route(pk)
		case packets.Puback:
			_ = pk.PubackDecode(body)
			c.route(pk)
		case packets.Pubrec:
			_ = pk.PubrecDecode(body)
			c.route(pk)
		case packets.Publish:
			if err := pk.PublishDecode(body); err != nil {
				return
			}
			if pk.FixedHeader.Qos == 1 {
				ack := packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Puback}, PacketID: pk.PacketID, ProtocolVersion: c.version}
				var b bytes.Buffer
				_ = ack.PubackEncode(&b)
				_, _ = c.conn.Write(b.Bytes())
			}
			select {
			case c.msgs <- pk:
			case <-c.done:
				return
			}
		case packets.Disconnect:
			return
		}
	}
}

func readFull(conn net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := conn.Read(b[n:])
		if err != nil {
			return n, err
		}
		n += m
	}
	return n, nil
}

func (c *rawClient) route(pk packets.Packet) {
	c.mu.Lock()
	ch := c.waiters[pk.PacketID]
	delete(c.waiters, pk.PacketID)
	c.mu.Unlock()
	if ch != nil {
		ch <- pk
	}
}

func (c *rawClient) send(pk packets.Packet, encode func(*packets.Packet, *bytes.Buffer) error) (packets.Packet, error) {
	c.mu.Lock()
	c.nextID++
	if c.nextID == 0 {
		c.nextID = 1
	}
	pk.PacketID = c.nextID
	ch := make(chan packets.Packet, 1)
	c.waiters[pk.PacketID] = ch
	c.mu.Unlock()
	pk.ProtocolVersion = c.version
	var buf bytes.Buffer
	if err := encode(&pk, &buf); err != nil {
		return packets.Packet{}, err
	}
	if _, err := c.conn.Write(buf.Bytes()); err != nil {
		return packets.Packet{}, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-c.closed:
		return packets.Packet{}, errors.New("connection closed")
	case <-time.After(15 * time.Second):
		return packets.Packet{}, errors.New("ack timeout")
	}
}

// Subscribe returns the SUBACK reason codes in filter order.
func (c *rawClient) Subscribe(subs ...packets.Subscription) []byte {
	c.t.Helper()
	pk := packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Subscribe, Qos: 1}, Filters: subs}
	r, err := c.send(pk, (*packets.Packet).SubscribeEncode)
	if err != nil {
		c.t.Fatalf("subscribe: %v", err)
	}
	return r.ReasonCodes
}

func (c *rawClient) Unsubscribe(filters ...string) {
	c.t.Helper()
	var subs packets.Subscriptions
	for _, f := range filters {
		subs = append(subs, packets.Subscription{Filter: f})
	}
	pk := packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Unsubscribe, Qos: 1}, Filters: subs}
	if _, err := c.send(pk, (*packets.Packet).UnsubscribeEncode); err != nil {
		c.t.Fatalf("unsubscribe: %v", err)
	}
}

type rawPub struct {
	Topic         string
	Payload       []byte
	QoS           byte
	Retain        bool
	ResponseTopic string
	Correlation   []byte
}

// Publish sends a PUBLISH. For QoS 1 it returns the PUBACK reason code; for
// QoS 0 it returns 0. A closed connection (MQTT 3.1.1 rejection) returns
// 0xFF.
func (c *rawClient) Publish(p rawPub) byte {
	c.t.Helper()
	pk := packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: p.QoS, Retain: p.Retain},
		TopicName:   p.Topic,
		Payload:     p.Payload,
	}
	if p.ResponseTopic != "" {
		pk.Properties.ResponseTopic = p.ResponseTopic
		pk.Mods.AllowResponseInfo = true
	}
	if len(p.Correlation) > 0 {
		pk.Properties.CorrelationData = p.Correlation
	}
	if p.QoS == 0 {
		pk.ProtocolVersion = c.version
		var buf bytes.Buffer
		if err := pk.PublishEncode(&buf); err != nil {
			c.t.Fatalf("encode publish: %v", err)
		}
		if _, err := c.conn.Write(buf.Bytes()); err != nil {
			return 0xFF
		}
		return 0
	}
	r, err := c.send(pk, (*packets.Packet).PublishEncode)
	if err != nil {
		return 0xFF
	}
	return r.ReasonCode
}

// Next waits for the next PUBLISH on any topic.
func (c *rawClient) Next(timeout time.Duration) (packets.Packet, bool) {
	select {
	case pk := <-c.msgs:
		return pk, true
	case <-time.After(timeout):
		return packets.Packet{}, false
	}
}

// NextOn waits for a PUBLISH on topic, discarding others.
func (c *rawClient) NextOn(topic string, timeout time.Duration) (packets.Packet, bool) {
	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return packets.Packet{}, false
		}
		pk, ok := c.Next(left)
		if !ok {
			return pk, false
		}
		if pk.TopicName == topic {
			return pk, true
		}
	}
}

// NextOnAll waits until one PUBLISH arrived on each topic and returns them
// by topic; other topics are discarded.
func (c *rawClient) NextOnAll(timeout time.Duration, topics ...string) map[string]packets.Packet {
	want := map[string]bool{}
	for _, t := range topics {
		want[t] = true
	}
	got := map[string]packets.Packet{}
	deadline := time.Now().Add(timeout)
	for len(got) < len(want) {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		pk, ok := c.Next(left)
		if !ok {
			break
		}
		if want[pk.TopicName] {
			if _, seen := got[pk.TopicName]; !seen {
				got[pk.TopicName] = pk
			}
		}
	}
	return got
}

// Drain discards buffered publishes.
func (c *rawClient) Drain(wait time.Duration) int {
	n := 0
	for {
		if _, ok := c.Next(wait); !ok {
			return n
		}
		n++
	}
}

func (c *rawClient) Close() {
	select {
	case <-c.done:
		return
	default:
		close(c.done)
	}
	pk := packets.Packet{FixedHeader: packets.FixedHeader{Type: packets.Disconnect}, ProtocolVersion: c.version}
	var buf bytes.Buffer
	_ = pk.DisconnectEncode(&buf)
	_, _ = c.conn.Write(buf.Bytes())
	_ = c.conn.Close()
	<-c.closed
}

func (c *rawClient) Closed() bool {
	select {
	case <-c.closed:
		return true
	case <-time.After(300 * time.Millisecond):
		return false
	}
}
