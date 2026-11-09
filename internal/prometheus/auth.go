package prometheus

import (
	"encoding/base64"
	"net/http"
	"strings"

	"monstermq.io/edge/internal/auth"
)

const (
	basicRealmHeader  = `Basic realm="MonsterMQ Prometheus"`
	bearerErrorHeader = `Bearer error="invalid_token"`
)

type errorResponse struct {
	Error string `json:"error"`
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.deps.Cfg == nil || !s.deps.Cfg.UserManagement.Enabled {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			if s.deps.Cfg.UserManagement.AllowAnonymousLocalhost && auth.IsLocalhostRequest(r) {
				ctx := auth.WithLocalhostPrincipal(r.Context())
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			anonAllowed := s.deps.Cfg.UserManagement.AnonymousEnabled
			if s.deps.Auth != nil {
				anonAllowed = s.deps.Auth.IsAnonymousEnabled()
			}
			if anonAllowed {
				next.ServeHTTP(w, r)
				return
			}
			writeUnauthorized(w, basicRealmHeader, "Authentication required")
			return
		}

		if len(authHeader) >= 7 && strings.EqualFold(authHeader[:7], "Bearer ") {
			token := authHeader[7:]
			if s.deps.Auth != nil {
				if user, ok := s.deps.Auth.ValidateSession(token); ok {
					ctx := auth.WithPrincipal(r.Context(), user)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
			writeUnauthorized(w, bearerErrorHeader, "Invalid or expired token")
			return
		}

		if len(authHeader) >= 6 && strings.EqualFold(authHeader[:6], "Basic ") {
			raw, err := base64.StdEncoding.DecodeString(authHeader[6:])
			if err == nil {
				username, password, hasColon := strings.Cut(string(raw), ":")
				if hasColon {
					if s.deps.Auth != nil {
						if user, ok := s.deps.Auth.Authenticate(r.Context(), username, password); ok {
							ctx := auth.WithPrincipal(r.Context(), *user)
							next.ServeHTTP(w, r.WithContext(ctx))
							return
						}
					}
					writeUnauthorized(w, basicRealmHeader, "Invalid credentials")
					return
				}
			}
		}

		writeUnauthorized(w, basicRealmHeader, "Authentication required")
	})
}

func writeUnauthorized(w http.ResponseWriter, wwwAuth, msg string) {
	w.Header().Set("WWW-Authenticate", wwwAuth)
	writeJSON(w, http.StatusUnauthorized, errorResponse{Error: msg})
}

func (s *Server) canRead(r *http.Request, topic string) bool {
	if s.deps.Cfg == nil || !s.deps.Cfg.UserManagement.Enabled {
		return true
	}
	if u, ok := auth.Principal(r.Context()); ok {
		if u.IsAdmin {
			return true
		}
		if s.deps.Auth == nil {
			return false
		}
		return s.deps.Auth.Allow(u.Username, topic, false)
	}
	if s.deps.Auth == nil {
		return s.deps.Cfg.UserManagement.AnonymousEnabled
	}
	return s.deps.Auth.Allow("", topic, false)
}
