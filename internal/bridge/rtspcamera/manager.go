package rtspcamera

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"monstermq.io/edge/internal/stores"
)

// Manager loads RTSP Camera configs from the DeviceConfigStore and runs one Connector per enabled device.
type Manager struct {
	store     stores.DeviceConfigStore
	publisher LocalPublisher
	subBus    LocalSubscriber
	logger    *slog.Logger
	nodeID    string

	srvCtx context.Context

	mu         sync.RWMutex
	connectors map[string]*Connector
	lastConfig map[string]string // camera name -> raw JSON config
}

// NewManager creates a new RTSP camera manager.
func NewManager(store stores.DeviceConfigStore, publisher LocalPublisher, subBus LocalSubscriber, nodeID string, logger *slog.Logger) *Manager {
	return &Manager{
		store:      store,
		publisher:  publisher,
		subBus:     subBus,
		logger:     logger,
		nodeID:     nodeID,
		srvCtx:     context.Background(),
		connectors: make(map[string]*Connector),
		lastConfig: make(map[string]string),
	}
}

// Start loads enabled RTSP camera devices and starts them.
func (m *Manager) Start(ctx context.Context) error {
	m.srvCtx = ctx
	m.logger.Info("rtsp camera manager starting", "node_id", m.nodeID)
	devices, err := m.store.GetEnabledByNode(ctx, m.nodeID)
	if err != nil {
		return err
	}
	for _, d := range devices {
		if d.Type != DeviceTypeRtspCamera {
			continue
		}
		m.startDevice(m.srvCtx, d)
	}
	return nil
}

// Stop shuts down all running camera connectors.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, c := range m.connectors {
		c.Stop()
		delete(m.connectors, name)
		delete(m.lastConfig, name)
	}
	m.logger.Info("rtsp camera manager stopped")
}

// Reload reconciles active connectors against DeviceConfigStore.
func (m *Manager) Reload(ctx context.Context) error {
	devices, err := m.store.GetEnabledByNode(ctx, m.nodeID)
	if err != nil {
		return err
	}
	wanted := make(map[string]stores.DeviceConfig)
	for _, d := range devices {
		if d.Type != DeviceTypeRtspCamera {
			continue
		}
		wanted[d.Name] = d
	}

	m.mu.Lock()
	current := make(map[string]*Connector, len(m.connectors))
	for k, v := range m.connectors {
		current[k] = v
	}
	currentRaw := make(map[string]string, len(m.lastConfig))
	for k, v := range m.lastConfig {
		currentRaw[k] = v
	}
	m.mu.Unlock()

	// Stop connectors that are no longer enabled or whose configuration changed.
	for name, c := range current {
		next, keep := wanted[name]
		if !keep || next.Config != currentRaw[name] {
			c.Stop()
			m.mu.Lock()
			delete(m.connectors, name)
			delete(m.lastConfig, name)
			m.mu.Unlock()
		}
	}

	// Start new or updated connectors using long-lived srvCtx.
	for name, d := range wanted {
		m.mu.RLock()
		_, exists := m.connectors[name]
		m.mu.RUnlock()
		if !exists {
			m.startDevice(m.srvCtx, d)
		}
	}
	return nil
}

// Connector returns an active connector by name, or nil if not found.
func (m *Manager) Connector(name string) *Connector {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connectors[name]
}

// Connectors returns a list of all active connectors.
func (m *Manager) Connectors() []*Connector {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Connector, 0, len(m.connectors))
	for _, c := range m.connectors {
		out = append(out, c)
	}
	return out
}

// TriggerSnapshot triggers an immediate snapshot on the named camera.
func (m *Manager) TriggerSnapshot(name string) error {
	c := m.Connector(name)
	if c == nil {
		return fmt.Errorf("camera %q is not running", name)
	}
	return c.TriggerSnapshot()
}

func (m *Manager) startDevice(ctx context.Context, d stores.DeviceConfig) {
	cfg, err := ParseConfig(d.Config)
	if err != nil {
		m.logger.Error("failed to parse rtsp camera config", "name", d.Name, "err", err)
		return
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		m.logger.Error("rtsp camera config validation failed", "name", d.Name, "errs", errs)
		return
	}

	c := NewConnector(d.Name, m.nodeID, cfg, m.publisher, m.subBus, m.logger)
	c.Start(ctx)

	m.mu.Lock()
	m.connectors[d.Name] = c
	m.lastConfig[d.Name] = d.Config
	m.mu.Unlock()
}
