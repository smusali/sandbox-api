// Package oauth is the Hydra authorization-code client.
// It binds each login to the initiating browser, exchanges the code, and
// registers the verified identity.
package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"

	"github.com/smusali/sandbox-api/internal/auth"
	"github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/httpapi"
	"github.com/smusali/sandbox-api/internal/postgres"
)

const (
	bindingCookie     = "sandbox_oauth_binding"
	bindingCookiePath = "/auth/callback"

	stateSaveAttempts = 3

	providerServerError = "server_error"
	providerTemporary   = "temporarily_unavailable"
)

var hydraScopes = []string{"openid", "offline"}

type stateStore interface {
	Save(ctx context.Context, state string, record transaction, ttl time.Duration) error
	Consume(ctx context.Context, state, bindingHash string) (string, error)
}

type exchanger interface {
	Exchange(ctx context.Context, code, verifier string) (accessToken, error)
}

type identityVerifier interface {
	Verify(ctx context.Context, raw string) (auth.Identity, error)
}

type userStore interface {
	UpsertUser(ctx context.Context, arg db.UpsertUserParams) (db.User, error)
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

// Flow serves the authorization-code login and callback.
type Flow struct {
	stateTTL     time.Duration
	timeout      time.Duration
	secureCookie bool
	audience     string
	oauth        *oauth2.Config
	states       stateStore
	tokens       exchanger
	verifier     identityVerifier
	users        userStore
}

type failure struct {
	public     *httpapi.Error
	keepCookie bool
}

func (f *failure) Error() string {
	if f == nil || f.public == nil {
		return "oauth failed"
	}
	return f.public.Error()
}

// New builds the login and callback handlers.
func New(settings Settings, states stateStore, tokens exchanger, verifier identityVerifier, users userStore) *Flow {
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

// Login starts an authorization-code request.
func (f *Flow) Login() http.Handler {
	return http.HandlerFunc(f.login)
}

// Callback completes an authorization-code request.
func (f *Flow) Callback() http.Handler {
	return http.HandlerFunc(f.callback)
}

func (f *Flow) login(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), f.timeout)
	defer cancel()

	verifier := oauth2.GenerateVerifier()
	binding, err := randomSecret()
	if err != nil {
		httpapi.WriteError(w, httpapi.Internal())
		return
	}
	record := transaction{
		BrowserBinding: bindingHash(binding),
		CodeVerifier:   verifier,
	}

	for attempt := 0; attempt < stateSaveAttempts; attempt++ {
		state, err := randomSecret()
		if err != nil {
			httpapi.WriteError(w, httpapi.Internal())
			return
		}
		err = f.states.Save(ctx, state, record, f.stateTTL)
		if err == nil {
			f.setBindingCookie(w, binding)
			httpapi.NoStore(w)
			http.Redirect(w, r, f.authorizeURL(state, verifier), http.StatusFound)
			return
		}
		if errors.Is(err, errStateExists) {
			continue
		}
		httpapi.WriteError(w, httpapi.ServiceUnavailable())
		return
	}
	httpapi.WriteError(w, httpapi.Internal())
}

func (f *Flow) callback(w http.ResponseWriter, r *http.Request) {
	if err := f.complete(w, r); err != nil {
		f.writeFailure(w, err)
	}
}

func (f *Flow) complete(w http.ResponseWriter, r *http.Request) *failure {
	ctx, cancel := context.WithTimeout(r.Context(), f.timeout)
	defer cancel()

	query := r.URL.Query()
	state := query.Get("state")
	if !validSecret(state) {
		return stateRejected()
	}
	cookie, err := r.Cookie(bindingCookie)
	if err != nil || cookie == nil || !validSecret(cookie.Value) {
		return stateRejected()
	}

	verifier, err := f.states.Consume(ctx, state, bindingHash(cookie.Value))
	if err != nil {
		return mapConsume(err)
	}
	if providerErr := query.Get("error"); providerErr != "" {
		if providerErr == providerServerError || providerErr == providerTemporary {
			return dependencyDown(false)
		}
		return authorizationRejected()
	}
	code := query.Get("code")
	if !validCode(code) {
		return authorizationRejected()
	}

	access, err := f.tokens.Exchange(ctx, code, verifier)
	if err != nil {
		return mapExchange(err)
	}
	identity, err := f.verifier.Verify(ctx, access.Value)
	if err != nil {
		return mapVerify(err)
	}
	user, err := f.users.UpsertUser(ctx, db.UpsertUserParams{
		OauthIssuer:  identity.Issuer,
		OauthSubject: identity.Subject,
		Email:        optionalEmail(identity.Email),
	})
	if err != nil {
		return mapUsers(err)
	}

	f.clearBindingCookie(w)
	httpapi.NoStore(w)
	httpapi.WriteToken(w, http.StatusOK, httpapi.NewTokenResponse(access.Value, access.ExpiresIn, httpapi.NewID(user.ID)))
	return nil
}

func (f *Flow) authorizeURL(state, verifier string) string {
	return f.oauth.AuthCodeURL(
		state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("audience", f.audience),
	)
}

func (f *Flow) setBindingCookie(w http.ResponseWriter, binding string) {
	http.SetCookie(w, f.bindingCookie(binding, int(f.stateTTL/time.Second)))
}

func (f *Flow) clearBindingCookie(w http.ResponseWriter) {
	http.SetCookie(w, f.bindingCookie("", -1))
}

func (f *Flow) bindingCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     bindingCookie,
		Value:    value,
		Path:     bindingCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   f.secureCookie,
	}
}

func (f *Flow) writeFailure(w http.ResponseWriter, err *failure) {
	if err == nil || !err.keepCookie {
		f.clearBindingCookie(w)
	}
	if err == nil || err.public == nil {
		httpapi.WriteError(w, httpapi.Internal())
		return
	}
	httpapi.WriteError(w, err.public)
}

func stateRejected() *failure {
	return &failure{public: httpapi.InvalidOAuthState()}
}

func authorizationRejected() *failure {
	return &failure{public: httpapi.OAuthAuthorizationFailed()}
}

func providerRejected() *failure {
	return &failure{public: httpapi.OAuthProviderError()}
}

func dependencyDown(keepCookie bool) *failure {
	return &failure{public: httpapi.ServiceUnavailable(), keepCookie: keepCookie}
}

func internalFailure() *failure {
	return &failure{public: httpapi.Internal()}
}

func mapConsume(err error) *failure {
	switch {
	case errors.Is(err, errStateMissing), errors.Is(err, errStateMismatch):
		return stateRejected()
	case errors.Is(err, errStateCorrupt), errors.Is(err, errStateReply):
		return internalFailure()
	default:
		return dependencyDown(true)
	}
}

func mapExchange(err error) *failure {
	switch {
	case errors.Is(err, errAuthorizationRejected):
		return authorizationRejected()
	case errors.Is(err, errProviderUnavailable):
		return dependencyDown(false)
	default:
		return providerRejected()
	}
}

func mapVerify(err error) *failure {
	switch {
	case errors.Is(err, auth.ErrKeySetUnavailable):
		return dependencyDown(false)
	case errors.Is(err, auth.ErrInvalidToken):
		return providerRejected()
	default:
		return internalFailure()
	}
}

func mapUsers(err error) *failure {
	if postgres.Unavailable(err) {
		return dependencyDown(false)
	}
	return internalFailure()
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
