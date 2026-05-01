package mqttclient

import (
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
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
	client := &fakePahoClient{connected: true}
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

type fakePahoClient struct {
	connected bool
	topic     string
	qos       byte
	retained  bool
	payload   interface{}
}

func (f *fakePahoClient) IsConnected() bool      { return f.connected }
func (f *fakePahoClient) IsConnectionOpen() bool { return f.connected }
func (f *fakePahoClient) Connect() paho.Token    { return fakeToken{} }
func (f *fakePahoClient) Disconnect(uint)        {}
func (f *fakePahoClient) Publish(topic string, qos byte, retained bool, payload interface{}) paho.Token {
	f.topic = topic
	f.qos = qos
	f.retained = retained
	f.payload = payload
	return fakeToken{}
}
func (f *fakePahoClient) Subscribe(string, byte, paho.MessageHandler) paho.Token {
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

type fakeToken struct{}

func (fakeToken) Wait() bool                     { return true }
func (fakeToken) WaitTimeout(time.Duration) bool { return true }
func (fakeToken) Done() <-chan struct{}          { ch := make(chan struct{}); close(ch); return ch }
func (fakeToken) Error() error                   { return nil }
