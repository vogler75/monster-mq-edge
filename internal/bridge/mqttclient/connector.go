package mqttclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"monstermq.io/edge/internal/queue"
	"monstermq.io/edge/internal/stores"
)

// Address is one inbound or outbound mapping rule. Field shapes mirror the
// dashboard's MqttClientAddressInput.
type Address struct {
	Mode        string `json:"mode"`        // SUBSCRIBE | PUBLISH
	RemoteTopic string `json:"remoteTopic"` // topic on the remote broker
	LocalTopic  string `json:"localTopic"`  // topic on the local broker
	QoS         int    `json:"qos"`
	Retain      bool   `json:"retain"`
	// RemovePath: when true, strip the literal prefix of the source-side topic
	// pattern (everything before the first + or #) from the matched topic
	// before mapping to the destination side.
	RemovePath bool `json:"removePath"`
}

// Config is the persisted JSON config (DeviceConfig.Config) for one bridge.
type Config struct {
	BrokerURL            string    `json:"brokerUrl"`
	ClientID             string    `json:"clientId"`
	Username             string    `json:"username,omitempty"`
	Password             string    `json:"password,omitempty"`
	CleanSession         bool      `json:"cleanSession"`
	KeepAlive            int       `json:"keepAlive,omitempty"`
	ConnectionTimeout    int       `json:"connectionTimeout,omitempty"`
	ReconnectDelay       int       `json:"reconnectDelay,omitempty"`
	Addresses            []Address `json:"addresses"`
	BufferEnabled        bool      `json:"bufferEnabled"`
	BufferImplementation string    `json:"bufferImplementation,omitempty"`
	BufferSize           int       `json:"bufferSize,omitempty"`
	PersistBuffer        bool      `json:"persistBuffer,omitempty"`
	DeleteOldestMessages bool      `json:"deleteOldestMessages,omitempty"`
	// ProtocolVersion: 3 or 4 MQTT 3.1.1, 5 or unset MQTT 5 (falls back to
	// 3.1.1 when the remote does not support MQTT 5).
	ProtocolVersion int `json:"protocolVersion,omitempty"`
	// Redundancy is the component mode of contract C6: ALWAYS (default), HOT_STANDBY or COLD_STANDBY.
	Redundancy string `json:"redundancy,omitempty"`

	// TLS, as MonsterMQ Core's MqttClientConnectionConfig (issue #20).
	SslVerifyCertificate *bool    `json:"sslVerifyCertificate,omitempty"`
	TlsCaCertPath        string   `json:"tlsCaCertPath,omitempty"`
	TlsClientCertPath    string   `json:"tlsClientCertPath,omitempty"`
	TlsClientKeyPath     string   `json:"tlsClientKeyPath,omitempty"`
	TlsClientKeyPassword string   `json:"tlsClientKeyPassword,omitempty"`
	TlsClientKeyFormat   string   `json:"tlsClientKeyFormat,omitempty"`
	TlsAlpnProtocols     []string `json:"tlsAlpnProtocols,omitempty"`
	TlsServerName        string   `json:"tlsServerName,omitempty"`
}

// Standby reports whether the bridge has a HOT_STANDBY or COLD_STANDBY mode.
func (c Config) Standby() bool {
	switch strings.ToUpper(c.Redundancy) {
	case "HOT_STANDBY", "COLD_STANDBY":
		return true
	}
	return false
}

// OutboundFilters returns the local filters of the PUBLISH addresses.
func (c Config) OutboundFilters() []string {
	var out []string
	for _, a := range c.Addresses {
		if strings.EqualFold(a.Mode, "PUBLISH") {
			out = append(out, outboundFilter(a.LocalTopic))
		}
	}
	return out
}

// LocalPublisher publishes a message into the local broker as clientID.
type LocalPublisher func(clientID string, msg stores.BrokerMessage) error

// LocalSubscriber lets the bridge listen to the local broker's publishes.
// BusAdapter implements it.
type LocalSubscriber interface {
	Subscribe(filters []string, buffer int) (id int, ch <-chan stores.BrokerMessage)
	Unsubscribe(id int)
}

// Connector is one bridge to one remote broker.
type Connector struct {
	name      string
	cfg       Config
	publisher LocalPublisher
	subBus    LocalSubscriber
	logger    *slog.Logger
	// newTransport is replaced in tests.
	newTransport func(context.Context, transportOptions) (transport, error)

	mu       sync.Mutex
	tr       transport
	opts     transportOptions // what Start connected with, for the fallback
	startCtx context.Context
	fallback sync.Once
	subID    int
	stopCh   chan struct{}
	queue    queue.MessageQueue
	wg       sync.WaitGroup

	IncIn  func()
	IncOut func()

	inCount   atomic.Int64
	outCount  atomic.Int64
	metricsMu sync.RWMutex
	latest    MetricsSnapshot
}

type MetricsSnapshot struct {
	MessagesIn  float64   `json:"messagesIn"`
	MessagesOut float64   `json:"messagesOut"`
	Timestamp   time.Time `json:"timestamp"`
}

func NewConnector(name string, cfg Config, publisher LocalPublisher, subBus LocalSubscriber, logger *slog.Logger) *Connector {
	return &Connector{
		name: name, cfg: cfg, publisher: publisher, subBus: subBus, logger: logger,
		stopCh:       make(chan struct{}),
		newTransport: newTransportFor,
	}
}

// localClientID is the client ID this bridge publishes under locally, so it
// can recognise — and not forward back — what it brought in itself.
// ClientIDPrefix starts the client ID a bridge publishes locally with.
const ClientIDPrefix = "bridge:"

func (c *Connector) localClientID() string { return ClientIDPrefix + c.name }

// protocolVersion is the MQTT version to connect with: 3 and 4 mean 3.1.1,
// 5 or unset mean MQTT 5.
func (c *Connector) protocolVersion() (int, error) {
	switch v := c.cfg.ProtocolVersion; v {
	case 0, 5:
		return 5, nil
	case 3, 4:
		return v, nil
	default:
		return 0, fmt.Errorf("unsupported protocolVersion %d", v)
	}
}

func (c *Connector) Name() string { return c.name }

func (c *Connector) recordIn() {
	c.inCount.Add(1)
	if c.IncIn != nil {
		c.IncIn()
	}
}

func (c *Connector) recordOut() {
	c.outCount.Add(1)
	if c.IncOut != nil {
		c.IncOut()
	}
}

func (c *Connector) SampleMetrics(now time.Time, interval time.Duration) MetricsSnapshot {
	seconds := interval.Seconds()
	if seconds <= 0 {
		seconds = 1
	}
	snap := MetricsSnapshot{
		MessagesIn:  float64(c.inCount.Swap(0)) / seconds,
		MessagesOut: float64(c.outCount.Swap(0)) / seconds,
		Timestamp:   now.UTC(),
	}
	c.metricsMu.Lock()
	c.latest = snap
	c.metricsMu.Unlock()
	return snap
}

func (c *Connector) LatestMetrics() MetricsSnapshot {
	c.metricsMu.RLock()
	snap := c.latest
	c.metricsMu.RUnlock()
	if snap.Timestamp.IsZero() {
		snap.Timestamp = time.Now().UTC()
	}
	return snap
}

// Start dials the remote broker and registers inbound/outbound forwarders.
func (c *Connector) Start(ctx context.Context) error {
	if errs := c.cfg.tlsErrors(); len(errs) > 0 {
		// A config error is not retried, as in MonsterMQ Core.
		return fmt.Errorf("Invalid MQTT client configuration: %s", strings.Join(errs, "; "))
	}
	u, err := parseBrokerURL(c.cfg.BrokerURL)
	if err != nil {
		return fmt.Errorf("bridge %s: %w", c.name, err)
	}
	pv, err := c.protocolVersion()
	if err != nil {
		return fmt.Errorf("bridge %s: %w", c.name, err)
	}
	var tlsCfg *tls.Config
	if isTLSScheme(u) {
		if tlsCfg, err = buildTLSConfig(c.cfg); err != nil {
			// Files are read again on every connection attempt: one that is
			// missing now (not yet provisioned, being renewed) is retried.
			c.logger.Warn("Invalid MQTT client TLS configuration", "name", c.name, "err", err)
		}
	}
	if err := c.initQueue(); err != nil {
		return err
	}
	o := c.remoteOptions(tlsCfg)
	o.ProtocolVersion = pv
	// Held while the transport is created: it may report a 3.1.1 CONNACK
	// before it is stored, and the fallback must wait for it.
	c.mu.Lock()
	c.opts, c.startCtx = o, ctx
	tr, err := c.newTransport(ctx, o)
	if err == nil {
		c.tr = tr
	}
	c.mu.Unlock()
	if err != nil {
		c.closeQueue()
		return fmt.Errorf("bridge %s: %w", c.name, err)
	}
	if c.usesPahoBuffer() {
		c.logger.Info("bridge message buffering enabled", "name", c.name, "type", "PAHO", "persisted", c.cfg.PersistBuffer)
	}
	c.startOutbound(ctx)
	c.startQueueWriter(ctx)
	return nil
}

func (c *Connector) remoteOptions(tlsCfg *tls.Config) transportOptions {
	o := transportOptions{
		URL:            c.cfg.BrokerURL,
		ClientID:       c.cfg.ClientID,
		Username:       c.cfg.Username,
		Password:       c.cfg.Password,
		KeepAlive:      30 * time.Second,
		ConnectTimeout: 30 * time.Second,
		ReconnectDelay: 30 * time.Second,
		CleanSession:   c.cfg.CleanSession,
		TLS:            tlsCfg,
		Logger:         c.logger,
		OnMessage:      c.handleInbound,
		OnConnected: func() {
			c.logger.Info("bridge connected", "name", c.name, "url", c.cfg.BrokerURL)
		},
		OnConnectError: func(err error) {
			if errors.Is(err, errRemoteNotMQTT5) {
				c.fallback.Do(func() {
					c.wg.Add(1)
					go c.fallBackToMQTT311()
				})
				return
			}
			c.logger.Warn("bridge connect failed; retrying", "name", c.name, "url", c.cfg.BrokerURL, "err", err)
		},
		OnConnectionDown: func(err error) {
			c.logger.Warn("bridge connection lost", "name", c.name, "err", err)
		},
	}
	if c.cfg.KeepAlive > 0 {
		o.KeepAlive = time.Duration(c.cfg.KeepAlive) * time.Second
	}
	if c.cfg.ConnectionTimeout > 0 {
		o.ConnectTimeout = configMillisOrSeconds(c.cfg.ConnectionTimeout)
	}
	if c.cfg.ReconnectDelay > 0 {
		o.ReconnectDelay = configMillisOrSeconds(c.cfg.ReconnectDelay)
	}
	for _, a := range c.cfg.Addresses {
		if strings.EqualFold(a.Mode, "SUBSCRIBE") {
			o.Subscriptions = append(o.Subscriptions, transportSubscription{Topic: a.RemoteTopic, QoS: subscribeQoS(a)})
		}
	}
	if c.usesPahoBuffer() {
		o.LibraryQueue = memoryQueue
		if c.cfg.PersistBuffer {
			o.LibraryQueue = pahoBufferDir(c.name)
		}
	}
	return o
}

const fallbackWarning = "bridge remote does not support MQTT 5; using MQTT 3.1.1 (MQTT 5 properties, inbound retain flag and No Local are not available)"

// fallBackToMQTT311 replaces the MQTT 5 transport with an MQTT 3.1.1 one after
// the remote answered with a 3.1.1 CONNACK. It runs on its own goroutine:
// closing the MQTT 5 transport from inside its callback would wait on itself.
func (c *Connector) fallBackToMQTT311() {
	defer c.wg.Done()
	c.mu.Lock()
	select {
	case <-c.stopCh:
		c.mu.Unlock()
		return
	default:
	}
	o := c.opts
	o.ProtocolVersion = 4
	tr, err := c.newTransport(c.startCtx, o)
	if err != nil {
		c.mu.Unlock()
		c.logger.Warn("bridge MQTT 3.1.1 fallback failed", "name", c.name, "err", err)
		return
	}
	old := c.tr
	c.tr = tr
	c.mu.Unlock()

	if c.usesPahoBuffer() {
		c.logger.Warn(fallbackWarning, "name", c.name, "url", c.cfg.BrokerURL,
			"note", "messages queued in the MQTT 5 PAHO buffer before the first connect are dropped")
	} else {
		c.logger.Warn(fallbackWarning, "name", c.name, "url", c.cfg.BrokerURL)
	}
	if old != nil {
		old.Close()
	}
}

// subscribeQoS is the QoS to subscribe with; -1 ("keep the publisher's")
// subscribes at 2 so no message is downgraded on the way in.
func subscribeQoS(a Address) byte {
	if a.QoS == -1 {
		return 2
	}
	return outboundQoS(a, stores.BrokerMessage{})
}

// handleInbound publishes a message from the remote broker locally, once per
// SUBSCRIBE mapping it matches, under this bridge's own client ID.
func (c *Connector) handleInbound(remoteTopic string, msg stores.BrokerMessage) {
	for _, a := range c.cfg.Addresses {
		a.RemoteTopic = plainFilter(a.RemoteTopic)
		if !strings.EqualFold(a.Mode, "SUBSCRIBE") || !matchTopic(a.RemoteTopic, remoteTopic) {
			continue
		}
		local := msg
		local.TopicName = mapInboundTopic(a, remoteTopic)
		local.IsRetain = a.Retain || msg.IsRetain
		local.QoS = outboundQoS(a, msg)
		c.recordIn()
		if err := c.publisher(c.localClientID(), local); err != nil {
			c.logger.Warn("bridge inbound publish failed", "name", c.name, "topic", local.TopicName, "err", err)
		}
	}
}

func (c *Connector) initQueue() error {
	if !c.usesMonsterBuffer() {
		return nil
	}
	size := c.bufferSize()
	blockSize := min(100, max(1, size))
	if c.cfg.PersistBuffer {
		q, err := queue.NewDiskQueue("mqtt-bridge", c.name, c.logger, size, blockSize, 100*time.Millisecond, "./buffer/mqttbridge")
		if err != nil {
			return fmt.Errorf("bridge %s: disk queue: %w", c.name, err)
		}
		c.queue = q
	} else {
		c.queue = queue.NewMemoryQueue(c.logger, size, blockSize, 100*time.Millisecond)
	}
	c.logger.Info("bridge message buffering enabled", "name", c.name, "type", map[bool]string{true: "DISK", false: "MEMORY"}[c.cfg.PersistBuffer], "size", size)
	return nil
}

func (c *Connector) usesMonsterBuffer() bool {
	return c.cfg.BufferEnabled && c.bufferImplementation() == "MONSTER"
}

func (c *Connector) usesPahoBuffer() bool {
	return c.cfg.BufferEnabled && c.bufferImplementation() == "PAHO"
}

func (c *Connector) bufferImplementation() string {
	impl := strings.ToUpper(strings.TrimSpace(c.cfg.BufferImplementation))
	if impl == "PAHO" {
		return "PAHO"
	}
	return "MONSTER"
}

func (c *Connector) bufferSize() int {
	if c.cfg.BufferSize <= 0 {
		return 5000
	}
	if c.cfg.BufferSize > 100000 {
		return 100000
	}
	return c.cfg.BufferSize
}

func pahoBufferDir(name string) string {
	safe := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(name)
	return filepath.Join(".", "buffer", "mqttbridge", "paho-"+safe)
}

func configMillisOrSeconds(value int) time.Duration {
	if value <= 0 {
		return 0
	}
	if value < 1000 {
		return time.Duration(value) * time.Second
	}
	return time.Duration(value) * time.Millisecond
}

// mapInboundTopic maps an incoming topic from the remote broker to the local
// topic to publish under, respecting the address's removePath flag and the
// LocalTopic prefix (if it has no wildcards).
func mapInboundTopic(a Address, remoteTopic string) string {
	if hasWildcard(a.RemoteTopic) {
		if a.RemovePath {
			base := literalPrefix(a.RemoteTopic)
			suffix := remoteTopic
			if base != "" && (remoteTopic == base || strings.HasPrefix(remoteTopic, base+"/")) {
				suffix = strings.TrimPrefix(strings.TrimPrefix(remoteTopic, base), "/")
			}
			return joinTopic(destinationPrefix(a.LocalTopic), suffix)
		}
		return joinTopic(destinationPrefix(a.LocalTopic), remoteTopic)
	}
	if a.LocalTopic != "" {
		return a.LocalTopic
	}
	return remoteTopic
}

func (c *Connector) startOutbound(ctx context.Context) {
	filters := c.cfg.OutboundFilters()
	if len(filters) == 0 {
		return
	}
	id, ch := c.subBus.Subscribe(filters, 1024) // the bus drops when this is full
	c.mu.Lock()
	c.subID = id
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.stopCh:
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				// What this bridge brought in itself must not go back out.
				if msg.Inline && msg.ClientID == c.localClientID() {
					continue
				}
				matches := matchingAddresses(c.cfg.Addresses, msg.TopicName)
				if len(matches) == 0 {
					c.logger.Warn("bridge outbound topic mapping failed", "name", c.name, "localTopic", msg.TopicName)
					continue
				}
				if c.usesMonsterBuffer() {
					c.enqueueLocalMessage(msg)
					continue
				}
				for _, addr := range matches {
					if !c.publishLocalMessage(addr, msg, false) {
						break
					}
				}
			}
		}
	}()
}

func matchingAddresses(addrs []Address, topic string) []Address {
	var out []Address
	for _, a := range addrs {
		if strings.EqualFold(a.Mode, "PUBLISH") && matchesLocalPattern(topic, a.LocalTopic) {
			out = append(out, a)
		}
	}
	return out
}

func (c *Connector) enqueueLocalMessage(msg stores.BrokerMessage) {
	if c.queue == nil {
		c.logger.Warn("bridge buffering enabled but queue is not initialized; dropping message", "name", c.name, "topic", msg.TopicName)
		return
	}
	c.queue.Add(msg)
	if c.logger != nil {
		c.logger.Debug("bridge outbound queued",
			"name", c.name,
			"localTopic", msg.TopicName,
			"queued", c.queue.Size(),
			"capacity", c.queue.Capacity(),
			"full", c.queue.IsQueueFull(),
		)
	}
}

func (c *Connector) startQueueWriter(ctx context.Context) {
	q := c.queue
	if q == nil {
		return
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		progress := []int(nil)
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.stopCh:
				return
			default:
			}

			var messages []stores.BrokerMessage
			count := q.PollBlock(func(message stores.BrokerMessage) {
				messages = append(messages, message)
			})
			if count == 0 {
				time.Sleep(10 * time.Millisecond)
				continue
			}

			if c.logger != nil {
				c.logger.Debug("bridge queue flush started",
					"name", c.name,
					"count", count,
					"queued", q.Size(),
					"capacity", q.Capacity(),
				)
			}
			if len(progress) != len(messages) {
				progress = make([]int, len(messages))
			}
			allPublished := true
		publishLoop:
			for i, message := range messages {
				matches := matchingAddresses(c.cfg.Addresses, message.TopicName)
				if len(matches) == 0 {
					c.logger.Warn("dropping buffered bridge message; no publish address matches anymore", "name", c.name, "topic", message.TopicName)
					continue
				}
				for j := progress[i]; j < len(matches); j++ {
					if !c.publishLocalMessage(matches[j], message, true) {
						progress[i] = j
						allPublished = false
						break publishLoop
					}
					progress[i] = j + 1
				}
			}
			if allPublished {
				q.PollCommit()
				progress = nil
				if c.logger != nil {
					c.logger.Debug("bridge queue flush committed",
						"name", c.name,
						"count", count,
						"remaining", q.Size(),
					)
				}
			} else {
				if c.logger != nil {
					c.logger.Debug("bridge queue flush retained for retry",
						"name", c.name,
						"count", count,
						"queued", q.Size(),
					)
				}
				time.Sleep(time.Second)
			}
		}
	}()
}

func (c *Connector) publishLocalMessage(addr Address, msg stores.BrokerMessage, buffered bool) bool {
	remote := mapOutboundTopic(addr, msg.TopicName)
	if remote == "" {
		c.logger.Warn("bridge outbound topic mapping failed", "name", c.name, "localTopic", msg.TopicName)
		return true
	}
	c.mu.Lock()
	tr := c.tr
	c.mu.Unlock()
	if tr == nil || !c.canPublish(tr) {
		c.requeue(msg, buffered, remote, "remote disconnected")
		return false
	}
	msg, live := remainingExpiry(msg, time.Now())
	if !live {
		c.logger.Debug("bridge outbound message expired", "name", c.name, "topic", remote)
		return true
	}
	qos := outboundQoS(addr, msg)
	retain := addr.Retain || msg.IsRetain
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tr.Publish(ctx, remote, msg, qos, retain); err != nil {
		if errors.Is(err, errTransportDown) {
			c.logger.Warn("bridge outbound publish failed", "name", c.name, "topic", remote, "err", err)
			c.requeue(msg, buffered, remote, err.Error())
			return false
		}
		// Too large or rejected by the remote: retrying can never succeed and
		// would wedge the buffer, so drop it.
		c.logger.Warn("bridge outbound message dropped", "name", c.name, "topic", remote,
			"payloadBytes", len(msg.Payload), "err", err)
		return true
	}
	c.recordOut()
	c.logger.Debug("bridge outbound published", "name", c.name, "localTopic", msg.TopicName,
		"remoteTopic", remote, "qos", qos, "retain", retain, "buffered", buffered)
	return true
}

// remainingExpiry lowers Message Expiry by the time the message waited
// since it was published; live is false once it has expired [MQTT-3.3.2-5/6].
func remainingExpiry(msg stores.BrokerMessage, now time.Time) (stores.BrokerMessage, bool) {
	if msg.MessageExpiryInterval == nil || msg.Time.IsZero() {
		return msg, true
	}
	left := int64(*msg.MessageExpiryInterval) - int64(now.Sub(msg.Time)/time.Second)
	if left <= 0 {
		return msg, false
	}
	v := uint32(left)
	msg.MessageExpiryInterval = &v
	return msg, true
}

// requeue puts an unsent message into the bridge buffer when it came straight
// from the bus; one that already came from the buffer stays there for the next
// flush.
func (c *Connector) requeue(msg stores.BrokerMessage, buffered bool, remote, reason string) {
	if !buffered && c.usesMonsterBuffer() {
		c.enqueueLocalMessage(msg)
		return
	}
	c.logger.Debug("bridge outbound publish not sent", "name", c.name, "localTopic", msg.TopicName,
		"remoteTopic", remote, "reason", reason, "buffered", buffered,
		"bufferEnabled", c.cfg.BufferEnabled, "bufferImplementation", c.bufferImplementation())
}

// canPublish: with the library's queue (PAHO buffer) the transport queues
// while disconnected, so it can always take the message.
func (c *Connector) canPublish(tr transport) bool {
	return c.usesPahoBuffer() || tr.Connected()
}

func outboundQoS(addr Address, msg stores.BrokerMessage) byte {
	if addr.QoS == -1 {
		return msg.QoS
	}
	if addr.QoS < 0 {
		return 0
	}
	if addr.QoS > 2 {
		return 2
	}
	return byte(addr.QoS)
}

// mapOutboundTopic maps a locally-published topic to the topic to forward to
// the remote broker.
func mapOutboundTopic(a Address, localTopic string) string {
	if hasWildcard(a.LocalTopic) {
		base := literalPrefix(a.LocalTopic)
		if base != "" && localTopic != base && !strings.HasPrefix(localTopic, base+"/") {
			return ""
		}
		if a.RemovePath {
			return joinTopic(destinationPrefix(a.RemoteTopic), strings.TrimPrefix(strings.TrimPrefix(localTopic, base), "/"))
		}
		return joinTopic(destinationPrefix(a.RemoteTopic), localTopic)
	}
	if localTopic == a.LocalTopic {
		return destinationPrefix(a.RemoteTopic)
	}
	if strings.HasPrefix(localTopic, strings.TrimRight(a.LocalTopic, "/")+"/") {
		suffix := strings.TrimPrefix(localTopic, strings.TrimRight(a.LocalTopic, "/")+"/")
		return joinTopic(destinationPrefix(a.RemoteTopic), suffix)
	}
	return ""
}

func outboundFilter(localTopic string) string {
	if hasWildcard(localTopic) {
		return localTopic
	}
	return strings.TrimRight(localTopic, "/") + "/#"
}

func matchesLocalPattern(topic, pattern string) bool {
	if hasWildcard(pattern) {
		return matchTopic(pattern, topic)
	}
	return topic == pattern || strings.HasPrefix(topic, strings.TrimRight(pattern, "/")+"/")
}

func destinationPrefix(pattern string) string {
	if hasWildcard(pattern) {
		return literalPrefix(pattern)
	}
	return strings.TrimRight(pattern, "/")
}

func joinTopic(prefix, suffix string) string {
	prefix = strings.TrimRight(prefix, "/")
	suffix = strings.TrimLeft(suffix, "/")
	switch {
	case prefix == "":
		return suffix
	case suffix == "":
		return prefix
	default:
		return prefix + "/" + suffix
	}
}

func hasWildcard(pattern string) bool {
	return strings.ContainsAny(pattern, "+#")
}

// literalPrefix returns the longest literal prefix of an MQTT topic pattern —
// i.e. everything up to but not including the first wildcard segment.
//
//	"sensor/#"        → "sensor"
//	"a/b/+/c"         → "a/b"
//	"+/x"             → ""
//	"plain/topic"     → "plain/topic"
func literalPrefix(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, p := range parts {
		if p == "+" || p == "#" {
			return strings.Join(parts[:i], "/")
		}
	}
	return pattern
}

func (c *Connector) Stop() {
	c.mu.Lock()
	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
	if c.subID != 0 && c.subBus != nil {
		c.subBus.Unsubscribe(c.subID)
		c.subID = 0
	}
	tr := c.tr
	c.tr = nil
	c.mu.Unlock()

	if tr != nil {
		tr.Close()
	}
	// A fallback already under way has done its wg.Add once this returns;
	// one not yet started never will.
	c.fallback.Do(func() {})
	c.wg.Wait()
	c.closeQueue()
}

func (c *Connector) closeQueue() {
	c.mu.Lock()
	q := c.queue
	c.queue = nil
	c.mu.Unlock()
	if q == nil {
		return
	}
	if err := q.Close(); err != nil {
		c.logger.Warn("bridge queue close failed", "name", c.name, "err", err)
	}
}

func matchTopic(pattern, topic string) bool {
	// MQTT 4.7.2: a filter starting with a wildcard must not match $-topics ($SYS/...).
	if strings.HasPrefix(topic, "$") && (strings.HasPrefix(pattern, "#") || strings.HasPrefix(pattern, "+")) {
		return false
	}
	pp := strings.Split(pattern, "/")
	tt := strings.Split(topic, "/")
	for i, p := range pp {
		if p == "#" {
			return true
		}
		if i >= len(tt) {
			return false
		}
		if p == "+" {
			continue
		}
		if p != tt[i] {
			return false
		}
	}
	return len(pp) == len(tt)
}
