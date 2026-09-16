package auth

import (
	"context"

	"monstermq.io/edge/internal/stores"
)

type principalKey struct{}

// WithPrincipal attaches an authenticated user to a request context.
func WithPrincipal(ctx context.Context, user stores.User) context.Context {
	return context.WithValue(ctx, principalKey{}, user)
}

// Principal returns the authenticated user attached to the context.
func Principal(ctx context.Context) (stores.User, bool) {
	u, ok := ctx.Value(principalKey{}).(stores.User)
	return u, ok
}
