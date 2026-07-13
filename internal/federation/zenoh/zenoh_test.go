package zenoh

import (
	"bytes"
	"testing"
	"time"

	"monstermq.io/edge/internal/stores"
)

func TestTopicMapping(t *testing.T) {
	tests := []struct {
		topic       string
		localPref   string
		remotePref  string
		expectedKey string
	}{
		{"factory/line1/temp", "", "monstermq/mqtt", "monstermq/mqtt/factory/line1/temp"},
		{"data/zenoh/sensor/cpu", "data/zenoh", "monstermq/mqtt", "monstermq/mqtt/sensor/cpu"},
		{"other/topic", "data/zenoh", "monstermq/mqtt", ""},
	}

	for _, tt := range tests {
		key := MapToZenohKey(tt.topic, tt.localPref, tt.remotePref)
		if key != tt.expectedKey {
			t.Errorf("MapToZenohKey(%q, %q, %q) = %q; want %q", tt.topic, tt.localPref, tt.remotePref, key, tt.expectedKey)
		}

		if tt.expectedKey != "" {
			mappedMqtt := MapToMqttTopic(tt.expectedKey, tt.localPref, tt.remotePref)
			if mappedMqtt != tt.topic {
				t.Errorf("MapToMqttTopic(%q, %q, %q) = %q; want %q", tt.expectedKey, tt.localPref, tt.remotePref, mappedMqtt, tt.topic)
			}
		}
	}
}

func TestSubscriptionKey(t *testing.T) {
	tests := []struct {
		filter      string
		localPref   string
		remotePref  string
		expectedKey string
	}{
		{"#", "", "monstermq/mqtt", "monstermq/mqtt/**"},
		{"+", "", "monstermq/mqtt", "monstermq/mqtt/*"},
		{"factory/+/temp", "", "monstermq/mqtt", "monstermq/mqtt/factory/*/temp"},
		{"data/zenoh/sensor/#", "data/zenoh", "monstermq/mqtt", "monstermq/mqtt/sensor/**"},
	}

	for _, tt := range tests {
		key := SubscriptionKey(tt.filter, tt.localPref, tt.remotePref)
		if key != tt.expectedKey {
			t.Errorf("SubscriptionKey(%q, %q, %q) = %q; want %q", tt.filter, tt.localPref, tt.remotePref, key, tt.expectedKey)
		}
	}
}

func TestMinimalFilters(t *testing.T) {
	filters := []string{"#", "a/#", "a/b/c", "a/+/c"}
	minimal := MinimalFilters(filters)
	if len(minimal) != 1 || minimal[0] != "#" {
		t.Errorf("MinimalFilters(%v) = %v; want [#]", filters, minimal)
	}

	filters2 := []string{"a/+", "a/b", "a/c"}
	minimal2 := MinimalFilters(filters2)
	if len(minimal2) != 1 || minimal2[0] != "a/+" {
		t.Errorf("MinimalFilters(%v) = %v; want [a/+]", filters2, minimal2)
	}
}

func TestSeenCache(t *testing.T) {
	cache := NewSeenCache(3, 1) // max size 3, TTL 1 second
	if !cache.Remember("uuid-1") {
		t.Error("expected true on first remember")
	}
	if cache.Remember("uuid-1") {
		t.Error("expected false on duplicate remember")
	}

	cache.Remember("uuid-2")
	cache.Remember("uuid-3")

	// Size is now 3. Adding another should evict one since max size is 3.
	cache.Remember("uuid-4")

	time.Sleep(1100 * time.Millisecond) // Wait for TTL expiry
	if !cache.Remember("uuid-1") {
		t.Error("expected true after TTL expiry")
	}
}

func TestEnvelopeSerialization(t *testing.T) {
	expiryVal := uint32(3600)
	fmtVal := byte(1)
	msg := stores.BrokerMessage{
		MessageUUID:            "test-uuid-123",
		MessageID:              456,
		TopicName:              "test/topic",
		Payload:                []byte("hello world"),
		QoS:                    1,
		IsRetain:               true,
		IsDup:                  false,
		IsQueued:               false,
		ClientID:               "client-a",
		Time:                   time.Now().Truncate(time.Millisecond),
		PayloadFormatIndicator: &fmtVal,
		MessageExpiryInterval:  &expiryVal,
		ContentType:            "text/plain",
		ResponseTopic:          "resp/topic",
		CorrelationData:        []byte("corr-123"),
		UserProperties:         map[string]string{"foo": "bar"},
	}

	envBytes, err := EncodeEnvelope("node-a", msg)
	if err != nil {
		t.Fatalf("EncodeEnvelope failed: %v", err)
	}

	origin, decodedMsg := DecodeEnvelope("test/topic", msg.Payload, envBytes)
	if origin != "node-a" {
		t.Errorf("decoded origin = %q; want %q", origin, "node-a")
	}
	if decodedMsg.MessageUUID != msg.MessageUUID {
		t.Errorf("decoded UUID = %q; want %q", decodedMsg.MessageUUID, msg.MessageUUID)
	}
	if decodedMsg.MessageID != msg.MessageID {
		t.Errorf("decoded MessageID = %d; want %d", decodedMsg.MessageID, msg.MessageID)
	}
	if !bytes.Equal(decodedMsg.Payload, msg.Payload) {
		t.Errorf("decoded Payload = %q; want %q", decodedMsg.Payload, msg.Payload)
	}
	if decodedMsg.QoS != msg.QoS {
		t.Errorf("decoded QoS = %d; want %d", decodedMsg.QoS, msg.QoS)
	}
	if decodedMsg.IsRetain != msg.IsRetain {
		t.Errorf("decoded IsRetain = %v; want %v", decodedMsg.IsRetain, msg.IsRetain)
	}
	if decodedMsg.ClientID != msg.ClientID {
		t.Errorf("decoded ClientID = %q; want %q", decodedMsg.ClientID, msg.ClientID)
	}
	if !decodedMsg.Time.Equal(msg.Time) {
		t.Errorf("decoded Time = %v; want %v", decodedMsg.Time, msg.Time)
	}
	if *decodedMsg.PayloadFormatIndicator != *msg.PayloadFormatIndicator {
		t.Errorf("decoded PayloadFormatIndicator = %d; want %d", *decodedMsg.PayloadFormatIndicator, *msg.PayloadFormatIndicator)
	}
	if *decodedMsg.MessageExpiryInterval != *msg.MessageExpiryInterval {
		t.Errorf("decoded MessageExpiryInterval = %d; want %d", *decodedMsg.MessageExpiryInterval, *msg.MessageExpiryInterval)
	}
	if decodedMsg.ContentType != msg.ContentType {
		t.Errorf("decoded ContentType = %q; want %q", decodedMsg.ContentType, msg.ContentType)
	}
	if decodedMsg.ResponseTopic != msg.ResponseTopic {
		t.Errorf("decoded ResponseTopic = %q; want %q", decodedMsg.ResponseTopic, msg.ResponseTopic)
	}
	if !bytes.Equal(decodedMsg.CorrelationData, msg.CorrelationData) {
		t.Errorf("decoded CorrelationData = %q; want %q", decodedMsg.CorrelationData, msg.CorrelationData)
	}
	if decodedMsg.UserProperties["foo"] != msg.UserProperties["foo"] {
		t.Errorf("decoded UserProperties = %v; want %v", decodedMsg.UserProperties, msg.UserProperties)
	}
}
