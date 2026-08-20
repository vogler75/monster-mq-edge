package redfish

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/stores"
)

// Subscriber listens on pubsub.Bus, parses incoming MQTT messages according to
// configured GatewayConfigs, and writes normalized sensor records to the LastValue MessageStore.
type Subscriber struct {
	bus       *pubsub.Bus
	lastVal   stores.MessageStore
	publishFn func(topic string, payload []byte, retain bool, qos byte) error
	logger    *slog.Logger

	mu       sync.RWMutex
	gateways map[string]*GatewayConfig
	subID    int
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// NewSubscriber creates a new Redfish message ingestion subscriber.
func NewSubscriber(
	bus *pubsub.Bus,
	lastVal stores.MessageStore,
	publishFn func(topic string, payload []byte, retain bool, qos byte) error,
	logger *slog.Logger,
) *Subscriber {
	return &Subscriber{
		bus:       bus,
		lastVal:   lastVal,
		publishFn: publishFn,
		logger:    logger,
		gateways:  make(map[string]*GatewayConfig),
	}
}

// SetGateways replaces the active gateway mapping configurations.
func (s *Subscriber) SetGateways(gateways map[string]*GatewayConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gateways = make(map[string]*GatewayConfig, len(gateways))
	for k, v := range gateways {
		if v != nil {
			s.gateways[k] = v
		}
	}
}

// Start begins consuming messages from the pubsub.Bus.
func (s *Subscriber) Start(ctx context.Context) {
	s.mu.Lock()
	if s.subID > 0 {
		s.mu.Unlock()
		return
	}
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	// Subscribe to all topics to match dynamically against configured gateway filters
	id, ch := s.bus.Subscribe([]string{"#"}, 256)
	s.subID = id
	s.mu.Unlock()

	go s.loop(ch)
}

// Stop terminates the subscriber loop.
func (s *Subscriber) Stop() {
	s.mu.Lock()
	if s.subID == 0 {
		s.mu.Unlock()
		return
	}
	s.bus.Unsubscribe(s.subID)
	s.subID = 0
	close(s.stopCh)
	s.mu.Unlock()

	select {
	case <-s.doneCh:
	case <-time.After(2 * time.Second):
	}
}

func (s *Subscriber) loop(ch <-chan stores.BrokerMessage) {
	defer close(s.doneCh)
	ctx := context.Background()

	for {
		select {
		case <-s.stopCh:
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			s.handleMessage(ctx, msg)
		}
	}
}

func (s *Subscriber) handleMessage(ctx context.Context, msg stores.BrokerMessage) {
	s.mu.RLock()
	gateways := make([]struct {
		name string
		gw   *GatewayConfig
	}, 0, len(s.gateways))
	for name, gw := range s.gateways {
		if gw != nil && matchesAny(gw.TopicFilters, msg.TopicName) {
			gateways = append(gateways, struct {
				name string
				gw   *GatewayConfig
			}{name: name, gw: gw})
		}
	}
	s.mu.RUnlock()

	if len(gateways) == 0 {
		return
	}

	for _, entry := range gateways {
		records, err := ExtractSensorRecords(msg.Payload, msg.TopicName, entry.gw)
		if err != nil {
			if s.logger != nil {
				s.logger.Debug("redfish payload extraction skipped", "gateway", entry.name, "topic", msg.TopicName, "err", err)
			}
			continue
		}

		for _, record := range records {
			record.GatewayName = entry.name
			topicPrefix := record.TopicPrefix
			if topicPrefix == "" {
				topicPrefix = "redfish"
			}

			normalizedTopic := fmt.Sprintf("%s/%s/sensors/%s", topicPrefix, record.ChassisID, record.SensorID)
			payloadBytes, err := json.Marshal(record)
			if err != nil {
				continue
			}

			bm := stores.BrokerMessage{
				TopicName: normalizedTopic,
				Payload:   payloadBytes,
				Time:      time.Now().UTC(),
				QoS:       0,
				IsRetain:  true,
			}

			if s.lastVal != nil {
				if err := s.lastVal.AddAll(ctx, []stores.BrokerMessage{bm}); err != nil && s.logger != nil {
					s.logger.Warn("failed to store redfish sensor to lastval", "topic", normalizedTopic, "err", err)
				}
			}

			if s.publishFn != nil {
				_ = s.publishFn(normalizedTopic, payloadBytes, true, 0)
			}
		}
	}
}

func matchesAny(filters []string, topic string) bool {
	for _, f := range filters {
		if matchTopic(strings.TrimSpace(f), topic) {
			return true
		}
	}
	return false
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
