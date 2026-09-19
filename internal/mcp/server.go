package mcp

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"monstermq.io/edge/internal/archive"
	"monstermq.io/edge/internal/auth"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/stores"
)

type Server struct {
	cfg       *config.Config
	storage   *stores.Storage
	archives  *archive.Manager
	authCache *auth.Cache
	publishFn func(topic string, payload []byte, retain bool, qos byte) error
	logger    *slog.Logger
	mcpServer *mcp.Server
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

// Handler returns an http.Handler that serves MCP Streamable HTTP / SSE with authentication.
func (s *Server) Handler() http.Handler {
	opts := &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	}
	handler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return s.mcpServer
	}, opts)
	return s.authMiddleware(handler)
}

func (s *Server) Start() error {
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	return nil
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
