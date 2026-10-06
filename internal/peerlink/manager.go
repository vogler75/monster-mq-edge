// Package peerlink forwards local publishes between MonsterMQ Edge brokers (plan-peerlink): each
// node captures its publishes into an in-memory log that the configured peers pull over the
// mmq-peer/1 protocol and inject into their own broker.
package peerlink

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/mqtt"
	"monstermq.io/edge/internal/mqtt/packets"
	"monstermq.io/edge/internal/peerlink/wire"
	"monstermq.io/edge/internal/tlsutil"
)

// RetainedAccess gives PeerLink access to the broker's retained store (plan 6.1, 13.4, 16.5).
type RetainedAccess interface {
	// Snapshot calls fn for every current retained message until fn returns false or ctx ends.
	// Packets carry TopicName, Payload, QoS, Properties, Origin, Created and Expiry.
	Snapshot(ctx context.Context, fn func(pk packets.Packet) bool) error
	// Has reports whether the topic currently has a retained value.
	Has(topic string) bool
	// Created returns the unix second of the topic's retained value (for the NEWER resync).
	Created(topic string) (int64, bool)
	// Get returns the topic's retained value (Origin, Created, Expiry and Properties set).
	Get(topic string) (packets.Packet, bool)
	// FlushReplicas writes the retained replicas of source that StorageHook batched (13.4) and
	// returns once they are stored. PeerLink calls it before every COMMIT.
	FlushReplicas(source string) error
}

// ClientState answers whether a client id is connected on this node (will supersession, 12.2).
type ClientState interface {
	Connected(id string) bool
}

// Metrics receives the forwarded-message counters for the metrics snapshot (20.2). Records
// injected on this node are counted as messageBusIn by StorageHook (13.4).
type Metrics interface {
	IncBusOut(n int)
}

// Deps are the inputs of New. Config, Setup and Server are required.
type Deps struct {
	Config config.PeerLinkConfig
	// Setup is cfg.ResolvePeerLink(): canonical NodeIds, the own entry removed.
	Setup  *config.PeerLinkSetup
	Server *mqtt.Server
	// MaxMessageSize is the broker MaxMessageSize (0 = unlimited).
	MaxMessageSize int
	// HMISyncBaseTopic resolves the default Capture.Exclude.
	HMISyncBaseTopic string
	// RetainedClass is this node's retained store class, announced in the handshake.
	RetainedClass wire.RetainedClass
	// NamespaceRoot returns the WinCC OA TopicRoot while WinCC OA native mode is active (embedded
	// host, WinCCOaNative.Enabled and Namespace), "" otherwise. It is the single predicate for the
	// namespace exclusion (capture, snapshot, receiver, announced root) and may later depend on the
	// WinCC OA host role. nil means never active.
	NamespaceRoot func() string
	// OASystem returns the local WinCC OA system name, or "" (nil) when native mode is off. It is
	// read once in Start.
	OASystem func() string
	// Retained is the retained store adapter. nil uses the engine's in-memory retained map, which
	// matches RetainedStoreType MEMORY.
	Retained RetainedAccess
	// Clients answers local connection state. nil uses the engine's client list.
	Clients ClientState
	Metrics Metrics
	Logger  *slog.Logger
	// ListenAddress overrides Listener.Address:Port (tests use "127.0.0.1:0").
	ListenAddress string
	// OnStateChange is called after a link changes state (connect, disconnect), e.g. to refresh
	// the native status object. It must not block.
	OnStateChange func()
}

// Manager owns the PeerLink log, capture hook, peer listener and pullers.
type Manager struct {
	cfg    config.PeerLinkConfig
	deps   Deps
	nodeID string
	logger *slog.Logger
	srv    *mqtt.Server
	base   time.Time

	hook       *Hook
	log        *Log
	retained   RetainedAccess
	clients    ClientState
	tls        *tlsMaterial
	instanceID uint64

	consumers    []*consumerSlot
	consumerByID map[string]*consumerSlot
	peerByID     map[string]config.PeerConfig
	pullers      []*puller
	pullerByID   map[string]*puller

	listener       net.Listener
	listenAddr     string
	allowedNets    []*net.IPNet
	authConfigured bool

	oaSystem string // set in Start before any goroutine runs

	ctx    context.Context
	cancel context.CancelFunc
	connWG sync.WaitGroup
	bgWG   sync.WaitGroup

	sessMu   sync.Mutex
	sessions map[*session]struct{}
	conns    map[net.Conn]struct{} // every accepted connection, also before authentication

	admMu       sync.Mutex
	preAuthIP   map[string]int
	preAuthAll  int
	maxPerIP    int
	peerIPs     map[string]bool
	authedIPs   map[string]time.Time // IPs of authenticated sessions (pre-auth cap bypass)
	adm         admissionCounters
	authFailMu  sync.Mutex
	authFailAll map[string]uint64

	appendRate   atomic.Uint64 // float64 bits, accounted bytes per second (EWMA)
	started      atomic.Bool
	closed       atomic.Bool
	pullersDone  atomic.Bool
	drained      atomic.Bool
	closeOnce    sync.Once
	rate         rateLimiter
	maxRecordCap int
}

type admissionCounters struct {
	accepted, refusedNetwork, refusedBusy, refusedSniff, refusedPlaintext, refusedHTTP, tlsFailures atomic.Uint64
}

const (
	maxPreAuthTotal = 16
	maxAuthedIPs    = 256
	// peerIPRefresh re-resolves peer Addresses, so a peer whose name did not resolve at boot or
	// whose address changed is exempt from the global pre-auth cap again.
	peerIPRefresh = time.Minute
)

// New builds the manager: log, hook, TLS material, injector clients and pullers. When any peer
// may pull from this node it also binds the peer listener, so a port conflict fails startup; the
// caller must Close the manager when a later build step fails. No goroutine accepts or dials
// before Start.
func New(deps Deps) (*Manager, error) {
	if deps.Setup == nil || deps.Server == nil {
		return nil, errors.New("peerlink: New needs Setup and Server")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	cfg := deps.Config
	m := &Manager{
		cfg:          cfg,
		deps:         deps,
		nodeID:       deps.Setup.NodeID,
		logger:       logger.With("component", "peerlink"),
		srv:          deps.Server,
		base:         time.Now(),
		consumerByID: make(map[string]*consumerSlot),
		peerByID:     make(map[string]config.PeerConfig),
		pullerByID:   make(map[string]*puller),
		sessions:     make(map[*session]struct{}),
		preAuthIP:    make(map[string]int),
		peerIPs:      make(map[string]bool),
		authedIPs:    make(map[string]time.Time),
		authFailAll:  make(map[string]uint64),
		maxPerIP:     cfg.Listener.GetMaxPreAuthPerIp(),
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	var idb [8]byte
	_, _ = rand.Read(idb[:])
	m.instanceID = binary.LittleEndian.Uint64(idb[:]) | 1

	m.retained = deps.Retained
	if m.retained == nil {
		m.retained = memoryRetained{srv: deps.Server}
	}
	m.clients = deps.Clients
	if m.clients == nil {
		m.clients = engineClients{srv: deps.Server}
	}

	var serveIDs []string
	needDialerTLS := false
	for _, peer := range deps.Setup.Peers {
		m.peerByID[peer.NodeID] = peer
		tp, err := tlsPeerOf(peer)
		if err != nil {
			return nil, err
		}
		secrets, err := tlsutil.DecodeSecrets(cfg.SecretsFor(peer))
		if err != nil {
			return nil, fmt.Errorf("peerlink: peer %s: %w", peer.NodeID, err)
		}
		if peer.GetServe() {
			slot := &consumerSlot{idx: len(m.consumers), nodeID: peer.NodeID, peer: peer, tlsPeer: tp, secrets: secrets,
				refused: make(map[uint64]time.Time), authFailures: make(map[string]uint64)}
			m.consumers = append(m.consumers, slot)
			m.consumerByID[peer.NodeID] = slot
			serveIDs = append(serveIDs, peer.NodeID)
			if len(secrets) > 0 {
				m.authConfigured = true
			}
		}
		if peer.Pulls() {
			p, err := newPuller(m, peer, tp, secrets)
			if err != nil {
				return nil, err
			}
			m.pullers = append(m.pullers, p)
			m.pullerByID[peer.NodeID] = p
			if cfg.DialerTLS(peer) {
				needDialerTLS = true
			}
		}
	}

	listenerTLS := cfg.Tls.Enabled && len(m.consumers) > 0
	tm, err := loadTLS(&cfg, m.nodeID, m.consumers, listenerTLS, needDialerTLS, m.logger)
	if err != nil {
		return nil, err
	}
	m.tls = tm
	if tm.server != nil && tm.clientAuth != tlsutil.ClientAuthNone {
		m.authConfigured = true
	}
	for _, p := range m.pullers {
		if cfg.DialerTLS(p.peer) {
			cc, err := tm.clientConfig(p)
			if err != nil {
				return nil, err
			}
			p.tlsCfg = cc
		}
	}

	for _, cidr := range cfg.Listener.AllowedNetworks {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("peerlink: Listener.AllowedNetworks: %w", err)
		}
		m.allowedNets = append(m.allowedNets, n)
	}

	m.maxRecordCap = cfg.Log.GetMaxRecordBytes(deps.MaxMessageSize)
	if len(m.consumers) > 0 {
		lg, err := NewLog(LogConfig{
			MaxMessages:    uint64(cfg.Log.GetMaxMessages()),
			MaxBytes:       uint64(cfg.Log.GetMaxBytes()),
			MaxRecordBytes: m.maxRecordCap,
			Consumers:      serveIDs,
		})
		if err != nil {
			return nil, err
		}
		m.log = lg
	}

	h, err := newHook(m)
	if err != nil {
		m.closeLog()
		return nil, err
	}
	m.hook = h

	// Retained capture in apply order (PL-43): the engine serializes the store update and the
	// hooks per topic. A WinCC OA retained store is left out: its writes wait for OA answers that
	// may be dispatched behind a publish of the same topic.
	if m.log != nil && deps.RetainedClass != wire.RetainedWinCCOA {
		deps.Server.Options.SerializeRetained = true
	}
	deps.Server.Options.QueueOfflineReplicas = cfg.Receive.Queue
	if w := cfg.Receive.GetInjectWorkers(); w > 1 {
		m.logger.Warn("peerlink: Receive.InjectWorkers is reserved; one injector per source applies records in order",
			"injectWorkers", w)
	}

	if len(m.consumers) > 0 {
		addr := deps.ListenAddress
		if addr == "" {
			addr = net.JoinHostPort(cfg.Listener.ListenAddress(), strconv.Itoa(cfg.Listener.GetPort()))
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			m.closeLog()
			return nil, fmt.Errorf("peerlink: listen %s: %w", addr, err)
		}
		m.listener = ln
		m.listenAddr = ln.Addr().String()
	} else if len(m.pullers) > 0 {
		// A pull-only node serves only the loopback status and resync endpoints (20.2, 16.5), on
		// the loopback address. They are an operator aid, so a port conflict is a WARN only.
		addr := deps.ListenAddress
		if addr == "" {
			addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Listener.GetPort()))
		}
		if ln, err := net.Listen("tcp", addr); err != nil {
			m.logger.Warn("peerlink: status endpoint not available on this pull-only node", "address", addr, "error", err)
		} else {
			m.listener = ln
			m.listenAddr = ln.Addr().String()
		}
	}
	if cfg.AllowUnauthenticatedPeers {
		m.logger.Warn("peerlink: AllowUnauthenticatedPeers is set; peers in AllowedNetworks need no authentication",
			"allowedNetworks", cfg.Listener.AllowedNetworks)
	}
	return m, nil
}

func newHook(m *Manager) (*Hook, error) {
	cfg := m.cfg
	f, err := newIncludeExclude(cfg.Capture.GetInclude(), cfg.Capture.GetExclude(m.deps.HMISyncBaseTopic))
	if err != nil {
		return nil, fmt.Errorf("peerlink: Capture filters: %w", err)
	}
	h := &Hook{
		m:             m,
		srv:           m.srv,
		log:           m.log,
		captureWills:  cfg.Capture.GetWills(),
		retainViaHook: m.srv.Options.Capabilities.RetainAvailable == 1,
		maxExpirySec:  maxExpiry(m.srv),
		filter:        f,
		sharedSkip:    cfg.Receive.GetSharedSubscriptions() != config.PeerLinkSharedDeliver,
	}
	if cfg.Capture.EchoSuppressMs > 0 {
		h.echo = newEchoTable(cfg.Capture.EchoSuppressMs)
	}
	if len(m.pullers) > 0 {
		h.sessions = newSessionTimes()
	}
	h.active.Store(m.log != nil)
	return h, nil
}

func (m *Manager) closeLog() {
	if m.log != nil {
		m.log.Close()
	}
}

// Hook returns the engine hook. Register it before StorageHook and QueueHook.
func (m *Manager) Hook() *Hook { return m.hook }

// Addr returns the bound peer listener address: all peers when any peer may pull from this node,
// the loopback status endpoint on a pull-only node, "" when nothing is bound.
func (m *Manager) Addr() string { return m.listenAddr }

// NodeID returns this node's canonical NodeId.
func (m *Manager) NodeID() string { return m.nodeID }

// RetainedViaOA reports whether retained replicas from source go to the in-memory retained view
// only, because WinCC OA replicates the retained datapoints between both sides (oaRetained, 9.5).
// It is one map lookup plus one atomic load, for StorageHook.
func (m *Manager) RetainedViaOA(source string) bool {
	p := m.pullerByID[source]
	return p != nil && p.oaRetained.Load()
}

// Start starts the peer listener and the pullers. It does not block.
func (m *Manager) Start() error {
	if m.closed.Load() {
		return errors.New("peerlink: manager closed")
	}
	if !m.started.CompareAndSwap(false, true) {
		return nil
	}
	if m.deps.OASystem != nil {
		m.oaSystem = m.deps.OASystem()
	}
	if m.listener != nil {
		// DNS may be slow; Start must not block the embedded host's Serve.
		m.bgWG.Add(1)
		go func() {
			defer m.bgWG.Done()
			m.resolvePeerIPs()
		}()
		m.bgWG.Add(1)
		go func() {
			defer m.bgWG.Done()
			m.acceptLoop()
		}()
		m.logger.Info("peerlink: listening", "address", m.listenAddr, "tls", m.tls.server != nil, "consumers", len(m.consumers))
	}
	for _, p := range m.pullers {
		p.start()
	}
	m.bgWG.Add(1)
	go func() {
		defer m.bgWG.Done()
		m.background()
	}()
	return nil
}

// resolvePeerIPs collects the IPs of peers with an Address, which bypass the global pre-auth cap.
func (m *Manager) resolvePeerIPs() {
	ctx, cancel := context.WithTimeout(m.ctx, 2*time.Second)
	defer cancel()
	ips := make(map[string]bool)
	for _, peer := range m.peerByID {
		if peer.Address == "" {
			continue
		}
		host, _, err := net.SplitHostPort(peer.Address)
		if err != nil {
			continue
		}
		if ip := net.ParseIP(host); ip != nil {
			ips[ip.String()] = true
			continue
		}
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ips[a.IP.String()] = true
		}
	}
	m.admMu.Lock()
	m.peerIPs = ips
	m.admMu.Unlock()
}

// background samples the append rate for capacitySeconds and raises the never-connected WARN.
func (m *Manager) background() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	neverWarn := time.Duration(m.cfg.Log.GetNeverConnectedWarnSec()) * time.Second
	warned := make([]bool, len(m.consumers))
	var lastBytes uint64
	if m.log != nil {
		lastBytes = m.log.Stats().AppendedBytes
	}
	start := time.Now()
	lastResolve := start
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-tick.C:
		}
		if m.listener != nil && time.Since(lastResolve) >= peerIPRefresh {
			lastResolve = time.Now()
			m.resolvePeerIPs()
		}
		if m.log != nil {
			st := m.log.Stats()
			delta := float64(st.AppendedBytes - lastBytes)
			lastBytes = st.AppendedBytes
			old := math.Float64frombits(m.appendRate.Load())
			m.appendRate.Store(math.Float64bits(old*0.8 + delta*0.2))
		}
		if neverWarn > 0 && m.log != nil && time.Since(start) >= neverWarn {
			stats := m.log.ConsumerStats()
			for i, c := range m.consumers {
				if !warned[i] && stats[i].State == LogConsumerNeverConnected {
					warned[i] = true
					m.logger.Warn("peerlink: configured consumer never connected; it pins the log", "peer", c.nodeID,
						"after", neverWarn)
				}
			}
		}
	}
}

// StopPullers stops every puller gracefully: each finishes its current batch, flushes the batched
// retained writes, sends COMMIT and GOAWAY(shutdown). It returns when all stopped or ctx ends.
func (m *Manager) StopPullers(ctx context.Context) {
	if !m.pullersDone.CompareAndSwap(false, true) {
		return
	}
	for _, p := range m.pullers {
		p.stop(true)
	}
	for _, p := range m.pullers {
		p.wait(ctx)
	}
}

// BeginDrain stops capturing wills: from here on they are the broker's own shutdown wills (7.5).
func (m *Manager) BeginDrain() {
	m.hook.draining.Store(true)
}

// DrainResult reports the source drain (15.6).
type DrainResult struct {
	Target               uint64
	FinalLEO             uint64
	Complete             bool
	Waited               time.Duration
	ShutdownUnserved     map[string]uint64
	UncapturedAtShutdown uint64
}

// Drain fixes drainTarget = leo, waits up to Log.DrainOnShutdownMs for every connected consumer to
// commit it, switches capture off, sends GOAWAY(shutdown) to every session, closes the peer
// listener and logs shutdownUnserved per consumer and uncapturedAtShutdown (15.6).
func (m *Manager) Drain(ctx context.Context) DrainResult {
	var res DrainResult
	if !m.drained.CompareAndSwap(false, true) {
		return res
	}
	m.hook.draining.Store(true)
	if m.log != nil {
		dr := m.log.Drain(ctx, time.Duration(m.cfg.Log.GetDrainOnShutdownMs())*time.Millisecond, nil)
		m.hook.active.Store(false)
		res.Target, res.FinalLEO, res.Complete, res.Waited = dr.Target, dr.FinalLEO, dr.Complete, dr.Waited
	}
	m.hook.active.Store(false)
	m.closeListener()
	m.closeSessions(wire.GoAwayShutdown)
	if m.log != nil {
		res.ShutdownUnserved = make(map[string]uint64)
		for i, cs := range m.log.ConsumerStats() {
			res.ShutdownUnserved[cs.NodeID] = cs.Lag
			m.consumers[i].shutdownUnserved.Store(cs.Lag)
			if cs.Lag > 0 {
				m.logger.Warn("peerlink: shutdown with unserved records", "peer", cs.NodeID, "shutdownUnserved", cs.Lag,
					"state", cs.State.String(), "drainTarget", res.Target, "finalLeo", res.FinalLEO)
			} else {
				m.logger.Info("peerlink: consumer drained", "peer", cs.NodeID, "shutdownUnserved", 0)
			}
		}
		res.UncapturedAtShutdown = m.log.Stats().UncapturedAtShutdown
	}
	if res.UncapturedAtShutdown > 0 {
		m.logger.Warn("peerlink: publishes after capture was switched off", "uncapturedAtShutdown", res.UncapturedAtShutdown)
	}
	return res
}

// Close stops everything without draining. It is idempotent and safe without Start.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.closed.Store(true)
		m.hook.active.Store(false)
		m.pullersDone.Store(true)
		for _, p := range m.pullers {
			p.stop(false)
		}
		m.closeListener()
		m.closeSessions(wire.GoAwayShutdown)
		m.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		for _, p := range m.pullers {
			p.wait(ctx)
		}
		cancel()
		m.connWG.Wait()
		m.bgWG.Wait()
		m.closeLog()
	})
	return nil
}

func (m *Manager) closeListener() {
	if m.listener != nil {
		_ = m.listener.Close()
	}
}

func (m *Manager) closeSessions(code wire.GoAwayCode) {
	m.sessMu.Lock()
	list := make([]*session, 0, len(m.sessions))
	for s := range m.sessions {
		list = append(list, s)
	}
	m.sessMu.Unlock()
	for _, s := range list {
		s.goAway(code, "")
	}
	m.sessMu.Lock()
	for c := range m.conns {
		_ = c.Close()
	}
	m.sessMu.Unlock()
	m.connWG.Wait()
}

// Resync requests an operator resync (NEWER, 16.5) from source: the link reconnects and pulls a
// retained snapshot that overwrites present values only when the source value is newer.
func (m *Manager) Resync(source string) error {
	p := m.pullerByID[source]
	if p == nil {
		return fmt.Errorf("no pull peer %q", source)
	}
	p.requestResync()
	return nil
}

// Status returns every counter and gauge (plan 20.1).
func (m *Manager) Status() Status {
	st := Status{Enabled: true, NodeID: m.nodeID, Listen: m.listenAddr, TLS: m.tls != nil && m.tls.server != nil}
	h := m.hook
	st.Log = LogStatus{
		SkipPeer:         h.skipPeer.Load(),
		SkipWill:         h.skipWill.Load(),
		Filtered:         h.filtered.Load(),
		EchoSuppressed:   h.echoSuppressed.Load(),
		SharedSkipped:    h.sharedSkipped.Load(),
		RefusedClientIDs: h.refusedIDs.Load(),
		UsernameStripped: h.usernameStripped.Load(),
		Active:           h.active.Load(),
		EvictedBy:        map[string]uint64{},
		CaptureDropped:   map[string]uint64{},
	}
	if m.log != nil {
		ls := m.log.Stats()
		st.Epoch = ls.Epoch
		l := &st.Log
		l.Epoch, l.LSO, l.LEO, l.LWM, l.Records, l.Bytes = ls.Epoch, ls.LSO, ls.LEO, ls.LWM, ls.Records, ls.Bytes
		l.MaxBytes, l.MaxMessages = ls.MaxBytes, ls.MaxMessages
		l.Appended = KindCounts{Client: ls.AppendedClient, Inline: ls.AppendedInline, Will: ls.AppendedWill}
		l.Trimmed, l.EvictedUnread = ls.Trimmed, ls.EvictedUnread
		l.EvictedBy["count"], l.EvictedBy["bytes"] = ls.EvictedByCount, ls.EvictedByBytes
		l.CaptureDropped["size"], l.CaptureDropped["invalid"] = ls.CaptureDroppedSize, ls.CaptureDroppedInvalid
		l.SpareMisses, l.UncapturedAtShutdown, l.Sealed = ls.SpareMisses, ls.UncapturedAtShutdown, ls.Sealed
		if rate := math.Float64frombits(m.appendRate.Load()); rate > 1 && ls.MaxBytes > ls.Bytes {
			v := float64(ls.MaxBytes-ls.Bytes) / rate
			l.CapacitySeconds = &v
		}
	}
	st.Admission = m.admissionStatus()
	if m.log != nil {
		stats := m.log.ConsumerStats()
		for i, c := range m.consumers {
			st.Consumers = append(st.Consumers, c.status(stats[i]))
		}
	}
	for _, p := range m.pullers {
		st.Sources = append(st.Sources, p.status())
	}
	return st
}

func (m *Manager) admissionStatus() AdmissionStatus {
	a := AdmissionStatus{
		Accepted:         m.adm.accepted.Load(),
		RefusedNetwork:   m.adm.refusedNetwork.Load(),
		RefusedBusy:      m.adm.refusedBusy.Load(),
		RefusedSniff:     m.adm.refusedSniff.Load(),
		RefusedPlaintext: m.adm.refusedPlaintext.Load(),
		RefusedHTTP:      m.adm.refusedHTTP.Load(),
		TLSFailures:      m.adm.tlsFailures.Load(),
		AuthFailures:     map[string]uint64{},
	}
	m.authFailMu.Lock()
	for k, v := range m.authFailAll {
		a.AuthFailures[k] = v
	}
	m.authFailMu.Unlock()
	m.admMu.Lock()
	a.PreAuth = m.preAuthAll
	m.admMu.Unlock()
	return a
}

// monoNs is the monotonic clock of the hook and the receiver (session times, echo table).
func (m *Manager) monoNs() int64 { return int64(time.Since(m.base)) }

// namespaceRoot is the own WinCC OA TopicRoot while native mode is active, "" otherwise.
func (m *Manager) namespaceRoot() string {
	if m.deps.NamespaceRoot == nil {
		return ""
	}
	return m.deps.NamespaceRoot()
}

// inNamespace is the namespace exclusion predicate (7.2 and 12.2 step 3, own root).
func (m *Manager) inNamespace(topic string) bool {
	return underRoot(topic, m.namespaceRoot())
}

func (m *Manager) stateChanged() {
	if m.deps.OnStateChange != nil {
		m.deps.OnStateChange()
	}
}

// ownCaps is the capability set this node offers on a link.
func (m *Manager) ownCaps(tlsLink bool) uint64 {
	caps := wire.CapsV1
	if tlsLink && !m.cfg.Fetch.CrcOnTls {
		caps &^= wire.CapBatchCRC
	}
	if m.cfg.Snapshot.GetMode() != config.PeerLinkSnapshotFill {
		caps &^= wire.CapSnapshotFill
	}
	return caps
}

// oaRetainedFor computes the per-link oaRetained flag (9.5): the peer is configured as the
// redundancy partner and both sides announce the WINCCOA retained class of the same OA system.
// When the partner flag is set on a WINCCOA node but the link does not qualify, why says why.
func oaRetainedFor(partner bool, local wire.RetainedClass, localSys string, remote wire.RetainedClass, remoteSys string) (ok bool, why string) {
	switch {
	case !partner || local != wire.RetainedWinCCOA:
		return false, ""
	case remote != wire.RetainedWinCCOA:
		return false, "peer retained store class is " + remote.String()
	case localSys == "" || localSys != remoteSys:
		return false, "WinCC OA systems differ"
	}
	return true, ""
}

// memoryRetained reads the engine's in-memory retained map (RetainedStoreType MEMORY).
type memoryRetained struct{ srv *mqtt.Server }

func (r memoryRetained) Snapshot(ctx context.Context, fn func(pk packets.Packet) bool) error {
	for _, pk := range r.srv.Topics.Retained.GetAll() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !fn(pk) {
			return nil
		}
	}
	return nil
}

func (r memoryRetained) Has(topic string) bool {
	_, ok := r.srv.Topics.Retained.Get(topic)
	return ok
}

func (r memoryRetained) Created(topic string) (int64, bool) {
	pk, ok := r.srv.Topics.Retained.Get(topic)
	return pk.Created, ok
}

func (r memoryRetained) Get(topic string) (packets.Packet, bool) {
	return r.srv.Topics.Retained.Get(topic)
}

func (memoryRetained) FlushReplicas(string) error { return nil }

type engineClients struct{ srv *mqtt.Server }

func (c engineClients) Connected(id string) bool {
	cl, ok := c.srv.Clients.Get(id)
	return ok && !cl.Closed()
}
