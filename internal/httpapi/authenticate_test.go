package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/smusali/sandbox-api/internal/auth"
	"github.com/smusali/sandbox-api/internal/db/sqlc"
)

const (
	authIssuer   = "http://localhost:4444"
	authAudience = "urn:e2b:sandbox-api"
	authClientID = "e2b-assignment"
	authKID      = "access-1"
	authSubject  = "foo@bar.com"
)

var (
	registeredUser = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	suppliedUser   = uuid.MustParse("22222222-2222-4222-8222-222222222222")

	protectedRoutes = []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/projects"},
		{http.MethodGet, "/v1/projects"},
		{http.MethodPost, "/v1/projects/6d502949-7e8c-4f47-8b0c-3f1e3b913001/members"},
		{http.MethodPost, "/v1/projects/6d502949-7e8c-4f47-8b0c-3f1e3b913001/sandboxes"},
		{http.MethodGet, "/v1/projects/6d502949-7e8c-4f47-8b0c-3f1e3b913001/sandboxes"},
		{http.MethodDelete, "/v1/sandboxes/7719ac21-1b89-4865-89b5-0ed2f8721003"},
	}
)

func TestAuthenticateProtectsEveryRoute(t *testing.T) {
	fx := newAccessFixture(t, http.StatusOK, time.Now().Add(time.Hour), authAudience)
	dir := &userDirectory{user: db.User{ID: registeredUser}}
	seen := 0
	handler := protectedRouter(t, fx.verifier, dir, time.Second, &seen)

	for _, route := range protectedRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, hostileRequest(route.method, route.path, fx.token))
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), fx.token) {
				t.Fatal("body contained the access token")
			}
		})
	}
	if seen != len(protectedRoutes) {
		t.Fatalf("handler calls = %d", seen)
	}
	if dir.calls != len(protectedRoutes) || dir.issuer != authIssuer || dir.subject != authSubject {
		t.Fatalf("lookup calls = %d, issuer = %q, subject = %q", dir.calls, dir.issuer, dir.subject)
	}
}

func TestAuthenticateRejectsCredentialsBeforeVerification(t *testing.T) {
	verifier := &stubVerifier{}
	dir := &userDirectory{}
	seen := 0
	handler := protectedRouter(t, verifier, dir, time.Second, &seen)

	tests := []struct {
		name   string
		header []string
	}{
		{name: "missing"},
		{name: "empty", header: []string{"Bearer"}},
		{name: "blank", header: []string{"Bearer "}},
		{name: "unsupported", header: []string{"Basic abc"}},
		{name: "repeated", header: []string{"Bearer one", "Bearer two"}},
		{name: "comma separated", header: []string{"Bearer a, Bearer b"}},
		{name: "extra token", header: []string{"Bearer a b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/projects?user_id="+suppliedUser.String(), strings.NewReader(`{"user_id":"`+suppliedUser.String()+`"}`))
			for _, value := range tt.header {
				req.Header.Add("Authorization", value)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assertAPIError(t, rec, http.StatusUnauthorized, CodeUnauthenticated, "A valid access token is required.")
			assertChallenge(t, rec, challengeBearer)
		})
	}
	if verifier.calls != 0 || dir.calls != 0 || seen != 0 {
		t.Fatalf("verifier = %d, directory = %d, handler = %d", verifier.calls, dir.calls, seen)
	}
}

func TestAuthenticateUsesHeaderToken(t *testing.T) {
	verifier := &stubVerifier{identity: auth.Identity{Issuer: authIssuer, Subject: authSubject}}
	dir := &userDirectory{user: db.User{ID: registeredUser}}
	seen := 0
	handler := protectedRouter(t, verifier, dir, time.Second, &seen)

	req := hostileRequest(http.MethodGet, "/v1/projects", "")
	req.Header.Set("Authorization", "bEaReR abc.def.ghi")
	req.URL.RawQuery = "access_token=query-token&user_id=" + suppliedUser.String()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if verifier.calls != 1 || verifier.raw != "abc.def.ghi" {
		t.Fatalf("verified %q, calls = %d", verifier.raw, verifier.calls)
	}
	if dir.calls != 1 || dir.issuer != authIssuer || dir.subject != authSubject {
		t.Fatalf("lookup issuer = %q, subject = %q, calls = %d", dir.issuer, dir.subject, dir.calls)
	}
}

func TestAuthenticateRejectsInvalidTokens(t *testing.T) {
	valid := newAccessFixture(t, http.StatusOK, time.Now().Add(time.Hour), authAudience)
	expired := newAccessFixture(t, http.StatusOK, time.Now().Add(-2*time.Minute), authAudience)
	idToken := newAccessFixture(t, http.StatusOK, time.Now().Add(time.Hour), authClientID)

	tests := []struct {
		name  string
		token string
		keys  *auth.Verifier
	}{
		{name: "malformed", token: "not-a-jwt", keys: valid.verifier},
		{name: "tampered", token: tamperToken(t, valid.token), keys: valid.verifier},
		{name: "expired", token: expired.token, keys: expired.verifier},
		{name: "id token", token: idToken.token, keys: idToken.verifier},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := &userDirectory{user: db.User{ID: registeredUser}}
			seen := 0
			handler := protectedRouter(t, tt.keys, dir, time.Second, &seen)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, hostileRequest(http.MethodGet, "/v1/projects", tt.token))
			assertAPIError(t, rec, http.StatusUnauthorized, CodeUnauthenticated, "A valid access token is required.")
			assertChallenge(t, rec, challengeInvalidToken)
			if strings.Contains(rec.Body.String(), tt.token) {
				t.Fatal("body contained the access token")
			}
			if dir.calls != 0 || seen != 0 {
				t.Fatalf("directory = %d, handler = %d", dir.calls, seen)
			}
		})
	}
}

func TestAuthenticateMissingUser(t *testing.T) {
	fx := newAccessFixture(t, http.StatusOK, time.Now().Add(time.Hour), authAudience)
	tests := []struct {
		name string
		err  error
	}{
		{name: "no row", err: pgx.ErrNoRows},
		{name: "wrapped no row", err: fmt.Errorf("lookup: %w", pgx.ErrNoRows)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := &userDirectory{err: tt.err}
			seen := 0
			handler := protectedRouter(t, fx.verifier, dir, time.Second, &seen)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, hostileRequest(http.MethodPost, "/v1/projects", fx.token))
			assertAPIError(t, rec, http.StatusUnauthorized, CodeUnauthenticated, "A valid access token is required.")
			assertChallenge(t, rec, challengeBearer)
			if dir.calls != 1 || seen != 0 {
				t.Fatalf("directory = %d, handler = %d", dir.calls, seen)
			}
		})
	}
}

func TestAuthenticateDependencyFailures(t *testing.T) {
	fx := newAccessFixture(t, http.StatusOK, time.Now().Add(time.Hour), authAudience)
	down := newAccessFixture(t, http.StatusServiceUnavailable, time.Now().Add(time.Hour), authAudience)

	t.Run("signing keys", func(t *testing.T) {
		dir := &userDirectory{user: db.User{ID: registeredUser}}
		seen := 0
		handler := protectedRouter(t, down.verifier, dir, time.Second, &seen)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, hostileRequest(http.MethodGet, "/v1/projects", down.token))
		assertUnavailable(t, rec)
		if dir.calls != 0 || seen != 0 {
			t.Fatalf("directory = %d, handler = %d", dir.calls, seen)
		}
	})

	t.Run("canceled request", func(t *testing.T) {
		verifier := &stubVerifier{block: true}
		dir := &userDirectory{user: db.User{ID: registeredUser}}
		seen := 0
		handler := protectedRouter(t, verifier, dir, time.Second, &seen)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := hostileRequest(http.MethodGet, "/v1/projects", "abc.def.ghi").WithContext(ctx)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assertUnavailable(t, rec)
		if dir.calls != 0 || seen != 0 {
			t.Fatalf("directory = %d, handler = %d", dir.calls, seen)
		}
	})

	t.Run("lookup deadline", func(t *testing.T) {
		verifier := &stubVerifier{block: true}
		dir := &userDirectory{user: db.User{ID: registeredUser}}
		seen := 0
		handler := protectedRouter(t, verifier, dir, 20*time.Millisecond, &seen)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, hostileRequest(http.MethodGet, "/v1/projects", "abc.def.ghi"))
		assertUnavailable(t, rec)
		if dir.calls != 0 || seen != 0 {
			t.Fatalf("directory = %d, handler = %d", dir.calls, seen)
		}
	})

	connection := &pgconn.PgError{Code: pgerrcode.ConnectionFailure}
	tests := []struct {
		name string
		err  error
	}{
		{name: "connection failure", err: connection},
		{name: "wrapped connection failure", err: fmt.Errorf("lookup: %w", connection)},
		{name: "lookup canceled", err: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := &userDirectory{err: tt.err}
			seen := 0
			handler := protectedRouter(t, fx.verifier, dir, time.Second, &seen)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, hostileRequest(http.MethodGet, "/v1/projects", fx.token))
			assertUnavailable(t, rec)
			if dir.calls != 1 || seen != 0 {
				t.Fatalf("directory = %d, handler = %d", dir.calls, seen)
			}
			if strings.Contains(rec.Body.String(), pgerrcode.ConnectionFailure) || strings.Contains(rec.Body.String(), fx.token) {
				t.Fatalf("body = %s", rec.Body.String())
			}
		})
	}
}

func TestAuthenticateDatabaseFailureIsInternal(t *testing.T) {
	fx := newAccessFixture(t, http.StatusOK, time.Now().Add(time.Hour), authAudience)
	dir := &userDirectory{err: &pgconn.PgError{Code: pgerrcode.UniqueViolation}}
	seen := 0
	handler := protectedRouter(t, fx.verifier, dir, time.Second, &seen)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hostileRequest(http.MethodGet, "/v1/projects", fx.token))
	assertAPIError(t, rec, http.StatusInternalServerError, CodeInternalError, "The request could not be completed.")
	assertNoChallenge(t, rec)
	if strings.Contains(rec.Body.String(), pgerrcode.UniqueViolation) || strings.Contains(rec.Body.String(), fx.token) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if seen != 0 {
		t.Fatalf("handler calls = %d", seen)
	}
}

func TestAuthenticateEmptyUserID(t *testing.T) {
	fx := newAccessFixture(t, http.StatusOK, time.Now().Add(time.Hour), authAudience)
	dir := &userDirectory{}
	seen := 0
	handler := protectedRouter(t, fx.verifier, dir, time.Second, &seen)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, hostileRequest(http.MethodGet, "/v1/projects", fx.token))
	assertAPIError(t, rec, http.StatusInternalServerError, CodeInternalError, "The request could not be completed.")
	assertNoChallenge(t, rec)
	if seen != 0 {
		t.Fatalf("handler calls = %d", seen)
	}
}

func TestPublicRoutesStayOpen(t *testing.T) {
	verifier := &stubVerifier{}
	dir := &userDirectory{}
	seen := 0
	handler := protectedRouter(t, verifier, dir, time.Second, &seen)
	for _, target := range []string{"/auth/login", "/auth/callback"} {
		t.Run(target, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.Header.Set("Authorization", "Basic no")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			assertNoChallenge(t, rec)
		})
	}
	if verifier.calls != 0 || dir.calls != 0 || seen != 0 {
		t.Fatalf("verifier = %d, directory = %d, handler = %d", verifier.calls, dir.calls, seen)
	}
}

func TestUnknownRouteIgnoresAuthenticator(t *testing.T) {
	seen := 0
	handler := protectedRouter(t, &stubVerifier{}, &userDirectory{}, time.Second, &seen)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/missing", nil))
	assertAPIError(t, rec, http.StatusNotFound, CodeNotFound, "The requested resource was not found.")
	assertNoChallenge(t, rec)
	if seen != 0 {
		t.Fatalf("handler calls = %d", seen)
	}
}

func TestAuthenticatedWrongMethod(t *testing.T) {
	fx := newAccessFixture(t, http.StatusOK, time.Now().Add(time.Hour), authAudience)
	dir := &userDirectory{user: db.User{ID: registeredUser}}
	seen := 0
	handler := protectedRouter(t, fx.verifier, dir, time.Second, &seen)

	anon := httptest.NewRecorder()
	handler.ServeHTTP(anon, httptest.NewRequest(http.MethodPut, "/v1/projects", nil))
	assertAPIError(t, anon, http.StatusUnauthorized, CodeUnauthenticated, "A valid access token is required.")
	assertChallenge(t, anon, challengeBearer)
	if anon.Header().Get("Allow") != "" {
		t.Fatalf("Allow = %q", anon.Header().Get("Allow"))
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer "+fx.token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assertAPIError(t, rec, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "The requested method is not allowed.")
	if rec.Header().Get("Allow") != "GET, POST" {
		t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
	}
	assertNoChallenge(t, rec)
	if seen != 0 || dir.calls != 1 {
		t.Fatalf("handler = %d, directory = %d", seen, dir.calls)
	}
}

type userDirectory struct {
	user    db.User
	err     error
	calls   int
	issuer  string
	subject string
}

func (d *userDirectory) GetUserByIdentity(ctx context.Context, arg db.GetUserByIdentityParams) (db.User, error) {
	d.calls++
	d.issuer = arg.OauthIssuer
	d.subject = arg.OauthSubject
	if d.err != nil {
		return db.User{}, d.err
	}
	return d.user, nil
}

type stubVerifier struct {
	identity auth.Identity
	err      error
	calls    int
	raw      string
	block    bool
}

func (s *stubVerifier) Verify(ctx context.Context, raw string) (auth.Identity, error) {
	s.calls++
	s.raw = raw
	if s.block {
		select {
		case <-ctx.Done():
			return auth.Identity{}, ctx.Err()
		case <-time.After(time.Second):
			return auth.Identity{}, errors.New("verification was not canceled")
		}
	}
	if err := ctx.Err(); err != nil {
		return auth.Identity{}, err
	}
	return s.identity, s.err
}

func protectedRouter(t *testing.T, verifier tokenVerifier, users userByIdentity, timeout time.Duration, seen *int) http.Handler {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Err() != nil {
			t.Errorf("handler context: %v", r.Context().Err())
		}
		principal, ok := auth.PrincipalFrom(r.Context())
		if !ok || principal.UserID != registeredUser {
			t.Errorf("principal = %+v, ok=%v", principal, ok)
		}
		if r.Header.Get("X-User-Id") == principal.UserID.String() && r.Header.Get("X-User-Id") != "" {
			t.Error("principal followed X-User-Id")
		}
		replaced := auth.WithPrincipal(r.Context(), auth.Principal{UserID: suppliedUser})
		again, ok := auth.PrincipalFrom(replaced)
		if !ok || again.UserID != registeredUser {
			t.Errorf("replaced principal = %+v, ok=%v", again, ok)
		}
		*seen++
		w.WriteHeader(http.StatusNoContent)
	})
	public := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := auth.PrincipalFrom(r.Context()); ok {
			t.Error("public route has a principal")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return NewRouter(Authenticate(verifier, users, timeout), Routes{
		Login:         public,
		Callback:      public,
		CreateProject: next,
		ListProjects:  next,
		AddMember:     next,
		CreateSandbox: next,
		ListSandboxes: next,
		StopSandbox:   next,
	})
}

func hostileRequest(method, path, token string) *http.Request {
	body := `{"user_id":"` + suppliedUser.String() + `"}`
	req := httptest.NewRequest(method, path+"?user_id="+suppliedUser.String(), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", suppliedUser.String())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func assertChallenge(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Header().Get("WWW-Authenticate") != want {
		t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func assertNoChallenge(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func assertUnavailable(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assertAPIError(t, rec, http.StatusServiceUnavailable, CodeServiceUnavailable, "A required service is unavailable.")
	assertNoChallenge(t, rec)
}

type accessFixture struct {
	verifier *auth.Verifier
	token    string
}

func newAccessFixture(t *testing.T, status int, exp time.Time, audience string) accessFixture {
	t.Helper()
	priv := testSigningKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		body, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{authPublicKey(priv)}})
		if err != nil {
			http.Error(w, "encode", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return accessFixture{
		verifier: auth.NewVerifier(authIssuer, authAudience, authClientID, server.URL, time.Second),
		token:    signAccessToken(t, priv, audience, exp),
	}
}

func signAccessToken(t *testing.T, priv *rsa.PrivateKey, audience string, exp time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.RS256,
		Key: &jose.JSONWebKey{
			Key:       priv,
			KeyID:     authKID,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		},
	}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer:   authIssuer,
		Subject:  authSubject,
		Audience: jwt.Audience{audience},
		Expiry:   jwt.NewNumericDate(exp),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func authPublicKey(priv *rsa.PrivateKey) jose.JSONWebKey {
	return jose.JSONWebKey{
		Key:       &priv.PublicKey,
		KeyID:     authKID,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}
}

func tamperToken(t *testing.T, raw string) string {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[2] == "" {
		t.Fatalf("token has %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sig[0] ^= 0x01
	parts[2] = base64.RawURLEncoding.EncodeToString(sig)
	return strings.Join(parts, ".")
}

var (
	signingOnce sync.Once
	signingKey  *rsa.PrivateKey
	signingErr  error
)

func testSigningKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	signingOnce.Do(func() {
		signingKey, signingErr = rsa.GenerateKey(rand.Reader, 2048)
	})
	if signingErr != nil {
		t.Fatal(signingErr)
	}
	return signingKey
}
