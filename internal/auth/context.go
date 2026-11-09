package auth

import (
	"context"

	"monstermq.io/edge/internal/stores"
)

type principalKey struct{}

type localhostKey struct{}

// WithPrincipal attaches an authenticated user to a request context.
func WithPrincipal(ctx context.Context, user stores.User) context.Context {
	return context.WithValue(ctx, principalKey{}, user)
}

// WithLocalhostPrincipal attaches the synthetic LocalhostUser of an
// unauthenticated localhost request (UserManagement.AllowAnonymousLocalhost)
// and marks the context, so topic checks can tell it from a stored user that
// happens to be named "localhost".
func WithLocalhostPrincipal(ctx context.Context) context.Context {
	return context.WithValue(WithPrincipal(ctx, LocalhostUser), localhostKey{}, true)
}

// IsLocalhostPrincipal reports whether the context carries the synthetic
// LocalhostUser set by WithLocalhostPrincipal. That user is not in the user
// store, so topic checks must allow it instead of looking it up.
func IsLocalhostPrincipal(ctx context.Context) bool {
	ok, _ := ctx.Value(localhostKey{}).(bool)
	return ok
}

// Principal returns the authenticated user attached to the context.
func Principal(ctx context.Context) (stores.User, bool) {
	u, ok := ctx.Value(principalKey{}).(stores.User)
	return u, ok
}
