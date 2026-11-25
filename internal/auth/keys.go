package auth

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const (
	// keyFreshness is how long a known signing key is used without contacting the provider.
	keyFreshness = 15 * time.Minute

	// refreshCooldown is the shortest gap between JWKS retrievals.
	refreshCooldown = 30 * time.Second

	// maxKeyAge is how long a cached key can still verify a token after the last successful retrieval.
	maxKeyAge = 24 * time.Hour

	maxJWKSBytes   = 64 * 1024
	maxSigningKeys = 16
	minRSABits     = 2048
)

var (
	errJWKSRequest  = errors.New("jwks request failed")
	errDuplicateKey = errors.New("duplicate signing key")
	errTooManyKeys  = errors.New("too many signing keys")
)

type keyCache struct {
	url         string
	timeout     time.Duration
	client      *http.Client
	now         func() time.Time
	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	attemptedAt time.Time
	inflight    *fetchResult
}

type fetchResult struct {
	done chan struct{}
	err  error
}

func newKeyCache(jwksURL string, timeout time.Duration, now func() time.Time) *keyCache {
	if now == nil {
		now = time.Now
	}
	return &keyCache{
		url:     jwksURL,
		timeout: timeout,
		now:     now,
		client: &http.Client{
			Timeout:       timeout,
			CheckRedirect: refuseRedirect,
		},
	}
}

func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func (c *keyCache) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	if key, ok := c.freshLocked(kid); ok {
		c.mu.Unlock()
		return key, nil
	}
	if c.coolingDownLocked() {
		key, err := c.cooledLocked(kid)
		c.mu.Unlock()
		return key, err
	}
	wait := c.inflight
	lead := false
	if wait == nil {
		wait = &fetchResult{done: make(chan struct{})}
		c.inflight = wait
		lead = true
	}
	c.mu.Unlock()

	if lead {
		go c.lead(wait)
	}
	if err := waitFor(ctx, wait); err != nil {
		if key, ok := c.usable(kid); ok {
			return key, nil
		}
		return nil, unavailable(err)
	}
	if key, ok := c.usable(kid); ok {
		return key, nil
	}
	return nil, invalidToken(errUnknownKey)
}

func (c *keyCache) lead(wait *fetchResult) {
	keys, err := c.retrieve()
	c.mu.Lock()
	c.attemptedAt = c.now()
	if err == nil {
		c.keys = keys
		c.fetchedAt = c.attemptedAt
	}
	c.inflight = nil
	wait.err = err
	close(wait.done)
	c.mu.Unlock()
}

func waitFor(ctx context.Context, wait *fetchResult) error {
	select {
	case <-wait.done:
		return wait.err
	case <-ctx.Done():
	}
	select {
	case <-wait.done:
		return wait.err
	default:
		return ctx.Err()
	}
}

func (c *keyCache) freshLocked(kid string) (*rsa.PublicKey, bool) {
	key, ok := c.usableLocked(kid)
	if !ok {
		return nil, false
	}
	if c.ageLocked() <= keyFreshness {
		return key, true
	}
	return nil, false
}

func (c *keyCache) usable(kid string) (*rsa.PublicKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usableLocked(kid)
}

func (c *keyCache) usableLocked(kid string) (*rsa.PublicKey, bool) {
	key := c.keys[kid]
	if key == nil || c.fetchedAt.IsZero() {
		return nil, false
	}
	if c.ageLocked() > maxKeyAge {
		return nil, false
	}
	return key, true
}

func (c *keyCache) ageLocked() time.Duration {
	age := c.now().Sub(c.fetchedAt)
	if age < 0 {
		return 0
	}
	return age
}

func (c *keyCache) coolingDownLocked() bool {
	if c.attemptedAt.IsZero() {
		return false
	}
	elapsed := c.now().Sub(c.attemptedAt)
	return elapsed >= 0 && elapsed < refreshCooldown
}

func (c *keyCache) cooledLocked(kid string) (*rsa.PublicKey, error) {
	if key, ok := c.usableLocked(kid); ok {
		return key, nil
	}
	if c.lastAttemptSucceededLocked() {
		return nil, invalidToken(errUnknownKey)
	}
	return nil, unavailable(errJWKSRequest)
}

func (c *keyCache) lastAttemptSucceededLocked() bool {
	return !c.fetchedAt.IsZero() && !c.fetchedAt.Before(c.attemptedAt)
}

func (c *keyCache) retrieve() (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", errJWKSRequest, resp.StatusCode)
	}
	if len(body) > maxJWKSBytes {
		return nil, fmt.Errorf("%w: response too large", errJWKSRequest)
	}

	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, err
	}
	return signingKeys(set)
}

func signingKeys(set jose.JSONWebKeySet) (map[string]*rsa.PublicKey, error) {
	keys := make(map[string]*rsa.PublicKey)
	for _, jwk := range set.Keys {
		pub := rsaSigningKey(jwk)
		if pub == nil {
			continue
		}
		if _, exists := keys[jwk.KeyID]; exists {
			return nil, errDuplicateKey
		}
		keys[jwk.KeyID] = pub
		if len(keys) > maxSigningKeys {
			return nil, errTooManyKeys
		}
	}
	return keys, nil
}

func rsaSigningKey(jwk jose.JSONWebKey) *rsa.PublicKey {
	if jwk.KeyID == "" || (jwk.Use != "" && jwk.Use != "sig") || (jwk.Algorithm != "" && jwk.Algorithm != string(jose.RS256)) {
		return nil
	}
	switch key := jwk.Key.(type) {
	case *rsa.PublicKey:
		if key == nil || key.N == nil || key.N.BitLen() < minRSABits {
			return nil
		}
		return key
	default:
		return nil
	}
}
