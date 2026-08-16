package broker

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"

	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/topic"
)

// StorageHook persists retained messages, sessions, subscriptions, and dispatches
// every published message to:
//   * the in-process pubsub bus (for GraphQL topicUpdates)
//   * the archive group manager (for last-value + history fanout)
//   * the metrics collector (one IncIn per publish, IncOut per Sent packet)
type StorageHook struct {
	mqtt.HookBase
	store            *stores.Storage
	bus              *pubsub.Bus
	subs             *topic.SubscriptionIndex
	archives         ArchiveDispatcher
	logger           *slog.Logger
	nodeID           string
	metrics          MetricsCounter
	retainedInMemory bool // when true, OnRetainMessage skips DB persistence
}

// ArchiveDispatcher receives every published message for archive-group fanout.
// Implemented by archive.Manager. Kept as an interface here to avoid an import cycle.
type ArchiveDispatcher interface {
	Dispatch(msg stores.BrokerMessage)
	// HasGroups reports whether any archive group is active; when false the
	// publish hot path skips building the BrokerMessage for dispatch.
	HasGroups() bool
}

// MetricsCounter is implemented by metrics.Collector.
type MetricsCounter interface {
	IncIn()
	IncOut()
}

func NewStorageHook(s *stores.Storage, bus *pubsub.Bus, subs *topic.SubscriptionIndex, dispatcher ArchiveDispatcher, nodeID string, logger *slog.Logger, m MetricsCounter, retainedInMemory bool) *StorageHook {
	return &StorageHook{store: s, bus: bus, subs: subs, archives: dispatcher, logger: logger, nodeID: nodeID, metrics: m, retainedInMemory: retainedInMemory}
}

func (h *StorageHook) ID() string { return "monstermq-storage" }

func (h *StorageHook) Provides(b byte) bool {
	if h.retainedInMemory && b == mqtt.OnSelectRetainedMessages {
		return false
	}
	return bytes.Contains([]byte{
		mqtt.OnSessionEstablished,
		mqtt.OnDisconnect,
		mqtt.OnSubscribed,
		mqtt.OnUnsubscribed,
		mqtt.OnPublished,
		mqtt.OnRetainMessage,
		mqtt.OnPacketSent,
		mqtt.OnSelectRetainedMessages,
		mqtt.OnClientExpired,
	}, []byte{b})
}

func (h *StorageHook) OnSessionEstablished(cl *mqtt.Client, _ packets.Packet) {
	pv := int(cl.Properties.ProtocolVersion)
	info := stores.SessionInfo{
		ClientID:        cl.ID,
		NodeID:          h.nodeID,
		CleanSession:    cl.Properties.Clean,
		Connected:       true,
		UpdateTime:      time.Now(),
		ClientAddress:   cl.Net.Remote,
		ProtocolVersion: pv,
		Information:     fmt.Sprintf(`{"ProtocolVersion":%d}`, pv),
	}
	if err := h.store.Sessions.SetClient(context.Background(), info); err != nil {
		h.logger.Warn("session persist failed", "client", cl.ID, "err", err)
	}
}

func (h *StorageHook) OnDisconnect(cl *mqtt.Client, _ error, expire bool) {
	if expire {
		if err := h.store.Sessions.DelClient(context.Background(), cl.ID); err != nil {
			h.logger.Warn("session delete failed on disconnect", "client", cl.ID, "err", err)
		}
	} else {
		if err := h.store.Sessions.SetConnected(context.Background(), cl.ID, false); err != nil {
			h.logger.Warn("session disconnect persist failed", "client", cl.ID, "err", err)
		}
	}
}

func (h *StorageHook) OnClientExpired(cl *mqtt.Client) {
	if err := h.store.Sessions.DelClient(context.Background(), cl.ID); err != nil {
		h.logger.Warn("session delete failed on client expiry", "client", cl.ID, "err", err)
	}
}

func (h *StorageHook) OnSubscribed(cl *mqtt.Client, pk packets.Packet, _ []byte) {
	rows := make([]stores.MqttSubscription, 0, len(pk.Filters))
	for _, f := range pk.Filters {
		rows = append(rows, stores.MqttSubscription{
			ClientID:          cl.ID,
			TopicFilter:       f.Filter,
			QoS:               f.Qos,
			NoLocal:           f.NoLocal,
			RetainAsPublished: f.RetainAsPublished,
			RetainHandling:    f.RetainHandling,
		})
		if h.subs != nil {
			h.subs.Subscribe(cl.ID, f.Filter, f.Qos)
		}
	}
	if err := h.store.Subscriptions.AddSubscriptions(context.Background(), rows); err != nil {
		h.logger.Warn("subscriptions persist failed", "client", cl.ID, "err", err)
	}
}

func (h *StorageHook) OnUnsubscribed(cl *mqtt.Client, pk packets.Packet) {
	rows := make([]stores.MqttSubscription, 0, len(pk.Filters))
	for _, f := range pk.Filters {
		rows = append(rows, stores.MqttSubscription{ClientID: cl.ID, TopicFilter: f.Filter})
		if h.subs != nil {
			h.subs.Unsubscribe(cl.ID, f.Filter)
		}
	}
	if err := h.store.Subscriptions.DelSubscriptions(context.Background(), rows); err != nil {
		h.logger.Warn("subscriptions delete failed", "client", cl.ID, "err", err)
	}
}

func (h *StorageHook) OnPacketSent(_ *mqtt.Client, pk packets.Packet, _ []byte) {
	if h.metrics != nil && pk.FixedHeader.Type == packets.Publish {
		h.metrics.IncOut()
	}
}

func (h *StorageHook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	if h.metrics != nil {
		h.metrics.IncIn()
	}
	hasBus := h.bus != nil && h.bus.HasSubscribers()
	hasArchive := h.archives != nil && h.archives.HasGroups()
	if !hasBus && !hasArchive {
		return // nobody consumes the message; skip uuid/copy/dispatch entirely
	}
	msg := stores.BrokerMessage{
		MessageUUID: uuid.NewString(),
		MessageID:   pk.PacketID,
		TopicName:   pk.TopicName,
		Payload:     append([]byte(nil), pk.Payload...),
		QoS:         pk.FixedHeader.Qos,
		IsRetain:    pk.FixedHeader.Retain,
		IsDup:       pk.FixedHeader.Dup,
		ClientID:    cl.ID,
		Time:        time.Now().UTC(),
	}
	if pk.Properties.MessageExpiryInterval > 0 {
		v := pk.Properties.MessageExpiryInterval
		msg.MessageExpiryInterval = &v
	}
	if hasBus {
		h.bus.Publish(msg)
	}
	if hasArchive {
		h.archives.Dispatch(msg)
	}
}

// OnRetainMessage is called when a message with retain=true is published. r=1 set, r=-1 clear.
func (h *StorageHook) OnRetainMessage(cl *mqtt.Client, pk packets.Packet, r int64) {
	ctx := context.Background()
	if r == -1 || len(pk.Payload) == 0 {
		_ = h.store.Retained.DelAll(ctx, []string{pk.TopicName})
		return
	}
	clientID := ""
	if cl != nil {
		clientID = cl.ID
	}
	msg := stores.BrokerMessage{
		MessageUUID: uuid.NewString(),
		TopicName:   pk.TopicName,
		Payload:     append([]byte(nil), pk.Payload...),
		QoS:         pk.FixedHeader.Qos,
		IsRetain:    true,
		ClientID:    clientID,
		Time:        time.Now().UTC(),
	}
	if err := h.store.Retained.AddAll(ctx, []stores.BrokerMessage{msg}); err != nil {
		h.logger.Warn("retained persist failed", "topic", pk.TopicName, "err", err)
	}
}

// OnSelectRetainedMessages returns matching retained messages from the store.
func (h *StorageHook) OnSelectRetainedMessages(filter string) ([]packets.Packet, error) {
	if h.retainedInMemory {
		return nil, nil
	}
	ctx := context.Background()
	var pks []packets.Packet
	err := h.store.Retained.FindMatchingMessages(ctx, filter, func(msg stores.BrokerMessage) bool {
		pk := packets.Packet{
			FixedHeader: packets.FixedHeader{
				Type:   packets.Publish,
				Qos:    msg.QoS,
				Retain: true,
			},
			TopicName: msg.TopicName,
			Payload:   msg.Payload,
		}
		if msg.MessageExpiryInterval != nil {
			pk.Properties.MessageExpiryInterval = *msg.MessageExpiryInterval
		}
		pks = append(pks, pk)
		return true
	})
	if err != nil {
		return nil, err
	}
	return pks, nil
}
