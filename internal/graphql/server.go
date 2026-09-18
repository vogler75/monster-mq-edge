package graphql

import (
	"context"
	"fmt"
	"log/slog"
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
	"monstermq.io/edge/internal/graphql/generated"
	"monstermq.io/edge/internal/graphql/resolvers"
	"monstermq.io/edge/internal/hmi"
	"monstermq.io/edge/internal/redfish"
	"monstermq.io/edge/internal/restapi"
)

// Server hosts the GraphQL HTTP and WebSocket endpoints, HMI dashboards, and Redfish API.
type Server struct {
	cfg     *config.Config
	logger  *slog.Logger
	router  *chi.Mux
	httpSrv *http.Server

	mu      sync.Mutex
	started bool
	stopped bool
	done    chan struct{}
}

func NewServer(cfg *config.Config, resolver *resolvers.Resolver, hmiMgr *hmi.Manager, redfishMgr *redfish.Manager, rest *restapi.Handler, logger *slog.Logger) *Server {
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
	authenticatedGQL := httpAuthMiddleware(cfg, authCache, gql)
	r.Handle("/graphql", authenticatedGQL)
	r.Handle("/graphql/", authenticatedGQL)
	// Apollo-style alias the existing dashboard might use.
	r.Handle("/query", authenticatedGQL)
	r.Get("/playground", playground.Handler("MonsterMQ Edge", "/graphql"))
	if cfg.RestApi.Enabled && rest != nil {
		r.Mount("/api/v1", rest.Router())
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

	return &Server{
		cfg:    cfg,
		logger: logger,
		router: r,
		httpSrv: &http.Server{
			Addr:              fmt.Sprintf(":%d", cfg.GraphQL.Port),
			Handler:           r,
			ReadHeaderTimeout: 10 * time.Second,
		},
		done: make(chan struct{}),
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

	s.logger.Info("graphql listening", "port", s.cfg.GraphQL.Port)
	if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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

	err := s.httpSrv.Shutdown(ctx)
	if started {
		select {
		case <-s.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
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
