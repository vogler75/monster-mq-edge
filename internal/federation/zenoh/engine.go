//go:build cgo

package zenoh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"unicode/utf8"

	"github.com/BooleanCat/option"
	"github.com/eclipse-zenoh/zenoh-go/zenoh"
	"monstermq.io/edge/internal/archive"
	"monstermq.io/edge/internal/config"
	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/stores"
	"monstermq.io/edge/internal/topic"
)

type ArchiveFinder interface {
	Snapshot() []*archive.Group
}

type Engine struct {
	cfg         *config.Config
	logger      *slog.Logger
	nodeID      string
	mochiServer *mqtt.Server
	storage     *stores.Storage
	archives    ArchiveFinder

	mu           sync.Mutex
	session      *zenoh.Session
	subscribers  []zenoh.Subscriber
	queryables   []zenoh.Queryable
	seenMessages *SeenCache
	zenohClient  *mqtt.Client
}

func NewEngine(cfg *config.Config, nodeID string, mochiServer *mqtt.Server, storage *stores.Storage, archives ArchiveFinder, logger *slog.Logger) *Engine {
	return &Engine{
		cfg:          cfg,
		logger:       logger.With("system", "zenoh-federation"),
		nodeID:       nodeID,
		mochiServer:  mochiServer,
		storage:      storage,
		archives:     archives,
		seenMessages: NewSeenCache(cfg.Zenoh.Deduplication.CacheSize, cfg.Zenoh.Deduplication.TtlSeconds),
	}
}

func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.logger.Info("starting Zenoh federation", "node", e.nodeID, "mode", e.cfg.Zenoh.Mode)

	// Create dedicated inline client for incoming Zenoh publications
	// LocalListener constant is not exported directly, but we can pass "zenoh-listener" or ""
	e.zenohClient = e.mochiServer.NewClient(nil, "zenoh-listener", "zenoh-client", true)

	// Build JSON config for Zenoh session
	configMap := map[string]any{
		"mode": e.cfg.Zenoh.Mode,
	}
	if len(e.cfg.Zenoh.Connect) > 0 {
		configMap["connect"] = map[string]any{
			"endpoints": e.cfg.Zenoh.Connect,
		}
	}

	jsonBytes, err := json.Marshal(configMap)
	if err != nil {
		return fmt.Errorf("serialize Zenoh config: %w", err)
	}

	zenohCfg, err := zenoh.NewConfigFromStr(string(jsonBytes))
	if err != nil {
		return fmt.Errorf("parse Zenoh config: %w", err)
	}

	session, err := zenoh.Open(zenohCfg, nil)
	if err != nil {
		return fmt.Errorf("open Zenoh session: %w", err)
	}
	e.session = &session

	// Setup Subscribers and Queryables based on minimal Allow filters
	subKeys := MinimalFilters(e.cfg.Zenoh.Allow)
	for _, subKey := range subKeys {
		zenohKey := SubscriptionKey(subKey, e.cfg.Zenoh.LocalPrefix, e.cfg.Zenoh.RemotePrefix)
		if zenohKey == "" {
			continue
		}

		keyexpr, err := zenoh.NewKeyExpr(zenohKey)
		if err != nil {
			e.logger.Warn("invalid key expression in subscriber", "key", zenohKey, "err", err)
			continue
		}

		// Subscribe
		sub, err := session.DeclareSubscriber(keyexpr, zenoh.Closure[zenoh.Sample]{Call: e.handleSample}, nil)
		if err != nil {
			e.logger.Warn("failed to declare subscriber", "key", zenohKey, "err", err)
			continue
		}
		e.subscribers = append(e.subscribers, sub)

		// Queryable
		qOpts := zenoh.QueryableOptions{}
		qOpts.Complete = false
		queryable, err := session.DeclareQueryable(keyexpr, zenoh.Closure[zenoh.Query]{Call: e.handleQuery}, &qOpts)
		if err != nil {
			e.logger.Warn("failed to declare queryable", "key", zenohKey, "err", err)
			continue
		}
		e.queryables = append(e.queryables, queryable)
	}

	e.logger.Info("Zenoh federation active", "subscribers", len(e.subscribers), "queryables", len(e.queryables))
	return nil
}

func (e *Engine) Stop() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.logger.Info("stopping Zenoh federation")
	for _, sub := range e.subscribers {
		sub.Drop()
	}
	e.subscribers = nil

	for _, q := range e.queryables {
		q.Drop()
	}
	e.queryables = nil

	if e.session != nil {
		e.session.Drop()
		e.session = nil
	}
	return nil
}

func (e *Engine) PublishMessageToBus(msg stores.BrokerMessage) {
	e.mu.Lock()
	session := e.session
	e.mu.Unlock()

	if session == nil {
		return
	}

	if !e.isAllowed(msg.TopicName) {
		return
	}

	// Loop prevention: check if this message originated externally.
	if msg.OriginNodeID != "" && msg.OriginNodeID != e.nodeID {
		return
	}

	zenohKeyStr := MapToZenohKey(msg.TopicName, e.cfg.Zenoh.LocalPrefix, e.cfg.Zenoh.RemotePrefix)
	if zenohKeyStr == "" {
		return
	}

	keyexpr, err := zenoh.NewKeyExpr(zenohKeyStr)
	if err != nil {
		e.logger.Error("invalid keyexpr for publish", "topic", msg.TopicName, "key", zenohKeyStr, "err", err)
		return
	}

	e.seenMessages.Remember(msg.MessageUUID)

	attachmentBytes, err := EncodeEnvelope(e.nodeID, msg)
	if err != nil {
		e.logger.Error("failed to encode metadata envelope", "err", err)
		return
	}

	putOpts := zenoh.PutOptions{}
	putOpts.Attachement = option.Some(zenoh.NewZBytes(attachmentBytes))
	putOpts.Reliability = option.Some(zenoh.ReliabilityReliable)

	if err := session.Put(keyexpr, zenoh.NewZBytes(msg.Payload), &putOpts); err != nil {
		e.logger.Warn("failed to put data to Zenoh", "topic", msg.TopicName, "err", err)
	}
}

func (e *Engine) handleSample(sample zenoh.Sample) {
	keyExprStr := sample.KeyExpr().String()
	mqttTopic := MapToMqttTopic(keyExprStr, e.cfg.Zenoh.LocalPrefix, e.cfg.Zenoh.RemotePrefix)
	if mqttTopic == "" {
		return
	}

	if !e.isAllowed(mqttTopic) {
		return
	}

	payloadBytes := sample.Payload().Bytes()
	var attachmentBytes []byte
	if sample.Attachement().IsSome() {
		attachmentBytes = sample.Attachement().Unwrap().Bytes()
	}

	origin, msg := DecodeEnvelope(mqttTopic, payloadBytes, attachmentBytes)

	// Check if this message was generated by us or has been seen before
	if origin == e.nodeID || !e.seenMessages.Remember(msg.MessageUUID) {
		return
	}

	// Inject locally using zenohClient.
	// Store message UUID in ReasonString and set origin in User Properties
	pk := packets.Packet{
		FixedHeader: packets.FixedHeader{
			Type:   packets.Publish,
			Qos:    msg.QoS,
			Retain: msg.IsRetain,
		},
		TopicName: msg.TopicName,
		Payload:   msg.Payload,
		PacketID:  uint16(msg.QoS),
		Properties: packets.Properties{
			ReasonString: msg.MessageUUID,
			User: []packets.UserProperty{
				{Key: "monstermq-origin-node", Val: origin},
			},
		},
	}
	if msg.MessageExpiryInterval != nil {
		pk.Properties.MessageExpiryInterval = *msg.MessageExpiryInterval
	}

	e.mu.Lock()
	zenohClient := e.zenohClient
	e.mu.Unlock()

	if zenohClient != nil {
		if err := e.mochiServer.InjectPacket(zenohClient, pk); err != nil {
			e.logger.Warn("failed to inject federated message locally", "topic", msg.TopicName, "err", err)
		}
	}
}

func (e *Engine) handleQuery(query zenoh.Query) {
	defer query.Drop()

	keyexpr := query.KeyExpr().String()
	topicPattern := MapToMqttTopic(keyexpr, e.cfg.Zenoh.LocalPrefix, e.cfg.Zenoh.RemotePrefix)
	if topicPattern == "" {
		return
	}

	var matchedMessages []stores.BrokerMessage
	repliedTopics := make(map[string]bool)

	// 1. Retrieve from Retained Store
	if e.storage.Retained != nil {
		_ = e.storage.Retained.FindMatchingMessages(context.Background(), topicPattern, func(msg stores.BrokerMessage) bool {
			if !repliedTopics[msg.TopicName] {
				repliedTopics[msg.TopicName] = true
				matchedMessages = append(matchedMessages, msg)
			}
			return true
		})
	}

	// 2. Retrieve from Deployed Last Value Stores (LVS)
	if e.archives != nil {
		for _, g := range e.archives.Snapshot() {
			store := g.LastValue()
			if store == nil {
				continue
			}
			_ = store.FindMatchingMessages(context.Background(), topicPattern, func(msg stores.BrokerMessage) bool {
				if !repliedTopics[msg.TopicName] {
					repliedTopics[msg.TopicName] = true
					matchedMessages = append(matchedMessages, msg)
				}
				return true
			})
		}
	}

	if len(matchedMessages) == 0 {
		return
	}

	if len(matchedMessages) == 1 {
		e.replyToQuery(query, matchedMessages[0])
	} else {
		e.replyMultipleToQuery(query, keyexpr, matchedMessages)
	}
}

func (e *Engine) replyToQuery(query zenoh.Query, msg stores.BrokerMessage) {
	mappedKeyStr := MapToZenohKey(msg.TopicName, e.cfg.Zenoh.LocalPrefix, e.cfg.Zenoh.RemotePrefix)
	if mappedKeyStr == "" {
		return
	}

	keyexpr, err := zenoh.NewKeyExpr(mappedKeyStr)
	if err != nil {
		e.logger.Error("invalid keyexpr for query reply", "topic", msg.TopicName, "key", mappedKeyStr, "err", err)
		return
	}

	attachmentBytes, err := EncodeEnvelope(e.nodeID, msg)
	if err != nil {
		e.logger.Error("failed to encode metadata envelope for query reply", "err", err)
		return
	}

	replyOpts := zenoh.QueryReplyOptions{}
	replyOpts.Attachement = option.Some(zenoh.NewZBytes(attachmentBytes))

	if err := query.Reply(keyexpr, zenoh.NewZBytes(msg.Payload), &replyOpts); err != nil {
		e.logger.Warn("failed to send Zenoh query reply", "topic", msg.TopicName, "err", err)
	}
}

func (e *Engine) replyMultipleToQuery(query zenoh.Query, queryKey string, messages []stores.BrokerMessage) {
	type consolidatedMsg struct {
		Topic       string `json:"topic"`
		Payload     string `json:"payload"`
		QoS         byte   `json:"qos"`
		ClientID    string `json:"clientId"`
		MessageUUID string `json:"messageUuid"`
		Timestamp   int64  `json:"timestamp"`
	}

	var jsonList []consolidatedMsg
	for _, msg := range messages {
		payloadStr := string(msg.Payload)
		if !utf8.Valid(msg.Payload) {
			payloadStr = base64.StdEncoding.EncodeToString(msg.Payload)
		}

		jsonList = append(jsonList, consolidatedMsg{
			Topic:       msg.TopicName,
			Payload:     payloadStr,
			QoS:         msg.QoS,
			ClientID:    msg.ClientID,
			MessageUUID: msg.MessageUUID,
			Timestamp:   msg.Time.UnixMilli(),
		})
	}

	jsonBytes, err := json.Marshal(jsonList)
	if err != nil {
		e.logger.Error("failed to marshal consolidated query reply", "err", err)
		return
	}

	keyexpr, err := zenoh.NewKeyExpr(queryKey)
	if err != nil {
		e.logger.Error("invalid query key for query reply", "key", queryKey, "err", err)
		return
	}

	replyOpts := zenoh.QueryReplyOptions{}
	// Note the typo in zenoh-go bindings API: NewEncodinFromString
	replyOpts.Encoding = option.Some(zenoh.NewEncodinFromString("application/json"))

	if err := query.Reply(keyexpr, zenoh.NewZBytes(jsonBytes), &replyOpts); err != nil {
		e.logger.Warn("failed to send consolidated Zenoh query reply", "err", err)
	}
}

func (e *Engine) isAllowed(t string) bool {
	allowed := false
	for _, f := range e.cfg.Zenoh.Allow {
		if topic.MatchFilter(f, t) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	for _, f := range e.cfg.Zenoh.Deny {
		if topic.MatchFilter(f, t) {
			return false
		}
	}
	return true
}
