// Package oauth is the Hydra authorization-code client.
// It binds each login to the initiating browser, exchanges the code, and
// registers the verified identity.
package oauth

import (
	"context"
	"errors"
	"net/url"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/smusali/sandbox-api/internal/auth"
	"github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/postgres"
)

const (
	// BindingCookieName is the browser binding cookie written by login.
	BindingCookieName = "sandbox_oauth_binding"
	// BindingCookiePath limits that cookie to the callback.
	BindingCookiePath = "/auth/callback"

	stateSaveAttempts = 3

	providerServerError = "server_error"
	providerTemporary   = "temporarily_unavailable"
)

var hydraScopes = []string{"openid", "offline"}

// Kind classifies a login or callback failure for the HTTP layer.
type Kind int

const (
	// KindState means the OAuth state is missing, incorrect, expired, or consumed.
	KindState Kind = iota + 1
	// KindAuthorization means Hydra rejected the authorization code or consent.
	KindAuthorization
	// KindProvider means the upstream token response was invalid.
	KindProvider
	// KindUnavailable means a required dependency could not complete the flow.
	KindUnavailable
	// KindInternal means the flow failed for an unexpected reason.
	KindInternal
)

// Error is one authorization-code failure.
type Error struct {
	Kind       Kind
	KeepCookie bool
}

func (e *Error) Error() string {
	if e == nil {
		return "oauth failed"
	}
	switch e.Kind {
	case KindState:
		return "oauth state rejected"
	case KindAuthorization:
		return "oauth authorization failed"
	case KindProvider:
		return "oauth provider error"
	case KindUnavailable:
		return "oauth dependency unavailable"
	default:
		return "oauth failed"
	}
}

// Settings is the OAuth client used for login and the token exchange.
type Settings struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
	AuthURL      string
	TokenURL     string
	Audience     string
	StateTTL     time.Duration
	Timeout      time.Duration
}

// Login is a stored authorization transaction and the redirect that starts it.
type Login struct {
	Location string
	Binding  string
	Secure   bool
	MaxAge   int
}

// Session is a completed callback.
// ExpiresIn is nil when the provider omitted the lifetime.
type Session struct {
	AccessToken string
	ExpiresIn   *int
	UserID      uuid.UUID
}

// Flow runs the authorization-code login and callback.
type Flow struct {
	stateTTL     time.Duration
	timeout      time.Duration
	secureCookie bool
	audience     string
	oauth        *oauth2.Config
	states       *RedisStore
	tokens       *Exchanger
	verifier     *auth.Verifier
	users        *db.Queries
}

// New builds a flow that stores transactions in states and users in users.
func New(settings Settings, states *RedisStore, tokens *Exchanger, verifier *auth.Verifier, users *db.Queries) *Flow {
	return &Flow{
		stateTTL:     settings.StateTTL,
		timeout:      settings.Timeout,
		secureCookie: cookieIsSecure(settings.RedirectURI),
		audience:     settings.Audience,
		oauth: &oauth2.Config{
			ClientID:    settings.ClientID,
			RedirectURL: settings.RedirectURI,
			Scopes:      hydraScopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:  settings.AuthURL,
				TokenURL: settings.TokenURL,
			},
		},
		states:   states,
		tokens:   tokens,
		verifier: verifier,
		users:    users,
	}
}

// SecureCookie reports whether the binding cookie requires HTTPS.
func (f *Flow) SecureCookie() bool {
	return f.secureCookie
}

// Begin stores a login transaction and returns the Hydra redirect.
func (f *Flow) Begin(ctx context.Context) (Login, error) {
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	verifier := oauth2.GenerateVerifier()
	binding, err := randomSecret()
	if err != nil {
		return Login{}, &Error{Kind: KindInternal}
	}
	record := transaction{
		BrowserBinding: bindingHash(binding),
		CodeVerifier:   verifier,
	}

	for attempt := 0; attempt < stateSaveAttempts; attempt++ {
		state, err := randomSecret()
		if err != nil {
			return Login{}, &Error{Kind: KindInternal}
		}
		err = f.states.Save(ctx, state, record, f.stateTTL)
		if err == nil {
			return Login{
				Location: f.authorizeURL(state, verifier),
				Binding:  binding,
				Secure:   f.secureCookie,
				MaxAge:   int(f.stateTTL / time.Second),
			}, nil
		}
		if errors.Is(err, errStateExists) {
			continue
		}
		return Login{}, &Error{Kind: KindUnavailable}
	}
	return Login{}, &Error{Kind: KindInternal}
}

// Finish consumes the login transaction, exchanges the code, and registers the user.
// binding is the cookie value from the browser that started Begin.
// providerError is the error query parameter Hydra sent, or empty.
func (f *Flow) Finish(ctx context.Context, state, binding, code, providerError string) (Session, error) {
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	if !validSecret(state) || !validSecret(binding) {
		return Session{}, &Error{Kind: KindState}
	}
	verifier, err := f.states.Consume(ctx, state, bindingHash(binding))
	if err != nil {
		return Session{}, mapConsume(err)
	}
	if providerError != "" {
		if providerError == providerServerError || providerError == providerTemporary {
			return Session{}, &Error{Kind: KindUnavailable}
		}
		return Session{}, &Error{Kind: KindAuthorization}
	}
	if !validCode(code) {
		return Session{}, &Error{Kind: KindAuthorization}
	}

	access, err := f.tokens.Exchange(ctx, code, verifier)
	if err != nil {
		return Session{}, mapExchange(err)
	}
	identity, err := f.verifier.Verify(ctx, access.Value)
	if err != nil {
		return Session{}, mapVerify(err)
	}
	user, err := f.users.UpsertUser(ctx, db.UpsertUserParams{
		OauthIssuer:  identity.Issuer,
		OauthSubject: identity.Subject,
		Email:        optionalEmail(identity.Email),
	})
	if err != nil {
		return Session{}, mapUsers(err)
	}
	return Session{
		AccessToken: access.Value,
		ExpiresIn:   access.ExpiresIn,
		UserID:      user.ID,
	}, nil
}

func (f *Flow) authorizeURL(state, verifier string) string {
	return f.oauth.AuthCodeURL(
		state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("audience", f.audience),
	)
}

func mapConsume(err error) error {
	switch {
	case errors.Is(err, errStateMissing), errors.Is(err, errStateMismatch):
		return &Error{Kind: KindState}
	case errors.Is(err, errStateCorrupt), errors.Is(err, errStateReply):
		return &Error{Kind: KindInternal}
	default:
		return &Error{Kind: KindUnavailable, KeepCookie: true}
	}
}

func mapExchange(err error) error {
	switch {
	case errors.Is(err, errAuthorizationRejected):
		return &Error{Kind: KindAuthorization}
	case errors.Is(err, errProviderUnavailable):
		return &Error{Kind: KindUnavailable}
	default:
		return &Error{Kind: KindProvider}
	}
}

func mapVerify(err error) error {
	switch {
	case errors.Is(err, auth.ErrKeySetUnavailable):
		return &Error{Kind: KindUnavailable}
	case errors.Is(err, auth.ErrInvalidToken):
		return &Error{Kind: KindProvider}
	default:
		return &Error{Kind: KindInternal}
	}
}

func mapUsers(err error) error {
	if postgres.Unavailable(err) {
		return &Error{Kind: KindUnavailable}
	}
	return &Error{Kind: KindInternal}
}

func optionalEmail(email string) *string {
	if email == "" {
		return nil
	}
	return &email
}

func cookieIsSecure(redirectURI string) bool {
	parsed, err := url.Parse(redirectURI)
	if err != nil {
		return true
	}
	return parsed.Scheme == "https"
}
