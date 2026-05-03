package winccoa

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"monstermq.io/edge/internal/stores"
)

// Manager loads WinCC OA device configs from the DeviceConfigStore and runs
// one Connector per enabled device.
type Manager struct {
	store     stores.DeviceConfigStore
	publisher LocalPublisher
	logger    *slog.Logger
	nodeID    string

	srvCtx context.Context

	mu         sync.Mutex
	connectors map[string]Connector
	lastConfig map[string]string
}

func NewManager(store stores.DeviceConfigStore, publisher LocalPublisher, nodeID string, logger *slog.Logger) *Manager {
	return &Manager{
		store:      store,
		publisher:  publisher,
		logger:     logger,
		nodeID:     nodeID,
		srvCtx:     context.Background(),
		connectors: map[string]Connector{},
		lastConfig: map[string]string{},
	}
}

func (m *Manager) Start(ctx context.Context) error {
	m.srvCtx = ctx
	m.logger.Info("winccoa manager start", "node_id", m.nodeID)
	devices, err := m.store.GetEnabledByNode(ctx, m.nodeID)
	if err != nil {
		return err
	}
	matched := 0
	for _, d := range devices {
		if d.Type != DeviceTypeWinCCOaClient {
			continue
		}
		matched++
		m.startDevice(d)
	}
	if matched == 0 {
		m.diagnoseNoDevices(ctx)
	}
	return nil
}

func (m *Manager) Reload(ctx context.Context) error {
	devices, err := m.store.GetEnabledByNode(ctx, m.nodeID)
	if err != nil {
		return err
	}
	wanted := map[string]stores.DeviceConfig{}
	for _, d := range devices {
		if d.Type != DeviceTypeWinCCOaClient {
			continue
		}
		wanted[d.Name] = d
	}
	if len(wanted) == 0 {
		m.diagnoseNoDevices(ctx)
	}

	m.mu.Lock()
	current := m.connectors
	currentRaw := m.lastConfig
	m.mu.Unlock()

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

	for name, d := range wanted {
		m.mu.Lock()
		_, running := m.connectors[name]
		m.mu.Unlock()
		if running {
			continue
		}
		m.startDevice(d)
	}
	return nil
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
				m.mu.Lock()
				items := make(map[string]Connector, len(m.connectors))
				for name, c := range m.connectors {
					items[name] = c
				}
				m.mu.Unlock()
				for name, c := range items {
					snap := c.SampleMetrics(now)
					if store == nil {
						continue
					}
					payload, _ := json.Marshal(snap)
					if err := store.StoreMetrics(ctx, stores.MetricWinCCOa, now.UTC(), name, string(payload)); err != nil {
						m.logger.Warn("winccoa metrics persist failed", "name", name, "err", err)
					}
				}
			}
		}
	}()
}

func (m *Manager) startDevice(d stores.DeviceConfig) {
	cfg, err := ParseConnectionConfig(d.Config)
	if err != nil {
		m.logger.Warn("winccoa config parse failed", "name", d.Name, "err", err)
		return
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		m.logger.Warn("winccoa config invalid", "name", d.Name, "errors", errs)
		return
	}
	m.logger.Info("winccoa starting connector", "name", d.Name, "namespace", d.Namespace, "addresses", len(cfg.Addresses))
	c := New(d.Name, cfg, d.Namespace, m.publisher, m.logger)
	if err := c.Start(m.srvCtx); err != nil {
		m.logger.Warn("winccoa start failed", "name", d.Name, "err", err)
		return
	}
	m.mu.Lock()
	m.connectors[d.Name] = c
	m.lastConfig[d.Name] = d.Config
	m.mu.Unlock()
}

func (m *Manager) diagnoseNoDevices(ctx context.Context) {
	all, err := m.store.GetAll(ctx)
	if err != nil {
		m.logger.Info("winccoa: no devices for this node", "node_id", m.nodeID, "scan_err", err)
		return
	}
	mismatched := []string{}
	disabled := []string{}
	for _, d := range all {
		if d.Type != DeviceTypeWinCCOaClient {
			continue
		}
		if !d.Enabled {
			disabled = append(disabled, d.Name+"(enabled=false)")
			continue
		}
		if d.NodeID != m.nodeID {
			mismatched = append(mismatched, d.Name+"(nodeId="+d.NodeID+")")
		}
	}
	if len(mismatched) == 0 && len(disabled) == 0 {
		m.logger.Info("winccoa: no devices configured", "node_id", m.nodeID)
		return
	}
	m.logger.Warn("winccoa: devices exist but none deploy on this node",
		"this_node_id", m.nodeID,
		"mismatched_node_id", mismatched,
		"disabled", disabled)
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.connectors {
		c.Stop()
	}
	m.connectors = map[string]Connector{}
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

func (m *Manager) Connector(name string) Connector {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connectors[name]
}
