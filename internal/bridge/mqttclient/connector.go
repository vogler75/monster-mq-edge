package mqttclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
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
	BufferSize           int       `json:"bufferSize,omitempty"`
	PersistBuffer        bool      `json:"persistBuffer,omitempty"`
	DeleteOldestMessages bool      `json:"deleteOldestMessages,omitempty"`
}

// LocalPublisher is implemented by mochi-mqtt's *Server (Publish).
type LocalPublisher func(topic string, payload []byte, retain bool, qos byte) error

// LocalSubscriber lets the bridge listen to the local broker for outbound
// publishes. Implemented via the in-process pubsub bus.
type LocalSubscriber interface {
	Subscribe(filters []string, buffer int) (id int, ch <-chan LocalMessage)
	Unsubscribe(id int)
}

// LocalMessage is what the bus delivers to the bridge.
type LocalMessage struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
}

// Connector is one bridge to one remote broker.
type Connector struct {
	name      string
	cfg       Config
	publisher LocalPublisher
	subBus    LocalSubscriber
	logger    *slog.Logger

	mu     sync.Mutex
	client paho.Client
	subID  int
	stopCh chan struct{}
	queue  queue.MessageQueue
	retry  bool
	wg     sync.WaitGroup

	IncIn  func()
	IncOut func()
}

func NewConnector(name string, cfg Config, publisher LocalPublisher, subBus LocalSubscriber, logger *slog.Logger) *Connector {
	return &Connector{
		name: name, cfg: cfg, publisher: publisher, subBus: subBus, logger: logger,
		stopCh: make(chan struct{}),
	}
}

func (c *Connector) Name() string { return c.name }

// Start dials the remote broker and registers inbound/outbound forwarders.
func (c *Connector) Start(ctx context.Context) error {
	opts := paho.NewClientOptions().AddBroker(c.cfg.BrokerURL)
	opts.SetClientID(c.cfg.ClientID)
	if c.cfg.Username != "" {
		opts.SetUsername(c.cfg.Username)
	}
	if c.cfg.Password != "" {
		opts.SetPassword(c.cfg.Password)
	}
	opts.SetCleanSession(c.cfg.CleanSession)
	if c.cfg.KeepAlive > 0 {
		opts.SetKeepAlive(time.Duration(c.cfg.KeepAlive) * time.Second)
	}
	if c.cfg.ConnectionTimeout > 0 {
		opts.SetConnectTimeout(configMillisOrSeconds(c.cfg.ConnectionTimeout))
	}
	if c.cfg.ReconnectDelay > 0 {
		opts.SetMaxReconnectInterval(configMillisOrSeconds(c.cfg.ReconnectDelay))
	} else {
		opts.SetMaxReconnectInterval(30 * time.Second)
	}
	opts.SetAutoReconnect(true)
	if strings.HasPrefix(c.cfg.BrokerURL, "ssl://") || strings.HasPrefix(c.cfg.BrokerURL, "tls://") || strings.HasPrefix(c.cfg.BrokerURL, "wss://") {
		opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	}

	opts.SetOnConnectHandler(func(client paho.Client) {
		c.logger.Info("bridge connected", "name", c.name, "url", c.cfg.BrokerURL)
		c.subscribeInbound(client)
	})
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		c.logger.Warn("bridge connection lost", "name", c.name, "err", err)
	})

	client := paho.NewClient(opts)
	c.mu.Lock()
	c.client = client
	c.mu.Unlock()

	if err := c.initQueue(); err != nil {
		return err
	}
	c.startOutbound(ctx)
	c.startQueueWriter(ctx)

	tok := client.Connect()
	if !tok.WaitTimeout(5 * time.Second) {
		c.logger.Warn("bridge connect timeout; connector will keep buffering publishes", "name", c.name, "url", c.cfg.BrokerURL)
		c.startConnectRetry(ctx)
		return nil
	}
	if err := tok.Error(); err != nil {
		c.logger.Warn("bridge connect failed; connector will keep buffering publishes", "name", c.name, "url", c.cfg.BrokerURL, "err", err)
		c.startConnectRetry(ctx)
		return nil
	}
	return nil
}

func (c *Connector) startConnectRetry(ctx context.Context) {
	c.mu.Lock()
	if c.retry {
		c.mu.Unlock()
		return
	}
	c.retry = true
	c.mu.Unlock()

	delay := configMillisOrSeconds(c.cfg.ReconnectDelay)
	if c.cfg.ReconnectDelay <= 0 {
		delay = 30 * time.Second
	}

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() {
			c.mu.Lock()
			c.retry = false
			c.mu.Unlock()
		}()
		timer := time.NewTimer(delay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.stopCh:
				return
			case <-timer.C:
			}

			c.mu.Lock()
			client := c.client
			c.mu.Unlock()
			if client == nil || client.IsConnected() {
				return
			}
			tok := client.Connect()
			if tok.WaitTimeout(5*time.Second) && tok.Error() == nil {
				c.logger.Info("bridge connected after retry", "name", c.name, "url", c.cfg.BrokerURL)
				return
			}
			if err := tok.Error(); err != nil {
				c.logger.Warn("bridge connect retry failed", "name", c.name, "url", c.cfg.BrokerURL, "err", err)
			} else {
				c.logger.Warn("bridge connect retry timeout", "name", c.name, "url", c.cfg.BrokerURL)
			}
			timer.Reset(delay)
		}
	}()
}

func (c *Connector) initQueue() error {
	if !c.cfg.BufferEnabled {
		return nil
	}
	size := c.cfg.BufferSize
	if size <= 0 {
		size = 5000
	}
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

func configMillisOrSeconds(value int) time.Duration {
	if value <= 0 {
		return 0
	}
	if value < 1000 {
		return time.Duration(value) * time.Second
	}
	return time.Duration(value) * time.Millisecond
}

func (c *Connector) subscribeInbound(client paho.Client) {
	if client == nil {
		return
	}
	for _, a := range c.cfg.Addresses {
		if !strings.EqualFold(a.Mode, "SUBSCRIBE") {
			continue
		}
		addr := a // capture
		tok := client.Subscribe(addr.RemoteTopic, byte(addr.QoS), func(_ paho.Client, m paho.Message) {
			localTopic := mapInboundTopic(addr, m.Topic())
			if c.IncIn != nil {
				c.IncIn()
			}
			if err := c.publisher(localTopic, m.Payload(), addr.Retain, byte(addr.QoS)); err != nil {
				c.logger.Warn("bridge inbound publish failed", "name", c.name, "topic", localTopic, "err", err)
			}
		})
		if !tok.WaitTimeout(5 * time.Second) {
			c.logger.Warn("bridge subscribe timeout", "name", c.name, "topic", addr.RemoteTopic)
		}
	}
}

// mapInboundTopic maps an incoming topic from the remote broker to the local
// topic to publish under, respecting the address's removePath flag and the
// LocalTopic prefix (if it has no wildcards).
func mapInboundTopic(a Address, remoteTopic string) string {
	if hasWildcard(a.RemoteTopic) && a.RemovePath {
		base := literalPrefix(a.RemoteTopic)
		suffix := remoteTopic
		if base != "" && (remoteTopic == base || strings.HasPrefix(remoteTopic, base+"/")) {
			suffix = strings.TrimPrefix(strings.TrimPrefix(remoteTopic, base), "/")
		}
		return joinTopic(destinationPrefix(a.LocalTopic), suffix)
	}
	return joinTopic(destinationPrefix(a.LocalTopic), remoteTopic)
}

func (c *Connector) startOutbound(ctx context.Context) {
	filters := []string{}
	addrByFilter := map[string]Address{}
	for _, a := range c.cfg.Addresses {
		if !strings.EqualFold(a.Mode, "PUBLISH") {
			continue
		}
		filters = append(filters, outboundFilter(a.LocalTopic))
		addrByFilter[a.LocalTopic] = a
	}
	if len(filters) == 0 {
		return
	}
	id, ch := c.subBus.Subscribe(filters, 256)
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
				matches := matchingAddresses(addrByFilter, msg.Topic)
				if len(matches) == 0 {
					c.logger.Warn("bridge outbound topic mapping failed", "name", c.name, "localTopic", msg.Topic)
					continue
				}
				if c.cfg.BufferEnabled {
					c.enqueueLocalMessage(localMessageToBrokerMessage(msg))
					continue
				}
				for _, addr := range matches {
					if !c.publishLocalMessage(addr, localMessageToBrokerMessage(msg), false) {
						break
					}
				}
			}
		}
	}()
}

func matchingAddresses(filters map[string]Address, topic string) []Address {
	var out []Address
	for filter, a := range filters {
		if matchesLocalPattern(topic, filter) {
			out = append(out, a)
		}
	}
	return out
}

func localMessageToBrokerMessage(msg LocalMessage) stores.BrokerMessage {
	return stores.BrokerMessage{
		TopicName: msg.Topic,
		Payload:   msg.Payload,
		QoS:       msg.QoS,
		IsRetain:  msg.Retain,
		Time:      time.Now(),
	}
}

func (c *Connector) enqueueLocalMessage(msg stores.BrokerMessage) {
	if c.queue == nil {
		c.logger.Warn("bridge buffering enabled but queue is not initialized; dropping message", "name", c.name, "topic", msg.TopicName)
		return
	}
	c.queue.Add(msg)
}

func (c *Connector) startQueueWriter(ctx context.Context) {
	if c.queue == nil {
		return
	}
	addrByFilter := map[string]Address{}
	for _, a := range c.cfg.Addresses {
		if strings.EqualFold(a.Mode, "PUBLISH") {
			addrByFilter[a.LocalTopic] = a
		}
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-c.stopCh:
				return
			default:
			}

			var messages []stores.BrokerMessage
			count := c.queue.PollBlock(func(message stores.BrokerMessage) {
				messages = append(messages, message)
			})
			if count == 0 {
				time.Sleep(10 * time.Millisecond)
				continue
			}

			allPublished := true
		publishLoop:
			for _, message := range messages {
				matches := matchingAddresses(addrByFilter, message.TopicName)
				if len(matches) == 0 {
					c.logger.Warn("dropping buffered bridge message; no publish address matches anymore", "name", c.name, "topic", message.TopicName)
					continue
				}
				for _, addr := range matches {
					if !c.publishLocalMessage(addr, message, true) {
						allPublished = false
						break publishLoop
					}
				}
			}
			if allPublished {
				c.queue.PollCommit()
			} else {
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
	client := c.client
	c.mu.Unlock()
	if client == nil || !client.IsConnected() {
		if !buffered && c.cfg.BufferEnabled {
			c.enqueueLocalMessage(msg)
		}
		return false
	}

	qos := byte(addr.QoS)
	tok := client.Publish(remote, qos, addr.Retain || msg.IsRetain, msg.Payload)
	if !tok.WaitTimeout(5 * time.Second) {
		c.logger.Warn("bridge outbound publish timeout", "name", c.name, "topic", remote)
		if !buffered && c.cfg.BufferEnabled {
			c.enqueueLocalMessage(msg)
		}
		return false
	}
	if err := tok.Error(); err != nil {
		c.logger.Warn("bridge outbound publish failed", "name", c.name, "topic", remote, "err", err)
		if !buffered && c.cfg.BufferEnabled {
			c.enqueueLocalMessage(msg)
		}
		return false
	}
	if c.IncOut != nil {
		c.IncOut()
	}
	return true
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
	if c.client != nil {
		c.client.Disconnect(250)
		c.client = nil
	}
	q := c.queue
	c.queue = nil
	c.mu.Unlock()

	c.wg.Wait()
	if q != nil {
		if err := q.Close(); err != nil {
			c.logger.Warn("bridge queue close failed", "name", c.name, "err", err)
		}
	}
}

func matchTopic(pattern, topic string) bool {
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
