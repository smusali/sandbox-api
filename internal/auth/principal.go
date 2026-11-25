package auth

import (
	"context"

	"github.com/google/uuid"
)

// Principal is the registered application user for one request.
type Principal struct {
	// UserID is the application user UUID resolved from the verified token.
	UserID uuid.UUID
}

// contextKey is the unexported request key for the authenticated user.
type contextKey struct{}

// WithPrincipal stores principal for later reads.
// An empty user id is ignored. A principal already stored on ctx is kept.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if principal.UserID == uuid.Nil {
		return ctx
	}
	if _, ok := PrincipalFrom(ctx); ok {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, principal)
}

// PrincipalFrom returns the registered user stored on ctx.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	if ctx == nil {
		return Principal{}, false
	}
	switch principal := ctx.Value(contextKey{}).(type) {
	case Principal:
		if principal.UserID == uuid.Nil {
			return Principal{}, false
		}
		return principal, true
	default:
		return Principal{}, false
	}
}
