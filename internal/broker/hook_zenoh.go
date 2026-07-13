package broker

import (
	"bytes"
	"log/slog"
	"time"

	"github.com/google/uuid"
	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/stores"
)

type ZenohPublisher interface {
	PublishMessageToBus(msg stores.BrokerMessage)
}

type ZenohHook struct {
	mqtt.HookBase
	zenoh  ZenohPublisher
	nodeID string
	logger *slog.Logger
}

func NewZenohHook(zenoh ZenohPublisher, nodeID string, logger *slog.Logger) *ZenohHook {
	return &ZenohHook{
		zenoh:  zenoh,
		nodeID: nodeID,
		logger: logger,
	}
}

func (h *ZenohHook) ID() string { return "monstermq-zenoh" }

func (h *ZenohHook) Provides(b byte) bool {
	return bytes.Contains([]byte{
		mqtt.OnPublished,
	}, []byte{b})
}

func (h *ZenohHook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	// Skip messages that originated from Zenoh client (inbound loop prevention)
	if cl.ID == "zenoh-client" {
		return
	}

	// Read special user properties to check if it has origin node
	var origin string
	for _, prop := range pk.Properties.User {
		if prop.Key == "monstermq-origin-node" {
			origin = prop.Val
			break
		}
	}

	// If the message has an origin and it's not our local node, do not republish to Zenoh
	if origin != "" && origin != h.nodeID {
		return
	}

	// Read or generate UUID
	msgUUID := pk.Properties.ReasonString
	if msgUUID == "" {
		msgUUID = uuid.NewString()
	}

	msg := stores.BrokerMessage{
		MessageUUID:  msgUUID,
		MessageID:    pk.PacketID,
		TopicName:    pk.TopicName,
		Payload:      append([]byte(nil), pk.Payload...),
		QoS:          pk.FixedHeader.Qos,
		IsRetain:     pk.FixedHeader.Retain,
		IsDup:        pk.FixedHeader.Dup,
		ClientID:     cl.ID,
		Time:         time.Now().UTC(),
		OriginNodeID: origin,
	}

	if pk.Properties.MessageExpiryInterval > 0 {
		v := pk.Properties.MessageExpiryInterval
		msg.MessageExpiryInterval = &v
	}

	// Forward to Zenoh federation
	h.zenoh.PublishMessageToBus(msg)
}
