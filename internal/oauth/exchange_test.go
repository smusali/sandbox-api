package oauth

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestExchangeReadsTheAccessToken(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		user, pass, err := basicUser(r.Header.Get("Authorization"))
		if err != nil || user != "id+x" || pass != "secret y" {
			t.Errorf("client = %q %q, err = %v", user, pass, err)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("form: %v", err)
		}
		if r.PostForm.Get("grant_type") != "authorization_code" ||
			r.PostForm.Get("code") != "auth-code" ||
			r.PostForm.Get("redirect_uri") != "http://localhost:8080/auth/callback" ||
			r.PostForm.Get("code_verifier") != "verifier-value" ||
			r.PostForm.Get("client_secret") != "" {
			t.Errorf("form = %s", r.PostForm.Encode())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"tok","token_type":"bearer","expires_in":3600,"refresh_token":"refresh","id_token":"id"}`)
	}))
	t.Cleanup(server.Close)

	exchanger := NewExchanger("id+x", "secret y", "http://localhost:8080/auth/callback", server.URL, time.Second)
	token, err := exchanger.Exchange(context.Background(), "auth-code", "verifier-value")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if token.Value != "tok" || token.ExpiresIn == nil || *token.ExpiresIn != 3600 {
		t.Fatalf("token = %+v", token)
	}
	if hits != 1 {
		t.Fatalf("requests = %d", hits)
	}
}

func TestExchangeKeepsExpiresInOnlyWhenPresent(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		present bool
		value   int
	}{
		{name: "omitted", body: `{"access_token":"tok","token_type":"Bearer"}`},
		{name: "null", body: `{"access_token":"tok","token_type":"Bearer","expires_in":null}`},
		{name: "zero", body: `{"access_token":"tok","token_type":"Bearer","expires_in":0}`, present: true, value: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := tokenServer(t, http.StatusOK, tt.body)
			exchanger := NewExchanger("id", "secret", "http://localhost:8080/auth/callback", server.URL, time.Second)
			token, err := exchanger.Exchange(context.Background(), "code", "verifier")
			if err != nil {
				t.Fatalf("Exchange() error = %v", err)
			}
			if tt.present {
				if token.ExpiresIn == nil || *token.ExpiresIn != tt.value {
					t.Fatalf("expires_in = %v", token.ExpiresIn)
				}
				return
			}
			if token.ExpiresIn != nil {
				t.Fatalf("expires_in = %d", *token.ExpiresIn)
			}
		})
	}
}

func TestExchangeClassifiesProviderResults(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "invalid grant", status: http.StatusBadRequest, body: `{"error":"invalid_grant"}`, want: errAuthorizationRejected},
		{name: "invalid client", status: http.StatusUnauthorized, body: `{"error":"invalid_client"}`, want: errAuthorizationRejected},
		{name: "client error without oauth error", status: http.StatusBadRequest, body: `{"message":"nope"}`, want: errProviderResponse},
		{name: "missing access token", status: http.StatusOK, body: `{"token_type":"bearer"}`, want: errProviderResponse},
		{name: "missing token type", status: http.StatusOK, body: `{"access_token":"tok"}`, want: errProviderResponse},
		{name: "negative lifetime", status: http.StatusOK, body: `{"access_token":"tok","token_type":"bearer","expires_in":-1}`, want: errProviderResponse},
		{name: "lifetime type", status: http.StatusOK, body: `{"access_token":"tok","token_type":"bearer","expires_in":"3600"}`, want: errProviderResponse},
		{name: "malformed json", status: http.StatusOK, body: `{"access_token":`, want: errProviderResponse},
		{name: "trailing json", status: http.StatusOK, body: `{"access_token":"tok","token_type":"bearer"}{}`, want: errProviderResponse},
		{name: "provider failure", status: http.StatusBadGateway, body: `nope`, want: errProviderUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(server.Close)

			exchanger := NewExchanger("id", "secret", "http://localhost:8080/auth/callback", server.URL, time.Second)
			_, err := exchanger.Exchange(context.Background(), "code", "verifier")
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if hits != 1 {
				t.Fatalf("requests = %d", hits)
			}
		})
	}
}

func TestExchangeRefusesRedirects(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "http://127.0.0.1:1/token", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	exchanger := NewExchanger("id", "secret", "http://localhost:8080/auth/callback", server.URL, time.Second)
	_, err := exchanger.Exchange(context.Background(), "code", "verifier")
	if !errors.Is(err, errProviderResponse) {
		t.Fatalf("error = %v", err)
	}
	if hits != 1 {
		t.Fatalf("requests = %d", hits)
	}
}

func TestExchangeDoesNotRetryATransportFailure(t *testing.T) {
	exchanger := NewExchanger("id", "secret", "http://localhost:8080/auth/callback", "http://127.0.0.1:1", time.Second)
	_, err := exchanger.Exchange(context.Background(), "code", "verifier")
	if !errors.Is(err, errProviderUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func TestExchangeRejectsAnOversizedBody(t *testing.T) {
	body := `{"access_token":"` + strings.Repeat("a", maxTokenBody) + `","token_type":"bearer"}`
	server := tokenServer(t, http.StatusOK, body)
	exchanger := NewExchanger("id", "secret", "http://localhost:8080/auth/callback", server.URL, time.Second)
	_, err := exchanger.Exchange(context.Background(), "code", "verifier")
	if !errors.Is(err, errProviderResponse) {
		t.Fatalf("error = %v", err)
	}
}

func tokenServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

func basicUser(header string) (string, string, error) {
	encoded, ok := strings.CutPrefix(header, "Basic ")
	if !ok {
		return "", "", errors.New("missing basic authorization")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", err
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", errors.New("missing basic separator")
	}
	user, err = url.QueryUnescape(user)
	if err != nil {
		return "", "", err
	}
	pass, err = url.QueryUnescape(pass)
	if err != nil {
		return "", "", err
	}
	return user, pass, nil
}
