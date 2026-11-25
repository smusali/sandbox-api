package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestDecodeConsume(t *testing.T) {
	tests := []struct {
		name    string
		parts   []string
		want    string
		wantErr error
	}{
		{name: "missing", parts: []string{"missing", ""}, wantErr: errStateMissing},
		{name: "mismatch", parts: []string{"mismatch", ""}, wantErr: errStateMismatch},
		{name: "corrupt", parts: []string{"corrupt", ""}, wantErr: errStateCorrupt},
		{name: "ok", parts: []string{"ok", "verifier"}, want: "verifier"},
		{name: "short", parts: []string{"ok"}, wantErr: errStateReply},
		{name: "missing with verifier", parts: []string{"missing", "verifier"}, wantErr: errStateReply},
		{name: "empty verifier", parts: []string{"ok", ""}, wantErr: errStateCorrupt},
		{name: "unknown", parts: []string{"other", ""}, wantErr: errStateReply},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeConsume(tt.parts)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("decodeConsume() = %q, %v", got, err)
			}
		})
	}
}

func TestRedisConsumeIsSingleUse(t *testing.T) {
	store, client := newRedisStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state := mustSecret(t)
	binding := mustSecret(t)
	verifier := mustSecret(t)
	hash := bindingHash(binding)
	remember(t, client, state)

	err := store.Save(ctx, state, transaction{BrowserBinding: hash, CodeVerifier: verifier}, 2*time.Minute)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	err = store.Save(ctx, state, transaction{BrowserBinding: "other", CodeVerifier: "replaced"}, 2*time.Minute)
	if !errors.Is(err, errStateExists) {
		t.Fatalf("second Save() error = %v", err)
	}

	raw, err := client.Get(ctx, stateKey(state)).Bytes()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if bytes.Contains(raw, []byte(binding)) {
		t.Fatalf("redis value contains the raw binding: %s", raw)
	}
	var stored transaction
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("stored json: %v", err)
	}
	if stored.BrowserBinding != hash || stored.CodeVerifier != verifier {
		t.Fatalf("stored = %+v", stored)
	}
	ttl, err := client.TTL(ctx, stateKey(state)).Result()
	if err != nil {
		t.Fatalf("TTL() error = %v", err)
	}
	if ttl <= 0 || ttl > 2*time.Minute {
		t.Fatalf("TTL = %s", ttl)
	}

	_, err = store.Consume(ctx, state, bindingHash(mustSecret(t)))
	if !errors.Is(err, errStateMismatch) {
		t.Fatalf("mismatch error = %v", err)
	}
	if _, err := client.Get(ctx, stateKey(state)).Result(); err != nil {
		t.Fatalf("state deleted on mismatch: %v", err)
	}

	got, err := store.Consume(ctx, state, hash)
	if err != nil || got != verifier {
		t.Fatalf("Consume() = %q, %v", got, err)
	}
	_, err = store.Consume(ctx, state, hash)
	if !errors.Is(err, errStateMissing) {
		t.Fatalf("replay error = %v", err)
	}
}

func TestRedisConsumeIsAtomic(t *testing.T) {
	store, client := newRedisStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state := mustSecret(t)
	hash := bindingHash(mustSecret(t))
	remember(t, client, state)
	if err := store.Save(ctx, state, transaction{BrowserBinding: hash, CodeVerifier: "verifier"}, time.Minute); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := store.Consume(ctx, state, hash)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var okCount, missingCount int
	for err := range results {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, errStateMissing):
			missingCount++
		default:
			t.Fatalf("Consume() error = %v", err)
		}
	}
	if okCount != 1 || missingCount != 1 {
		t.Fatalf("ok = %d, missing = %d", okCount, missingCount)
	}
}

func TestRedisCorruptStateIsLeftInPlace(t *testing.T) {
	store, client := newRedisStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	state := mustSecret(t)
	remember(t, client, state)
	if err := client.Set(ctx, stateKey(state), "{", time.Minute).Err(); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	_, err := store.Consume(ctx, state, bindingHash(mustSecret(t)))
	if !errors.Is(err, errStateCorrupt) {
		t.Fatalf("Consume() error = %v", err)
	}
	raw, err := client.Get(ctx, stateKey(state)).Result()
	if err != nil || raw != "{" {
		t.Fatalf("stored = %q, %v", raw, err)
	}
}

func newRedisStore(t *testing.T) (*RedisStore, *redis.Client) {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Skipf("redis unreachable: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
	})
	return NewRedisStore(client), client
}

func remember(t *testing.T, client *redis.Client, state string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = client.Del(ctx, stateKey(state)).Err()
	})
}
