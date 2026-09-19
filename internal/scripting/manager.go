package scripting

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"monstermq.io/edge/internal/archive"
	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/stores"
	storepg "monstermq.io/edge/internal/stores/postgres"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
)

const DeviceTypeScript = "Script"

// Manager coordinates all active script devices on this edge broker node.
type Manager struct {
	store      stores.DeviceConfigStore
	storage    *stores.Storage
	archives   *archive.Manager
	publishFn  func(topic string, payload []byte, retain bool, qos byte) error
	bus        *pubsub.Bus
	nodeID     string
	logger     *slog.Logger

	global *GlobalStore
	db     *DatabaseManager

	srvCtx context.Context

	mu         sync.RWMutex
	connectors map[string]*Connector
	lastConfig map[string]string
}

func NewManager(
	store stores.DeviceConfigStore,
	storage *stores.Storage,
	archives *archive.Manager,
	sqliteDB *storesqlite.DB,
	pgDB *storepg.DB,
	publishFn func(topic string, payload []byte, retain bool, qos byte) error,
	bus *pubsub.Bus,
	nodeID string,
	logger *slog.Logger,
) *Manager {
	var archiveConfigStore stores.ArchiveConfigStore
	if storage != nil {
		archiveConfigStore = storage.ArchiveConfig
	}
	dbMgr := NewDatabaseManager(archiveConfigStore, sqliteDB, pgDB)

	m := &Manager{
		store:      store,
		storage:    storage,
		archives:   archives,
		publishFn:  publishFn,
		bus:        bus,
		nodeID:     nodeID,
		logger:     logger,
		global:     NewGlobalStore(),
		db:         dbMgr,
		srvCtx:     context.Background(),
		connectors: make(map[string]*Connector),
		lastConfig: make(map[string]string),
	}
	return m
}

func (m *Manager) Start(ctx context.Context) error {
	m.srvCtx = ctx
	if m.store == nil {
		return nil
	}

	devices, err := m.store.GetEnabledByNode(ctx, m.nodeID)
	if err != nil {
		return fmt.Errorf("load script devices: %w", err)
	}

	for _, d := range devices {
		if d.Type != DeviceTypeScript {
			continue
		}
		if err := m.startDeviceLocked(d); err != nil {
			m.logger.Warn("failed to start script", "name", d.Name, "err", err)
		}
	}
	return nil
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, c := range m.connectors {
		c.Stop()
	}
	m.connectors = make(map[string]*Connector)
	m.lastConfig = make(map[string]string)

	if m.db != nil {
		m.db.Close()
	}
}

// Reload reconciles live connectors with persisted configs (starts, restarts, or stops).
func (m *Manager) Reload(ctx context.Context) error {
	if m.store == nil {
		return nil
	}

	devices, err := m.store.GetEnabledByNode(ctx, m.nodeID)
	if err != nil {
		return err
	}

	activeOnNode := make(map[string]stores.DeviceConfig)
	for _, d := range devices {
		if d.Type == DeviceTypeScript {
			activeOnNode[d.Name] = d
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Stop connectors no longer present or disabled
	for name, conn := range m.connectors {
		if _, stillActive := activeOnNode[name]; !stillActive {
			conn.Stop()
			delete(m.connectors, name)
			delete(m.lastConfig, name)
			m.logger.Info("removed script connector", "name", name)
		}
	}

	// Start new or restart changed
	for name, d := range activeOnNode {
		prevRaw, running := m.lastConfig[name]
		if running && prevRaw == d.Config {
			continue // No change
		}
		if running {
			m.connectors[name].Stop()
			delete(m.connectors, name)
			delete(m.lastConfig, name)
			m.logger.Info("restarting modified script", "name", name)
		}
		if err := m.startDeviceLocked(d); err != nil {
			m.logger.Warn("failed to start script on reload", "name", name, "err", err)
		}
	}

	return nil
}

func (m *Manager) startDeviceLocked(d stores.DeviceConfig) error {
	cfg, err := ParseConfig(d.Config)
	if err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	eng, err := NewEngine(d.Name, cfg.Script)
	if err != nil {
		return fmt.Errorf("compile script: %w", err)
	}

	var msgStore stores.MessageStore
	if m.storage != nil {
		msgStore = m.storage.Retained
	}

	conn := NewConnector(
		d.Name,
		*cfg,
		eng,
		m.store,
		m.global,
		m.db,
		m.archives,
		msgStore,
		m.publishFn,
		m.bus,
		m.CallScript,
		m.nodeID,
		m.logger,
	)

	if err := conn.Start(m.srvCtx); err != nil {
		return err
	}

	m.connectors[d.Name] = conn
	m.lastConfig[d.Name] = d.Config
	return nil
}

// CallScript invokes an active script by name with custom arguments.
func (m *Manager) CallScript(name string, args map[string]any) (any, error) {
	m.mu.RLock()
	conn, ok := m.connectors[name]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("script %q is not running", name)
	}
	return conn.Call(args)
}

// GetConnector returns an active connector by name if running.
func (m *Manager) GetConnector(name string) *Connector {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connectors[name]
}

// TestScript executes a script configuration in dry-run mode against test input.
func (m *Manager) TestScript(name string, cfg ScriptConfig, testTopic, testPayload string, testArgs map[string]any) (*ScriptExecutionResult, error) {
	eng, err := NewEngine(name, cfg.Script)
	if err != nil {
		return nil, fmt.Errorf("compile script: %w", err)
	}

	var msgStore stores.MessageStore
	if m.storage != nil {
		msgStore = m.storage.Retained
	}

	conn := NewConnector(
		name,
		cfg,
		eng,
		m.store,
		m.global,
		m.db,
		m.archives,
		msgStore,
		m.publishFn,
		m.bus,
		m.CallScript,
		m.nodeID,
		m.logger,
	)

	return conn.Test(testTopic, testPayload, testArgs), nil
}
