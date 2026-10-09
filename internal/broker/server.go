package broker

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	mqtt "monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/hooks/auth"
	"monstermq.io/edge/internal/mqtt/listeners"

	"monstermq.io/edge/internal/archive"
	mauth "monstermq.io/edge/internal/auth"
	"monstermq.io/edge/internal/bridge/mqttclient"
	"monstermq.io/edge/internal/bridge/rtspcamera"
	"monstermq.io/edge/internal/bridge/winccoa"
	"monstermq.io/edge/internal/bridge/winccua"
	"monstermq.io/edge/internal/config"
	gql "monstermq.io/edge/internal/graphql"
	"monstermq.io/edge/internal/graphql/resolvers"
	"monstermq.io/edge/internal/hmi"
	"monstermq.io/edge/internal/hostinfo"
	mlog "monstermq.io/edge/internal/log"
	"monstermq.io/edge/internal/mcp"
	"monstermq.io/edge/internal/metrics"
	"monstermq.io/edge/internal/oahost"
	"monstermq.io/edge/internal/peerlink"
	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/redfish"
	"monstermq.io/edge/internal/restapi"
	"monstermq.io/edge/internal/scripting"
	"monstermq.io/edge/internal/stores"
	storememory "monstermq.io/edge/internal/stores/memory"
	storemongo "monstermq.io/edge/internal/stores/mongodb"
	"monstermq.io/edge/internal/stores/oastore"
	storepg "monstermq.io/edge/internal/stores/postgres"
	storesqlite "monstermq.io/edge/internal/stores/sqlite"
	"monstermq.io/edge/internal/topic"
	"monstermq.io/edge/internal/version"
	"monstermq.io/edge/internal/winccoanative"
)

// Server is the top-level lifecycle holder for the edge broker.
type Server struct {
	cfg         *config.Config
	logger      *slog.Logger
	mqtt        *mqtt.Server
	storage     *stores.Storage
	bus         *pubsub.Bus
	subs        *topic.SubscriptionIndex
	archives    *archive.Manager
	authCache   *mauth.Cache
	collector   *metrics.Collector
	bridges     *mqttclient.Manager
	winCCUa     *winccua.Manager
	winCCOa     *winccoa.Manager
	rtspCameras *rtspcamera.Manager
	scripts     *scripting.Manager
	gqlSrv      *gql.Server
	mcpSrv      *mcp.Server
	redfishMgr  *redfish.Manager
	hostMonitor *hostinfo.Collector
	hmiSync     *hmi.SyncService
	storageHook *StorageHook
	native      *winccoanative.Service
	peer        *peerlink.Manager
	refreshStop context.CancelFunc
	// peerStatus wakes the native status refresher after a PeerLink state
	// change; nil unless PeerLink and the native namespace are both on.
	peerStatus     chan struct{}
	peerStatusStop context.CancelFunc
	peerStatusDone chan struct{}
	// publishersStopped/gqlStopped keep the PeerLink shutdown order from
	// stopping a subsystem twice.
	publishersStopped bool
	gqlStopped        bool
	// stopMu guards the stop functions set by Serve and read by Close,
	// which run on different goroutines.
	stopMu       sync.Mutex
	retainedStop context.CancelFunc
	metricsCtx   context.Context
	metricsStop  context.CancelFunc
}

// Options lets an embedding host assemble the broker. The zero value gives
// the standalone behavior.
type Options struct {
	// Storage, when set, is used instead of the DefaultStoreType factory.
	// The broker closes it on Close unless KeepStorageOpen is set.
	Storage         *stores.Storage
	KeepStorageOpen bool
	// ConfigureStorage runs after the factory (or Storage) and the volatile
	// store overrides, before any store is read. It may replace individual
	// stores, e.g. with WinCC OA datapoint stores.
	ConfigureStorage func(ctx context.Context, s *stores.Storage) error
	// OA is the embedding host client. Native WinCC OA features are only
	// active when it is set and cfg.WinCCOaNative.Enabled is true.
	OA *oahost.Client
	// NativeReconcile overrides the native interest reconcile interval.
	NativeReconcile time.Duration
}

func New(cfg *config.Config, logger *slog.Logger, logBus *mlog.Bus) (*Server, error) {
	return NewWithOptions(cfg, logger, logBus, Options{})
}

// NewWithOptions builds the broker. On error every resource acquired so
// far (storage, listeners, background refreshers) is released, so a failed
// start inside an embedding host leaves nothing bound or running.
func NewWithOptions(cfg *config.Config, logger *slog.Logger, logBus *mlog.Bus, opts Options) (*Server, error) {
	var undo []func()
	srv, err := build(cfg, logger, logBus, opts, &undo)
	if err != nil {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return nil, err
	}
	return srv, nil
}

func build(cfg *config.Config, logger *slog.Logger, logBus *mlog.Bus, opts Options, undo *[]func()) (*Server, error) {
	ctx := context.Background()

	// Process-wide soft limit; set here so the standalone binary and the
	// embedded manager (which inherits its environment) both honour it.
	if limit := cfg.Runtime.MemoryLimitBytes(); limit > 0 {
		debug.SetMemoryLimit(limit)
		logger.Info("runtime memory limit", "mb", cfg.Runtime.MemoryLimitMB)
	}

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
	switch {
	case opts.Storage != nil:
		storage = opts.Storage
		if opts.KeepStorageOpen {
			storage.Closer = nil
		}
	case cfg.DefaultStoreType == config.StoreSQLite, cfg.DefaultStoreType == "":
		storage, sqliteDB, err = storesqlite.Build(ctx, cfg)
	case cfg.DefaultStoreType == config.StorePostgres:
		storage, pgDB, err = storepg.Build(ctx, cfg)
	case cfg.DefaultStoreType == config.StoreMongoDB:
		storage, mongoDB, err = storemongo.Build(ctx, cfg)
	case cfg.DefaultStoreType == config.StoreWinCCOA:
		// Configs, sessions, retained messages and users become WinCC OA
		// datapoints below; queue and metrics stay in memory. No SQLite
		// handle is exposed, so no archive group writes to a file here.
		if storage, err = storesqlite.BuildMemory(ctx, cfg); err == nil {
			storage.Backend = config.StoreWinCCOA
		}
	default:
		return nil, fmt.Errorf("unsupported DefaultStoreType %q", cfg.DefaultStoreType)
	}
	if err != nil {
		return nil, fmt.Errorf("storage init: %w", err)
	}
	if sqliteDB != nil {
		path, absErr := filepath.Abs(cfg.SQLite.Path)
		if absErr != nil {
			path = cfg.SQLite.Path
		}
		logger.Info("sqlite database", "path", path)
	}
	if err := configureVolatileStores(ctx, cfg, storage); err != nil {
		_ = storage.Close()
		return nil, err
	}
	nativeOn := opts.OA != nil && cfg.WinCCOaNative.Enabled
	names := winccoanative.Names{
		Root:       cfg.WinCCOaNative.TopicRoot,
		Tags:       cfg.WinCCOaNative.TagsName,
		Types:      cfg.WinCCOaNative.TypesName,
		Systems:    cfg.WinCCOaNative.SystemsName,
		Topics:     cfg.WinCCOaNative.TopicsName,
		NoShortcut: !cfg.WinCCOaNative.Shortcut(),
	}.WithDefaults()
	if err := names.Validate(); err != nil {
		_ = storage.Close()
		return nil, fmt.Errorf("WinCCOaNative topic names: %w", err)
	}
	if cfg.UsesWinCCOaStores() && !nativeOn {
		_ = storage.Close()
		return nil, fmt.Errorf("store type WINCCOA (Config/Session/RetainedStoreType) needs the WinCC OA manager (WCCOAmmq) with WinCCOaNative enabled")
	}
	// oaStoreSystem is the local WinCC OA system name when the retained
	// store is WINCCOA, for PeerLink's oaRetained decision; it does not
	// depend on the native namespace being on.
	var oaStoreSystem string
	if nativeOn && cfg.UsesWinCCOaStores() {
		var err error
		if oaStoreSystem, err = useOAStores(ctx, cfg, storage, oahost.API{C: opts.OA}, names.Root, logger); err != nil {
			_ = storage.Close()
			return nil, err
		}
	}
	if opts.ConfigureStorage != nil {
		if err := opts.ConfigureStorage(ctx, storage); err != nil {
			_ = storage.Close()
			return nil, fmt.Errorf("storage configure: %w", err)
		}
	}
	if err := ensureDefaultAdmin(ctx, cfg, storage, logger); err != nil {
		return nil, fmt.Errorf("default admin init: %w", err)
	}
	if err := configureMetricsStore(cfg, storage); err != nil {
		_ = storage.Close()
		return nil, err
	}
	*undo = append(*undo, func() { _ = storage.Close() })

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
	refreshCtx, refreshStop := context.WithCancel(context.Background())
	authCache.StartRefresher(refreshCtx, 30*time.Second)
	*undo = append(*undo, refreshStop)

	// 3. Pub/sub bus + subscription index + archive manager
	bus := pubsub.NewBus()
	subs := topic.NewSubscriptionIndex()
	// The inline client is not persisted; drop records older versions
	// stored for it.
	if err := storage.Sessions.DelClient(ctx, mqtt.InlineClientId); err != nil {
		logger.Warn("inline session cleanup failed", "err", err)
	}
	if err := hydrateSubscriptionIndex(ctx, subs, storage); err != nil {
		logger.Warn("subscription index hydrate failed", "err", err)
	}
	archives := archive.NewManager(cfg, storage, sqliteDB, pgDB, mongoDB, logger)
	if err := archives.Load(ctx); err != nil {
		logger.Warn("archive groups load failed", "err", err)
	}

	// 4. Native MQTT server engine
	caps := mqtt.NewDefaultServerCapabilities()
	if cfg.MaxMessageSize > 0 {
		caps.MaximumPacketSize = uint32(cfg.MaxMessageSize)
	}
	server := mqtt.New(&mqtt.Options{
		InlineClient: true,
		Logger:       logger,
		Capabilities: caps,
	})
	server.Info.Version = version.Version
	*undo = append(*undo, func() { _ = server.Close() })

	var authHook *AuthHook
	if cfg.UserManagement.Enabled {
		authHook = NewAuthHook(
			authCache,
			storage.Users,
			cfg.EffectiveUseIdentityAsUsername(),
			cfg.EffectiveAutoCreateUser(),
			cfg.UserManagement.AllowAnonymousLocalhost,
			logger,
		)
		authHook.native = func() winccoanative.Names { return names }
		if err := server.AddHook(authHook, nil); err != nil {
			return nil, fmt.Errorf("add monstermq auth hook: %w", err)
		}
	} else {
		if err := server.AddHook(new(auth.AllowHook), nil); err != nil {
			return nil, fmt.Errorf("add allow-all hook: %w", err)
		}
	}

	if !cfg.AllowRootWildcard() {
		if err := server.AddHook(new(rootWildcardHook), nil); err != nil {
			return nil, fmt.Errorf("add root wildcard hook: %w", err)
		}
	}

	// PeerLink (plan-peerlink 6.2). Resolved before the native service so
	// its status object can be wired; the manager is built after the stores
	// and the engine exist.
	var peerSetup *config.PeerLinkSetup
	var pl *peerlink.Manager
	if cfg.PeerLink.Enabled {
		setup, err := cfg.ResolvePeerLink()
		if err != nil {
			return nil, err
		}
		for _, msg := range setup.Infos {
			logger.Info(msg)
		}
		for _, msg := range setup.Warnings {
			logger.Warn(msg)
		}
		peerSetup = setup
	}

	// Native WinCC OA namespace. Added before the storage hook so accepted
	// commands are consumed (not archived or delivered) and rejected
	// filters never reach persistence.
	var native *winccoanative.Service
	if nativeOn && cfg.WinCCOaNative.Namespace {
		native = winccoanative.NewService(oahost.API{C: opts.OA}, server, winccoanative.Options{
			Names:             names,
			NodeID:            cfg.NodeID,
			NoSource:          cfg.WinCCOaNative.EchoPolicy == config.WinCCOaEchoNoSource,
			ReconcileInterval: opts.NativeReconcile,
			AllowRootWildcard: cfg.AllowRootWildcard(),
			TopicDPNames:      cfg.WinCCOaNative.TopicDpNames == config.WinCCOaTopicDpName,
			PeerLinkStatus:    peerLinkNativeStatus(peerSetup != nil, &pl),
			RetainedStatuses: func(filter string) map[string][]byte {
				out := map[string][]byte{}
				if cfg.RetainedStore() == config.StoreMemory {
					for _, pk := range server.Topics.Messages(filter) {
						out[pk.TopicName] = pk.Payload
					}
					return out
				}
				_ = storage.Retained.FindMatchingMessages(context.Background(), filter, func(m stores.BrokerMessage) bool {
					out[m.TopicName] = m.Payload
					return true
				})
				return out
			},
			SessionExists: func(clientID string) bool {
				if _, ok := server.Clients.Get(clientID); ok {
					return true
				}
				present, err := storage.Sessions.IsPresent(context.Background(), clientID)
				return err != nil || present
			},
		}, logger)
		if authHook != nil {
			// The service adds the local system (known after its start,
			// before any listener), which maps shortcut topics for ACLs.
			authHook.native = native.Names
		}
		if err := server.AddHook(NewWinCCOaNativeHook(native, server, logger), nil); err != nil {
			return nil, fmt.Errorf("add winccoa native hook: %w", err)
		}
	}

	if opts.OA != nil {
		server.AddSysTopics(winccoaSysTopics(opts.OA, native))
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
	storageHook := NewStorageHook(storage, bus, subs, archives, cfg.NodeID, logger, counter, retainedInMemory, server)
	if nativeOn && cfg.WinCCOaNative.Namespace {
		storageHook.replicated = func(t string) bool { return names.Classify(t) == winccoanative.KindTopics }
	}
	var queueOpts []QueueHookOption
	var peerStatus chan struct{}
	if peerSetup != nil {
		policy := NewPeerPolicy(cfg.PeerLink.Receive)
		storageHook.SetPeerPolicy(policy)
		queueOpts = append(queueOpts, WithPeerLink(policy, cfg.NodeID, peerSetup.NodeID))
		deps := peerlink.Deps{
			Config:           cfg.PeerLink,
			Setup:            peerSetup,
			Server:           server,
			MaxMessageSize:   cfg.MaxMessageSize,
			HMISyncBaseTopic: cfg.HMI.SyncBaseTopic,
			RetainedClass:    peerLinkRetainedClass(cfg.RetainedStore()),
			NamespaceRoot:    peerLinkNamespaceRoot(nativeOn && cfg.WinCCOaNative.Namespace, names.Root),
			Retained: &peerRetained{
				engine: server,
				store:  storage.Retained,
				hook:   storageHook,
				memory: retainedInMemory,
			},
			Logger: logger,
		}
		if oaStoreSystem != "" {
			deps.OASystem = func() string { return oaStoreSystem }
		}
		if native != nil {
			if deps.OASystem == nil {
				deps.OASystem = native.LocalSystem
			}
			peerStatus = make(chan struct{}, 1)
			deps.OnStateChange = func() {
				select {
				case peerStatus <- struct{}{}:
				default:
				}
			}
		}
		if collector != nil {
			deps.Metrics = collector
		}
		m, err := peerlink.New(deps)
		if err != nil {
			return nil, err
		}
		*undo = append(*undo, func() { _ = m.Close() })
		pl = m
		storageHook.SetRetainedViaOA(pl.RetainedViaOA)
		// Local interest that is not an MQTT session (plan-peerlink-interest-routing 4.1). The
		// HOT/COLD bridge provider (C6) is wired with the bridge manager below.
		if sink := pl.InterestSink(); sink != nil {
			if cfg.PeerLink.Receive.GetBus() {
				bus.SetObserver(sink)
			}
			if cfg.PeerLink.Receive.GetArchive() {
				provide := func() { sink.SetProvided("archive", nil, archives.InterestFilters()) }
				archives.SetOnChange(provide)
				provide()
			}
			if err := restorePeerLinkInterest(ctx, pl, storage); err != nil {
				logger.Warn("peerlink: restore interest of offline sessions failed", "err", err)
			}
		}
		// Before StorageHook and QueueHook: the capture append runs ahead of
		// the potentially blocking queue write in OnPublished.
		if err := server.AddHook(pl.Hook(), nil); err != nil {
			return nil, fmt.Errorf("add peerlink hook: %w", err)
		}
		if addr := pl.Addr(); addr != "" {
			logger.Info("peerlink listener", "address", addr, "nodeId", pl.NodeID())
		}
		warnPeerLinkDevices(ctx, cfg, storage, peerSetup, logger)
	}

	if err := server.AddHook(storageHook, nil); err != nil {
		return nil, fmt.Errorf("add storage hook: %w", err)
	}

	if cfg.QueuedMessagesEnabled {
		logger.Info("queued messages: enabled", "store", cfg.QueueStore(), "max", cfg.GetMaxQueueMessages())
		if err := server.AddHook(NewQueueHook(storage, subs, server, logger, cfg.GetMaxQueueMessages(), queueOpts...), nil); err != nil {
			return nil, fmt.Errorf("add queue hook: %w", err)
		}
	} else {
		logger.Info("queued messages: disabled (relying on in-memory inflight)")
	}

	// 5. Restore retained messages from storage into in-memory retained map.
	// Skipped when RetainedStoreType is MEMORY: nothing is persisted, so there's
	// nothing to restore — the in-memory map is the source of truth.
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
		tlsCfg, err := loadTLS(TLSParams{
			CertPath:           cfg.EffectiveTCPSKeyStorePath(),
			KeyPath:            cfg.EffectiveTCPSKeyPath(),
			Password:           cfg.EffectiveTCPSKeyStorePassword(),
			ClientAuth:         cfg.EffectiveTCPSClientAuth(),
			TrustStorePath:     cfg.EffectiveTCPSTrustStorePath(),
			TrustStorePassword: cfg.EffectiveTCPSTrustStorePassword(),
			TrustStoreType:     cfg.EffectiveTCPSTrustStoreType(),
		})
		if err != nil {
			return nil, fmt.Errorf("tcps tls config: %w", err)
		}
		l := listeners.NewTCP(listeners.Config{ID: "tcps", Address: fmt.Sprintf("%s:%d", cfg.TCPS.ListenAddress(), cfg.TCPS.Port), TLSConfig: tlsCfg})
		if err := server.AddListener(l); err != nil {
			return nil, fmt.Errorf("add tcps listener: %w", err)
		}
		logger.Info("mqtt listener", "type", "tcps", "port", cfg.TCPS.Port, "client_auth", cfg.EffectiveTCPSClientAuth(), "identity_as_username", cfg.EffectiveUseIdentityAsUsername())
	}
	if cfg.WSS.Enabled {
		tlsCfg, err := loadTLS(TLSParams{
			CertPath:   cfg.EffectiveWSSKeyStorePath(),
			KeyPath:    cfg.EffectiveWSSKeyPath(),
			Password:   cfg.EffectiveWSSKeyStorePassword(),
			ClientAuth: config.ClientAuthNone,
		})
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
		busAdapter := &mqttclient.BusAdapter{Bus: bus, BridgeOutbound: cfg.PeerLink.Enabled && cfg.PeerLink.Receive.BridgeOutbound}
		bridges = mqttclient.NewManager(storage.DeviceConfig, publishFn, busAdapter, cfg.NodeID, logger)
		if collector != nil {
			bridges.SetCounters(collector.IncBridgeIn, collector.IncBridgeOut)
		}
		if pl != nil {
			if sink := pl.InterestSink(); sink != nil {
				bridges.SetOnChange(func() {
					filters, err := bridges.StandbyFilters(ctx)
					if err != nil {
						logger.Warn("peerlink: read standby bridge filters failed", "err", err)
						return
					}
					sink.SetProvided("redundancy", filters, nil)
				})
			}
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
		if nativeOn {
			winCCOa.SetReservedRoot(names.Root)
		}
	}

	// 7c. Host Monitoring
	var hostMonitor *hostinfo.Collector
	if cfg.HostMonitoring.Enabled {
		hostMonitor = hostinfo.NewCollector(cfg.NodeID, cfg.HostMonitoring.IntervalSeconds, cfg.HostMonitoring.BaseTopic, cfg.HostMonitoring.QoS, publishFn, logger)
	}

	// 7d. HMI Manager & Sync Service
	var hmiMgr *hmi.Manager
	var hmiSync *hmi.SyncService
	if cfg.HMI.Enabled || cfg.Features.Hmi {
		if cfg.HMI.Path == "" {
			logger.Warn("HMI is enabled, but HMI.Path is not specified in configuration. HMI server will not be started.")
		} else {
			hmiMgr = hmi.NewManager(cfg, storage.DeviceConfig)
			if cfg.HMI.SyncEnabled {
				hmiSync = hmi.NewSyncService(hmiMgr, server, publishFn, cfg.NodeID, cfg.HMI.SyncBaseTopic, logger)
			}
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

	// 7f. RTSP Camera manager
	var rtspCameras *rtspcamera.Manager
	if cfg.Features.RtspCamera {
		rtspCameras = rtspcamera.NewManager(storage.DeviceConfig, publishFn, &rtspcamera.BusAdapter{Bus: bus}, cfg.NodeID, logger)
	}

	// 7g. Scripting manager (Starlark/Python scripts)
	var scripts *scripting.Manager
	if cfg.Features.PythonScripts {
		scripts = scripting.NewManager(storage.DeviceConfig, storage, archives, sqliteDB, pgDB, publishFn, bus, cfg.NodeID, logger)
	}

	// 8. MCP server (Streamable HTTP / SSE mounted at /mcp)
	var mcpSrv *mcp.Server
	var mcpHandler http.Handler
	if cfg.MCP.Enabled || cfg.Features.Mcp {
		mcpSrv = mcp.NewServer(cfg, storage, archives, authCache, publishFn, logger)
		mcpHandler = mcpSrv.Handler()
		logger.Info("mcp server enabled", "path", "/mcp")
	}

	// 9. GraphQL server (HTTP + WebSocket)
	var gqlSrv *gql.Server
	if cfg.GraphQL.Enabled && (cfg.GraphQL.HTTPEnabled() || cfg.GraphQL.TLSEnabled()) {
		resolver := resolvers.New(cfg, storage, bus, archives, bridges, winCCUa, winCCOa, authCache, collector, logBus, logger, server, publishFn, hmiMgr, redfishMgr, rtspCameras, scripts)
		var rest *restapi.Handler
		if cfg.RestApi.Enabled {
			rest = restapi.New(cfg, authCache, storage.Retained, archives, bus, publishFn)
		}
		var tlsConfig *tls.Config
		if cfg.GraphQL.TLSEnabled() {
			certPath := cfg.EffectiveGraphQLCertPath()
			keyPath := cfg.EffectiveGraphQLKeyPath()
			if err := EnsureCertificate(certPath, keyPath, logger); err != nil {
				logger.Error("ensure certificate failed", "err", err)
			}
			var err error
			tlsConfig, err = loadTLS(TLSParams{
				CertPath: certPath,
				KeyPath:  keyPath,
				Password: cfg.EffectiveGraphQLKeyPassword(),
			})
			if err != nil {
				logger.Error("load graphql tls config failed", "err", err)
			}
		}
		gqlSrv = gql.NewServer(cfg, resolver, hmiMgr, redfishMgr, rest, mcpHandler, tlsConfig, logger)
	}

	return &Server{
		cfg: cfg, logger: logger, mqtt: server,
		storage: storage, bus: bus, subs: subs, archives: archives, authCache: authCache,
		collector: collector, bridges: bridges, winCCUa: winCCUa, winCCOa: winCCOa, rtspCameras: rtspCameras, scripts: scripts, gqlSrv: gqlSrv,
		mcpSrv: mcpSrv, redfishMgr: redfishMgr, hostMonitor: hostMonitor, hmiSync: hmiSync,
		storageHook: storageHook, native: native, peer: pl, peerStatus: peerStatus, refreshStop: refreshStop,
	}, nil
}

func configureVolatileStores(ctx context.Context, cfg *config.Config, storage *stores.Storage) error {
	if cfg.RetainedStore() == config.StoreMemory {
		storage.Retained = storememory.NewMessageStore("retainedmessages")
	}
	if storage.Backend == config.StoreSQLite || storage.Backend == config.StoreWinCCOA {
		return nil // the SQLite factory (also the WINCCOA base) handled MEMORY itself
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

// useOAStores replaces the selected stores with WinCC OA datapoint stores.
// With a WINCCOA retained store it returns the local WinCC OA system name.
// The datapoint types are checked and every record is loaded here, so a
// missing DPT or an unreachable OA stops startup instead of running with
// an incompatible or empty configuration.
func useOAStores(ctx context.Context, cfg *config.Config, storage *stores.Storage, api oahost.API, nativeRoot string, logger *slog.Logger) (string, error) {
	needCfg := cfg.ConfigStore() == config.StoreWinCCOA
	needSes := cfg.SessionStore() == config.StoreWinCCOA
	needRet := cfg.RetainedStore() == config.StoreWinCCOA
	needUsr := cfg.UserStore() == config.StoreWinCCOA
	if err := oastore.EnsureTypes(ctx, api, needCfg, needSes, needRet, needUsr); err != nil {
		return "", fmt.Errorf("winccoa stores: %w", err)
	}
	var sys string
	st := oastore.New(api, 10*time.Second, logger)
	if needCfg {
		if err := st.Device.Load(ctx); err != nil {
			return "", fmt.Errorf("winccoa stores: %w", err)
		}
	}
	if needSes {
		if err := st.Sessions.Load(ctx); err != nil {
			return "", fmt.Errorf("winccoa stores: %w", err)
		}
	}
	if needCfg {
		storage.DeviceConfig = st.Device
		storage.ArchiveConfig = st.Archive
	}
	if needSes {
		storage.Sessions = st.Sessions
		storage.Subscriptions = st.Sessions
	}
	if needRet {
		// The native namespace (status topics) never gets datapoints.
		st.Retained.KeepInMemory(nativeRoot)
		if err := st.Retained.Load(ctx); err != nil {
			return "", fmt.Errorf("winccoa stores: %w", err)
		}
		storage.Retained = st.Retained
		info, err := api.SysInfo(ctx)
		if err != nil {
			return "", fmt.Errorf("winccoa stores: local system: %w", err)
		}
		sys = info.LocalSystem
	}
	if needUsr {
		if err := st.Users.Load(ctx); err != nil {
			return "", fmt.Errorf("winccoa stores: %w", err)
		}
		storage.Users = st.Users
	}
	logger.Info("winccoa datapoint stores active", "config", needCfg, "sessions", needSes, "retained", needRet, "users", needUsr, "system", sys)
	return sys, nil
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
		if !mqtt.IsValidFilter(s.TopicFilter, false) {
			return true
		}
		subs.Subscribe(s.ClientID, s.TopicFilter, s.QoS)
		return true
	})
}

// restorePeerLinkInterest announces the subscriptions of stored persistent sessions: the broker
// restores them only when their client reconnects, yet they hold interest while offline.
func restorePeerLinkInterest(ctx context.Context, pl *peerlink.Manager, storage *stores.Storage) error {
	sessions := map[string]stores.SessionInfo{}
	if err := storage.Sessions.IterateSessions(ctx, func(si stores.SessionInfo) bool {
		if !si.CleanSession {
			sessions[si.ClientID] = si
		}
		return true
	}); err != nil {
		return err
	}
	if len(sessions) == 0 {
		return nil
	}
	filters := map[string][]string{}
	if err := storage.Subscriptions.IterateSubscriptions(ctx, func(sub stores.MqttSubscription) bool {
		if _, ok := sessions[sub.ClientID]; ok && mqtt.IsValidFilter(sub.TopicFilter, false) {
			filters[sub.ClientID] = append(filters[sub.ClientID], sub.TopicFilter)
		}
		return true
	}); err != nil {
		return err
	}
	now := time.Now()
	for client, fs := range filters {
		si := sessions[client]
		updated := si.UpdateTime
		if si.Connected {
			updated = now // not marked offline: the node stopped while the client was connected
		}
		pl.RestoreInterest(client, fs, byte(si.ProtocolVersion), si.CleanSession,
			uint32(max(0, min(si.SessionExpiryInterval, math.MaxUint32))), updated)
	}
	return nil
}

// startNative resolves the OA local system, restores persisted native
// interests and removes persisted subscriptions that no longer validate.
func (s *Server) startNative() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.native.Start(ctx); err != nil {
		return err
	}
	persisted := map[string][]string{}
	_ = s.storage.Subscriptions.IterateSubscriptions(ctx, func(sub stores.MqttSubscription) bool {
		persisted[sub.ClientID] = append(persisted[sub.ClientID], sub.TopicFilter)
		return true
	})
	for client, filters := range s.native.Restore(persisted) {
		rows := make([]stores.MqttSubscription, 0, len(filters))
		for _, f := range filters {
			rows = append(rows, stores.MqttSubscription{ClientID: client, TopicFilter: f})
			s.subs.Unsubscribe(client, f)
		}
		if err := s.storage.Subscriptions.DelSubscriptions(ctx, rows); err != nil {
			s.logger.Warn("drop invalid native subscriptions failed", "client", client, "err", err)
		}
	}
	return nil
}

func (s *Server) Serve() error {
	if s.native != nil {
		if err := s.startNative(); err != nil {
			return fmt.Errorf("winccoa native start: %w", err)
		}
	}
	if s.peer != nil {
		// After startNative: the local OA system name is known before any
		// HELLO is sent or answered. Start does not block.
		if err := s.peer.Start(); err != nil {
			return fmt.Errorf("peerlink start: %w", err)
		}
		s.startPeerStatus()
	}
	if s.collector != nil {
		metricsCtx, metricsStop := context.WithCancel(context.Background())
		s.stopMu.Lock()
		s.metricsCtx, s.metricsStop = metricsCtx, metricsStop
		s.stopMu.Unlock()
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
	if s.storageHook != nil {
		stop := s.storageHook.StartRetention(context.Background(), time.Second)
		s.stopMu.Lock()
		s.retainedStop = stop
		s.stopMu.Unlock()
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
	if s.rtspCameras != nil {
		if err := s.rtspCameras.Start(context.Background()); err != nil {
			s.logger.Warn("rtsp cameras start error", "err", err)
		}
	}
	if s.scripts != nil {
		if err := s.scripts.Start(context.Background()); err != nil {
			s.logger.Warn("scripts start error", "err", err)
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
	if s.hmiSync != nil {
		if err := s.hmiSync.Start(); err != nil {
			s.logger.Warn("hmi sync start error", "err", err)
		}
	}
	if s.gqlSrv != nil {
		go func() {
			if err := s.gqlSrv.Start(); err != nil {
				s.logger.Error("graphql server error", "err", err)
			}
		}()
	}
	return s.mqtt.Serve()
}

func (s *Server) Close() error {
	if s.peer != nil {
		s.closePeerLink()
	}
	if s.native != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.native.Stop(ctx)
		cancel()
	}
	s.stopPublishers()
	s.stopMu.Lock()
	metricsStop, retainedStop := s.metricsStop, s.retainedStop
	s.stopMu.Unlock()
	if metricsStop != nil {
		metricsStop()
	}
	if s.collector != nil {
		s.collector.Stop()
	}
	s.stopGraphQL()
	if s.archives != nil {
		s.archives.Stop()
	}
	if retainedStop != nil {
		retainedStop()
	}
	if s.refreshStop != nil {
		s.refreshStop()
	}
	if err := s.mqtt.Close(); err != nil {
		return err
	}
	if s.storage != nil {
		return s.storage.Close()
	}
	return nil
}

// closePeerLink runs PeerLink shutdown steps 1-6 of plan 6.2: pullers stop
// gracefully, wills are no longer captured, the MQTT listeners close so
// clients fail over, the internal publishers stop, the log drains to a fixed
// target, and the peer server closes.
func (s *Server) closePeerLink() {
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	s.peer.StopPullers(stopCtx)
	cancelStop()
	s.peer.BeginDrain()
	s.mqtt.CloseListeners()
	s.stopPublishers()
	s.stopGraphQL()
	s.stopPeerStatus()
	// Drain has its own budget: Log.DrainOnShutdownMs plus a margin for the
	// GOAWAYs, independent of how long the pullers took to stop.
	drain := time.Duration(s.cfg.PeerLink.Log.GetDrainOnShutdownMs())*time.Millisecond + 5*time.Second
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drain)
	s.peer.Drain(drainCtx)
	cancelDrain()
	_ = s.peer.Close()
}

// stopPublishers stops the subsystems that publish through the inline
// client. It runs once.
func (s *Server) stopPublishers() {
	if s.publishersStopped {
		return
	}
	s.publishersStopped = true
	if s.bridges != nil {
		s.bridges.Stop()
	}
	if s.winCCUa != nil {
		s.winCCUa.Stop()
	}
	if s.winCCOa != nil {
		s.winCCOa.Stop()
	}
	if s.rtspCameras != nil {
		s.rtspCameras.Stop()
	}
	if s.scripts != nil {
		s.scripts.Stop()
	}
	if s.hostMonitor != nil {
		s.hostMonitor.Stop()
	}
	if s.hmiSync != nil {
		s.hmiSync.Stop()
	}
	if s.redfishMgr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.redfishMgr.Stop(ctx)
	}
}

// stopGraphQL stops the GraphQL/HTTP server, which also serves the REST and
// MCP publish APIs. It runs once.
func (s *Server) stopGraphQL() {
	if s.gqlStopped || s.gqlSrv == nil {
		return
	}
	s.gqlStopped = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.gqlSrv.Stop(ctx)
}

// Storage exposes the store stack for GraphQL resolvers (M6+).
func (s *Server) Storage() *stores.Storage                { return s.storage }
func (s *Server) Bus() *pubsub.Bus                        { return s.bus }
func (s *Server) Subscriptions() *topic.SubscriptionIndex { return s.subs }
func (s *Server) Archives() *archive.Manager              { return s.archives }
func (s *Server) Bridges() *mqttclient.Manager            { return s.bridges }
func (s *Server) AuthCache() *mauth.Cache                 { return s.authCache }
func (s *Server) MQTT() *mqtt.Server                      { return s.mqtt }

// WinCCOa returns the WinCC OA device manager (nil when the feature is off).
func (s *Server) WinCCOa() *winccoa.Manager { return s.winCCOa }

// Native returns the WinCC OA namespace service (nil when not embedded).
func (s *Server) Native() *winccoanative.Service { return s.native }

// PeerLink returns the PeerLink manager (nil when PeerLink is disabled).
func (s *Server) PeerLink() *peerlink.Manager { return s.peer }
