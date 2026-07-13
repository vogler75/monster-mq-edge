package zenoh

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"monstermq.io/edge/internal/stores"
)

const envelopeVersion = 1

type metadataEnvelope struct {
	Version                int               `json:"version"`
	Origin                 string            `json:"origin"`
	MessageUUID            string            `json:"messageUuid"`
	MessageID              uint16            `json:"messageId"`
	QoS                    byte              `json:"qos"`
	Retain                 bool              `json:"retain"`
	Dup                    bool              `json:"dup"`
	Queued                 bool              `json:"queued"`
	ClientID               string            `json:"clientId"`
	SenderID               string            `json:"senderId"`
	Time                   int64             `json:"time"`
	MessageExpiryInterval  *uint32           `json:"messageExpiryInterval,omitempty"`
	PayloadFormatIndicator *byte             `json:"payloadFormatIndicator,omitempty"`
	ContentType            string            `json:"contentType,omitempty"`
	ResponseTopic          string            `json:"responseTopic,omitempty"`
	CorrelationData        string            `json:"correlationData,omitempty"`
	UserProperties         map[string]string `json:"userProperties,omitempty"`
}

// EncodeEnvelope serializes the metadata envelope to JSON bytes
func EncodeEnvelope(origin string, msg stores.BrokerMessage) ([]byte, error) {
	var corrData string
	if len(msg.CorrelationData) > 0 {
		corrData = base64.StdEncoding.EncodeToString(msg.CorrelationData)
	}

	env := metadataEnvelope{
		Version:                envelopeVersion,
		Origin:                 origin,
		MessageUUID:            msg.MessageUUID,
		MessageID:              msg.MessageID,
		QoS:                    msg.QoS,
		Retain:                 msg.IsRetain,
		Dup:                    msg.IsDup,
		Queued:                 msg.IsQueued,
		ClientID:               msg.ClientID,
		SenderID:               msg.ClientID, // Map ClientID to SenderID as fallback
		Time:                   msg.Time.UnixMilli(),
		MessageExpiryInterval:  msg.MessageExpiryInterval,
		PayloadFormatIndicator: msg.PayloadFormatIndicator,
		ContentType:            msg.ContentType,
		ResponseTopic:          msg.ResponseTopic,
		CorrelationData:        corrData,
		UserProperties:         msg.UserProperties,
	}

	return json.Marshal(env)
}

// DecodeEnvelope parses the JSON metadata envelope or falls back to raw message
func DecodeEnvelope(topic string, payload []byte, attachment []byte) (string, stores.BrokerMessage) {
	if len(attachment) == 0 {
		return "", nativeMessage(topic, payload)
	}

	var env metadataEnvelope
	if err := json.Unmarshal(attachment, &env); err != nil || env.Version != envelopeVersion {
		return "", nativeMessage(topic, payload)
	}

	var corrData []byte
	if env.CorrelationData != "" {
		if d, err := base64.StdEncoding.DecodeString(env.CorrelationData); err == nil {
			corrData = d
		}
	}

	msgUUID := env.MessageUUID
	if msgUUID == "" {
		msgUUID = uuid.NewString()
	}

	msg := stores.BrokerMessage{
		MessageUUID:            msgUUID,
		MessageID:              env.MessageID,
		TopicName:              topic,
		Payload:                payload,
		QoS:                    env.QoS,
		IsRetain:               env.Retain,
		IsDup:                  env.Dup,
		IsQueued:               env.Queued,
		ClientID:               env.ClientID,
		Time:                   time.UnixMilli(env.Time),
		OriginNodeID:           env.Origin,
		PayloadFormatIndicator: env.PayloadFormatIndicator,
		MessageExpiryInterval:  env.MessageExpiryInterval,
		ContentType:            env.ContentType,
		ResponseTopic:          env.ResponseTopic,
		CorrelationData:        corrData,
		UserProperties:         env.UserProperties,
	}

	return env.Origin, msg
}

func nativeMessage(topic string, payload []byte) stores.BrokerMessage {
	return stores.BrokerMessage{
		MessageUUID: uuid.NewString(),
		MessageID:   0,
		TopicName:   topic,
		Payload:     payload,
		QoS:         0,
		IsRetain:    false,
		IsDup:       false,
		IsQueued:    false,
		ClientID:    "zenoh",
		Time:        time.Now().UTC(),
	}
}
