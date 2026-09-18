package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"monstermq.io/edge/internal/archive"
	"monstermq.io/edge/internal/auth"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
)

type Server struct {
	cfg        *config.Config
	storage    *stores.Storage
	archives   *archive.Manager
	authCache  *auth.Cache
	publishFn  func(topic string, payload []byte, retain bool, qos byte) error
	logger     *slog.Logger
	mcpServer  *mcp.Server
	httpServer *http.Server

	mu      sync.Mutex
	started bool
	stopped bool
	done    chan struct{}
}

func NewServer(cfg *config.Config, storage *stores.Storage, archives *archive.Manager, authCache *auth.Cache, publishFn func(topic string, payload []byte, retain bool, qos byte) error, logger *slog.Logger) *Server {
	impl := &mcp.Implementation{
		Name:    "monstermq-edge-mcp-server",
		Version: "1.0.0",
	}

	mcpSrv := mcp.NewServer(impl, nil)

	s := &Server{
		cfg:       cfg,
		storage:   storage,
		archives:  archives,
		authCache: authCache,
		publishFn: publishFn,
		logger:    logger,
		mcpServer: mcpSrv,
		done:      make(chan struct{}),
	}

	opts := &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	}
	handler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return mcpSrv
	}, opts)

	mux := http.NewServeMux()
	mux.Handle("/mcp", s.authMiddleware(handler))
	mux.Handle("/mcp/", s.authMiddleware(handler))

	s.httpServer = &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.MCP.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	s.registerTools()
	return s
}

func (s *Server) Start() error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("mcp server already started")
	}
	s.started = true
	s.mu.Unlock()

	defer close(s.done)

	s.logger.Info("mcp server listening", "port", s.cfg.MCP.Port)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	started := s.started
	s.mu.Unlock()

	err := s.httpServer.Shutdown(ctx)
	if started {
		select {
		case <-s.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.UserManagement.Enabled {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			if s.cfg.UserManagement.AllowAnonymousLocalhost && auth.IsLocalhostRequest(r) {
				next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), auth.LocalhostUser)))
				return
			}
			if s.cfg.UserManagement.AnonymousEnabled {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32600,"message":"Authentication required. Provide a valid token or credentials."}}`))
			return
		}

		parts := strings.Fields(authHeader)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			if user, ok := s.authCache.ValidateSession(parts[1]); ok {
				next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), user)))
				return
			}
		} else if len(parts) == 2 && strings.EqualFold(parts[0], "Basic") {
			username, password, ok := r.BasicAuth()
			if user, valid := s.authCache.Authenticate(r.Context(), username, password); ok && valid {
				next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), *user)))
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", "Bearer error=\"invalid_token\"")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32600,"message":"Invalid or expired credentials."}}`))
	})
}
