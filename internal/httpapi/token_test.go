package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/smusali/sandbox-api/internal/auth"
)

func TestTokenErrorMapsVerification(t *testing.T) {
	if TokenError(nil) != nil {
		t.Fatal("nil error mapped")
	}
	if TokenError(errors.New("other")) != nil {
		t.Fatal("unrelated error mapped")
	}

	invalid := TokenError(verifyToken(t, "not-a-jwt", http.StatusOK))
	if invalid == nil || invalid.Status != http.StatusUnauthorized || invalid.Code != CodeUnauthenticated || invalid.WWWAuthenticate != `Bearer error="invalid_token"` {
		t.Fatalf("invalid = %+v", invalid)
	}

	unavailable := TokenError(verifyToken(t, signedAccessToken(t), http.StatusServiceUnavailable))
	if unavailable == nil || unavailable.Status != http.StatusServiceUnavailable || unavailable.Code != CodeServiceUnavailable || unavailable.WWWAuthenticate != "" {
		t.Fatalf("unavailable = %+v", unavailable)
	}
}

func verifyToken(t *testing.T, raw string, status int) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)

	verifier := auth.NewVerifier(
		"http://localhost:4444",
		"urn:e2b:sandbox-api",
		"e2b-assignment",
		server.URL,
		time.Second,
	)
	_, err := verifier.Verify(context.Background(), raw)
	return err
}

func signedAccessToken(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.RS256,
		Key: &jose.JSONWebKey{
			Key:       priv,
			KeyID:     "access-1",
			Algorithm: string(jose.RS256),
			Use:       "sig",
		},
	}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer:   "http://localhost:4444",
		Subject:  "foo@bar.com",
		Audience: jwt.Audience{"urn:e2b:sandbox-api"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
