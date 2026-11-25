package httpapi

import (
	"errors"

	"github.com/smusali/sandbox-api/internal/auth"
)

// TokenError maps a token verification failure onto the public error catalog.
// An unrelated error returns nil.
func TokenError(err error) *Error {
	var failure *auth.Error
	if !errors.As(err, &failure) || failure == nil {
		return nil
	}
	if errors.Is(failure, auth.ErrKeySetUnavailable) {
		return ServiceUnavailable()
	}
	if errors.Is(failure, auth.ErrInvalidToken) {
		return InvalidToken()
	}
	return nil
}
