package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/smusali/sandbox-api/internal/auth"
	"github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/postgres"
)

var errEmptyUserID = errors.New("registered user has an empty id")

// Authenticate requires one verified access token and a registered user.
// The user id is stored on the request context. A missing registration is
// rejected. This middleware does not create users and does not decide
// project membership.
func Authenticate(verifier *auth.Verifier, users *db.Queries, timeout time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			principal, err := authenticate(ctx, r.Header.Values("Authorization"), verifier, users)
			cancel()
			if err != nil {
				WriteError(w, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), principal)))
		})
	}
}

func authenticate(ctx context.Context, values []string, verifier *auth.Verifier, users *db.Queries) (auth.Principal, error) {
	token, err := bearerToken(values)
	if err != nil {
		return auth.Principal{}, Unauthenticated()
	}
	identity, err := verifier.Verify(ctx, token)
	if err != nil {
		return auth.Principal{}, verificationFailure(err)
	}
	user, err := users.GetUserByIdentity(ctx, db.GetUserByIdentityParams{
		OauthIssuer:  identity.Issuer,
		OauthSubject: identity.Subject,
	})
	if err != nil {
		return auth.Principal{}, userLookupFailure(err)
	}
	if user.ID == uuid.Nil {
		return auth.Principal{}, errEmptyUserID
	}
	return auth.Principal{UserID: user.ID}, nil
}

func verificationFailure(err error) error {
	if apiErr := TokenError(err); apiErr != nil {
		return apiErr
	}
	if postgres.Unavailable(err) {
		return ServiceUnavailable()
	}
	return Internal()
}

func userLookupFailure(err error) error {
	// A verified token with no application user is rejected here.
	if errors.Is(err, pgx.ErrNoRows) {
		return Unauthenticated()
	}
	if postgres.Unavailable(err) {
		return ServiceUnavailable()
	}
	return err
}
