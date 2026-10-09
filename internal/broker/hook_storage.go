package broker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"monstermq.io/edge/internal/config"
	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/hooks/storage"
	"monstermq.io/edge/internal/mqtt/packets"

	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/topic"
)

// StorageHook persists retained messages, sessions, subscriptions, and dispatches
// every published message to:
//   - the in-process pubsub bus (for GraphQL topicUpdates)
//   - the archive group manager (for last-value + history fanout)
//   - the metrics collector (one IncIn per publish, IncOut per Sent packet)
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
	server           *mqtt.Server
	// replicated reports topics whose publishes the broker does not deliver
	// itself but receives back from WinCC OA (native topics branch); the
	// bus and archives see only the delivered message.
	replicated func(topic string) bool

	// PeerLink replicas (pk.Forward != nil). Set before the server starts.
	peer PeerPolicy
	// retainedViaOA reports sources whose link has oaRetained: WinCC OA
	// already stores and replicates their MMQRetained writes, so a retained
	// replica only updates the in-memory view of the oastore (ApplyCached).
	retainedViaOA func(source string) bool
	replicas      *replicaRetained
}

// PeerPolicy is how the broker hooks treat PeerLink replicas, from
// PeerLink.Receive. Live delivery and the retained store always get them.
type PeerPolicy struct {
	Bus     bool // pubsub bus: GraphQL topicUpdates, scripts, REST SSE, bridges
	Archive bool // archive groups
	Queue   bool // offline queues of persistent sessions (QueueHook)
}

// DefaultPeerPolicy is the policy of an unset PeerLink.Receive section.
func DefaultPeerPolicy() PeerPolicy { return PeerPolicy{Bus: true, Archive: true} }

// NewPeerPolicy reads the policy from the PeerLink receive settings.
func NewPeerPolicy(r config.PeerLinkReceive) PeerPolicy {
	return PeerPolicy{Bus: r.GetBus(), Archive: r.GetArchive(), Queue: r.Queue}
}

// retainedCache is implemented by the WinCC OA retained store.
type retainedCache interface {
	ApplyCached(msgs []stores.BrokerMessage)
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
	IncBusIn()
	IncClientIn(clientID string)
	IncClientOut(clientID string)
	ForgetClient(clientID string)
}

func NewStorageHook(s *stores.Storage, bus *pubsub.Bus, subs *topic.SubscriptionIndex, dispatcher ArchiveDispatcher, nodeID string, logger *slog.Logger, m MetricsCounter, retainedInMemory bool, server *mqtt.Server) *StorageHook {
	return &StorageHook{store: s, bus: bus, subs: subs, archives: dispatcher, logger: logger, nodeID: nodeID, metrics: m, retainedInMemory: retainedInMemory, server: server,
		peer: DefaultPeerPolicy(), replicas: newReplicaRetained()}
}

// SetPeerPolicy sets how PeerLink replicas reach the bus and the archives.
// Call before the server starts.
func (h *StorageHook) SetPeerPolicy(p PeerPolicy) { h.peer = p }

// SetRetainedViaOA sets the per-source oaRetained predicate of PeerLink.
// Call before the server starts; it must be cheap (one atomic load).
func (h *StorageHook) SetRetainedViaOA(fn func(source string) bool) { h.retainedViaOA = fn }

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
		mqtt.StoredClientByID,
	}, []byte{b})
}

func (h *StorageHook) OnSessionEstablished(cl *mqtt.Client, _ packets.Packet) {
	ctx := context.Background()
	if cl.Properties.Clean {
		if err := h.store.Sessions.DelClient(ctx, cl.ID); err != nil {
			h.logger.Warn("clean session purge failed", "client", cl.ID, "err", err)
		}
		if h.subs != nil {
			h.subs.DisconnectClient(cl.ID)
		}
	}

	pv := int(cl.Properties.ProtocolVersion)
	var sei int64
	if cl.Properties.Props.SessionExpiryIntervalFlag {
		sei = int64(cl.Properties.Props.SessionExpiryInterval)
	}
	// The stored clean_session flag means "not persistent", as in the Java
	// broker: for MQTT 5 that is a zero session expiry, not the Clean Start
	// flag (Clean Start only discards the previous session, done above).
	cleanSession := cl.Properties.Clean
	if pv == 5 {
		cleanSession = sei == 0
	}
	info := stores.SessionInfo{
		ClientID:              cl.ID,
		NodeID:                h.nodeID,
		CleanSession:          cleanSession,
		Connected:             true,
		UpdateTime:            time.Now(),
		ClientAddress:         cl.Net.Remote,
		ProtocolVersion:       pv,
		SessionExpiryInterval: sei,
		Information:           fmt.Sprintf(`{"ProtocolVersion":%d,"Username":%q,"sessionExpiryInterval":%d,"clientAddress":%q}`, pv, string(cl.Properties.Username), sei, cl.Net.Remote),
	}
	if err := h.store.Sessions.SetClient(ctx, info); err != nil {
		h.logger.Warn("session persist failed", "client", cl.ID, "err", err)
	}

	if !cl.Properties.Clean {
		persistedSubs, err := h.store.Subscriptions.GetSubscriptionsForClient(ctx, cl.ID)
		if err == nil && len(persistedSubs) > 0 {
			var toDelete []stores.MqttSubscription
			for _, sub := range persistedSubs {
				if !mqtt.IsValidFilter(sub.TopicFilter, false) || (h.server != nil && !h.server.Hooks().OnACLCheck(cl, sub.TopicFilter, false)) {
					toDelete = append(toDelete, sub)
					if h.subs != nil {
						h.subs.Unsubscribe(cl.ID, sub.TopicFilter)
					}
				}
			}
			if len(toDelete) > 0 {
				h.logger.Warn("pruned rejected or invalid persisted subscriptions", "client", cl.ID, "count", len(toDelete))
				if err := h.store.Subscriptions.DelSubscriptions(ctx, toDelete); err != nil {
					h.logger.Warn("failed to delete rejected subscriptions from store", "client", cl.ID, "err", err)
				}
			}
		}
	}
}

func (h *StorageHook) StoredClientByID(id string, username []byte) (string, []storage.Subscription, []storage.Message, error) {
	ctx := context.Background()
	sess, err := h.store.Sessions.GetSession(ctx, id)
	if err != nil || sess == nil {
		return "", nil, nil, err
	}
	if sess.CleanSession {
		return "", nil, nil, nil
	}

	// Check if session has expired
	if sess.SessionExpiryInterval > 0 {
		if sess.UpdateTime.Add(time.Duration(sess.SessionExpiryInterval) * time.Second).Before(time.Now()) {
			_ = h.store.Sessions.DelClient(ctx, id)
			if h.subs != nil {
				h.subs.DisconnectClient(id)
			}
			return "", nil, nil, nil
		}
	}

	persistedSubs, err := h.store.Subscriptions.GetSubscriptionsForClient(ctx, id)
	if err != nil {
		return "", nil, nil, err
	}

	res := make([]storage.Subscription, 0, len(persistedSubs))
	for _, sub := range persistedSubs {
		if !mqtt.IsValidFilter(sub.TopicFilter, false) {
			continue
		}
		if len(username) > 0 && h.server != nil {
			dummyCl := &mqtt.Client{
				ID: id,
				Properties: mqtt.ClientProperties{
					Username: username,
				},
			}
			if !h.server.Hooks().OnACLCheck(dummyCl, sub.TopicFilter, false) {
				continue
			}
		}

		res = append(res, storage.Subscription{
			Client:            sub.ClientID,
			Filter:            sub.TopicFilter,
			Qos:               sub.QoS,
			NoLocal:           sub.NoLocal,
			RetainAsPublished: sub.RetainAsPublished,
			RetainHandling:    sub.RetainHandling,
			Identifier:        sub.SubscriptionID,
		})
	}

	remote := sess.ClientAddress
	if remote == "" {
		remote = "persisted"
	}
	return remote, res, nil, nil
}

func (h *StorageHook) OnDisconnect(cl *mqtt.Client, _ error, expire bool) {
	if h.metrics != nil {
		h.metrics.ForgetClient(cl.ID)
	}
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

func (h *StorageHook) OnSubscribed(cl *mqtt.Client, pk packets.Packet, reasonCodes []byte) {
	if cl.Net.Inline {
		return // in-process subscriptions are recreated on every start
	}
	rows := make([]stores.MqttSubscription, 0, len(pk.Filters))
	for i, f := range pk.Filters {
		granted := false
		grantedQoS := f.Qos
		if len(reasonCodes) == 0 {
			granted = true
		} else if i < len(reasonCodes) && reasonCodes[i] <= packets.CodeGrantedQos2.Code {
			granted = true
			grantedQoS = reasonCodes[i]
		}
		if !granted {
			continue
		}

		rows = append(rows, stores.MqttSubscription{
			ClientID:          cl.ID,
			TopicFilter:       f.Filter,
			QoS:               grantedQoS,
			NoLocal:           f.NoLocal,
			RetainAsPublished: f.RetainAsPublished,
			RetainHandling:    f.RetainHandling,
			SubscriptionID:    f.Identifier,
		})
		if h.subs != nil {
			h.subs.Subscribe(cl.ID, f.Filter, grantedQoS)
		}
	}
	if len(rows) > 0 {
		if err := h.store.Subscriptions.AddSubscriptions(context.Background(), rows); err != nil {
			h.logger.Warn("subscriptions persist failed", "client", cl.ID, "err", err)
		}
	}
}

func (h *StorageHook) OnUnsubscribed(cl *mqtt.Client, pk packets.Packet, reasonCodes []byte) {
	if cl.Net.Inline {
		return
	}
	rows := make([]stores.MqttSubscription, 0, len(pk.Filters))
	for i, f := range pk.Filters {
		if len(reasonCodes) > 0 && i < len(reasonCodes) && reasonCodes[i] >= packets.ErrUnspecifiedError.Code {
			continue
		}
		rows = append(rows, stores.MqttSubscription{ClientID: cl.ID, TopicFilter: f.Filter})
		if h.subs != nil {
			h.subs.Unsubscribe(cl.ID, f.Filter)
		}
	}
	if len(rows) > 0 {
		if err := h.store.Subscriptions.DelSubscriptions(context.Background(), rows); err != nil {
			h.logger.Warn("subscriptions delete failed", "client", cl.ID, "err", err)
		}
	}
}

func (h *StorageHook) OnPacketSent(cl *mqtt.Client, pk packets.Packet, _ []byte) {
	if h.metrics != nil && pk.FixedHeader.Type == packets.Publish {
		h.metrics.IncOut()
		if cl != nil {
			h.metrics.IncClientOut(cl.ID)
		}
	}
}

func (h *StorageHook) OnPublished(cl *mqtt.Client, pk packets.Packet) {
	if f := pk.Forward; f != nil {
		h.publishedReplica(&pk, f)
		return
	}
	if h.metrics != nil {
		h.metrics.IncIn()
		if !cl.Net.Inline {
			h.metrics.IncClientIn(cl.ID)
		}
	}
	if pk.Ignore && h.replicated != nil && h.replicated(pk.TopicName) {
		return
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
	h.dispatch(msg, pk.Properties.MessageExpiryInterval, hasBus, hasArchive)
}

// publishedReplica dispatches a PeerLink replica with the publisher, time and
// dup flag of the source. Wills and snapshot values stay off the bus and the
// archives, as local wills do.
func (h *StorageHook) publishedReplica(pk *packets.Packet, f *packets.Forward) {
	if h.metrics != nil {
		h.metrics.IncBusIn()
	}
	if f.Will || f.Snapshot {
		return
	}
	hasBus := h.peer.Bus && h.bus != nil && h.bus.HasSubscribers()
	hasArchive := h.peer.Archive && h.archives != nil && h.archives.HasGroups()
	if !hasBus && !hasArchive {
		return
	}
	t := time.Now().UTC()
	if f.TimeNs > 0 {
		t = time.Unix(0, f.TimeNs).UTC()
	}
	msg := stores.BrokerMessage{
		MessageUUID: replicaUUID(f),
		MessageID:   pk.PacketID,
		TopicName:   pk.TopicName,
		Payload:     append([]byte(nil), pk.Payload...),
		QoS:         pk.FixedHeader.Qos,
		IsRetain:    pk.FixedHeader.Retain,
		IsDup:       f.Dup,
		ClientID:    f.ClientID,
		Time:        t,
		OriginNode:  f.SourceNode,
	}
	h.dispatch(msg, pk.Properties.MessageExpiryInterval, hasBus, hasArchive)
}

func (h *StorageHook) dispatch(msg stores.BrokerMessage, expiry uint32, hasBus, hasArchive bool) {
	if expiry > 0 {
		msg.MessageExpiryInterval = &expiry
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
	if f := pk.Forward; f != nil {
		h.retainReplica(&pk, f, r)
		return
	}
	// A local write wins over a replica value of the topic still waiting
	// for its flush, and lands after a flush already writing it.
	h.replicas.supersede(pk.TopicName, true)
	if r == -1 || len(pk.Payload) == 0 {
		h.writeRetained(stores.BrokerMessage{TopicName: pk.TopicName})
		return
	}
	clientID, username := "", ""
	if cl != nil {
		clientID = cl.ID
		username = string(cl.Properties.Username)
	}
	msg := retainedMessage(&pk, clientID, username)
	msg.MessageUUID = uuid.NewString()
	h.writeRetained(msg)
}

// retainReplica stores a retained PeerLink replica. The row keeps the source
// publisher and the backdated receiver-frame time, from which the expiry of a
// later retained delivery is rebuilt. DB stores get the replicas of a source
// in one write per FlushReplicas.
func (h *StorageHook) retainReplica(pk *packets.Packet, f *packets.Forward, r int64) {
	msg := stores.BrokerMessage{TopicName: pk.TopicName, OriginNode: f.SourceNode}
	if r != -1 && len(pk.Payload) > 0 {
		msg = retainedMessage(pk, f.ClientID, f.Username)
		msg.MessageUUID = replicaUUID(f)
		msg.OriginNode = f.SourceNode
	}
	if h.retainedViaOA != nil && h.retainedViaOA(f.SourceNode) {
		if c, ok := h.store.Retained.(retainedCache); ok {
			h.replicas.supersede(pk.TopicName, false)
			c.ApplyCached([]stores.BrokerMessage{msg})
			return
		}
	}
	if h.retainedInMemory {
		h.writeRetained(msg)
		return
	}
	h.replicas.add(f.SourceNode, msg)
}

func retainedMessage(pk *packets.Packet, clientID, username string) stores.BrokerMessage {
	createdAt := time.Now().UTC()
	if pk.Created > 0 {
		createdAt = time.Unix(pk.Created, 0).UTC()
	}
	msg := stores.BrokerMessage{
		TopicName: pk.TopicName,
		Payload:   append([]byte(nil), pk.Payload...),
		QoS:       pk.FixedHeader.Qos,
		IsRetain:  true,
		ClientID:  clientID,
		Username:  username,
		Time:      createdAt,
	}
	if pk.Expiry > pk.Created && pk.Created > 0 {
		v := uint32(pk.Expiry - pk.Created)
		msg.MessageExpiryInterval = &v
	} else if pk.Properties.MessageExpiryInterval > 0 {
		v := pk.Properties.MessageExpiryInterval
		msg.MessageExpiryInterval = &v
	}
	return msg
}

// writeRetained sets a retained value, or deletes it when the payload is empty.
func (h *StorageHook) writeRetained(msg stores.BrokerMessage) {
	ctx := context.Background()
	if len(msg.Payload) == 0 {
		_ = h.store.Retained.DelAll(ctx, []string{msg.TopicName})
		return
	}
	if err := h.store.Retained.AddAll(ctx, []stores.BrokerMessage{msg}); err != nil {
		h.logger.Warn("retained persist failed", "topic", msg.TopicName, "err", err)
	}
}

// FlushReplicas writes the retained replicas of source collected since its
// last flush: the last value per topic, with one AddAll and one DelAll. The
// PeerLink injector calls it before every COMMIT. When a write fails, its
// values go back to the pending set, unless a newer local or replica value of
// the topic arrived meanwhile, and the next flush retries them; the error is
// returned for the caller to count and log.
func (h *StorageHook) FlushReplicas(source string) error {
	h.replicas.flushMu.Lock()
	defer h.replicas.flushMu.Unlock()
	pending := h.replicas.take(source)
	if len(pending) == 0 {
		return nil
	}
	var setsFailed, delsFailed bool
	defer func() {
		h.replicas.done(source, pending, func(msg stores.BrokerMessage) bool {
			if len(msg.Payload) == 0 {
				return delsFailed
			}
			return setsFailed
		})
	}()
	var sets []stores.BrokerMessage
	var dels []string
	for t, msg := range pending {
		if len(msg.Payload) == 0 {
			dels = append(dels, t)
		} else {
			sets = append(sets, msg)
		}
	}
	ctx := context.Background()
	var errs []error
	if len(sets) > 0 {
		if err := h.store.Retained.AddAll(ctx, sets); err != nil {
			setsFailed = true
			errs = append(errs, fmt.Errorf("%d retained sets: %w", len(sets), err))
		}
	}
	if len(dels) > 0 {
		if err := h.store.Retained.DelAll(ctx, dels); err != nil {
			delsFailed = true
			errs = append(errs, fmt.Errorf("%d retained deletes: %w", len(dels), err))
		}
	}
	return errors.Join(errs...)
}

// PendingReplicas reports how many retained replica topics of source wait
// for FlushReplicas.
func (h *StorageHook) PendingReplicas(source string) int {
	return h.replicas.pendingCount(source)
}

// replicaUUID derives the message UUID of a replica from its global record
// id (source, epoch, offset) instead of reading crypto/rand per message:
// fnv64a(source) xor epoch, then the offset, as a version 8 (custom) UUID.
// Snapshot values have no offset and get a random one.
func replicaUUID(f *packets.Forward) string {
	if f.Offset == 0 {
		return uuid.NewString()
	}
	h := uint64(14695981039346656037)
	for i := 0; i < len(f.SourceNode); i++ {
		h ^= uint64(f.SourceNode[i])
		h *= 1099511628211
	}
	var u uuid.UUID
	binary.BigEndian.PutUint64(u[:8], h^f.Epoch)
	binary.BigEndian.PutUint64(u[8:], f.Offset)
	u[6] = u[6]&0x0f | 0x80
	u[8] = u[8]&0x3f | 0x80
	return u.String()
}

// replicaRetained holds the retained replica writes of each source in DB
// modes until FlushReplicas. The newest arrival of a topic wins: a replica
// drops older pending values of other sources, a local write drops all, and
// flushes run one at a time, so a running flush cannot overwrite a value that
// arrived after it started. Local retained writes check active first, so they
// cost one atomic load while nothing is pending.
type replicaRetained struct {
	flushMu  sync.Mutex // serializes flushes
	mu       sync.Mutex
	flushed  *sync.Cond                                 // a flush finished
	pending  map[string]map[string]stores.BrokerMessage // source -> topic -> last value
	flushing map[string]int                             // topic -> flushes writing it
	newer    map[string]struct{}                        // flushing topics a local write superseded
	active   atomic.Int64                               // pending entries + flushing entries
}

func newReplicaRetained() *replicaRetained {
	r := &replicaRetained{
		pending:  map[string]map[string]stores.BrokerMessage{},
		flushing: map[string]int{},
		newer:    map[string]struct{}{},
	}
	r.flushed = sync.NewCond(&r.mu)
	return r
}

func (r *replicaRetained) add(source string, msg stores.BrokerMessage) {
	r.mu.Lock()
	for src, m := range r.pending {
		if _, ok := m[msg.TopicName]; ok && src != source {
			delete(m, msg.TopicName)
			r.active.Add(-1)
		}
	}
	m := r.pending[source]
	if m == nil {
		m = map[string]stores.BrokerMessage{}
		r.pending[source] = m
	}
	if _, ok := m[msg.TopicName]; !ok {
		r.active.Add(1)
	}
	m[msg.TopicName] = msg
	r.mu.Unlock()
}

// supersede drops the pending replica values of topic and, with wait, waits
// for flushes that are writing it, so the write that follows lands last.
func (r *replicaRetained) supersede(topic string, wait bool) {
	if r.active.Load() == 0 {
		return
	}
	r.mu.Lock()
	for _, m := range r.pending {
		if _, ok := m[topic]; ok {
			delete(m, topic)
			r.active.Add(-1)
		}
	}
	if r.flushing[topic] > 0 {
		// A failed flush must not put its older value back after this write.
		r.newer[topic] = struct{}{}
	}
	for wait && r.flushing[topic] > 0 {
		r.flushed.Wait()
	}
	r.mu.Unlock()
}

func (r *replicaRetained) take(source string) map[string]stores.BrokerMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.pending[source]
	if len(m) == 0 {
		return nil
	}
	delete(r.pending, source)
	for t := range m {
		r.flushing[t]++
	}
	return m
}

// done ends a flush of source. Values whose write failed go back to the
// pending set of source unless a newer value of the topic is pending or a
// local write superseded it during the flush.
func (r *replicaRetained) done(source string, m map[string]stores.BrokerMessage, failed func(stores.BrokerMessage) bool) {
	r.mu.Lock()
	requeued := 0
	for t, msg := range m {
		if r.flushing[t]--; r.flushing[t] <= 0 {
			delete(r.flushing, t)
		}
		_, superseded := r.newer[t]
		if r.flushing[t] == 0 {
			delete(r.newer, t)
		}
		if !failed(msg) || superseded || r.pendingTopic(t) {
			continue
		}
		p := r.pending[source]
		if p == nil {
			p = map[string]stores.BrokerMessage{}
			r.pending[source] = p
		}
		p[t] = msg
		requeued++
	}
	r.active.Add(-int64(len(m) - requeued))
	r.flushed.Broadcast()
	r.mu.Unlock()
}

func (r *replicaRetained) pendingTopic(t string) bool {
	for _, m := range r.pending {
		if _, ok := m[t]; ok {
			return true
		}
	}
	return false
}

func (r *replicaRetained) pendingCount(source string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending[source])
}

// OnSelectRetainedMessages returns matching retained messages from the store.
func (h *StorageHook) OnSelectRetainedMessages(filter string) ([]packets.Packet, error) {
	if h.retainedInMemory {
		return nil, nil
	}
	ctx := context.Background()
	now := time.Now().Unix()
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
		pk.Created = msg.Time.Unix()
		if msg.MessageExpiryInterval != nil && *msg.MessageExpiryInterval > 0 {
			pk.Expiry = pk.Created + int64(*msg.MessageExpiryInterval)
			if pk.Expiry <= now {
				return true // expired but not yet purged by StartRetention
			}
			pk.Properties.MessageExpiryInterval = uint32(pk.Expiry - now) // remaining lifetime [MQTT-3.3.2-6]
		}
		pks = append(pks, pk)
		return true
	})
	if err != nil {
		return nil, err
	}
	return pks, nil
}

// StartRetention starts a periodic background goroutine to purge expired retained messages
// from the persistent store. Returns a cancel function.
func (h *StorageHook) StartRetention(ctx context.Context, interval time.Duration) context.CancelFunc {
	ctx, cancel := context.WithCancel(ctx)
	if h.retainedInMemory {
		return cancel
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := h.store.Retained.PurgeExpired(ctx); err != nil {
					h.logger.Warn("retained messages purge failed", "err", err)
				}
			}
		}
	}()
	// Run once immediately on startup
	go func() {
		if _, err := h.store.Retained.PurgeExpired(ctx); err != nil {
			h.logger.Warn("retained messages initial purge failed", "err", err)
		}
	}()
	return cancel
}
