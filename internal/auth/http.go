package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"

	"monstermq.io/edge/internal/stores"
)

// LocalhostUser is the synthetic user representing an unauthenticated localhost connection.
var LocalhostUser = stores.User{
	Username:     "localhost",
	Enabled:      true,
	CanSubscribe: true,
	CanPublish:   true,
	IsAdmin:      true,
}

// IsLocalhost returns true if the remote address string resolves to IPv4 127.0.0.1, IPv6 ::1, or localhost.
func IsLocalhost(remoteAddr string) bool {
	if remoteAddr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// IsLocalhostRequest returns true if the HTTP request originated from IPv4 127.0.0.1.
func IsLocalhostRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	return IsLocalhost(r.RemoteAddr)
}


// AuthenticateHeader applies the same Basic and bearer credentials to HTTP APIs.
func AuthenticateHeader(ctx context.Context, cache *Cache, header string) (context.Context, error) {
	if strings.TrimSpace(header) == "" {
		return ctx, nil
	}
	parts := strings.Fields(header)
	if len(parts) != 2 {
		return ctx, errors.New("invalid authorization header")
	}
	switch {
	case strings.EqualFold(parts[0], "Basic"):
		raw, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return ctx, errors.New("invalid basic credentials")
		}
		username, password, ok := strings.Cut(string(raw), ":")
		if !ok {
			return ctx, errors.New("invalid basic credentials")
		}
		user, valid := cache.Authenticate(ctx, username, password)
		if !valid {
			return ctx, errors.New("invalid basic credentials")
		}
		return WithPrincipal(ctx, *user), nil
	case strings.EqualFold(parts[0], "Bearer"):
		user, valid := cache.ValidateSession(parts[1])
		if !valid {
			return ctx, errors.New("invalid or expired session token")
		}
		return WithPrincipal(ctx, user), nil
	default:
		return ctx, errors.New("unsupported authorization scheme")
	}
}
