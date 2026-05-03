package mqttclient

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"monstermq.io/edge/internal/stores"
)

// Manager loads MQTT-bridge device configs from the DeviceConfigStore and runs
// one Connector per enabled device.
type Manager struct {
	store     stores.DeviceConfigStore
	publisher LocalPublisher
	subBus    LocalSubscriber
	logger    *slog.Logger
	nodeID    string

	// srvCtx is the long-lived server context saved by Start(). Connector
	// goroutines must use this, not the short-lived GraphQL request context
	// that Reload() receives; otherwise the outbound subscriber exits as soon
	// as the HTTP response is sent.
	srvCtx context.Context

	mu         sync.Mutex
	connectors map[string]*Connector
	lastConfig map[string]string // device name → raw JSON last deployed

	incIn  func(int)
	incOut func(int)
}

func (m *Manager) SetCounters(incIn, incOut func(int)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.incIn = incIn
	m.incOut = incOut
	for _, c := range m.connectors {
		m.attachCounters(c)
	}
}

func (m *Manager) attachCounters(c *Connector) {
	if m.incIn != nil {
		c.IncIn = func() { m.incIn(1) }
	}
	if m.incOut != nil {
		c.IncOut = func() { m.incOut(1) }
	}
}

func (m *Manager) StartMetrics(ctx context.Context, store stores.MetricsStore, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				for _, c := range m.Snapshot() {
					snap := c.SampleMetrics(now, interval)
					if store == nil {
						continue
					}
					payload, _ := json.Marshal(snap)
					if err := store.StoreMetrics(ctx, stores.MetricMqttClient, now.UTC(), c.Name(), string(payload)); err != nil {
						m.logger.Warn("mqtt bridge metrics persist failed", "name", c.Name(), "err", err)
					}
				}
			}
		}
	}()
}

func NewManager(store stores.DeviceConfigStore, publisher LocalPublisher, subBus LocalSubscriber, nodeID string, logger *slog.Logger) *Manager {
	return &Manager{
		store: store, publisher: publisher, subBus: subBus, logger: logger, nodeID: nodeID,
		srvCtx:     context.Background(),
		connectors: map[string]*Connector{},
		lastConfig: map[string]string{},
	}
}

func (m *Manager) Start(ctx context.Context) error {
	m.srvCtx = ctx
	devices, err := m.store.GetEnabledByNode(ctx, m.nodeID)
	if err != nil {
		return err
	}
	for _, d := range devices {
		if d.Type != "" && d.Type != "MQTT_CLIENT" {
			continue
		}
		var cfg Config
		if err := json.Unmarshal([]byte(d.Config), &cfg); err != nil {
			m.logger.Warn("bridge config parse failed", "name", d.Name, "err", err)
			continue
		}
		if cfg.ClientID == "" {
			cfg.ClientID = "edge-" + m.nodeID + "-" + d.Name
		}
		c := NewConnector(d.Name, cfg, m.publisher, m.subBus, m.logger)
		m.mu.Lock()
		m.attachCounters(c)
		m.mu.Unlock()
		if err := c.Start(m.srvCtx); err != nil {
			m.logger.Warn("bridge start failed", "name", d.Name, "err", err)
			continue
		}
		m.mu.Lock()
		m.connectors[d.Name] = c
		m.lastConfig[d.Name] = d.Config
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.connectors {
		c.Stop()
	}
	m.connectors = map[string]*Connector{}
}

// Reload reconciles the live connector set against the persisted device
// configs: starts new ones, restarts changed ones, stops removed/disabled
// ones. Call after every GraphQL mutation that touches an MQTT client.
func (m *Manager) Reload(ctx context.Context) error {
	devices, err := m.store.GetEnabledByNode(ctx, m.nodeID)
	if err != nil {
		return err
	}
	wanted := map[string]Config{}
	wantedRaw := map[string]string{}
	for _, d := range devices {
		if d.Type != "" && d.Type != "MQTT_CLIENT" {
			continue
		}
		var cfg Config
		if err := json.Unmarshal([]byte(d.Config), &cfg); err != nil {
			m.logger.Warn("bridge config parse failed", "name", d.Name, "err", err)
			continue
		}
		if cfg.ClientID == "" {
			cfg.ClientID = "edge-" + m.nodeID + "-" + d.Name
		}
		wanted[d.Name] = cfg
		wantedRaw[d.Name] = d.Config
	}

	m.mu.Lock()
	current := m.connectors
	currentRaw := m.lastConfig
	m.mu.Unlock()

	// Stop bridges that are no longer wanted, or whose stored config changed.
	for name, c := range current {
		if newRaw, keep := wantedRaw[name]; !keep || newRaw != currentRaw[name] {
			c.Stop()
			m.mu.Lock()
			delete(m.connectors, name)
			delete(m.lastConfig, name)
			m.mu.Unlock()
		}
	}

	// Start bridges that should be running but aren't.
	for name, cfg := range wanted {
		m.mu.Lock()
		_, running := m.connectors[name]
		m.mu.Unlock()
		if running {
			continue
		}
		c := NewConnector(name, cfg, m.publisher, m.subBus, m.logger)
		m.mu.Lock()
		m.attachCounters(c)
		m.mu.Unlock()
		if err := c.Start(m.srvCtx); err != nil {
			m.logger.Warn("bridge start failed", "name", name, "err", err)
			continue
		}
		m.mu.Lock()
		m.connectors[name] = c
		m.lastConfig[name] = wantedRaw[name]
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) Active() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.connectors))
	for name := range m.connectors {
		out = append(out, name)
	}
	return out
}

func (m *Manager) Snapshot() []*Connector {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Connector, 0, len(m.connectors))
	for _, c := range m.connectors {
		out = append(out, c)
	}
	return out
}

func (m *Manager) Get(name string) *Connector {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connectors[name]
}
