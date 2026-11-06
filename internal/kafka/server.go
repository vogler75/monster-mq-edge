package kafka

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"monstermq.io/edge/internal/auth"
	"monstermq.io/edge/internal/pubsub"
	"monstermq.io/edge/internal/stores"
)

// Server status strings reported through GraphQL.
const (
	StatusStarting = "STARTING"
	StatusRunning  = "RUNNING"
	StatusError    = "ERROR"
	StatusStopped  = "STOPPED"
)

// Deps are the broker services a Kafka server needs.
type Deps struct {
	Bus *pubsub.Bus
	// Publish injects a message into the MQTT engine as the given inline client.
	Publish func(clientID string, msg stores.BrokerMessage) error
	// Release drops the inline client created by Publish.
	Release               func(clientID string)
	AuthCache             *auth.Cache
	UserManagementEnabled bool
	// NewStore opens the queue store of one stream. storeType is the effective
	// per-stream store type override (stream, then server level), nil when the
	// broker defaults apply; suffix is TableSuffix(streamName). Implementations
	// should be idempotent; New also calls EnsureTable on the result.
	NewStore func(ctx context.Context, storeType *string, suffix string) (stores.KafkaQueueStore, error)
	NodeID   string
	Logger   *slog.Logger
}

// Server is one Kafka-Server device: a plain-TCP Kafka listener plus the
// orchestrator that mirrors MQTT messages into the stream queues.
type Server struct {
	cfg  ServerConfig
	deps Deps
	log  *slog.Logger

	ln             net.Listener
	advertisedHost string
	advertisedPort int

	streams       []*stream // configuration order
	streamsByName map[string]*stream

	// selfPub holds UUIDs of messages Produce published to MQTT after storing
	// them itself; the orchestrator skips them (value: time.Time).
	selfPub sync.Map

	orch orchestrator

	status atomic.Value // string
	mu     sync.Mutex
	conns  map[*conn]struct{}
	wg     sync.WaitGroup
	stop   chan struct{}
	once   sync.Once
}

// New binds the listener and opens the queue store of every stream. The server
// does not accept connections nor subscribe to MQTT until Start.
func New(cfg ServerConfig, deps Deps) (*Server, error) {
	if errs := cfg.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("invalid kafka server config: %v", errs)
	}
	if deps.NewStore == nil {
		return nil, errors.New("kafka server: NewStore is required")
	}
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, deps: deps, log: log, streamsByName: map[string]*stream{}, conns: map[*conn]struct{}{}, stop: make(chan struct{})}
	s.orch.s = s
	s.status.Store(StatusStarting)

	ctx := context.Background()
	for _, m := range cfg.Streams {
		storeType := m.StoreType
		if storeType == nil || *storeType == "" {
			storeType = cfg.StoreType
		}
		if storeType != nil && *storeType == "" {
			storeType = nil
		}
		suffix := TableSuffix(m.StreamName)
		st, err := deps.NewStore(ctx, storeType, suffix)
		if err == nil {
			err = st.EnsureTable(ctx)
			if err != nil {
				_ = st.Close()
			}
		}
		if err != nil {
			s.closeStores()
			s.status.Store(StatusError)
			return nil, fmt.Errorf("kafka stream %q store: %w", m.StreamName, err)
		}
		stm := &stream{cfg: m, suffix: suffix, store: st, retention: time.Duration(m.RetentionHours) * time.Hour}
		s.streams = append(s.streams, stm)
		s.streamsByName[m.StreamName] = stm
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		s.closeStores()
		s.status.Store(StatusError)
		return nil, fmt.Errorf("kafka server listen %s:%d: %w", cfg.Host, cfg.Port, err)
	}
	s.ln = ln

	s.advertisedHost = resolveAdvertisedHost(cfg)
	s.advertisedPort = ln.Addr().(*net.TCPAddr).Port
	if cfg.AdvertisedPort != nil {
		s.advertisedPort = *cfg.AdvertisedPort
	}
	return s, nil
}

func (s *Server) closeStores() {
	for _, st := range s.streams {
		_ = st.store.Close()
	}
}

// resolveAdvertisedHost is advertisedHost, else host; 0.0.0.0 resolves to the
// first non-loopback IPv4 address of the machine, else "localhost".
func resolveAdvertisedHost(cfg ServerConfig) string {
	host := cfg.Host
	if cfg.AdvertisedHost != nil && *cfg.AdvertisedHost != "" {
		host = *cfg.AdvertisedHost
	}
	if host != "0.0.0.0" && host != "" && host != "::" {
		return host
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
				if v4 := ipn.IP.To4(); v4 != nil {
					return v4.String()
				}
			}
		}
	}
	return "localhost"
}

// Start subscribes the orchestrator and begins accepting connections.
func (s *Server) Start() error {
	if err := s.orch.start(); err != nil {
		s.status.Store(StatusError)
		return err
	}
	s.status.Store(StatusRunning)
	s.log.Info("Apache Kafka protocol server listening", "server", s.cfg.Name, "addr", s.ln.Addr().String(), "streams", len(s.streams))
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.stop:
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			s.log.Error("Kafka accept failed", "server", s.cfg.Name, "err", err)
			s.status.Store(StatusError)
			return
		}
		if tc, ok := nc.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		c := newConn(s, nc)
		s.mu.Lock()
		select {
		case <-s.stop:
			s.mu.Unlock()
			_ = nc.Close()
			return
		default:
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			c.serve()
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
		}()
	}
}

// Stop closes the listener and connections, flushes the orchestrator and
// closes the stream stores. It is safe to call more than once.
func (s *Server) Stop() {
	s.once.Do(func() {
		close(s.stop)
		_ = s.ln.Close()
		s.mu.Lock()
		for c := range s.conns {
			c.close()
		}
		s.mu.Unlock()
		s.orch.stop()
		s.wg.Wait()
		s.closeStores()
		s.status.Store(StatusStopped)
	})
}

// Port is the actual listening port.
func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Status is RUNNING, STARTING, ERROR or STOPPED.
func (s *Server) Status() string { return s.status.Load().(string) }

// Config returns the configuration the server was created with.
func (s *Server) Config() ServerConfig { return s.cfg }
