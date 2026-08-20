package broker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/hooks/auth"
	"monstermq.io/edge/internal/mqtt/listeners"

	"monstermq.io/edge/internal/archive"
	mauth "monstermq.io/edge/internal/auth"
	"monstermq.io/edge/internal/bridge/mqttclient"
	"monstermq.io/edge/internal/bridge/winccoa"
	"monstermq.io/edge/internal/bridge/winccua"
	"monstermq.io/edge/internal/config"
	gql "monstermq.io/edge/internal/graphql"
	"monstermq.io/edge/internal/graphql/resolvers"
	"monstermq.io/edge/internal/hostinfo"
	"monstermq.io/edge/internal/hmi"
	mlog "monstermq.io/edge/internal/log"
	"monstermq.io/edge/internal/mcp"
	"monstermq.io/edge/internal/metrics"
	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/redfish"
	"monstermq.io/edge/internal/stores"
	storememory "monstermq.io/edge/internal/stores/memory"
	storemongo "monstermq.io/edge/internal/stores/mongodb"
	storepg "monstermq.io/edge/internal/stores/postgres"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
	"monstermq.io/edge/internal/topic"
)

// Server is the top-level lifecycle holder for the edge broker.
type Server struct {
	cfg         *config.Config
	logger      *slog.Logger
	mochi       *mqtt.Server
	storage     *stores.Storage
	bus         *pubsub.Bus
	subs        *topic.SubscriptionIndex
	archives    *archive.Manager
	authCache   *mauth.Cache
	collector   *metrics.Collector
	bridges     *mqttclient.Manager
	winCCUa     *winccua.Manager
	winCCOa     *winccoa.Manager
	gqlSrv      *gql.Server
	mcpSrv      *mcp.Server
	redfishMgr  *redfish.Manager
	hostMonitor *hostinfo.Collector
	metricsCtx  context.Context
	metricsStop context.CancelFunc
}

func New(cfg *config.Config, logger *slog.Logger, logBus *mlog.Bus) (*Server, error) {
	ctx := context.Background()

	// 1. Storage — picks the backend based on DefaultStoreType.
	// SQLITE additionally exposes a *DB handle so the archive manager can
	// create per-group last-value/archive tables on the same connection.
	var (
		storage  *stores.Storage
		sqliteDB *storesqlite.DB
		pgDB     *storepg.DB
		mongoDB  *storemongo.DB
		err      error
	)
	switch cfg.DefaultStoreType {
	case config.StoreSQLite, "":
		storage, sqliteDB, err = storesqlite.Build(ctx, cfg)
	case config.StorePostgres:
		storage, pgDB, err = storepg.Build(ctx, cfg)
	case config.StoreMongoDB:
		storage, mongoDB, err = storemongo.Build(ctx, cfg)
	default:
		return nil, fmt.Errorf("unsupported DefaultStoreType %q", cfg.DefaultStoreType)
	}
	if err != nil {
		return nil, fmt.Errorf("storage init: %w", err)
	}
	if err := configureVolatileStores(ctx, cfg, storage); err != nil {
		_ = storage.Close()
		return nil, err
	}
	if err := ensureDefaultAdmin(ctx, cfg, storage, logger); err != nil {
		return nil, fmt.Errorf("default admin init: %w", err)
	}
	if err := configureMetricsStore(cfg, storage); err != nil {
		_ = storage.Close()
		return nil, err
	}

	if storage.Queue != nil && cfg.QueueStore() != config.StoreMemory {
		batchSize := cfg.GetQueueBatchSize()
		flushInterval := time.Duration(cfg.GetQueueFlushIntervalMs()) * time.Millisecond
		batchedQueue, err := stores.NewBatchingQueueStore(ctx, storage.Queue, batchSize, flushInterval)
		if err != nil {
			_ = storage.Close()
			return nil, fmt.Errorf("queue batching init: %w", err)
		}
		storage.Queue = batchedQueue
		prependStorageCloser(storage, storage.Queue.Close)
	}

	// 2. Auth cache
	authCache := mauth.NewCache(storage.Users, cfg.UserManagement.AnonymousEnabled || !cfg.UserManagement.Enabled, cfg.UserManagement.AclCheckOnSub())
	if err := authCache.Refresh(ctx); err != nil {
		logger.Warn("user cache refresh failed", "err", err)
	}
	authCache.StartRefresher(context.Background(), 30*time.Second)

	// 3. Pub/sub bus + subscription index + archive manager
	bus := pubsub.NewBus()
	subs := topic.NewSubscriptionIndex()
	if err := hydrateSubscriptionIndex(ctx, subs, storage); err != nil {
		logger.Warn("subscription index hydrate failed", "err", err)
	}
	archives := archive.NewManager(cfg, storage, sqliteDB, pgDB, mongoDB, logger)
	if err := archives.Load(ctx); err != nil {
		logger.Warn("archive groups load failed", "err", err)
	}

	// 4. Mochi broker
	server := mqtt.New(&mqtt.Options{InlineClient: true, Logger: logger})

	if cfg.UserManagement.Enabled {
		if err := server.AddHook(NewAuthHook(authCache), nil); err != nil {
			return nil, fmt.Errorf("add monstermq auth hook: %w", err)
		}
	} else {
		if err := server.AddHook(new(auth.AllowHook), nil); err != nil {
			return nil, fmt.Errorf("add allow-all hook: %w", err)
		}
	}

	// Metrics collector (counts hooked into the storage hook)
	interval := time.Duration(cfg.Metrics.CollectionIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = time.Second
	}
	var collector *metrics.Collector
	if cfg.Metrics.Enabled {
		collector = metrics.New(storage.Metrics, cfg.NodeID, interval, logger)
	}

	var counter MetricsCounter // nil interface, not interface-holding-nil-pointer
	if collector != nil {
		counter = collector
	}
	retainedInMemory := cfg.RetainedStore() == config.StoreMemory
	storageHook := NewStorageHook(storage, bus, subs, archives, cfg.NodeID, logger, counter, retainedInMemory)
	if err := server.AddHook(storageHook, nil); err != nil {
		return nil, fmt.Errorf("add storage hook: %w", err)
	}

	if cfg.QueuedMessagesEnabled {
		logger.Info("queued messages: enabled", "store", cfg.QueueStore(), "max", cfg.GetMaxQueueMessages())
		if err := server.AddHook(NewQueueHook(storage, subs, server, logger, cfg.GetMaxQueueMessages()), nil); err != nil {
			return nil, fmt.Errorf("add queue hook: %w", err)
		}
	} else {
		logger.Info("queued messages: disabled (relying on mochi-mqtt in-memory inflight)")
	}

	// 5. Restore retained messages from storage into mochi's in-memory retained map.
	// Skipped when RetainedStoreType is MEMORY: nothing is persisted, so there's
	// nothing to restore — mochi's own in-memory map is the source of truth.
	// Also skipped when RetainedStoreType is a DB store: they are loaded on-demand
	// via OnSelectRetainedMessages hook.
	if retainedInMemory {
		logger.Info("retained messages: in-memory mode (no DB persistence)")
	} else {
		logger.Info("retained messages: database-backed on-demand mode (bypassing pre-load)")
	}

	// 6. Listeners
	if cfg.TCP.Enabled {
		l := listeners.NewTCP(listeners.Config{ID: "tcp", Address: fmt.Sprintf("%s:%d", cfg.TCP.ListenAddress(), cfg.TCP.Port)})
		if err := server.AddListener(l); err != nil {
			return nil, fmt.Errorf("add tcp listener: %w", err)
		}
		logger.Info("mqtt listener", "type", "tcp", "port", cfg.TCP.Port)
	}
	if cfg.WS.Enabled {
		l := listeners.NewWebsocket(listeners.Config{ID: "ws", Address: fmt.Sprintf("%s:%d", cfg.WS.ListenAddress(), cfg.WS.Port)})
		if err := server.AddListener(l); err != nil {
			return nil, fmt.Errorf("add ws listener: %w", err)
		}
		logger.Info("mqtt listener", "type", "ws", "port", cfg.WS.Port)
	}
	if cfg.TCPS.Enabled {
		tlsCfg, err := loadTLS(cfg.TCPS.KeyStorePath, cfg.TCPS.KeyStorePassword)
		if err != nil {
			return nil, fmt.Errorf("tls config: %w", err)
		}
		l := listeners.NewTCP(listeners.Config{ID: "tcps", Address: fmt.Sprintf("%s:%d", cfg.TCPS.ListenAddress(), cfg.TCPS.Port), TLSConfig: tlsCfg})
		if err := server.AddListener(l); err != nil {
			return nil, fmt.Errorf("add tcps listener: %w", err)
		}
		logger.Info("mqtt listener", "type", "tcps", "port", cfg.TCPS.Port)
	}
	if cfg.WSS.Enabled {
		tlsCfg, err := loadTLS(cfg.WSS.KeyStorePath, cfg.WSS.KeyStorePassword)
		if err != nil {
			return nil, fmt.Errorf("wss tls config: %w", err)
		}
		l := listeners.NewWebsocket(listeners.Config{ID: "wss", Address: fmt.Sprintf("%s:%d", cfg.WSS.ListenAddress(), cfg.WSS.Port), TLSConfig: tlsCfg})
		if err := server.AddListener(l); err != nil {
			return nil, fmt.Errorf("add wss listener: %w", err)
		}
		logger.Info("mqtt listener", "type", "wss", "port", cfg.WSS.Port)
	}

	// 7. MQTT bridge manager
	publishFn := func(topic string, payload []byte, retain bool, qos byte) error {
		return server.Publish(topic, payload, retain, qos)
	}
	var bridges *mqttclient.Manager
	if cfg.Features.MqttClient {
		bridges = mqttclient.NewManager(storage.DeviceConfig, publishFn, &mqttclient.BusAdapter{Bus: bus}, cfg.NodeID, logger)
		if collector != nil {
			bridges.SetCounters(collector.IncBridgeIn, collector.IncBridgeOut)
		}
	}

	// 7b. WinCC Unified bridge manager (deploys one connector per device,
	// either GraphQL/WebSocket or local Open Pipe IPC depending on config).
	var winCCUa *winccua.Manager
	if cfg.Features.WinCCUa {
		winCCUa = winccua.NewManager(storage.DeviceConfig, publishFn, cfg.NodeID, logger)
	}

	var winCCOa *winccoa.Manager
	if cfg.Features.WinCCOa {
		winCCOa = winccoa.NewManager(storage.DeviceConfig, publishFn, cfg.NodeID, logger)
	}

	// 7c. Host Monitoring
	var hostMonitor *hostinfo.Collector
	if cfg.HostMonitoring.Enabled {
		hostMonitor = hostinfo.NewCollector(cfg.NodeID, cfg.HostMonitoring.IntervalSeconds, cfg.HostMonitoring.BaseTopic, cfg.HostMonitoring.QoS, publishFn, logger)
	}

	// 7d. HMI Manager
	var hmiMgr *hmi.Manager
	if cfg.HMI.Enabled || cfg.Features.Hmi {
		if cfg.HMI.Path == "" {
			logger.Warn("HMI is enabled, but HMI.Path is not specified in configuration. HMI server will not be started.")
		} else {
			hmiMgr = hmi.NewManager(cfg, storage.DeviceConfig)
		}
	}

	// 7e. Redfish Manager
	var redfishMgr *redfish.Manager
	var lastVal stores.MessageStore
	if defGroup := archives.Get("Default"); defGroup != nil {
		lastVal = defGroup.LastValue()
	}
	if lastVal == nil && storage.Retained != nil {
		lastVal = storage.Retained
	}
	if cfg.Redfish.Enabled || cfg.Features.Redfish {
		redfishMgr = redfish.NewManager(cfg, storage.DeviceConfig, bus, lastVal, publishFn, cfg.NodeID, logger)
	}

	// 8. GraphQL server (HTTP + WebSocket)
	var gqlSrv *gql.Server
	if cfg.GraphQL.Enabled {
		resolver := resolvers.New(cfg, storage, bus, archives, bridges, winCCUa, winCCOa, authCache, collector, logBus, logger, server, publishFn, hmiMgr, redfishMgr)
		gqlSrv = gql.NewServer(cfg, resolver, hmiMgr, redfishMgr, logger)
	}

	// 9. MCP server (Streamable HTTP / SSE)
	var mcpSrv *mcp.Server
	if cfg.MCP.Enabled {
		mcpSrv = mcp.NewServer(cfg, storage, archives, authCache, publishFn, logger)
	}

	return &Server{
		cfg: cfg, logger: logger, mochi: server,
		storage: storage, bus: bus, subs: subs, archives: archives, authCache: authCache,
		collector: collector, bridges: bridges, winCCUa: winCCUa, winCCOa: winCCOa, gqlSrv: gqlSrv,
		mcpSrv: mcpSrv, redfishMgr: redfishMgr, hostMonitor: hostMonitor,
	}, nil
}

func configureVolatileStores(ctx context.Context, cfg *config.Config, storage *stores.Storage) error {
	if cfg.RetainedStore() == config.StoreMemory {
		storage.Retained = storememory.NewMessageStore("retainedmessages")
	}
	if storage.Backend == config.StoreSQLite {
		return nil
	}
	if cfg.SessionStore() == config.StoreMemory {
		db, err := storesqlite.OpenMemory("monstermq-sessions-" + cfg.NodeID)
		if err != nil {
			return err
		}
		sessions := storesqlite.NewSessionStore(db)
		if err := sessions.EnsureTable(ctx); err != nil {
			_ = db.Close()
			return err
		}
		storage.Sessions = sessions
		storage.Subscriptions = sessions
		appendStorageCloser(storage, db.Close)
	}
	if cfg.QueueStore() == config.StoreMemory {
		storage.Queue = storememory.NewQueueStore(30 * time.Second)
	}
	return nil
}

func appendStorageCloser(storage *stores.Storage, closeFn func() error) {
	prev := storage.Closer
	storage.Closer = func() error {
		var first error
		if prev != nil {
			first = prev()
		}
		if err := closeFn(); err != nil && first == nil {
			first = err
		}
		return first
	}
}

func prependStorageCloser(storage *stores.Storage, closeFn func() error) {
	prev := storage.Closer
	storage.Closer = func() error {
		var first error
		if err := closeFn(); err != nil {
			first = err
		}
		if prev != nil {
			if err := prev(); err != nil && first == nil {
				first = err
			}
		}
		return first
	}
}

func configureMetricsStore(cfg *config.Config, storage *stores.Storage) error {
	switch cfg.MetricsStore() {
	case config.StoreNone:
		storage.Metrics = nil
	case config.StoreMemory:
		storage.Metrics = storememory.NewMetricsStore(cfg.Metrics.MaxHistoryRows)
	case storage.Backend:
		return nil
	default:
		return fmt.Errorf("Metrics.StoreType %q does not match DefaultStoreType %q; only MEMORY and NONE can be selected independently", cfg.MetricsStore(), storage.Backend)
	}
	return nil
}

// hydrateSubscriptionIndex loads every persisted subscription into the
// in-memory dual-index so the queue hook can resolve subscribers without
// scanning the storage layer per published message.
func hydrateSubscriptionIndex(ctx context.Context, subs *topic.SubscriptionIndex, storage *stores.Storage) error {
	return storage.Subscriptions.IterateSubscriptions(ctx, func(s stores.MqttSubscription) bool {
		subs.Subscribe(s.ClientID, s.TopicFilter, s.QoS)
		return true
	})
}

func (s *Server) Serve() error {
	if s.collector != nil {
		s.metricsCtx, s.metricsStop = context.WithCancel(context.Background())
		s.collector.Start(s.metricsCtx, func() (sessions, subs int, queued int64) {
			ctx := context.Background()
			_ = s.storage.Sessions.IterateSessions(ctx, func(stores.SessionInfo) bool { sessions++; return true })
			_ = s.storage.Subscriptions.IterateSubscriptions(ctx, func(stores.MqttSubscription) bool { subs++; return true })
			queued, _ = s.storage.Queue.CountAll(ctx)
			return
		})
		if s.archives != nil {
			s.archives.StartMetrics(s.metricsCtx, s.storage.Metrics, s.collector.Interval())
		}
		if s.bridges != nil {
			s.bridges.StartMetrics(s.metricsCtx, s.storage.Metrics, s.collector.Interval())
		}
		if s.winCCOa != nil {
			s.winCCOa.StartMetrics(s.metricsCtx, s.storage.Metrics, s.collector.Interval())
		}
	}
	if s.archives != nil {
		s.archives.RunRetention(context.Background())
	}
	if s.bridges != nil {
		if err := s.bridges.Start(context.Background()); err != nil {
			s.logger.Warn("bridges start error", "err", err)
		}
	}
	if s.winCCUa != nil {
		if err := s.winCCUa.Start(context.Background()); err != nil {
			s.logger.Warn("winccua start error", "err", err)
		}
	}
	if s.winCCOa != nil {
		if err := s.winCCOa.Start(context.Background()); err != nil {
			s.logger.Warn("winccoa start error", "err", err)
		}
	}
	if s.hostMonitor != nil {
		s.hostMonitor.Start(context.Background())
	}
	if s.redfishMgr != nil {
		if err := s.redfishMgr.Start(context.Background()); err != nil {
			s.logger.Warn("redfish start error", "err", err)
		}
	}
	if s.gqlSrv != nil {
		go func() {
			if err := s.gqlSrv.Start(); err != nil {
				s.logger.Error("graphql server error", "err", err)
			}
		}()
	}
	if s.mcpSrv != nil {
		go func() {
			if err := s.mcpSrv.Start(); err != nil {
				s.logger.Error("mcp server error", "err", err)
			}
		}()
	}
	return s.mochi.Serve()
}

func (s *Server) Close() error {
	if s.bridges != nil {
		s.bridges.Stop()
	}
	if s.winCCUa != nil {
		s.winCCUa.Stop()
	}
	if s.winCCOa != nil {
		s.winCCOa.Stop()
	}
	if s.hostMonitor != nil {
		s.hostMonitor.Stop()
	}
	if s.redfishMgr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.redfishMgr.Stop(ctx)
	}
	if s.metricsStop != nil {
		s.metricsStop()
	}
	if s.collector != nil {
		s.collector.Stop()
	}
	if s.gqlSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.gqlSrv.Stop(ctx)
	}
	if s.mcpSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.mcpSrv.Stop(ctx)
	}
	if s.archives != nil {
		s.archives.Stop()
	}
	if err := s.mochi.Close(); err != nil {
		return err
	}
	if s.storage != nil {
		return s.storage.Close()
	}
	return nil
}

// Storage exposes the store stack for GraphQL resolvers (M6+).
func (s *Server) Storage() *stores.Storage                { return s.storage }
func (s *Server) Bus() *pubsub.Bus                        { return s.bus }
func (s *Server) Subscriptions() *topic.SubscriptionIndex { return s.subs }
func (s *Server) Archives() *archive.Manager              { return s.archives }
func (s *Server) Mochi() *mqtt.Server                     { return s.mochi }
