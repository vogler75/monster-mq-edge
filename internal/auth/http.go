package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
)

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
