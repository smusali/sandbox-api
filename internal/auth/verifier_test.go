package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	testIssuer   = "http://localhost:4444"
	testAudience = "urn:e2b:sandbox-api"
	testClientID = "e2b-assignment"
	accessKID    = "access-1"
	idKID        = "id-1"
	testSubject  = "foo@bar.com"
)

func TestVerifyHydraAccessToken(t *testing.T) {
	h := newHarness(t, time.Second)
	verified := true
	unverified := false

	within := h.accessSpec()
	exp := h.clock.now().Add(-clockSkew / 2)
	nbf := h.clock.now().Add(clockSkew / 2)
	iat := nbf
	within.exp = &exp
	within.nbf = &nbf
	within.iat = &iat

	stringAud := h.accessSpec()
	stringAud.stringAud = true

	noTimes := h.accessSpec()
	noTimes.omitNBF = true
	noTimes.omitIAT = true

	lowerTyp := h.accessSpec()
	lowerTyp.typ = "jwt"

	accessTyp := h.accessSpec()
	accessTyp.typ = "at+jwt"

	omitTyp := h.accessSpec()
	omitTyp.omitTyp = true

	withEmail := h.accessSpec()
	withEmail.email = "person@example.com"
	withEmail.emailVerified = &verified

	rejectedEmail := h.accessSpec()
	rejectedEmail.email = "person@example.com"
	rejectedEmail.emailVerified = &unverified

	missingVerified := h.accessSpec()
	missingVerified.email = "person@example.com"

	emptyVerified := h.accessSpec()
	emptyVerified.emailVerified = &verified

	tests := []struct {
		name  string
		spec  tokenSpec
		email string
	}{
		{name: "audience array", spec: h.accessSpec()},
		{name: "audience string", spec: stringAud},
		{name: "lowercase type", spec: lowerTyp},
		{name: "access token type", spec: accessTyp},
		{name: "missing type", spec: omitTyp},
		{name: "optional time claims", spec: noTimes},
		{name: "clock skew", spec: within},
		{name: "verified email", spec: withEmail, email: "person@example.com"},
		{name: "unverified email", spec: rejectedEmail},
		{name: "email without verification", spec: missingVerified},
		{name: "verified empty email", spec: emptyVerified},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id, err := h.verifier.Verify(context.Background(), h.sign(test.spec))
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			if id.Issuer != testIssuer || id.Subject != testSubject || id.Email != test.email {
				t.Fatalf("identity = %+v", id)
			}
		})
	}
}

func TestVerifyRejectsTokensBeforeKeyFetch(t *testing.T) {
	h := newHarness(t, time.Second)
	var evilHits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		evilHits.Add(1)
	}))
	t.Cleanup(evil.Close)

	jku := h.accessSpec()
	jku.extra = map[jose.HeaderKey]string{headerJKU: evil.URL}
	x5u := h.accessSpec()
	x5u.extra = map[jose.HeaderKey]string{headerX5U: evil.URL + "/cert"}
	embedded := h.accessSpec()
	embedded.embed = true
	critical := h.accessSpec()
	critical.crit = true
	noKid := h.accessSpec()
	noKid.kid = ""
	refreshTyp := h.accessSpec()
	refreshTyp.typ = "refresh+jwt"

	tests := []struct {
		name  string
		token string
	}{
		{name: "empty", token: ""},
		{name: "malformed", token: "not-a-jwt"},
		{name: "opaque refresh token", token: "ory_rt_refresh_token"},
		{name: "unsigned", token: unsignedToken()},
		{name: "hmac", token: hmacToken(t)},
		{name: "jku", token: h.sign(jku)},
		{name: "x5u", token: h.sign(x5u)},
		{name: "embedded jwk", token: h.sign(embedded)},
		{name: "critical header", token: h.sign(critical)},
		{name: "missing kid", token: h.sign(noKid)},
		{name: "refresh type", token: h.sign(refreshTyp)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id, err := h.verifier.Verify(context.Background(), test.token)
			requireInvalid(t, err)
			if id != (Identity{}) {
				t.Fatalf("identity = %+v", id)
			}
		})
	}
	if h.hits.Load() != 0 || evilHits.Load() != 0 {
		t.Fatalf("key fetches = %d, untrusted fetches = %d", h.hits.Load(), evilHits.Load())
	}
}

func TestVerifyRejectsClaims(t *testing.T) {
	h := newHarness(t, time.Second)
	beyond := h.clock.now().Add(clockSkew + time.Second)
	expiredAt := h.clock.now().Add(-clockSkew - time.Second)

	wrongIssuer := h.accessSpec()
	wrongIssuer.iss = "http://issuer.example"
	missingAudience := h.accessSpec()
	missingAudience.omitAud = true
	otherAudience := h.accessSpec()
	otherAudience.aud = []string{"https://other.example"}
	idToken := h.accessSpec()
	idToken.priv = h.idKey
	idToken.kid = idKID
	idToken.aud = []string{testClientID}
	bothAudiences := h.accessSpec()
	bothAudiences.aud = []string{testAudience, testClientID}
	emptySubject := h.accessSpec()
	emptySubject.sub = ""
	blankSubject := h.accessSpec()
	blankSubject.sub = " \t "
	missingExpiry := h.accessSpec()
	missingExpiry.omitExp = true
	expired := h.accessSpec()
	expired.exp = &expiredAt
	notYet := h.accessSpec()
	notYet.nbf = &beyond
	issuedLater := h.accessSpec()
	issuedLater.iat = &beyond

	tests := []struct {
		name string
		spec tokenSpec
	}{
		{name: "issuer", spec: wrongIssuer},
		{name: "missing audience", spec: missingAudience},
		{name: "unexpected audience", spec: otherAudience},
		{name: "id token", spec: idToken},
		{name: "both audiences", spec: bothAudiences},
		{name: "empty subject", spec: emptySubject},
		{name: "blank subject", spec: blankSubject},
		{name: "missing expiration", spec: missingExpiry},
		{name: "expired", spec: expired},
		{name: "not yet valid", spec: notYet},
		{name: "issued in the future", spec: issuedLater},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			id, err := h.verifier.Verify(context.Background(), h.sign(test.spec))
			requireInvalid(t, err)
			if id != (Identity{}) {
				t.Fatalf("identity = %+v", id)
			}
		})
	}
}

func TestVerifyBadSignatureDoesNotRefresh(t *testing.T) {
	h := newHarness(t, time.Second)
	raw := h.sign(h.accessSpec())
	if _, err := h.verifier.Verify(context.Background(), raw); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	_, err := h.verifier.Verify(context.Background(), tamper(t, raw))
	requireInvalid(t, err)
	if h.hits.Load() != 1 {
		t.Fatalf("hits = %d", h.hits.Load())
	}
}

type harness struct {
	t        *testing.T
	clock    *fakeClock
	hits     atomic.Int32
	fail     atomic.Bool
	hold     atomic.Bool
	server   *httptest.Server
	setMu    sync.Mutex
	set      jose.JSONWebKeySet
	verifier *Verifier
	access   *rsa.PrivateKey
	idKey    *rsa.PrivateKey
	entered  chan struct{}
	release  chan struct{}
	gateOnce sync.Once
}

func newHarness(t *testing.T, timeout time.Duration) *harness {
	t.Helper()
	access, idKey := testKeys(t)
	h := &harness{
		t:      t,
		clock:  newFakeClock(),
		access: access,
		idKey:  idKey,
		set: jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			publicJWK(access, accessKID),
			publicJWK(idKey, idKID),
		}},
	}
	h.server = httptest.NewServer(http.HandlerFunc(h.serveJWKS))
	t.Cleanup(h.server.Close)
	h.verifier = NewVerifier(testIssuer, testAudience, testClientID, h.server.URL, timeout)
	h.verifier.now = h.clock.now
	h.verifier.keys.now = h.clock.now
	return h
}

func (h *harness) serveJWKS(w http.ResponseWriter, r *http.Request) {
	h.hits.Add(1)
	if h.release != nil {
		h.gateOnce.Do(func() { close(h.entered) })
		<-h.release
	}
	if h.hold.Load() {
		<-r.Context().Done()
		return
	}
	if h.fail.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	h.setMu.Lock()
	body, err := json.Marshal(h.set)
	h.setMu.Unlock()
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (h *harness) replaceKeys(keys ...jose.JSONWebKey) {
	h.setMu.Lock()
	h.set.Keys = keys
	h.setMu.Unlock()
}

func (h *harness) accessSpec() tokenSpec {
	now := h.clock.now()
	exp := now.Add(time.Hour)
	nbf := now
	iat := now
	return tokenSpec{
		priv: h.access,
		kid:  accessKID,
		typ:  "JWT",
		iss:  testIssuer,
		sub:  testSubject,
		aud:  []string{testAudience},
		exp:  &exp,
		nbf:  &nbf,
		iat:  &iat,
	}
}

type tokenSpec struct {
	priv          *rsa.PrivateKey
	kid           string
	typ           string
	omitTyp       bool
	embed         bool
	extra         map[jose.HeaderKey]string
	crit          bool
	iss           string
	sub           string
	aud           []string
	omitAud       bool
	stringAud     bool
	exp           *time.Time
	omitExp       bool
	nbf           *time.Time
	omitNBF       bool
	iat           *time.Time
	omitIAT       bool
	email         string
	emailVerified *bool
}

type signedClaims struct {
	Issuer        string           `json:"iss,omitempty"`
	Subject       string           `json:"sub,omitempty"`
	Audience      []string         `json:"aud,omitempty"`
	Expiry        *jwt.NumericDate `json:"exp,omitempty"`
	NotBefore     *jwt.NumericDate `json:"nbf,omitempty"`
	IssuedAt      *jwt.NumericDate `json:"iat,omitempty"`
	Email         string           `json:"email,omitempty"`
	EmailVerified *bool            `json:"email_verified,omitempty"`
}

func (h *harness) sign(spec tokenSpec) string {
	h.t.Helper()
	opts := &jose.SignerOptions{EmbedJWK: spec.embed}
	if !spec.omitTyp && spec.typ != "" {
		opts.WithType(jose.ContentType(spec.typ))
	}
	if spec.crit {
		opts.WithCritical("b64")
	}
	for name, value := range spec.extra {
		opts.WithHeader(name, value)
	}
	priv := spec.priv
	if priv == nil {
		priv = h.access
	}
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.RS256,
		Key: &jose.JSONWebKey{
			Key:       priv,
			KeyID:     spec.kid,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		},
	}, opts)
	if err != nil {
		h.t.Fatal(err)
	}

	var raw string
	if spec.stringAud {
		raw, err = jwt.Signed(signer).Claims(jwt.Claims{
			Issuer:    spec.iss,
			Subject:   spec.sub,
			Audience:  jwt.Audience(spec.aud),
			Expiry:    numeric(spec.exp, spec.omitExp),
			NotBefore: numeric(spec.nbf, spec.omitNBF),
			IssuedAt:  numeric(spec.iat, spec.omitIAT),
		}).Serialize()
	} else {
		claims := signedClaims{
			Issuer:        spec.iss,
			Subject:       spec.sub,
			Expiry:        numeric(spec.exp, spec.omitExp),
			NotBefore:     numeric(spec.nbf, spec.omitNBF),
			IssuedAt:      numeric(spec.iat, spec.omitIAT),
			Email:         spec.email,
			EmailVerified: spec.emailVerified,
		}
		if !spec.omitAud {
			claims.Audience = spec.aud
		}
		raw, err = jwt.Signed(signer).Claims(claims).Serialize()
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return raw
}

func numeric(at *time.Time, omit bool) *jwt.NumericDate {
	if omit || at == nil {
		return nil
	}
	return jwt.NewNumericDate(*at)
}

func publicJWK(priv *rsa.PrivateKey, kid string) jose.JSONWebKey {
	return jose.JSONWebKey{
		Key:       &priv.PublicKey,
		KeyID:     kid,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}
}

func requireInvalid(t *testing.T, err error) {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || !errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrKeySetUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func requireUnavailable(t *testing.T, err error) {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || !errors.Is(err, ErrKeySetUnavailable) || errors.Is(err, ErrInvalidToken) {
		t.Fatalf("error = %v", err)
	}
}

func tamper(t *testing.T, raw string) string {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[2] == "" {
		t.Fatalf("token = %q", raw)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sig[0] ^= 0x01
	parts[2] = base64.RawURLEncoding.EncodeToString(sig)
	return strings.Join(parts, ".")
}

func unsignedToken() string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"http://localhost:4444","sub":"foo@bar.com"}`))
	return header + "." + payload + "."
}

func hmacToken(t *testing.T) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: []byte("0123456789abcdef0123456789abcdef")},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer:   testIssuer,
		Subject:  testSubject,
		Audience: jwt.Audience{testAudience},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{at: time.Unix(1_700_000_000, 0).UTC()}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

var (
	keysOnce   sync.Once
	accessPriv *rsa.PrivateKey
	idPriv     *rsa.PrivateKey
	keysErr    error
)

func testKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	keysOnce.Do(func() {
		accessPriv, keysErr = rsa.GenerateKey(rand.Reader, 2048)
		if keysErr != nil {
			return
		}
		idPriv, keysErr = rsa.GenerateKey(rand.Reader, 2048)
	})
	if keysErr != nil {
		t.Fatal(keysErr)
	}
	return accessPriv, idPriv
}
