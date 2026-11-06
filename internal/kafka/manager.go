package kafka

import (
	"context"
	"log/slog"
	"sync"

	"monstermq.io/edge/internal/stores"
)

// DeviceTypeKafkaServer is the device type of a Kafka protocol server.
const DeviceTypeKafkaServer = "Kafka-Server"

// StoreFactory returns the NewStore function of one Kafka server. It is
// called per (re)deploy so the factory can name in-memory stores per server.
type StoreFactory func(cfg ServerConfig) func(ctx context.Context, storeType *string, suffix string) (stores.KafkaQueueStore, error)

// Manager loads Kafka-Server device configs from the DeviceConfigStore and
// runs one Server per enabled device on this node.
type Manager struct {
	store  stores.DeviceConfigStore
	deps   Deps // NewStore is filled per server from factory
	factor StoreFactory
	logger *slog.Logger
	nodeID string

	// srvCtx is the long-lived server context saved by Start; Reload receives
	// a short-lived request context.
	srvCtx context.Context

	mu      sync.Mutex
	servers map[string]*Server
	lastKey map[string]string // device name -> namespace + raw JSON last deployed
	lastErr map[string]string // device name -> last start error
}

// NewManager creates a manager; deps.NewStore is ignored (factory is used).
func NewManager(store stores.DeviceConfigStore, deps Deps, factory StoreFactory, nodeID string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	deps.NodeID = nodeID
	deps.Logger = logger
	return &Manager{
		store: store, deps: deps, factor: factory, logger: logger, nodeID: nodeID,
		srvCtx:  context.Background(),
		servers: map[string]*Server{},
		lastKey: map[string]string{},
		lastErr: map[string]string{},
	}
}

func (m *Manager) Start(ctx context.Context) error {
	m.srvCtx = ctx
	return m.Reload(ctx)
}

func (m *Manager) Stop() {
	m.mu.Lock()
	current := m.servers
	m.servers = map[string]*Server{}
	m.lastKey = map[string]string{}
	m.mu.Unlock()
	for _, s := range current {
		s.Stop()
	}
}

type wantedServer struct {
	cfg ServerConfig
	key string
}

// Reload reconciles the running servers with the stored device configs:
// starts new ones, restarts changed ones and stops removed or disabled ones.
// Call it after every mutation that touches a Kafka server.
func (m *Manager) Reload(ctx context.Context) error {
	devices, err := m.store.GetEnabledByNode(ctx, m.nodeID)
	if err != nil {
		return err
	}
	wanted := map[string]wantedServer{}
	for _, d := range devices {
		if d.Type != DeviceTypeKafkaServer {
			continue
		}
		cfg, err := ParseConfig([]byte(d.Config))
		if err != nil {
			m.logger.Warn("kafka server config parse failed", "name", d.Name, "err", err)
			m.setErr(d.Name, err.Error())
			continue
		}
		cfg.Name, cfg.Namespace, cfg.NodeID, cfg.Enabled = d.Name, d.Namespace, d.NodeID, d.Enabled
		wanted[d.Name] = wantedServer{cfg: cfg, key: d.Namespace + "\x00" + d.Config}
	}

	m.mu.Lock()
	var stale []*Server
	for name, s := range m.servers {
		if w, keep := wanted[name]; !keep || w.key != m.lastKey[name] {
			stale = append(stale, s)
			delete(m.servers, name)
			delete(m.lastKey, name)
		}
	}
	for name := range m.lastErr {
		if _, keep := wanted[name]; !keep {
			delete(m.lastErr, name)
		}
	}
	m.mu.Unlock()
	// Stop first: the new instance may bind the same port and stores.
	for _, s := range stale {
		s.Stop()
	}

	for name, w := range wanted {
		m.mu.Lock()
		_, running := m.servers[name]
		m.mu.Unlock()
		if running {
			continue
		}
		deps := m.deps
		deps.NewStore = m.factor(w.cfg)
		srv, err := New(w.cfg, deps)
		if err == nil {
			err = srv.Start()
		}
		if err != nil {
			m.logger.Warn("kafka server start failed", "name", name, "err", err)
			m.setErr(name, err.Error())
			continue
		}
		m.mu.Lock()
		m.servers[name] = srv
		m.lastKey[name] = w.key
		delete(m.lastErr, name)
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) setErr(name, msg string) {
	m.mu.Lock()
	m.lastErr[name] = msg
	m.mu.Unlock()
}

// Get returns the running server of a device, or nil.
func (m *Manager) Get(name string) *Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.servers[name]
}

// Snapshot returns all running servers.
func (m *Manager) Snapshot() []*Server {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Server, 0, len(m.servers))
	for _, s := range m.servers {
		out = append(out, s)
	}
	return out
}

// Status is the server status of a device: the server's own status when it
// runs, ERROR when the last start failed, else STOPPED.
func (m *Manager) Status(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.servers[name]; ok {
		return s.Status()
	}
	if _, ok := m.lastErr[name]; ok {
		return StatusError
	}
	return StatusStopped
}

// LastError returns the last start error of a device ("" if none).
func (m *Manager) LastError(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr[name]
}
