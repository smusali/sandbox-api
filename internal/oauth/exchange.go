package oauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxTokenBody = 64 << 10

var (
	errAuthorizationRejected = errors.New("oauth authorization rejected")
	errProviderResponse      = errors.New("oauth provider response invalid")
	errProviderUnavailable   = errors.New("oauth provider unavailable")
	errProviderBusy          = errors.New("oauth provider busy")
	errRedirectRefused       = errors.New("oauth token redirect refused")
)

// accessToken is the access token taken from a token response.
// ExpiresIn is nil when the provider omitted the lifetime.
type accessToken struct {
	Value     string
	ExpiresIn *int
}

type tokenPayload struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   *int   `json:"expires_in"`
}

type tokenErrorPayload struct {
	Error string `json:"error"`
}

// Exchanger trades an authorization code for an access token.
type Exchanger struct {
	clientID     string
	clientSecret string
	redirectURI  string
	tokenURL     string
	client       *http.Client
}

// NewExchanger posts to tokenURL with HTTP Basic client authentication.
// timeout bounds the token request. Redirects are refused.
func NewExchanger(clientID, clientSecret, redirectURI, tokenURL string, timeout time.Duration) *Exchanger {
	return &Exchanger{
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURI:  redirectURI,
		tokenURL:     tokenURL,
		client: &http.Client{
			Timeout:       timeout,
			CheckRedirect: refuseRedirect,
		},
	}
}

// Exchange performs one authorization-code request.
// A provider 500 is attempted again because the fixture can reject a concurrent write.
// A timeout or a dropped connection is not retried.
func (e *Exchanger) Exchange(ctx context.Context, code, verifier string) (accessToken, error) {
	var token accessToken
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		token, err = e.exchangeOnce(ctx, code, verifier)
		if !errors.Is(err, errProviderBusy) {
			return token, err
		}
	}
	return accessToken{}, errProviderUnavailable
}

func (e *Exchanger) exchangeOnce(ctx context.Context, code, verifier string) (accessToken, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {e.redirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return accessToken{}, errProviderResponse
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", basicClient(e.clientID, e.clientSecret))

	resp, err := e.client.Do(req)
	if err != nil {
		if errors.Is(err, errRedirectRefused) {
			return accessToken{}, errProviderResponse
		}
		return accessToken{}, errProviderUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusInternalServerError {
		return accessToken{}, errProviderBusy
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBody+1))
	if err != nil {
		return accessToken{}, errProviderUnavailable
	}
	if len(body) > maxTokenBody {
		return accessToken{}, errProviderResponse
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return accessToken{}, classifyClientError(body)
	}
	if resp.StatusCode != http.StatusOK {
		return accessToken{}, errProviderResponse
	}
	return decodeAccessToken(body)
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return errRedirectRefused
}

func basicClient(id, secret string) string {
	user := url.QueryEscape(id)
	pass := url.QueryEscape(secret)
	raw := user + ":" + pass
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}

func classifyClientError(body []byte) error {
	var payload tokenErrorPayload
	if err := json.Unmarshal(body, &payload); err != nil || payload.Error == "" {
		return errProviderResponse
	}
	return errAuthorizationRejected
}

func decodeAccessToken(body []byte) (accessToken, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var payload tokenPayload
	if err := dec.Decode(&payload); err != nil {
		return accessToken{}, errProviderResponse
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return accessToken{}, errProviderResponse
	}
	if payload.AccessToken == "" || !strings.EqualFold(payload.TokenType, "bearer") {
		return accessToken{}, errProviderResponse
	}
	if payload.ExpiresIn != nil && *payload.ExpiresIn < 0 {
		return accessToken{}, errProviderResponse
	}
	return accessToken{Value: payload.AccessToken, ExpiresIn: payload.ExpiresIn}, nil
}
