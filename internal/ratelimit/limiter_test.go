package ratelimit

import (
	"context"
	"errors"
	"net"
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
