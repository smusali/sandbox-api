package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/oauth2"

	"github.com/smusali/sandbox-api/internal/auth"
	"github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/httpapi"
)

func TestLoginRedirectsAndBindsTheBrowser(t *testing.T) {
	store := &fakeStore{}
	flow := New(testSettings("http://localhost:8080/auth/callback"), store, &fakeExchanger{}, &fakeVerifier{}, &fakeUsers{})

	rec := httptest.NewRecorder()
	flow.Login().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	if strings.Contains(rec.Header().Get("Location"), "e2b-assignment-secret") {
		t.Fatalf("location contains the client secret: %s", rec.Header().Get("Location"))
	}

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("location: %v", err)
	}
	if loc.Scheme != "http" || loc.Host != "localhost:4444" || loc.Path != "/oauth2/auth" {
		t.Fatalf("location = %s", loc)
	}
	query := loc.Query()
	if query.Get("response_type") != "code" ||
		query.Get("client_id") != "e2b-assignment" ||
		query.Get("redirect_uri") != "http://localhost:8080/auth/callback" ||
		query.Get("scope") != "openid offline" ||
		query.Get("code_challenge_method") != "S256" ||
		query.Get("audience") != "urn:e2b:sandbox-api" {
		t.Fatalf("query = %s", loc.RawQuery)
	}
	state := query.Get("state")
	if !validSecret(state) {
		t.Fatalf("state = %q", state)
	}
	record, ok := store.get(state)
	if !ok {
		t.Fatal("state was not stored")
	}
	if query.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(record.CodeVerifier) {
		t.Fatalf("challenge = %q", query.Get("code_challenge"))
	}

	cookie := setCookie(t, rec)
	if !cookie.HttpOnly || cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie = %#v", cookie)
	}
	if cookie.Name != bindingCookie || cookie.Path != bindingCookiePath || cookie.MaxAge != 300 {
		t.Fatalf("cookie = %#v", cookie)
	}
	if !validSecret(cookie.Value) || cookie.Value == record.BrowserBinding {
		t.Fatalf("cookie value = %q, stored binding = %q", cookie.Value, record.BrowserBinding)
	}
	if record.BrowserBinding != bindingHash(cookie.Value) {
		t.Fatalf("stored binding = %q", record.BrowserBinding)
	}
}

func TestLoginCookieIsSecureForHTTPS(t *testing.T) {
	flow := New(testSettings("https://api.example.com/auth/callback"), &fakeStore{}, &fakeExchanger{}, &fakeVerifier{}, &fakeUsers{})
	rec := httptest.NewRecorder()
	flow.Login().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	cookie := setCookie(t, rec)
	if !cookie.Secure {
		t.Fatal("Secure is false")
	}
}

func TestLoginLeavesNoCookieWhenRedisIsDown(t *testing.T) {
	store := &fakeStore{saveErr: downError{}}
	flow := New(testSettings("http://localhost:8080/auth/callback"), store, &fakeExchanger{}, &fakeVerifier{}, &fakeUsers{})
	rec := httptest.NewRecorder()
	flow.Login().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	assertAPIError(t, rec, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "A required service is unavailable.")
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("Set-Cookie = %q", rec.Header().Get("Set-Cookie"))
	}
	if store.saveCalls() != 1 || store.len() != 0 {
		t.Fatalf("saves = %d, records = %d", store.saveCalls(), store.len())
	}
}

func TestLoginRetriesWhenStateExists(t *testing.T) {
	store := &fakeStore{failNX: 1}
	flow := New(testSettings("http://localhost:8080/auth/callback"), store, &fakeExchanger{}, &fakeVerifier{}, &fakeUsers{})
	rec := httptest.NewRecorder()
	flow.Login().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if store.saveCalls() != 2 || store.len() != 1 {
		t.Fatalf("saves = %d, records = %d", store.saveCalls(), store.len())
	}
}

func TestLoginDoesNotOverwriteAnExistingState(t *testing.T) {
	store := &fakeStore{failNX: stateSaveAttempts}
	flow := New(testSettings("http://localhost:8080/auth/callback"), store, &fakeExchanger{}, &fakeVerifier{}, &fakeUsers{})
	rec := httptest.NewRecorder()
	flow.Login().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))

	assertAPIError(t, rec, http.StatusInternalServerError, "INTERNAL_ERROR", "The request could not be completed.")
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("Set-Cookie = %q", rec.Header().Get("Set-Cookie"))
	}
	if store.saveCalls() != stateSaveAttempts || store.len() != 0 {
		t.Fatalf("saves = %d, records = %d", store.saveCalls(), store.len())
	}
}

func TestLoginKeepsEarlierTransactions(t *testing.T) {
	store := &fakeStore{}
	flow := New(testSettings("http://localhost:8080/auth/callback"), store, &fakeExchanger{}, &fakeVerifier{}, &fakeUsers{})
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		flow.Login().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d", rec.Code)
		}
	}
	if store.len() != 2 {
		t.Fatalf("records = %d", store.len())
	}
}

func TestCallbackSuccess(t *testing.T) {
	fix := newCallback(t)
	expires := 3600
	fix.tokens.token.ExpiresIn = &expires

	rec := httptest.NewRecorder()
	fix.flow.Callback().ServeHTTP(rec, fix.request())

	userID := fix.users.onlyID(t)
	assertToken(t, rec, userID, &expires)
	if rec.Header().Get("Location") != "" {
		t.Fatalf("Location = %q", rec.Header().Get("Location"))
	}
	clearedCookie(t, rec)
	if fix.tokens.calls() != 1 || fix.tokens.lastCode != "auth-code" || fix.tokens.lastVerifier != fix.verifierSecret {
		t.Fatalf("exchange = %+v", fix.tokens)
	}
	if fix.verifier.calls() != 1 || fix.verifier.last != "access-token-value" {
		t.Fatalf("verify calls = %d", fix.verifier.calls())
	}
	if fix.users.calls() != 1 || fix.users.last.Email != nil {
		t.Fatalf("upsert = %+v", fix.users.last)
	}
	if fix.users.last.OauthIssuer != "http://localhost:4444" || fix.users.last.OauthSubject != "foo@bar.com" {
		t.Fatalf("identity = %+v", fix.users.last)
	}
	if _, ok := fix.store.get(fix.state); ok {
		t.Fatal("state was not consumed")
	}
}

func TestCallbackOmitsExpiresInWhenTheProviderDoes(t *testing.T) {
	fix := newCallback(t)
	fix.tokens.token.ExpiresIn = nil

	rec := httptest.NewRecorder()
	fix.flow.Callback().ServeHTTP(rec, fix.request())

	assertToken(t, rec, fix.users.onlyID(t), nil)
}

func TestCallbackKeepsZeroExpiresIn(t *testing.T) {
	fix := newCallback(t)
	zero := 0
	fix.tokens.token.ExpiresIn = &zero

	rec := httptest.NewRecorder()
	fix.flow.Callback().ServeHTTP(rec, fix.request())

	assertToken(t, rec, fix.users.onlyID(t), &zero)
}

func TestCallbackPreservesTheUserWhenEmailChanges(t *testing.T) {
	fix := newCallback(t)
	first := httptest.NewRecorder()
	fix.flow.Callback().ServeHTTP(first, fix.request())
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", first.Code, first.Body.String())
	}
	if fix.users.last.Email != nil {
		t.Fatalf("email = %v", fix.users.last.Email)
	}

	fix.reseed()
	fix.verifier.identity.Email = "foo@bar.com"
	second := httptest.NewRecorder()
	fix.flow.Callback().ServeHTTP(second, fix.request())

	assertToken(t, second, fix.users.onlyID(t), fix.tokens.token.ExpiresIn)
	if firstUser(t, first) != firstUser(t, second) {
		t.Fatalf("user ids = %s and %s", first.Body.String(), second.Body.String())
	}
	if fix.users.last.Email == nil || *fix.users.last.Email != "foo@bar.com" {
		t.Fatalf("email = %v", fix.users.last.Email)
	}
	if fix.users.calls() != 2 {
		t.Fatalf("upserts = %d", fix.users.calls())
	}
}

func TestConcurrentCallbacksShareOneUser(t *testing.T) {
	fix := newCallback(t)
	first := fix.request()
	fix.reseed()
	second := fix.request()

	type result struct {
		id  string
		err string
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, req := range []*http.Request{first, second} {
		wg.Add(1)
		go func(req *http.Request) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			fix.flow.Callback().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				results <- result{err: rec.Body.String()}
				return
			}
			var body httpapi.TokenResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				results <- result{err: err.Error()}
				return
			}
			results <- result{id: body.UserID}
		}(req)
	}
	wg.Wait()
	close(results)

	var seen string
	var count int
	for result := range results {
		count++
		if result.err != "" {
			t.Fatalf("callback: %s", result.err)
		}
		if seen == "" {
			seen = result.id
			continue
		}
		if result.id != seen {
			t.Fatalf("user ids = %s and %s", seen, result.id)
		}
	}
	if count != 2 || seen == "" || fix.users.count() != 1 {
		t.Fatalf("callbacks = %d, users = %d, id = %q", count, fix.users.count(), seen)
	}
}

func TestReplayedCallbackIsRejected(t *testing.T) {
	fix := newCallback(t)
	req := fix.request()
	first := httptest.NewRecorder()
	fix.flow.Callback().ServeHTTP(first, req)
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", first.Code, first.Body.String())
	}

	second := httptest.NewRecorder()
	fix.flow.Callback().ServeHTTP(second, req)
	assertAPIError(t, second, http.StatusBadRequest, "INVALID_OAUTH_STATE", "The OAuth state is invalid.")
	if fix.tokens.calls() != 1 || fix.users.calls() != 1 {
		t.Fatalf("exchange = %d, upserts = %d", fix.tokens.calls(), fix.users.calls())
	}
}

func TestCallbackFailures(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*callbackFixture)
		status       int
		code         string
		message      string
		keepState    bool
		keepCookie   bool
		consumed     bool
		exchangeCall bool
		userCall     bool
	}{
		{
			name: "missing state",
			mutate: func(fix *callbackFixture) {
				fix.query.Del("state")
			},
			status:     http.StatusBadRequest,
			code:       "INVALID_OAUTH_STATE",
			message:    "The OAuth state is invalid.",
			keepState:  true,
			keepCookie: false,
		},
		{
			name: "malformed state",
			mutate: func(fix *callbackFixture) {
				fix.query.Set("state", "not-a-state")
			},
			status:     http.StatusBadRequest,
			code:       "INVALID_OAUTH_STATE",
			message:    "The OAuth state is invalid.",
			keepState:  true,
			keepCookie: false,
		},
		{
			name: "incorrect state",
			mutate: func(fix *callbackFixture) {
				fix.query.Set("state", mustSecret(nil))
			},
			status:     http.StatusBadRequest,
			code:       "INVALID_OAUTH_STATE",
			message:    "The OAuth state is invalid.",
			keepState:  true,
			keepCookie: false,
			consumed:   true,
		},
		{
			name: "missing cookie",
			mutate: func(fix *callbackFixture) {
				fix.includeCookie = false
			},
			status:     http.StatusBadRequest,
			code:       "INVALID_OAUTH_STATE",
			message:    "The OAuth state is invalid.",
			keepState:  true,
			keepCookie: false,
		},
		{
			name: "malformed cookie",
			mutate: func(fix *callbackFixture) {
				fix.binding = "not-a-binding"
			},
			status:     http.StatusBadRequest,
			code:       "INVALID_OAUTH_STATE",
			message:    "The OAuth state is invalid.",
			keepState:  true,
			keepCookie: false,
		},
		{
			name: "browser mismatch",
			mutate: func(fix *callbackFixture) {
				fix.binding = mustSecret(nil)
			},
			status:     http.StatusBadRequest,
			code:       "INVALID_OAUTH_STATE",
			message:    "The OAuth state is invalid.",
			keepState:  true,
			keepCookie: false,
			consumed:   true,
		},
		{
			name: "expired state",
			mutate: func(fix *callbackFixture) {
				fix.store.delete(fix.state)
			},
			status:     http.StatusBadRequest,
			code:       "INVALID_OAUTH_STATE",
			message:    "The OAuth state is invalid.",
			keepState:  false,
			keepCookie: false,
			consumed:   true,
		},
		{
			name: "missing code",
			mutate: func(fix *callbackFixture) {
				fix.query.Del("code")
			},
			status:     http.StatusBadRequest,
			code:       "OAUTH_AUTHORIZATION_FAILED",
			message:    "OAuth authorization failed.",
			keepState:  false,
			keepCookie: false,
			consumed:   true,
		},
		{
			name: "denied consent",
			mutate: func(fix *callbackFixture) {
				fix.query.Set("error", "access_denied")
			},
			status:     http.StatusBadRequest,
			code:       "OAUTH_AUTHORIZATION_FAILED",
			message:    "OAuth authorization failed.",
			keepState:  false,
			keepCookie: false,
			consumed:   true,
		},
		{
			name: "provider unavailable during authorization",
			mutate: func(fix *callbackFixture) {
				fix.query.Set("error", "server_error")
			},
			status:     http.StatusServiceUnavailable,
			code:       "SERVICE_UNAVAILABLE",
			message:    "A required service is unavailable.",
			keepState:  false,
			keepCookie: false,
			consumed:   true,
		},
		{
			name: "temporarily unavailable",
			mutate: func(fix *callbackFixture) {
				fix.query.Set("error", "temporarily_unavailable")
			},
			status:     http.StatusServiceUnavailable,
			code:       "SERVICE_UNAVAILABLE",
			message:    "A required service is unavailable.",
			keepState:  false,
			keepCookie: false,
			consumed:   true,
		},
		{
			name: "invalid grant",
			mutate: func(fix *callbackFixture) {
				fix.tokens.err = errAuthorizationRejected
			},
			status:       http.StatusBadRequest,
			code:         "OAUTH_AUTHORIZATION_FAILED",
			message:      "OAuth authorization failed.",
			keepState:    false,
			keepCookie:   false,
			consumed:     true,
			exchangeCall: true,
		},
		{
			name: "malformed token response",
			mutate: func(fix *callbackFixture) {
				fix.tokens.err = errProviderResponse
			},
			status:       http.StatusBadGateway,
			code:         "OAUTH_PROVIDER_ERROR",
			message:      "The OAuth provider returned an invalid response.",
			keepState:    false,
			keepCookie:   false,
			consumed:     true,
			exchangeCall: true,
		},
		{
			name: "token endpoint unreachable",
			mutate: func(fix *callbackFixture) {
				fix.tokens.err = errProviderUnavailable
			},
			status:       http.StatusServiceUnavailable,
			code:         "SERVICE_UNAVAILABLE",
			message:      "A required service is unavailable.",
			keepState:    false,
			keepCookie:   false,
			consumed:     true,
			exchangeCall: true,
		},
		{
			name: "rejected access token",
			mutate: func(fix *callbackFixture) {
				fix.verifier.err = auth.ErrInvalidToken
			},
			status:       http.StatusBadGateway,
			code:         "OAUTH_PROVIDER_ERROR",
			message:      "The OAuth provider returned an invalid response.",
			keepState:    false,
			keepCookie:   false,
			consumed:     true,
			exchangeCall: true,
		},
		{
			name: "signing keys unavailable",
			mutate: func(fix *callbackFixture) {
				fix.verifier.err = auth.ErrKeySetUnavailable
			},
			status:       http.StatusServiceUnavailable,
			code:         "SERVICE_UNAVAILABLE",
			message:      "A required service is unavailable.",
			keepState:    false,
			keepCookie:   false,
			consumed:     true,
			exchangeCall: true,
		},
		{
			name: "redis unavailable",
			mutate: func(fix *callbackFixture) {
				fix.store.consumeErr = downError{}
			},
			status:     http.StatusServiceUnavailable,
			code:       "SERVICE_UNAVAILABLE",
			message:    "A required service is unavailable.",
			keepState:  true,
			keepCookie: true,
			consumed:   true,
		},
		{
			name: "corrupt state",
			mutate: func(fix *callbackFixture) {
				fix.store.consumeErr = errStateCorrupt
			},
			status:     http.StatusInternalServerError,
			code:       "INTERNAL_ERROR",
			message:    "The request could not be completed.",
			keepState:  true,
			keepCookie: false,
			consumed:   true,
		},
		{
			name: "postgres unavailable",
			mutate: func(fix *callbackFixture) {
				fix.users.err = &pgconn.PgError{Code: pgerrcode.ConnectionFailure}
			},
			status:       http.StatusServiceUnavailable,
			code:         "SERVICE_UNAVAILABLE",
			message:      "A required service is unavailable.",
			keepState:    false,
			keepCookie:   false,
			consumed:     true,
			exchangeCall: true,
			userCall:     true,
		},
		{
			name: "postgres deadline",
			mutate: func(fix *callbackFixture) {
				fix.users.err = context.DeadlineExceeded
			},
			status:       http.StatusServiceUnavailable,
			code:         "SERVICE_UNAVAILABLE",
			message:      "A required service is unavailable.",
			keepState:    false,
			keepCookie:   false,
			consumed:     true,
			exchangeCall: true,
			userCall:     true,
		},
		{
			name: "postgres query failure",
			mutate: func(fix *callbackFixture) {
				fix.users.err = &pgconn.PgError{Code: pgerrcode.UniqueViolation}
			},
			status:       http.StatusInternalServerError,
			code:         "INTERNAL_ERROR",
			message:      "The request could not be completed.",
			keepState:    false,
			keepCookie:   false,
			consumed:     true,
			exchangeCall: true,
			userCall:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fix := newCallback(t)
			tt.mutate(fix)
			rec := httptest.NewRecorder()
			fix.flow.Callback().ServeHTTP(rec, fix.request())

			assertAPIError(t, rec, tt.status, tt.code, tt.message)
			if strings.Contains(rec.Body.String(), "access-token-value") || strings.Contains(rec.Body.String(), fix.verifierSecret) {
				t.Fatalf("body leaked a credential: %s", rec.Body.String())
			}
			_, stored := fix.store.get(fix.state)
			if stored != tt.keepState {
				t.Fatalf("stored = %v, want %v", stored, tt.keepState)
			}
			if (fix.store.consumeCalls() > 0) != tt.consumed {
				t.Fatalf("consumed = %v, want %v", fix.store.consumeCalls() > 0, tt.consumed)
			}
			if tt.keepCookie {
				if rec.Header().Get("Set-Cookie") != "" {
					t.Fatalf("Set-Cookie = %q", rec.Header().Get("Set-Cookie"))
				}
			} else {
				clearedCookie(t, rec)
			}
			if (fix.tokens.calls() > 0) != tt.exchangeCall {
				t.Fatalf("exchange calls = %d", fix.tokens.calls())
			}
			if (fix.users.calls() > 0) != tt.userCall {
				t.Fatalf("upsert calls = %d", fix.users.calls())
			}
		})
	}
}

func TestAuthRoutesRejectOtherMethods(t *testing.T) {
	fix := newCallback(t)
	handler := httpapi.NewRouter(nil, httpapi.Routes{
		Login:    fix.flow.Login(),
		Callback: fix.flow.Callback(),
	})
	for _, path := range []string{"/auth/login", "/auth/callback"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		assertAPIError(t, rec, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "The requested method is not allowed.")
		if rec.Header().Get("Allow") != "GET" {
			t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
		}
	}
}

type callbackFixture struct {
	flow           *Flow
	store          *fakeStore
	tokens         *fakeExchanger
	verifier       *fakeVerifier
	users          *fakeUsers
	state          string
	binding        string
	verifierSecret string
	query          url.Values
	includeCookie  bool
}

func newCallback(t *testing.T) *callbackFixture {
	t.Helper()
	expires := 3600
	fix := &callbackFixture{
		store:         &fakeStore{},
		tokens:        &fakeExchanger{token: accessToken{Value: "access-token-value", ExpiresIn: &expires}},
		verifier:      &fakeVerifier{identity: auth.Identity{Issuer: "http://localhost:4444", Subject: "foo@bar.com"}},
		users:         &fakeUsers{},
		query:         url.Values{},
		includeCookie: true,
	}
	fix.flow = New(testSettings("http://localhost:8080/auth/callback"), fix.store, fix.tokens, fix.verifier, fix.users)
	fix.reseed()
	return fix
}

func (f *callbackFixture) reseed() {
	f.state = mustSecret(nil)
	f.binding = mustSecret(nil)
	f.verifierSecret = mustSecret(nil)
	_ = f.store.Save(context.Background(), f.state, transaction{
		BrowserBinding: bindingHash(f.binding),
		CodeVerifier:   f.verifierSecret,
	}, time.Minute)
	f.query.Set("state", f.state)
	f.query.Set("code", "auth-code")
	f.query.Del("error")
	f.includeCookie = true
}

func (f *callbackFixture) request() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?"+f.query.Encode(), nil)
	if f.includeCookie {
		req.AddCookie(&http.Cookie{Name: bindingCookie, Value: f.binding})
	}
	return req
}

func testSettings(redirectURI string) Settings {
	return Settings{
		ClientID:     "e2b-assignment",
		ClientSecret: "e2b-assignment-secret",
		RedirectURI:  redirectURI,
		AuthURL:      "http://localhost:4444/oauth2/auth",
		TokenURL:     "http://localhost:4444/oauth2/token",
		Audience:     "urn:e2b:sandbox-api",
		StateTTL:     5 * time.Minute,
		Timeout:      5 * time.Second,
	}
}

type fakeStore struct {
	mu         sync.Mutex
	records    map[string]transaction
	failNX     int
	saveErr    error
	consumeErr error
	saves      int
	consumes   int
}

func (s *fakeStore) Save(ctx context.Context, state string, record transaction, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if s.saveErr != nil {
		return s.saveErr
	}
	if s.failNX > 0 {
		s.failNX--
		return errStateExists
	}
	if s.records == nil {
		s.records = map[string]transaction{}
	}
	if _, ok := s.records[state]; ok {
		return errStateExists
	}
	s.records[state] = record
	return nil
}

func (s *fakeStore) Consume(ctx context.Context, state, bindingHash string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consumes++
	if s.consumeErr != nil {
		return "", s.consumeErr
	}
	record, ok := s.records[state]
	if !ok {
		return "", errStateMissing
	}
	if record.BrowserBinding != bindingHash {
		return "", errStateMismatch
	}
	delete(s.records, state)
	return record.CodeVerifier, nil
}

func (s *fakeStore) get(state string) (transaction, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[state]
	return record, ok
}

func (s *fakeStore) delete(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, state)
}

func (s *fakeStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

func (s *fakeStore) saveCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

func (s *fakeStore) consumeCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.consumes
}

type fakeExchanger struct {
	mu           sync.Mutex
	token        accessToken
	err          error
	exchanges    int
	lastCode     string
	lastVerifier string
}

func (f *fakeExchanger) Exchange(ctx context.Context, code, verifier string) (accessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exchanges++
	f.lastCode = code
	f.lastVerifier = verifier
	if f.err != nil {
		return accessToken{}, f.err
	}
	return f.token, nil
}

func (f *fakeExchanger) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges
}

type fakeVerifier struct {
	mu       sync.Mutex
	identity auth.Identity
	err      error
	verified int
	last     string
}

func (f *fakeVerifier) Verify(ctx context.Context, raw string) (auth.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verified++
	f.last = raw
	if f.err != nil {
		return auth.Identity{}, f.err
	}
	return f.identity, nil
}

func (f *fakeVerifier) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.verified
}

type fakeUsers struct {
	mu         sync.Mutex
	byIdentity map[string]db.User
	err        error
	upserts    int
	last       db.UpsertUserParams
}

func (f *fakeUsers) UpsertUser(ctx context.Context, arg db.UpsertUserParams) (db.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	f.last = arg
	if f.err != nil {
		return db.User{}, f.err
	}
	if f.byIdentity == nil {
		f.byIdentity = map[string]db.User{}
	}
	key := arg.OauthIssuer + "\n" + arg.OauthSubject
	user, ok := f.byIdentity[key]
	if !ok {
		user = db.User{
			ID:           uuid.New(),
			OauthIssuer:  arg.OauthIssuer,
			OauthSubject: arg.OauthSubject,
			CreatedAt:    time.Unix(1_700_000_000, 0).UTC(),
		}
	}
	if arg.Email != nil {
		email := *arg.Email
		user.Email = &email
	}
	f.byIdentity[key] = user
	return user, nil
}

func (f *fakeUsers) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upserts
}

func (f *fakeUsers) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byIdentity)
}

func (f *fakeUsers) onlyID(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.byIdentity) != 1 {
		t.Fatalf("users = %d", len(f.byIdentity))
	}
	for _, user := range f.byIdentity {
		return user.ID.String()
	}
	return ""
}

type downError struct{}

func (downError) Error() string   { return "dial tcp: connection refused" }
func (downError) Timeout() bool   { return false }
func (downError) Temporary() bool { return false }

func mustSecret(t *testing.T) string {
	if t != nil {
		t.Helper()
	}
	secret, err := randomSecret()
	if err != nil {
		if t != nil {
			t.Fatal(err)
		}
		panic(err)
	}
	return secret
}

func setCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	resp := rec.Result()
	defer resp.Body.Close()
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d", len(cookies))
	}
	return cookies[0]
}

func clearedCookie(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	cookie := setCookie(t, rec)
	if cookie.Name != bindingCookie || cookie.Path != bindingCookiePath || cookie.MaxAge >= 0 || !cookie.HttpOnly {
		t.Fatalf("cookie = %#v", cookie)
	}
}

func assertToken(t *testing.T, rec *httptest.ResponseRecorder, userID string, expires *int) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	raw := rec.Body.String()
	if strings.Contains(raw, "refresh_token") || strings.Contains(raw, "id_token") {
		t.Fatalf("body = %s", raw)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var body httpapi.TokenResponse
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing data: %v", err)
	}
	if body.AccessToken != "access-token-value" || body.TokenType != httpapi.TokenTypeBearer || body.UserID != userID {
		t.Fatalf("token = %+v", body)
	}
	if expires == nil {
		if body.ExpiresIn != nil || strings.Contains(raw, "expires_in") {
			t.Fatalf("body = %s", raw)
		}
		return
	}
	if body.ExpiresIn == nil || *body.ExpiresIn != *expires {
		t.Fatalf("expires_in = %v", body.ExpiresIn)
	}
}

func assertAPIError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d, body %s", rec.Code, status, rec.Body.String())
	}
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing data: %v", err)
	}
	if body.Error.Code != code || body.Error.Message != message {
		t.Fatalf("error = %#v", body.Error)
	}
}

func firstUser(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body httpapi.TokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.UserID
}

var _ net.Error = downError{}
