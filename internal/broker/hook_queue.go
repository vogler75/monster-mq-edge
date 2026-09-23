package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"

	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/topic"
)

// QueueHook persists publishes for offline persistent (clean=false) subscribers
// in the configured QueueStore and replays them when the client reconnects.
//
// Without this hook the broker still works — the MQTT engine holds inflight messages
// in memory per client — but those messages are lost when the broker restarts.
// With it enabled, every publish that matches a disconnected persistent
// session's subscription is enqueued to a row in the messagequeue table; on
// reconnect the rows are dequeued and written directly to the now-online client.
type QueueHook struct {
	mqtt.HookBase
	store            *stores.Storage
	subs             *topic.SubscriptionIndex
	server           *mqtt.Server
	logger           *slog.Logger
	maxQueueMessages int
	mu               sync.RWMutex
	persistent       map[string]bool
	// offline is the subset of persistent clients that are currently
	// disconnected — the only ones OnPublished ever enqueues for. Kept
	// separately so the publish hot path can bail out with a single
	// length check instead of resolving subscribers on every message.
	offline         map[string]struct{}
	clientUsernames map[string]string

	pendingByPacketID map[string]map[uint16]string // clientID -> packetID -> messageUUID
	pendingByUUID     map[string]map[string]uint16 // clientID -> messageUUID -> packetID
}

func NewQueueHook(s *stores.Storage, subs *topic.SubscriptionIndex, server *mqtt.Server, logger *slog.Logger, maxQueue int) *QueueHook {
	h := &QueueHook{
		store:             s,
		subs:              subs,
		server:            server,
		logger:            logger,
		maxQueueMessages:  maxQueue,
		persistent:        make(map[string]bool),
		offline:           make(map[string]struct{}),
		clientUsernames:   make(map[string]string),
		pendingByPacketID: make(map[string]map[uint16]string),
		pendingByUUID:     make(map[string]map[string]uint16),
	}
	h.hydratePersistentClients()
	return h
}

func (h *QueueHook) hydratePersistentClients() {
	ctx := context.Background()
	err := h.store.Sessions.IterateSessions(ctx, func(info stores.SessionInfo) bool {
		if !info.CleanSession {
			h.mu.Lock()
			h.persistent[info.ClientID] = true
			h.offline[info.ClientID] = struct{}{} // nobody is connected yet at hydrate time
			if info.Information != "" {
				var parsed struct {
					Username string `json:"Username"`
				}
				if json.Unmarshal([]byte(info.Information), &parsed) == nil && parsed.Username != "" {
					h.clientUsernames[info.ClientID] = parsed.Username
				}
			}
			h.mu.Unlock()
		}
		return true
	})
	if err != nil {
		h.logger.Error("queue hook: failed to hydrate persistent clients", "err", err)
	}
}

func (h *QueueHook) ID() string { return "monstermq-queue" }

func (h *QueueHook) Provides(b byte) bool {
	return bytes.Contains([]byte{
		mqtt.OnPublished,
		mqtt.OnSessionEstablished,
		mqtt.OnDisconnect,
		mqtt.OnClientExpired,
		mqtt.OnQosComplete,
		mqtt.StoredQueuedMessages,
	}, []byte{b})
}

// OnPublished resolves matching subscriptions via the in-memory subscription
// index, filters for persistent (clean=false) sessions that are currently
// disconnected, and enqueues a copy of the message for each.
func (h *QueueHook) OnPublished(_ *mqtt.Client, pk packets.Packet) {
	h.mu.RLock()
	noneOffline := len(h.offline) == 0
	h.mu.RUnlock()
	if noneOffline {
		return
	}

	ctx := context.Background()
	subs, err := h.collectOfflineSubscribers(ctx, pk.TopicName)
	if err != nil {
		h.logger.Warn("queue hook: collect offline subs failed", "topic", pk.TopicName, "err", err)
		return
	}
	if len(subs) == 0 {
		return
	}

	msg := stores.BrokerMessage{
		MessageUUID: uuid.NewString(),
		MessageID:   pk.PacketID,
		TopicName:   pk.TopicName,
		Payload:     append([]byte(nil), pk.Payload...),
		QoS:         pk.FixedHeader.Qos,
		IsRetain:    pk.FixedHeader.Retain,
		Time:        time.Now().UTC(),
	}
	result, err := h.store.Queue.EnqueueMultiLimited(ctx, msg, subs, int64(h.maxQueueMessages))
	if err != nil {
		h.logger.Warn("queue hook: enqueue failed", "topic", pk.TopicName, "n", len(subs), "err", err)
		return
	}
	if len(result.Rejected) > 0 {
		h.logger.Warn("queue hook: client queues full, message dropped", "topic", pk.TopicName, "clients", len(result.Rejected), "limit", h.maxQueueMessages)
	}
}

// collectOfflineSubscribers resolves the persisted subscription set for the
// topic via the in-memory dual index (O(1) exact + O(depth) wildcard) and
// keeps only those whose owning session is persistent (clean=false) and
// currently disconnected.
func (h *QueueHook) collectOfflineSubscribers(ctx context.Context, topicName string) ([]string, error) {
	if h.subs == nil {
		return nil, nil
	}
	candidates := h.subs.FindSubscribers(topicName)
	if len(candidates) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(candidates))
	h.mu.RLock()
	for _, c := range candidates {
		if _, off := h.offline[c.ClientID]; !off {
			continue
		}
		out = append(out, c.ClientID)
	}
	h.mu.RUnlock()
	// Confirm against live connection state: the offline set is maintained by
	// session hooks and can briefly lag a reconnect.
	live := out[:0]
	for _, cid := range out {
		if cl, ok := h.server.Clients.Get(cid); ok && !cl.Closed() {
			continue
		}

		// Defense-in-depth: check if offline client is authorized to subscribe/read topicName
		if h.server != nil {
			var clientForCheck *mqtt.Client
			if cl, ok := h.server.Clients.Get(cid); ok {
				clientForCheck = cl
			} else {
				h.mu.RLock()
				uname := h.clientUsernames[cid]
				h.mu.RUnlock()
				clientForCheck = &mqtt.Client{
					ID: cid,
					Properties: mqtt.ClientProperties{
						Username: []byte(uname),
					},
				}
			}
			if !h.server.Hooks().OnACLCheck(clientForCheck, topicName, false) {
				continue
			}
		}

		live = append(live, cid)
	}
	return live, nil
}

// OnSessionEstablished dequeues any stored messages for the (re)connecting
// client and writes them out as PUBLISH packets. Only runs for persistent
// (clean=false) sessions.
//
// The MQTT engine also maintains an in-memory inflight buffer per client that
// survives a clean=false disconnect (within the same process). On reconnect,
// the engine calls cl.ResendInflightMessages BEFORE this hook fires. So if the engine
// already had something to resend, the client just received it via that path
// and we must NOT also replay our DB queue, or every message arrives twice.
//
// Gating rule:
//   - in-memory inflight non-empty → in-process reconnect; handled in memory.
//     Purge our DB queue so it doesn't double-fire.
//   - in-memory inflight empty     → post-restart (or first attach); no in-memory
//     history. Drain our DB queue and replay.
func (h *QueueHook) OnSessionEstablished(cl *mqtt.Client, _ packets.Packet) {
	persistent := !((cl.Properties.ProtocolVersion == 5 && cl.Properties.Props.SessionExpiryInterval == 0) || (cl.Properties.ProtocolVersion < 5 && cl.Properties.Clean))
	h.mu.Lock()
	if persistent {
		h.persistent[cl.ID] = true
		h.clientUsernames[cl.ID] = string(cl.Properties.Username)
	} else {
		delete(h.persistent, cl.ID)
		delete(h.clientUsernames, cl.ID)
	}
	delete(h.offline, cl.ID)
	h.mu.Unlock()

	if cl.Properties.Clean {
		h.clearPendingAcks(cl.ID)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := h.store.Queue.PurgeForClient(ctx, cl.ID); err != nil {
			h.logger.Warn("queue hook: purge for clean client failed", "client", cl.ID, "err", err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := h.store.Queue.ResetVisibility(ctx, cl.ID); err != nil {
		h.logger.Warn("queue hook: reset visibility failed", "client", cl.ID, "err", err)
	}

	for {
		batch, err := h.store.Queue.Dequeue(ctx, cl.ID, 100)
		if err != nil {
			h.logger.Warn("queue hook: dequeue failed", "client", cl.ID, "err", err)
			return
		}
		if len(batch) == 0 {
			return
		}
		for _, m := range batch {
			// Defense-in-depth: verify ACL authorization before writing packet to reconnected client
			if h.server != nil && !h.server.Hooks().OnACLCheck(cl, m.TopicName, false) {
				h.logger.Warn("queue hook: dropping queued message due to ACL denial on replay", "client", cl.ID, "topic", m.TopicName)
				if err := h.store.Queue.Ack(ctx, cl.ID, m.MessageUUID); err != nil {
					h.logger.Warn("queue hook: ack unauthorized message failed", "client", cl.ID, "uuid", m.MessageUUID, "err", err)
				}
				continue
			}

			// If message is already tracked as pending ack for this client (e.g. resent by engine in-process),
			// do not duplicate write or allocate a new packet ID.
			if h.isPendingAckUUID(cl.ID, m.MessageUUID) {
				continue
			}

			pk := packets.Packet{
				FixedHeader: packets.FixedHeader{
					Type:   packets.Publish,
					Qos:    m.QoS,
					Retain: false,
				},
				TopicName: m.TopicName,
				Payload:   m.Payload,
				Origin:    cl.ID,
			}

			if m.QoS == 0 {
				if err := cl.WritePacket(pk); err != nil {
					h.logger.Warn("queue hook: write packet failed", "client", cl.ID, "topic", m.TopicName, "err", err)
					return
				}
				if err := h.store.Queue.Ack(ctx, cl.ID, m.MessageUUID); err != nil {
					h.logger.Warn("queue hook: ack failed", "client", cl.ID, "uuid", m.MessageUUID, "err", err)
				}
			} else {
				// QoS 1 or QoS 2: allocate packet ID, add to inflight, and track correlation
				pid, err := cl.NextPacketID()
				if err != nil {
					h.logger.Warn("queue hook: next packet id failed", "client", cl.ID, "err", err)
					return
				}
				pk.PacketID = uint16(pid)
				if ok := cl.State.Inflight.Set(pk); ok {
					cl.State.Inflight.DecreaseSendQuota()
					if h.server != nil {
						atomic.AddInt64(&h.server.Info.Inflight, 1)
					}
				}
				h.recordPendingAck(cl.ID, pk.PacketID, m.MessageUUID)

				if err := cl.WritePacket(pk); err != nil {
					h.logger.Warn("queue hook: write packet failed", "client", cl.ID, "topic", m.TopicName, "err", err)
					cl.State.Inflight.Delete(pk.PacketID)
					cl.State.Inflight.IncreaseSendQuota()
					if h.server != nil {
						atomic.AddInt64(&h.server.Info.Inflight, -1)
					}
					h.deletePendingAck(cl.ID, pk.PacketID)
					return
				}
			}
		}
	}
}

func (h *QueueHook) recordPendingAck(clientID string, packetID uint16, messageUUID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pendingByPacketID[clientID] == nil {
		h.pendingByPacketID[clientID] = make(map[uint16]string)
	}
	if h.pendingByUUID[clientID] == nil {
		h.pendingByUUID[clientID] = make(map[string]uint16)
	}
	h.pendingByPacketID[clientID][packetID] = messageUUID
	h.pendingByUUID[clientID][messageUUID] = packetID
}

func (h *QueueHook) isPendingAckUUID(clientID string, messageUUID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if m, ok := h.pendingByUUID[clientID]; ok {
		_, exists := m[messageUUID]
		return exists
	}
	return false
}

func (h *QueueHook) deletePendingAck(clientID string, packetID uint16) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if uuid, ok := h.pendingByPacketID[clientID][packetID]; ok {
		delete(h.pendingByPacketID[clientID], packetID)
		if h.pendingByUUID[clientID] != nil {
			delete(h.pendingByUUID[clientID], uuid)
		}
		if len(h.pendingByPacketID[clientID]) == 0 {
			delete(h.pendingByPacketID, clientID)
			delete(h.pendingByUUID, clientID)
		}
	}
}

func (h *QueueHook) clearPendingAcks(clientID string) {
	h.mu.Lock()
	delete(h.pendingByPacketID, clientID)
	delete(h.pendingByUUID, clientID)
	h.mu.Unlock()
}

func (h *QueueHook) OnQosComplete(cl *mqtt.Client, pk packets.Packet) {
	if cl == nil || pk.PacketID == 0 {
		return
	}
	h.mu.Lock()
	uuid, ok := h.pendingByPacketID[cl.ID][pk.PacketID]
	if ok {
		delete(h.pendingByPacketID[cl.ID], pk.PacketID)
		if h.pendingByUUID[cl.ID] != nil {
			delete(h.pendingByUUID[cl.ID], uuid)
		}
		if len(h.pendingByPacketID[cl.ID]) == 0 {
			delete(h.pendingByPacketID, cl.ID)
			delete(h.pendingByUUID, cl.ID)
		}
	}
	h.mu.Unlock()

	if ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.store.Queue.Ack(ctx, cl.ID, uuid); err != nil {
			h.logger.Warn("queue hook: ack failed on qos complete", "client", cl.ID, "packet_id", pk.PacketID, "uuid", uuid, "err", err)
		}
	}
}

func (h *QueueHook) OnDisconnect(cl *mqtt.Client, _ error, expire bool) {
	h.mu.Lock()
	if expire {
		delete(h.persistent, cl.ID)
		delete(h.offline, cl.ID)
		delete(h.clientUsernames, cl.ID)
	} else if h.persistent[cl.ID] {
		h.offline[cl.ID] = struct{}{}
		h.clientUsernames[cl.ID] = string(cl.Properties.Username)
	}
	h.mu.Unlock()
	if expire {
		h.clearPendingAcks(cl.ID)
	}
}

func (h *QueueHook) OnClientExpired(cl *mqtt.Client) {
	h.mu.Lock()
	delete(h.persistent, cl.ID)
	delete(h.offline, cl.ID)
	delete(h.clientUsernames, cl.ID)
	h.mu.Unlock()
	h.clearPendingAcks(cl.ID)
}
