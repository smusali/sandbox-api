package auth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

func TestKeyCacheFreshStaleAndMaximumAge(t *testing.T) {
	h := newHarness(t, time.Second)
	raw := h.sign(h.accessSpec())
	ctx := context.Background()

	if _, err := h.verifier.Verify(ctx, raw); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	h.fail.Store(true)
	if _, err := h.verifier.Verify(ctx, raw); err != nil {
		t.Fatalf("fresh cache error = %v", err)
	}
	if h.hits.Load() != 1 {
		t.Fatalf("hits = %d", h.hits.Load())
	}

	h.clock.advance(keyFreshness + time.Second)
	if _, err := h.verifier.Verify(ctx, raw); err != nil {
		t.Fatalf("stale cache error = %v", err)
	}
	if h.hits.Load() != 2 {
		t.Fatalf("hits = %d", h.hits.Load())
	}

	h.clock.advance(maxKeyAge)
	_, err := h.verifier.Verify(ctx, raw)
	requireUnavailable(t, err)
	if h.hits.Load() != 3 {
		t.Fatalf("hits = %d", h.hits.Load())
	}
}

func TestKeyCacheUnknownKeyRefresh(t *testing.T) {
	h := newHarness(t, time.Second)
	missing := h.accessSpec()
	missing.kid = "missing"
	raw := h.sign(missing)
	ctx := context.Background()

	_, err := h.verifier.Verify(ctx, raw)
	requireInvalid(t, err)
	_, err = h.verifier.Verify(ctx, raw)
	requireInvalid(t, err)
	if h.hits.Load() != 1 {
		t.Fatalf("hits = %d", h.hits.Load())
	}

	rotated, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	h.replaceKeys(publicJWK(rotated, "rotated"))
	h.clock.advance(refreshCooldown)
	spec := h.accessSpec()
	spec.priv = rotated
	spec.kid = "rotated"
	if _, err := h.verifier.Verify(ctx, h.sign(spec)); err != nil {
		t.Fatalf("rotated key error = %v", err)
	}
	if h.hits.Load() != 2 {
		t.Fatalf("hits = %d", h.hits.Load())
	}
}

func TestKeyCacheSharesOneRetrieval(t *testing.T) {
	h := newHarness(t, 2*time.Second)
	h.entered = make(chan struct{})
	h.release = make(chan struct{})
	raw := h.sign(h.accessSpec())

	const callers = 20
	errCh := make(chan error, callers)
	for range callers {
		go func() {
			_, err := h.verifier.Verify(context.Background(), raw)
			errCh <- err
		}()
	}

	select {
	case <-h.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for jwks")
	}
	close(h.release)

	for range callers {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	if h.hits.Load() != 1 {
		t.Fatalf("hits = %d", h.hits.Load())
	}
}

func TestKeyCacheUnavailable(t *testing.T) {
	h := newHarness(t, time.Second)
	h.fail.Store(true)
	raw := h.sign(h.accessSpec())
	ctx := context.Background()

	_, err := h.verifier.Verify(ctx, raw)
	requireUnavailable(t, err)
	_, err = h.verifier.Verify(ctx, raw)
	requireUnavailable(t, err)
	if h.hits.Load() != 1 {
		t.Fatalf("hits = %d", h.hits.Load())
	}
}

func TestKeyCacheRetrievalTimeout(t *testing.T) {
	h := newHarness(t, 200*time.Millisecond)
	h.hold.Store(true)
	_, err := h.verifier.Verify(context.Background(), h.sign(h.accessSpec()))
	requireUnavailable(t, err)
}

func TestKeyCacheDoesNotFollowRedirects(t *testing.T) {
	h := newHarness(t, time.Second)
	var target atomic.Int32
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/target", http.StatusFound)
	})
	mux.HandleFunc("/target", func(http.ResponseWriter, *http.Request) {
		target.Add(1)
	})

	verifier := NewVerifier(testIssuer, testAudience, testClientID, server.URL+"/jwks", time.Second)
	verifier.now = h.clock.now
	_, err := verifier.Verify(context.Background(), h.sign(h.accessSpec()))
	requireUnavailable(t, err)
	if target.Load() != 0 {
		t.Fatalf("redirect target hits = %d", target.Load())
	}
}

func TestKeyCacheRejectsOversizedAndDuplicateDocuments(t *testing.T) {
	oversized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte(" "), maxJWKSBytes+1))
	}))
	t.Cleanup(oversized.Close)

	h := newHarness(t, time.Second)
	verifier := NewVerifier(testIssuer, testAudience, testClientID, oversized.URL, time.Second)
	verifier.now = h.clock.now
	_, err := verifier.Verify(context.Background(), h.sign(h.accessSpec()))
	requireUnavailable(t, err)

	h.replaceKeys(publicJWK(h.access, accessKID), publicJWK(h.access, accessKID))
	_, err = h.verifier.Verify(context.Background(), h.sign(h.accessSpec()))
	requireUnavailable(t, err)
}

func TestKeyCacheSkipsShortRSAKey(t *testing.T) {
	h := newHarness(t, time.Second)
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	h.replaceKeys(publicJWK(small, "small"))
	spec := h.accessSpec()
	spec.priv = small
	spec.kid = "small"
	_, err = h.verifier.Verify(context.Background(), h.sign(spec))
	requireInvalid(t, err)
}

func TestSigningKeysBounds(t *testing.T) {
	access, _ := testKeys(t)
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	enc := publicJWK(access, "enc")
	enc.Use = "enc"
	otherAlg := publicJWK(access, "rs384")
	otherAlg.Algorithm = "RS384"

	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &ec.PublicKey, KeyID: "ec", Algorithm: string(jose.ES256), Use: "sig"},
		publicJWK(small, "small"),
		enc,
		otherAlg,
		publicJWK(access, accessKID),
	}}
	keys, err := signingKeys(set)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[accessKID] == nil {
		t.Fatalf("keys = %d", len(keys))
	}

	set.Keys = nil
	for i := range maxSigningKeys {
		set.Keys = append(set.Keys, publicJWK(access, "k"+strconv.Itoa(i)))
	}
	keys, err = signingKeys(set)
	if err != nil || len(keys) != maxSigningKeys {
		t.Fatalf("keys = %d, err = %v", len(keys), err)
	}
	set.Keys = append(set.Keys, publicJWK(access, "extra"))
	if _, err := signingKeys(set); !errors.Is(err, errTooManyKeys) {
		t.Fatalf("err = %v", err)
	}

	set.Keys = []jose.JSONWebKey{publicJWK(access, "same"), publicJWK(access, "same")}
	if _, err := signingKeys(set); !errors.Is(err, errDuplicateKey) {
		t.Fatalf("err = %v", err)
	}
}
