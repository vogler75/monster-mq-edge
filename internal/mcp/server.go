package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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
	}

	s.registerTools()
	return s
}

func (s *Server) Start() error {
	opts := &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	}
	handler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return s.mcpServer
	}, opts)

	mux := http.NewServeMux()
	mux.Handle("/mcp", s.authMiddleware(handler))
	mux.Handle("/mcp/", s.authMiddleware(handler))

	s.httpServer = &http.Server{
		Addr:              fmt.Sprintf(":%d", s.cfg.MCP.Port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	s.logger.Info("mcp server listening", "port", s.cfg.MCP.Port)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.UserManagement.Enabled {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
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

		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token != "" {
				next.ServeHTTP(w, r)
				return
			}
		} else if strings.HasPrefix(authHeader, "Basic ") {
			username, password, ok := r.BasicAuth()
			if ok && s.authCache.Validate(username, password) {
				next.ServeHTTP(w, r)
				return
			}
		}

		if s.cfg.UserManagement.AnonymousEnabled {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", "Bearer error=\"invalid_token\"")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32600,"message":"Invalid or expired credentials."}}`))
	})
}
