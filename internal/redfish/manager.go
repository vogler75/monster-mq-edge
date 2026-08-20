package redfish

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/stores"
)

// Manager coordinates the lifecycle of the Redfish subsystem.
type Manager struct {
	cfg         *config.Config
	devStore    stores.DeviceConfigStore
	bus         *pubsub.Bus
	lastVal     stores.MessageStore
	publishFn   func(topic string, payload []byte, retain bool, qos byte) error
	logger      *slog.Logger
	nodeID      string

	subscriber  *Subscriber
	server      *Server
	httpServer  *http.Server
	mu          sync.RWMutex
	gateways    map[string]*GatewayConfig
	running     bool
}

// NewManager creates a Redfish Manager instance.
func NewManager(
	cfg *config.Config,
	devStore stores.DeviceConfigStore,
	bus *pubsub.Bus,
	lastVal stores.MessageStore,
	publishFn func(topic string, payload []byte, retain bool, qos byte) error,
	nodeID string,
	logger *slog.Logger,
) *Manager {
	server := NewServer(
		nodeID,
		cfg.Redfish.DefaultChassisId,
		cfg.Redfish.DefaultSystemId,
		cfg.Redfish.DefaultManagerId,
		lastVal,
		logger,
	)

	subscriber := NewSubscriber(bus, lastVal, publishFn, logger)

	return &Manager{
		cfg:        cfg,
		devStore:   devStore,
		bus:        bus,
		lastVal:    lastVal,
		publishFn:  publishFn,
		nodeID:     nodeID,
		logger:     logger,
		server:     server,
		subscriber: subscriber,
		gateways:   make(map[string]*GatewayConfig),
	}
}

// Handler returns the HTTP handler for mounting on the main broker HTTP listener.
func (m *Manager) Handler() http.Handler {
	return m.server.Handler()
}

// Start loads dynamic configurations from DeviceConfigStore and starts subscribers/listeners.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return nil
	}

	if err := m.loadGateways(ctx); err != nil && m.logger != nil {
		m.logger.Warn("failed to load initial redfish device configs", "err", err)
	}

	m.subscriber.Start(ctx)
	m.running = true

	// If dedicated standalone port is configured (> 0 and != GraphQL port)
	if m.cfg.Redfish.Enabled && m.cfg.Redfish.Port > 0 && m.cfg.Redfish.Port != m.cfg.GraphQL.Port {
		addr := fmt.Sprintf(":%d", m.cfg.Redfish.Port)
		m.httpServer = &http.Server{
			Addr:              addr,
			Handler:           m.server.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if m.logger != nil {
				m.logger.Info("redfish standalone rest listener starting", "port", m.cfg.Redfish.Port)
			}
			if err := m.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed && m.logger != nil {
				m.logger.Error("redfish standalone listener failed", "err", err)
			}
		}()
	}

	return nil
}

// Stop shuts down the subscriber and standalone listener.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.running {
		return nil
	}

	m.subscriber.Stop()
	m.running = false

	if m.httpServer != nil {
		return m.httpServer.Shutdown(ctx)
	}
	return nil
}

// Reload refreshes configurations from the DeviceConfigStore.
func (m *Manager) Reload(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadGateways(ctx)
}

func (m *Manager) loadGateways(ctx context.Context) error {
	if m.devStore == nil {
		return nil
	}

	devs, err := m.devStore.GetByType(ctx, "Redfish")
	if err != nil {
		return err
	}

	gws := make(map[string]*GatewayConfig)
	for _, d := range devs {
		if !d.Enabled {
			continue
		}
		var cfg GatewayConfig
		if err := json.Unmarshal([]byte(d.Config), &cfg); err != nil {
			if m.logger != nil {
				m.logger.Warn("invalid redfish gateway config JSON", "name", d.Name, "err", err)
			}
			continue
		}
		gws[d.Name] = &cfg
	}

	m.gateways = gws
	m.subscriber.SetGateways(gws)
	m.server.SetGateways(gws)

	if m.logger != nil {
		m.logger.Info("redfish gateways reloaded", "activeCount", len(gws))
	}
	return nil
}

// GetLiveSensors returns the current status of all known sensors for a chassis (or all chassis).
func (m *Manager) GetLiveSensors(ctx context.Context, chassisID *string) []NormalizedSensorRecord {
	if chassisID != nil && *chassisID != "" {
		return m.server.getSensorsForChassis(ctx, *chassisID)
	}
	return m.server.getAllSensorRecords(ctx)
}
