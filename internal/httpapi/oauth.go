package httpapi

import (
	"errors"
	"net/http"

	"github.com/smusali/sandbox-api/internal/oauth"
)

// OAuth serves the authorization-code login and callback.
type OAuth struct {
	flow *oauth.Flow
}

// NewOAuth binds an authorization-code flow to the HTTP handlers.
func NewOAuth(flow *oauth.Flow) *OAuth {
	return &OAuth{flow: flow}
}

// Login handles GET /auth/login.
func (h *OAuth) Login() http.Handler {
	return http.HandlerFunc(h.login)
}

// Callback handles GET /auth/callback.
func (h *OAuth) Callback() http.Handler {
	return http.HandlerFunc(h.callback)
}

func (h *OAuth) login(w http.ResponseWriter, r *http.Request) {
	login, err := h.flow.Begin(r.Context())
	if err != nil {
		WriteError(w, oauthPublic(err))
		return
	}
	http.SetCookie(w, bindingCookie(login.Binding, login.MaxAge, login.Secure))
	NoStore(w)
	http.Redirect(w, r, login.Location, http.StatusFound)
}

func (h *OAuth) callback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	binding := ""
	if cookie, err := r.Cookie(oauth.BindingCookieName); err == nil && cookie != nil {
		binding = cookie.Value
	}
	session, err := h.flow.Finish(r.Context(), query.Get("state"), binding, query.Get("code"), query.Get("error"))
	if err != nil {
		h.writeFailure(w, err)
		return
	}
	http.SetCookie(w, bindingCookie("", -1, h.flow.SecureCookie()))
	NoStore(w)
	WriteToken(w, http.StatusOK, NewTokenResponse(session.AccessToken, session.ExpiresIn, NewID(session.UserID)))
}

func (h *OAuth) writeFailure(w http.ResponseWriter, err error) {
	var failure *oauth.Error
	if !errors.As(err, &failure) || failure == nil || !failure.KeepCookie {
		http.SetCookie(w, bindingCookie("", -1, h.flow.SecureCookie()))
	}
	WriteError(w, oauthPublic(err))
}

func bindingCookie(value string, maxAge int, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     oauth.BindingCookieName,
		Value:    value,
		Path:     oauth.BindingCookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	}
}

func oauthPublic(err error) error {
	var failure *oauth.Error
	if !errors.As(err, &failure) || failure == nil {
		return Internal()
	}
	switch failure.Kind {
	case oauth.KindState:
		return InvalidOAuthState()
	case oauth.KindAuthorization:
		return OAuthAuthorizationFailed()
	case oauth.KindProvider:
		return OAuthProviderError()
	case oauth.KindUnavailable:
		return ServiceUnavailable()
	default:
		return Internal()
	}
}
