package winccua

import (
	"context"
	"log/slog"
	"sync"

	"monstermq.io/edge/internal/stores"
)

// Manager loads WinCC Unified device configs from the DeviceConfigStore and
// runs one Connector per enabled device. Mirrors bridge/mqttclient/Manager.
type Manager struct {
	store     stores.DeviceConfigStore
	publisher LocalPublisher
	logger    *slog.Logger
	nodeID    string

	// srvCtx is the long-lived server context saved by Start(). Connector
	// goroutines must use this, not the short-lived GraphQL request context
	// that Reload() receives — otherwise the connector is killed the moment
	// the HTTP response is sent and the request context is cancelled.
	srvCtx context.Context

	mu         sync.Mutex
	connectors map[string]Connector
	lastConfig map[string]string // device name → raw JSON last deployed
}

func NewManager(store stores.DeviceConfigStore, publisher LocalPublisher, nodeID string, logger *slog.Logger) *Manager {
	return &Manager{
		store: store, publisher: publisher, logger: logger, nodeID: nodeID,
		srvCtx:     context.Background(), // safe default; overwritten by Start()
		connectors: map[string]Connector{},
		lastConfig: map[string]string{},
	}
}

func (m *Manager) Start(ctx context.Context) error {
	m.srvCtx = ctx // save before starting any connectors
	m.logger.Info("winccua manager start", "node_id", m.nodeID)
	devices, err := m.store.GetEnabledByNode(ctx, m.nodeID)
	if err != nil {
		return err
	}
	matched := 0
	for _, d := range devices {
		if d.Type != DeviceTypeWinCCUaClient {
			continue
		}
		matched++
		m.startDevice(ctx, d)
	}
	if matched == 0 {
		m.diagnoseNoDevices(ctx)
	}
	return nil
}

// Reload reconciles the live connector set against the persisted device
// configs: starts new ones, restarts changed ones, stops removed/disabled
// ones. Call after every GraphQL mutation that touches a WinCC UA client.
func (m *Manager) Reload(ctx context.Context) error {
	devices, err := m.store.GetEnabledByNode(ctx, m.nodeID)
	if err != nil {
		return err
	}
	wanted := map[string]stores.DeviceConfig{}
	for _, d := range devices {
		if d.Type != DeviceTypeWinCCUaClient {
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

	// Stop bridges that are no longer wanted, or whose stored config changed.
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

	// Start bridges that should be running but aren't.
	for name, d := range wanted {
		m.mu.Lock()
		_, running := m.connectors[name]
		m.mu.Unlock()
		if running {
			continue
		}
		m.startDevice(ctx, d)
	}
	return nil
}

func (m *Manager) startDevice(_ context.Context, d stores.DeviceConfig) {
	cfg, err := ParseConnectionConfig(d.Config)
	if err != nil {
		m.logger.Warn("winccua config parse failed", "name", d.Name, "err", err)
		return
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		m.logger.Warn("winccua config invalid", "name", d.Name, "errors", errs)
		return
	}
	m.logger.Info("winccua starting connector",
		"name", d.Name, "namespace", d.Namespace, "mode", cfg.DataAccessMode,
		"addresses", len(cfg.Addresses))
	c := New(d.Name, cfg, d.Namespace, m.publisher, m.logger)
	// Always use the server context so the connector goroutine outlives the
	// (potentially short-lived) request context from a Reload() call.
	if err := c.Start(m.srvCtx); err != nil {
		m.logger.Warn("winccua start failed", "name", d.Name, "err", err)
		return
	}
	m.mu.Lock()
	m.connectors[d.Name] = c
	m.lastConfig[d.Name] = d.Config
	m.mu.Unlock()
}

// diagnoseNoDevices runs when no WinCC UA devices match this node and
// surfaces the most common cause: a configured device whose nodeId differs
// from the broker's. Without this, the manager looks like a no-op even
// though a device clearly exists in the database.
func (m *Manager) diagnoseNoDevices(ctx context.Context) {
	all, err := m.store.GetAll(ctx)
	if err != nil {
		m.logger.Info("winccua: no devices for this node", "node_id", m.nodeID, "scan_err", err)
		return
	}
	mismatched := []string{}
	disabled := []string{}
	for _, d := range all {
		if d.Type != DeviceTypeWinCCUaClient {
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
		m.logger.Info("winccua: no devices configured", "node_id", m.nodeID)
		return
	}
	m.logger.Warn("winccua: devices exist but none deploy on this node",
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

// Connector returns the running connector for the named device, or nil.
func (m *Manager) Connector(name string) Connector {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connectors[name]
}
