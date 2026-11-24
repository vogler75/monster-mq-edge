package mqtt

import (
	"io"
	"log/slog"
	"testing"

	"monstermq.io/edge/internal/mqtt/packets"
)

// receiveLivePublish subscribes an MQTT 5 client to topic, publishes pk on
// the inline client and returns the PUBLISH that client receives.
func receiveLivePublish(t *testing.T, topic string, pk packets.Packet) packets.Packet {
	t.Helper()
	s := New(&Options{
		InlineClient: true,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(func() { _ = s.Close() })
	if err := s.AddHook(new(aclRecHook), nil); err != nil {
		t.Fatal(err)
	}

	conn, r := connectV5PipeClient(t, s, "expiry-sub", 10)
	writeClientPacket(t, conn, packets.Packet{
		FixedHeader: packets.FixedHeader{Type: packets.Subscribe, Qos: 1},
		PacketID:    1,
		Filters:     packets.Subscriptions{{Filter: topic}},
	})
	if suback := readServerPacket(t, r); suback.FixedHeader.Type != packets.Suback {
		t.Fatalf("expected SUBACK, got %v", suback.FixedHeader.Type)
	}

	pk.FixedHeader.Type = packets.Publish
	pk.TopicName = topic
	pk.Payload = []byte("v1")
	errc := make(chan error, 1)
	go func() { errc <- s.PublishPacket(pk) }()
	got := readServerPacket(t, r)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if got.FixedHeader.Type != packets.Publish {
		t.Fatalf("expected PUBLISH, got %v", got.FixedHeader.Type)
	}
	return got
}

// A publish without a Message Expiry Interval reaches subscribers without one:
// the server maximum only limits how long the server keeps the message.
func TestLivePublishWithoutExpiryCarriesNoExpiry(t *testing.T) {
	got := receiveLivePublish(t, "dfm/model", packets.Packet{FixedHeader: packets.FixedHeader{Retain: true}})
	if got.Properties.MessageExpiryInterval != 0 {
		t.Fatalf("MessageExpiryInterval = %d, want none", got.Properties.MessageExpiryInterval)
	}
}

// A publish with a Message Expiry Interval reaches subscribers with the time
// it has left [MQTT-3.3.2-6].
func TestLivePublishWithExpiryCarriesRemainingTime(t *testing.T) {
	got := receiveLivePublish(t, "dfm/model", packets.Packet{
		FixedHeader: packets.FixedHeader{Retain: true},
		Properties:  packets.Properties{MessageExpiryInterval: 600},
	})
	if v := got.Properties.MessageExpiryInterval; v < 599 || v > 600 {
		t.Fatalf("MessageExpiryInterval = %d, want 599-600", v)
	}
}
