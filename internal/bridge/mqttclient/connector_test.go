package mqttclient

import (
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"monstermq.io/edge/internal/queue"
	"monstermq.io/edge/internal/stores"
)

func TestOutboundFilterTreatsFixedLocalTopicAsPrefix(t *testing.T) {
	if got := outboundFilter("opcua/factory"); got != "opcua/factory/#" {
		t.Fatalf("outboundFilter = %q", got)
	}
	if got := outboundFilter("opcua/factory/#"); got != "opcua/factory/#" {
		t.Fatalf("outboundFilter wildcard = %q", got)
	}
}

func TestMapOutboundTopicFixedLocalPrefix(t *testing.T) {
	addr := Address{
		Mode:        "PUBLISH",
		LocalTopic:  "opcua/factory",
		RemoteTopic: "cloud/factory",
		RemovePath:  true,
	}
	if got := mapOutboundTopic(addr, "opcua/factory"); got != "cloud/factory" {
		t.Fatalf("exact topic mapped to %q", got)
	}
	if got := mapOutboundTopic(addr, "opcua/factory/line1/temp"); got != "cloud/factory/line1/temp" {
		t.Fatalf("child topic mapped to %q", got)
	}
}

func TestMapOutboundTopicWildcardLocalPrefix(t *testing.T) {
	addr := Address{
		Mode:        "PUBLISH",
		LocalTopic:  "opcua/factory/#",
		RemoteTopic: "cloud/factory",
		RemovePath:  true,
	}
	if got := mapOutboundTopic(addr, "opcua/factory/line1/temp"); got != "cloud/factory/line1/temp" {
		t.Fatalf("wildcard topic mapped to %q", got)
	}
}

func TestMapOutboundTopicRemoteWildcardUsesLiteralPrefix(t *testing.T) {
	addr := Address{
		Mode:        "PUBLISH",
		LocalTopic:  "opcua/factory/#",
		RemoteTopic: "cloud/factory/#",
		RemovePath:  true,
	}
	if got := mapOutboundTopic(addr, "opcua/factory/line1/temp"); got != "cloud/factory/line1/temp" {
		t.Fatalf("remote wildcard topic mapped to %q", got)
	}
}

func TestMapInboundTopicLocalWildcardUsesLiteralPrefix(t *testing.T) {
	addr := Address{
		Mode:        "SUBSCRIBE",
		RemoteTopic: "cloud/factory/#",
		LocalTopic:  "opcua/factory/#",
		RemovePath:  true,
	}
	if got := mapInboundTopic(addr, "cloud/factory/line1/temp"); got != "opcua/factory/line1/temp" {
		t.Fatalf("local wildcard topic mapped to %q", got)
	}
}

func TestMapInboundTopicWithoutRemovePathAppendsFullRemoteTopic(t *testing.T) {
	addr := Address{
		Mode:        "SUBSCRIBE",
		RemoteTopic: "cloud/factory/#",
		LocalTopic:  "opcua/factory",
		RemovePath:  false,
	}
	if got := mapInboundTopic(addr, "cloud/factory/line1/temp"); got != "opcua/factory/cloud/factory/line1/temp" {
		t.Fatalf("full remote topic mapped to %q", got)
	}
}

func TestConfigMillisOrSecondsSupportsOldSecondValues(t *testing.T) {
	if got := configMillisOrSeconds(10).Seconds(); got != 10 {
		t.Fatalf("duration for small value = %v seconds, want 10", got)
	}
	if got := configMillisOrSeconds(30000).Seconds(); got != 30 {
		t.Fatalf("duration for millisecond value = %v seconds, want 30", got)
	}
}

func TestPublishLocalMessageUsesConfiguredAddressQoS(t *testing.T) {
	client := &fakePahoClient{connected: true, open: true}
	c := NewConnector("test", Config{}, nil, nil, nil)
	c.client = client

	ok := c.publishLocalMessage(Address{
		LocalTopic:  "local/#",
		RemoteTopic: "remote/#",
		RemovePath:  true,
		QoS:         1,
	}, stores.BrokerMessage{
		TopicName: "local/a",
		Payload:   []byte("x"),
		QoS:       0,
	}, false)
	if !ok {
		t.Fatal("publishLocalMessage returned false")
	}
	if client.qos != 1 {
		t.Fatalf("published qos = %d, want configured address qos 1", client.qos)
	}
	if client.topic != "remote/a" {
		t.Fatalf("published topic = %q, want remote/a", client.topic)
	}
}

func TestPublishLocalMessageKeepsOriginQoS(t *testing.T) {
	client := &fakePahoClient{connected: true, open: true}
	c := NewConnector("test", Config{}, nil, nil, nil)
	c.client = client

	ok := c.publishLocalMessage(Address{
		LocalTopic:  "local/#",
		RemoteTopic: "remote/#",
		RemovePath:  true,
		QoS:         -1,
	}, stores.BrokerMessage{
		TopicName: "local/a",
		Payload:   []byte("x"),
		QoS:       2,
	}, false)
	if !ok {
		t.Fatal("publishLocalMessage returned false")
	}
	if client.qos != 2 {
		t.Fatalf("published qos = %d, want origin qos 2", client.qos)
	}
}

func TestPublishLocalMessageQueuesWhenClientIsReconnecting(t *testing.T) {
	client := &fakePahoClient{connected: true, open: false}
	c := NewConnector("test", Config{BufferEnabled: true}, nil, nil, nil)
	c.client = client
	c.queue = queue.NewMemoryQueue(nil, 10, 10, 10*time.Millisecond)

	ok := c.publishLocalMessage(Address{
		LocalTopic:  "local/#",
		RemoteTopic: "remote/#",
		RemovePath:  true,
		QoS:         0,
	}, stores.BrokerMessage{
		TopicName: "local/a",
		Payload:   []byte("x"),
		QoS:       0,
	}, false)
	if ok {
		t.Fatal("publishLocalMessage returned true while connection was not open")
	}
	if client.publishCount != 0 {
		t.Fatalf("Publish called %d times, want 0", client.publishCount)
	}
	if got := c.queue.Size(); got != 1 {
		t.Fatalf("queue size = %d, want 1", got)
	}
}

func TestPublishLocalMessageUsesPahoBufferWhenConfigured(t *testing.T) {
	client := &fakePahoClient{connected: true, open: false}
	c := NewConnector("test", Config{BufferEnabled: true, BufferImplementation: "PAHO"}, nil, nil, nil)
	c.client = client

	ok := c.publishLocalMessage(Address{
		LocalTopic:  "local/#",
		RemoteTopic: "remote/#",
		RemovePath:  true,
		QoS:         0,
	}, stores.BrokerMessage{
		TopicName: "local/a",
		Payload:   []byte("x"),
		QoS:       0,
	}, false)
	if !ok {
		t.Fatal("publishLocalMessage returned false for PAHO reconnect buffering")
	}
	if client.publishCount != 1 {
		t.Fatalf("Publish called %d times, want 1", client.publishCount)
	}
}

func TestSubscribeInboundPreservesRetainFlag(t *testing.T) {
	cases := []struct {
		name         string
		addrRetain   bool
		msgRetained  bool
		wantRetained bool
	}{
		{"remote retained true, addr retain false", false, true, true},
		{"remote retained false, addr retain false", false, false, false},
		{"remote retained false, addr retain true", true, false, true},
		{"remote retained true, addr retain true", true, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakePahoClient{connected: true, open: true}
			var publishedRetain bool
			var publishedTopic string
			var publishedPayload []byte
			pub := func(topic string, payload []byte, retain bool, qos byte) error {
				publishedTopic = topic
				publishedPayload = payload
				publishedRetain = retain
				return nil
			}
			c := NewConnector("test", Config{
				Addresses: []Address{
					{
						Mode:        "SUBSCRIBE",
						RemoteTopic: "remote/+",
						LocalTopic:  "local",
						RemovePath:  true,
						Retain:      tc.addrRetain,
						QoS:         0,
					},
				},
			}, pub, nil, nil)

			c.subscribeInbound(client)
			if client.subHandler == nil {
				t.Fatal("subHandler was not set")
			}

			msg := &fakePahoMessage{
				topic:    "remote/sensor",
				payload:  []byte("123"),
				retained: tc.msgRetained,
			}
			client.subHandler(client, msg)

			if publishedTopic != "local/sensor" {
				t.Fatalf("publishedTopic = %q, want local/sensor", publishedTopic)
			}
			if string(publishedPayload) != "123" {
				t.Fatalf("publishedPayload = %q, want 123", string(publishedPayload))
			}
			if publishedRetain != tc.wantRetained {
				t.Fatalf("publishedRetain = %v, want %v", publishedRetain, tc.wantRetained)
			}
		})
	}
}

type fakePahoClient struct {
	connected    bool
	open         bool
	topic        string
	qos          byte
	retained     bool
	payload      interface{}
	publishCount int
	subHandler   paho.MessageHandler
}

func (f *fakePahoClient) IsConnected() bool      { return f.connected }
func (f *fakePahoClient) IsConnectionOpen() bool { return f.open }
func (f *fakePahoClient) Connect() paho.Token    { return fakeToken{} }
func (f *fakePahoClient) Disconnect(uint)        {}
func (f *fakePahoClient) Publish(topic string, qos byte, retained bool, payload interface{}) paho.Token {
	f.publishCount++
	f.topic = topic
	f.qos = qos
	f.retained = retained
	f.payload = payload
	return fakeToken{}
}
func (f *fakePahoClient) Subscribe(topic string, qos byte, handler paho.MessageHandler) paho.Token {
	f.subHandler = handler
	return fakeToken{}
}
func (f *fakePahoClient) SubscribeMultiple(map[string]byte, paho.MessageHandler) paho.Token {
	return fakeToken{}
}
func (f *fakePahoClient) Unsubscribe(...string) paho.Token     { return fakeToken{} }
func (f *fakePahoClient) AddRoute(string, paho.MessageHandler) {}
func (f *fakePahoClient) OptionsReader() paho.ClientOptionsReader {
	return paho.ClientOptionsReader{}
}

type fakePahoMessage struct {
	duplicate bool
	qos       byte
	retained  bool
	topic     string
	messageID uint16
	payload   []byte
}

func (m *fakePahoMessage) Duplicate() bool   { return m.duplicate }
func (m *fakePahoMessage) Qos() byte         { return m.qos }
func (m *fakePahoMessage) Retained() bool    { return m.retained }
func (m *fakePahoMessage) Topic() string     { return m.topic }
func (m *fakePahoMessage) MessageID() uint16 { return m.messageID }
func (m *fakePahoMessage) Payload() []byte   { return m.payload }
func (m *fakePahoMessage) Ack()              {}

type fakeToken struct{}

func (fakeToken) Wait() bool                     { return true }
func (fakeToken) WaitTimeout(time.Duration) bool { return true }
func (fakeToken) Done() <-chan struct{}          { ch := make(chan struct{}); close(ch); return ch }
func (fakeToken) Error() error                   { return nil }

