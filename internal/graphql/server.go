package graphql

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	gqlgraphql "github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"github.com/vektah/gqlparser/v2/ast"

	"os"
	"strings"

	"monstermq.io/edge/internal/auth"
	"monstermq.io/edge/internal/config"
	"monstermq.io/edge/internal/dashboard"
	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/graphql/resolvers"
	"monstermq.io/edge/internal/hmi"
	"monstermq.io/edge/internal/redfish"
	"monstermq.io/edge/internal/restapi"
)

// Server hosts the GraphQL HTTP and WebSocket endpoints, HMI dashboards, and Redfish API.
type Server struct {
	cfg       *config.Config
	logger    *slog.Logger
	router    *chi.Mux
	tlsConfig *tls.Config
	httpSrv   *http.Server
	httpsSrv  *http.Server

	mu      sync.Mutex
	started bool
	stopped bool
	done    chan struct{}
}

func NewServer(cfg *config.Config, resolver *resolvers.Resolver, hmiMgr *hmi.Manager, redfishMgr *redfish.Manager, rest *restapi.Handler, mcpHandler http.Handler, tlsConfig *tls.Config, logger *slog.Logger) *Server {
	var authCache *auth.Cache
	if resolver != nil {
		authCache = resolver.AuthCache
	}
	es := generated.NewExecutableSchema(generated.Config{Resolvers: resolver})
	gql := handler.New(es)
	gql.AddTransport(transport.Options{})
	gql.AddTransport(transport.GET{})
	gql.AddTransport(transport.POST{})
	gql.AddTransport(transport.MultipartForm{})
	gql.AddTransport(transport.Websocket{
		KeepAlivePingInterval: 10 * time.Second,
		Upgrader: websocket.Upgrader{
			CheckOrigin:     func(r *http.Request) bool { return true },
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
		},
		InitFunc: func(ctx context.Context, payload transport.InitPayload) (context.Context, *transport.InitPayload, error) {
			if !cfg.UserManagement.Enabled {
				return ctx, nil, nil
			}
			if _, ok := auth.Principal(ctx); ok {
				return ctx, nil, nil
			}
			value, _ := payload["Authorization"].(string)
			if value == "" {
				value, _ = payload["authorization"].(string)
			}
			return authenticateContext(ctx, value, cfg, authCache)
		},
	})
	gql.SetQueryCache(lru.New[*ast.QueryDocument](100))
	gql.Use(extension.Introspection{})
	gql.AroundOperations(func(ctx context.Context, next gqlgraphql.OperationHandler) gqlgraphql.ResponseHandler {
		if !cfg.UserManagement.Enabled {
			return next(ctx)
		}
		op := gqlgraphql.GetOperationContext(ctx).Operation
		if isLoginOnly(op) {
			return next(ctx)
		}
		if _, ok := auth.Principal(ctx); !ok && !cfg.UserManagement.AnonymousEnabled {
			return func(context.Context) *gqlgraphql.Response {
				return gqlgraphql.ErrorResponse(ctx, "authentication required")
			}
		}
		if op.Operation == ast.Mutation {
			for _, field := range rootFields(op.SelectionSet) {
				if field.Name == "login" || field.Name == "publish" || field.Name == "publishBatch" {
					continue
				}
				user, authenticated := auth.Principal(ctx)
				if !authenticated || !user.IsAdmin {
					return func(context.Context) *gqlgraphql.Response {
						return gqlgraphql.ErrorResponse(ctx, "administrator access required")
					}
				}
			}
		}
		return next(ctx)
	})

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)
	if cfg.GraphQL.TLSEnabled() && cfg.GraphQL.RequireHTTPSFromOutside {
		r.Use(requireHTTPSMiddleware(cfg))
	}
	authenticatedGQL := httpAuthMiddleware(cfg, authCache, gql)
	r.Handle("/graphql", authenticatedGQL)
	r.Handle("/graphql/", authenticatedGQL)
	// Apollo-style alias the existing dashboard might use.
	r.Handle("/query", authenticatedGQL)
	r.Get("/playground", playground.Handler("MonsterMQ Edge", "/graphql"))
	if cfg.RestApi.Enabled && rest != nil {
		r.Mount("/api/v1", rest.Router())
	}
	if (cfg.MCP.Enabled || cfg.Features.Mcp) && mcpHandler != nil {
		r.Handle("/mcp", mcpHandler)
		r.Handle("/mcp/*", mcpHandler)
	}

	if (cfg.HMI.Enabled || cfg.Features.Hmi) && hmiMgr != nil {
		mountPath := cfg.HMI.MountPath
		if mountPath == "" {
			mountPath = "/hmi"
		}

		hmiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.UserManagement.Enabled {
				isLocal := auth.IsLocalhostRequest(r)
				if !(cfg.UserManagement.AllowAnonymousLocalhost && isLocal) {
					authenticated := false
					if authHdr := r.Header.Get("Authorization"); authHdr != "" {
						if _, err := auth.AuthenticateHeader(r.Context(), resolver.AuthCache, authHdr); err != nil {
							http.Error(w, "Invalid credentials", http.StatusUnauthorized)
							return
						}
						authenticated = true
					}
					if !authenticated {
						if tok := r.URL.Query().Get("token"); tok != "" {
							if _, ok := resolver.AuthCache.ValidateSession(tok); !ok {
								http.Error(w, "Invalid session token", http.StatusUnauthorized)
								return
							}
							authenticated = true
						}
					}
					if !authenticated && !cfg.UserManagement.AnonymousEnabled {
						http.Error(w, "Authentication required", http.StatusUnauthorized)
						return
					}
				}
			}

			relPath := strings.TrimPrefix(r.URL.Path, mountPath)
			relPath = strings.TrimPrefix(relPath, "/")

			parts := strings.Split(relPath, "/")
			firstSegment := parts[0]

			var dashName string
			var fileSubPath string

			if firstSegment != "" {
				if _, err := hmiMgr.GetHmi(firstSegment); err == nil {
					dashName = firstSegment
					fileSubPath = strings.Join(parts[1:], "/")
				}
			}

			if dashName == "" {
				dashName = hmiMgr.GetMainDashboardName()
				fileSubPath = relPath
			}

			if !hmiMgr.IsHmiEnabled(dashName) {
				http.Error(w, fmt.Sprintf("HMI dashboard %q is disabled", dashName), http.StatusNotFound)
				return
			}

			if fileSubPath == "" || strings.HasSuffix(r.URL.Path, "/") {
				fileSubPath = "index.html"
			}

			fullPath, err := hmiMgr.ResolveDashboardPath(dashName, fileSubPath)
			if err != nil {
				http.NotFound(w, r)
				return
			}

			if info, err := os.Stat(fullPath); err == nil && !info.IsDir() {
				http.ServeFile(w, r, fullPath)
				return
			}

			http.NotFound(w, r)
		})

		r.Handle(mountPath+"/*", hmiHandler)
		r.Handle(mountPath, hmiHandler)
	}

	if redfishMgr != nil {
		mountPath := cfg.Redfish.MountPath
		if mountPath == "" {
			mountPath = "/redfish/v1"
		}
		mountPath = strings.TrimSuffix(mountPath, "/")
		r.Mount(mountPath, redfishMgr.Handler())
	}

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	if cfg.Dashboard.Enabled {
		dashHandler := dashboard.Handler(cfg.Dashboard.Path, logger)
		r.Handle("/*", dashHandler)
	}

	var httpSrv *http.Server
	if cfg.GraphQL.HTTPEnabled() {
		httpAddr := cfg.GraphQL.Address
		if httpAddr == "" {
			httpAddr = "0.0.0.0"
		}
		httpSrv = &http.Server{
			Addr:              fmt.Sprintf("%s:%d", httpAddr, cfg.GraphQL.Port),
			Handler:           r,
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	var httpsSrv *http.Server
	if cfg.GraphQL.TLSEnabled() && tlsConfig != nil {
		tlsAddr := cfg.GraphQL.TLSAddress
		if tlsAddr == "" {
			tlsAddr = "0.0.0.0"
		}
		httpsSrv = &http.Server{
			Addr:              fmt.Sprintf("%s:%d", tlsAddr, cfg.GraphQL.TLSPort),
			Handler:           r,
			TLSConfig:         tlsConfig,
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	return &Server{
		cfg:       cfg,
		logger:    logger,
		router:    r,
		tlsConfig: tlsConfig,
		httpSrv:   httpSrv,
		httpsSrv:  httpsSrv,
		done:      make(chan struct{}),
	}
}

func httpAuthMiddleware(cfg *config.Config, cache *auth.Cache, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cfg.UserManagement.Enabled {
			next.ServeHTTP(w, r)
			return
		}
		if cfg.UserManagement.AllowAnonymousLocalhost && auth.IsLocalhostRequest(r) && r.Header.Get("Authorization") == "" {
			ctx := auth.WithPrincipal(r.Context(), auth.LocalhostUser)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		ctx, _, err := authenticateContext(r.Context(), r.Header.Get("Authorization"), cfg, cache)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `%s`, `{"errors":[{"message":"invalid credentials"}]}`)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func authenticateContext(ctx context.Context, authorization string, cfg *config.Config, cache *auth.Cache) (context.Context, *transport.InitPayload, error) {
	if !cfg.UserManagement.Enabled {
		return ctx, nil, nil
	}
	ctx, err := auth.AuthenticateHeader(ctx, cache, authorization)
	return ctx, nil, err
}

func isLoginOnly(op *ast.OperationDefinition) bool {
	if op == nil || op.Operation != ast.Mutation {
		return false
	}
	fields := rootFields(op.SelectionSet)
	return len(fields) == 1 && fields[0].Name == "login"
}

func rootFields(selections ast.SelectionSet) []*ast.Field {
	fields := make([]*ast.Field, 0, len(selections))
	for _, selection := range selections {
		switch value := selection.(type) {
		case *ast.Field:
			fields = append(fields, value)
		case *ast.InlineFragment:
			fields = append(fields, rootFields(value.SelectionSet)...)
		case *ast.FragmentSpread:
			if value.Definition != nil {
				fields = append(fields, rootFields(value.Definition.SelectionSet)...)
			}
		}
	}
	return fields
}

func (s *Server) Start() error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("graphql server already started")
	}
	s.started = true
	s.mu.Unlock()

	defer close(s.done)

	errCh := make(chan error, 2)
	servers := 0

	if s.httpSrv != nil {
		servers++
		s.logger.Info("graphql http listening", "addr", s.httpSrv.Addr)

		go func() {
			err := s.httpSrv.ListenAndServe()
			if err != nil && err != http.ErrServerClosed {
				s.logger.Error("http server error", "err", err)
				errCh <- err
				return
			}
			errCh <- nil
		}()
	}

	if s.httpsSrv != nil {
		servers++
		s.logger.Info("graphql https listening", "addr", s.httpsSrv.Addr)

		go func() {
			err := s.httpsSrv.ListenAndServeTLS("", "")
			if err != nil && err != http.ErrServerClosed {
				s.logger.Error("https server error", "err", err)
				errCh <- err
				return
			}
			errCh <- nil
		}()
	}

	if servers == 0 {
		return nil
	}

	var firstErr error
	for i := 0; i < servers; i++ {
		if err := <-errCh; err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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

	var firstErr error
	if s.httpSrv != nil {
		if err := s.httpSrv.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.httpsSrv != nil {
		if err := s.httpsSrv.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if started {
		select {
		case <-s.done:
		case <-ctx.Done():
			if firstErr == nil {
				firstErr = ctx.Err()
			}
		}
	}
	return firstErr
}

func requireHTTPSMiddleware(cfg *config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil || auth.IsLocalhostRequest(r) {
				next.ServeHTTP(w, r)
				return
			}

			host, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				host = r.Host
			}
			tlsPort := cfg.GraphQL.TLSPort
			uri := r.URL.RequestURI()
			if uri == "" {
				uri = r.URL.Path
			}
			if uri == "" {
				uri = "/"
			}
			var target string
			if tlsPort == 443 {
				target = fmt.Sprintf("https://%s%s", host, uri)
			} else {
				target = fmt.Sprintf("https://%s:%d%s", host, tlsPort, uri)
			}
			http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		})
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
