package ratelimit

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func TestClientOptionsDisablesReplay(t *testing.T) {
	opt := ClientOptions("localhost:6379", 5*time.Second)
	if opt.Addr != "localhost:6379" || opt.DialTimeout != 5*time.Second || opt.ReadTimeout != 5*time.Second || opt.WriteTimeout != 5*time.Second || opt.MaxRetries != -1 {
		t.Fatalf("options = %+v", opt)
	}
}

func TestNewRejectsClientAndWindow(t *testing.T) {
	client := redis.NewClient(ClientOptions("127.0.0.1:1", time.Second))
	t.Cleanup(func() { _ = client.Close() })
	tests := []struct {
		name    string
		client  *redis.Client
		window  time.Duration
		wantErr error
	}{
		{name: "nil client", window: time.Minute, wantErr: errNilClient},
		{name: "zero window", client: client, wantErr: errInvalidWindow},
		{name: "negative window", client: client, window: -time.Second, wantErr: errInvalidWindow},
		{name: "sub millisecond", client: client, window: 500 * time.Microsecond, wantErr: errInvalidWindow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.client, tt.window)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("New() error = %v", err)
			}
		})
	}
	if _, err := New(client, time.Millisecond); err != nil {
		t.Fatalf("New() error = %v", err)
	}
}

func TestDecisionFrom(t *testing.T) {
	tests := []struct {
		name      string
		values    []int64
		allowed   bool
		remaining time.Duration
		wantErr   error
	}{
		{name: "allowed", values: []int64{decisionAllowed, 1500}, allowed: true, remaining: 1500 * time.Millisecond},
		{name: "denied", values: []int64{decisionDenied, 24}, remaining: 24 * time.Millisecond},
		{name: "zero ttl", values: []int64{decisionAllowed, 0}, allowed: true},
		{name: "short", values: []int64{decisionAllowed}, wantErr: ErrUnavailable},
		{name: "long", values: []int64{decisionAllowed, 1, 1}, wantErr: ErrUnavailable},
		{name: "other decision", values: []int64{2, 1}, wantErr: ErrUnavailable},
		{name: "negative ttl", values: []int64{decisionAllowed, -1}, wantErr: ErrUnavailable},
		{name: "overflow", values: []int64{decisionAllowed, 1<<62 + 1}, wantErr: ErrUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decisionFrom(tt.values)
			if !errors.Is(err, tt.wantErr) || got.Allowed != tt.allowed || got.Remaining != tt.remaining {
				t.Fatalf("decisionFrom() = %+v, %v", got, err)
			}
		})
	}
}

func TestAllowRejectsPolicyBeforeRedis(t *testing.T) {
	var dialed atomic.Bool
	client := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1",
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			dialed.Store(true)
			return nil, errors.New("unexpected dial")
		},
		DialTimeout:  time.Second,
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = client.Close() })
	limiter, err := New(client, time.Minute)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx := context.Background()
	projectID := uuid.New()
	tests := []struct {
		name      string
		projectID uuid.UUID
		limit     int32
	}{
		{name: "zero project", projectID: uuid.Nil, limit: 20},
		{name: "zero limit", projectID: projectID, limit: 0},
		{name: "negative limit", projectID: projectID, limit: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := limiter.Allow(ctx, tt.projectID, tt.limit)
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("Allow() error = %v", err)
			}
		})
	}
	if dialed.Load() {
		t.Fatal("Allow() dialed Redis")
	}
}

func TestRedisAdmitsFixedWindow(t *testing.T) {
	window := 30 * time.Second
	limiter, client := newLimiter(t, window)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	projectID := uuid.New()
	resetKey(t, client, projectID)

	const limit int32 = 20
	for n := int32(1); n <= limit; n++ {
		decision, err := limiter.Allow(ctx, projectID, limit)
		if err != nil || !decision.Allowed || decision.Remaining <= 0 || decision.Remaining > window {
			t.Fatalf("Allow(%d) = %+v, %v", n, decision, err)
		}
	}
	decision, err := limiter.Allow(ctx, projectID, limit)
	if err != nil || decision.Allowed || decision.Remaining <= 0 || decision.Remaining > window {
		t.Fatalf("Allow(21) = %+v, %v", decision, err)
	}
	raw, err := client.Get(ctx, creationKey(projectID)).Result()
	if err != nil || raw != "20" {
		t.Fatalf("stored = %q, %v", raw, err)
	}
}

func TestRedisExpirationStaysFixed(t *testing.T) {
	window := 30 * time.Second
	limiter, client := newLimiter(t, window)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	projectID := uuid.New()
	resetKey(t, client, projectID)
	key := creationKey(projectID)

	decision, err := limiter.Allow(ctx, projectID, 2)
	if err != nil || !decision.Allowed {
		t.Fatalf("Allow() = %+v, %v", decision, err)
	}
	first := pttl(t, client, key)
	time.Sleep(100 * time.Millisecond)
	decision, err = limiter.Allow(ctx, projectID, 2)
	if err != nil || !decision.Allowed {
		t.Fatalf("Allow() = %+v, %v", decision, err)
	}
	second := pttl(t, client, key)
	if second >= first {
		t.Fatalf("ttl refreshed from %s to %s", first, second)
	}
	time.Sleep(100 * time.Millisecond)
	decision, err = limiter.Allow(ctx, projectID, 2)
	if err != nil || decision.Allowed {
		t.Fatalf("Allow() = %+v, %v", decision, err)
	}
	if stored := mustGet(t, client, key); stored != "2" {
		t.Fatalf("stored = %q", stored)
	}
	third := pttl(t, client, key)
	if third >= second {
		t.Fatalf("ttl refreshed from %s to %s", second, third)
	}
}

func TestRedisProjectsAreIndependent(t *testing.T) {
	limiter, client := newLimiter(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := uuid.New()
	second := uuid.New()
	resetKey(t, client, first)
	resetKey(t, client, second)

	decision, err := limiter.Allow(ctx, first, 1)
	if err != nil || !decision.Allowed {
		t.Fatalf("Allow(first) = %+v, %v", decision, err)
	}
	decision, err = limiter.Allow(ctx, first, 1)
	if err != nil || decision.Allowed {
		t.Fatalf("Allow(first again) = %+v, %v", decision, err)
	}
	decision, err = limiter.Allow(ctx, second, 1)
	if err != nil || !decision.Allowed {
		t.Fatalf("Allow(second) = %+v, %v", decision, err)
	}
	if stored := mustGet(t, client, creationKey(first)); stored != "1" {
		t.Fatalf("first = %q", stored)
	}
	if stored := mustGet(t, client, creationKey(second)); stored != "1" {
		t.Fatalf("second = %q", stored)
	}
}

func TestRedisWindowExpires(t *testing.T) {
	limiter, client := newLimiter(t, 300*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	projectID := uuid.New()
	resetKey(t, client, projectID)
	key := creationKey(projectID)

	decision, err := limiter.Allow(ctx, projectID, 20)
	if err != nil || !decision.Allowed {
		t.Fatalf("Allow() = %+v, %v", decision, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := client.Get(ctx, key).Err()
		if errors.Is(err, redis.Nil) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("key remained: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	decision, err = limiter.Allow(ctx, projectID, 20)
	if err != nil || !decision.Allowed {
		t.Fatalf("Allow() = %+v, %v", decision, err)
	}
	if stored := mustGet(t, client, key); stored != "1" {
		t.Fatalf("stored = %q", stored)
	}
}

func TestRedisInvalidStateIsRemoved(t *testing.T) {
	limiter, client := newLimiter(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tests := []struct {
		name  string
		prime func(key string) error
	}{
		{name: "letters", prime: func(key string) error { return client.Set(ctx, key, "bad", time.Minute).Err() }},
		{name: "empty", prime: func(key string) error { return client.Set(ctx, key, "", time.Minute).Err() }},
		{name: "zero", prime: func(key string) error { return client.Set(ctx, key, "0", time.Minute).Err() }},
		{name: "fraction", prime: func(key string) error { return client.Set(ctx, key, "1.5", time.Minute).Err() }},
		{name: "negative", prime: func(key string) error { return client.Set(ctx, key, "-1", time.Minute).Err() }},
		{name: "leading zero", prime: func(key string) error { return client.Set(ctx, key, "01", time.Minute).Err() }},
		{name: "too long", prime: func(key string) error { return client.Set(ctx, key, "9999999999999999", time.Minute).Err() }},
		{name: "no expiration", prime: func(key string) error { return client.Set(ctx, key, "4", 0).Err() }},
		{name: "wrong type", prime: func(key string) error { return client.HSet(ctx, key, "field", "value").Err() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projectID := uuid.New()
			resetKey(t, client, projectID)
			key := creationKey(projectID)
			if err := tt.prime(key); err != nil {
				t.Fatalf("prime() error = %v", err)
			}
			if _, err := limiter.Allow(ctx, projectID, 20); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("Allow() error = %v", err)
			}
			exists, err := client.Exists(ctx, key).Result()
			if err != nil || exists != 0 {
				t.Fatalf("exists = %d, %v", exists, err)
			}
			decision, err := limiter.Allow(ctx, projectID, 20)
			if err != nil || !decision.Allowed {
				t.Fatalf("Allow() = %+v, %v", decision, err)
			}
			if stored := mustGet(t, client, key); stored != "1" {
				t.Fatalf("stored = %q", stored)
			}
		})
	}
}

func TestRedisScriptRejectsBadPolicy(t *testing.T) {
	_, client := newLimiter(t, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	projectID := uuid.New()
	resetKey(t, client, projectID)
	key := creationKey(projectID)
	err := client.Eval(ctx, admitScript, []string{key}, int64(0), int64(1000)).Err()
	if !redis.HasErrorPrefix(err, invalidLimitPolicy) {
		t.Fatalf("Eval() error = %v", err)
	}
	exists, existsErr := client.Exists(ctx, key).Result()
	if existsErr != nil || exists != 0 {
		t.Fatalf("exists = %d, %v", exists, existsErr)
	}
}

func TestRedisUnavailable(t *testing.T) {
	client := redis.NewClient(ClientOptions("127.0.0.1:1", 200*time.Millisecond))
	t.Cleanup(func() { _ = client.Close() })
	limiter, err := New(client, time.Minute)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = limiter.Allow(ctx, uuid.New(), 20)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Allow() error = %v", err)
	}
}

func TestRedisConcurrentAllowance(t *testing.T) {
	limiter, client := newLimiter(t, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	projectID := uuid.New()
	resetKey(t, client, projectID)

	const limit int32 = 20
	const callers = 40
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, callers)
	var allowed atomic.Int32
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			decision, err := limiter.Allow(ctx, projectID, limit)
			if err != nil {
				results <- err
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			}
			results <- nil
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("Allow() error = %v", err)
		}
	}
	if got := allowed.Load(); got != limit {
		t.Fatalf("allowed = %d", got)
	}
	if stored := mustGet(t, client, creationKey(projectID)); stored != "20" {
		t.Fatalf("stored = %q", stored)
	}
}

func newLimiter(t *testing.T, window time.Duration) (*Limiter, *redis.Client) {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	client := redis.NewClient(ClientOptions(addr, 2*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Skipf("redis unreachable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	limiter, err := New(client, window)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return limiter, client
}

func resetKey(t *testing.T, client *redis.Client, projectID uuid.UUID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Del(ctx, creationKey(projectID)).Err(); err != nil {
		t.Fatalf("Del() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = client.Del(ctx, creationKey(projectID)).Err()
	})
}

func pttl(t *testing.T, client *redis.Client, key string) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ttl, err := client.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL() error = %v", err)
	}
	if ttl <= 0 {
		t.Fatalf("PTTL() = %s", ttl)
	}
	return ttl
}

func mustGet(t *testing.T, client *redis.Client, key string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := client.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	return raw
}
