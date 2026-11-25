// Package auth verifies Hydra-issued access tokens using a process-local
// cache of trusted signing keys, and carries the registered user for a
// request.
package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	// clockSkew is how far exp, nbf, and iat may differ from the local clock.
	clockSkew = time.Minute

	headerJKU  jose.HeaderKey = "jku"
	headerX5U  jose.HeaderKey = "x5u"
	headerCrit jose.HeaderKey = "crit"
)

var (
	// ErrInvalidToken means the presented credential is not a usable access token.
	ErrInvalidToken = errors.New("invalid access token")

	// ErrKeySetUnavailable means a required signing-key retrieval failed.
	ErrKeySetUnavailable = errors.New("signing keys unavailable")

	errUntrustedHeader = errors.New("untrusted token header")
	errMissingKeyID    = errors.New("missing key identifier")
	errMissingExpiry   = errors.New("missing expiration")
	errMissingSubject  = errors.New("missing subject")
	errTokenPurpose    = errors.New("token is not an access token")
	errUnknownKey      = errors.New("unknown signing key")
)

// Identity is the OAuth identity taken from a verified access token.
// Email is optional profile metadata. It is empty when the token does not
// include a verified email address, and it is not the application identity.
type Identity struct {
	Issuer  string
	Subject string
	Email   string
}

// Error is one token verification failure.
// Unwrap yields ErrInvalidToken or ErrKeySetUnavailable.
type Error struct {
	kind  error
	cause error
}

func (e *Error) Error() string {
	if e == nil || e.kind == nil {
		return ErrInvalidToken.Error()
	}
	if e.cause == nil {
		return e.kind.Error()
	}
	return e.kind.Error() + ": " + e.cause.Error()
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.kind
}

// Verifier checks compact access tokens against one trusted key set.
type Verifier struct {
	issuer   string
	audience string
	clientID string
	now      func() time.Time
	keys     *keyCache
}

// NewVerifier verifies tokens for issuer and audience.
// clientID is rejected when it appears in the token audience, which is how
// ID tokens are distinguished from access tokens. jwksURL is the only URL
// the verifier fetches. timeout bounds each key-set retrieval.
func NewVerifier(issuer, audience, clientID, jwksURL string, timeout time.Duration) *Verifier {
	now := time.Now
	return &Verifier{
		issuer:   issuer,
		audience: audience,
		clientID: clientID,
		now:      now,
		keys:     newKeyCache(jwksURL, timeout, now),
	}
}

// Verify checks one compact access token and returns its verified identity.
func (v *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	parsed, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil || parsed == nil {
		return Identity{}, invalidToken(err)
	}
	if len(parsed.Headers) != 1 {
		return Identity{}, invalidToken(errUntrustedHeader)
	}
	header := parsed.Headers[0]
	if err := rejectHeader(header); err != nil {
		return Identity{}, invalidToken(err)
	}
	if header.KeyID == "" {
		return Identity{}, invalidToken(errMissingKeyID)
	}

	key, err := v.keys.publicKey(ctx, header.KeyID)
	if err != nil {
		return Identity{}, err
	}

	var claims accessClaims
	if err := parsed.Claims(key, &claims); err != nil {
		return Identity{}, invalidToken(err)
	}
	if err := v.accept(claims); err != nil {
		return Identity{}, invalidToken(err)
	}
	return Identity{
		Issuer:  v.issuer,
		Subject: claims.Subject,
		Email:   trustedEmail(claims.Email, claims.EmailVerified),
	}, nil
}

type accessClaims struct {
	jwt.Claims
	Email         string `json:"email,omitempty"`
	EmailVerified *bool  `json:"email_verified,omitempty"`
}

func (v *Verifier) accept(claims accessClaims) error {
	if claims.Expiry == nil {
		return errMissingExpiry
	}
	if err := claims.ValidateWithLeeway(jwt.Expected{
		Issuer:      v.issuer,
		AnyAudience: jwt.Audience{v.audience},
		Time:        v.now(),
	}, clockSkew); err != nil {
		return err
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return errMissingSubject
	}
	if v.clientID != "" && claims.Audience.Contains(v.clientID) {
		return errTokenPurpose
	}
	return nil
}

func trustedEmail(email string, verified *bool) string {
	if verified == nil || !*verified {
		return ""
	}
	if strings.TrimSpace(email) == "" {
		return ""
	}
	return email
}

func rejectHeader(header jose.Header) error {
	if header.Algorithm != string(jose.RS256) {
		return errUntrustedHeader
	}
	if header.JSONWebKey != nil || headerPresent(header, headerJKU) || headerPresent(header, headerX5U) || headerPresent(header, headerCrit) {
		return errUntrustedHeader
	}
	if !allowedTokenType(header) {
		return errUntrustedHeader
	}
	return nil
}

func headerPresent(header jose.Header, key jose.HeaderKey) bool {
	_, ok := header.ExtraHeaders[key]
	return ok
}

func allowedTokenType(header jose.Header) bool {
	value, ok := header.ExtraHeaders[jose.HeaderType]
	if !ok || value == nil {
		return true
	}
	text, isString := headerString(header, jose.HeaderType)
	if !isString {
		return false
	}
	switch strings.ToUpper(text) {
	case "JWT", "AT+JWT":
		return true
	default:
		return false
	}
}

func headerString(header jose.Header, key jose.HeaderKey) (string, bool) {
	value, ok := header.ExtraHeaders[key]
	if !ok || value == nil {
		return "", false
	}
	switch text := value.(type) {
	case string:
		return text, true
	default:
		return "", false
	}
}

func invalidToken(cause error) error {
	return &Error{kind: ErrInvalidToken, cause: cause}
}

func unavailable(cause error) error {
	return &Error{kind: ErrKeySetUnavailable, cause: cause}
}
