package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const stateKeyPrefix = "sandbox-api:oauth:state:"

const consumeStateScript = `
local raw = redis.call("GET", KEYS[1])
if not raw then
  return {"missing", ""}
end
local binding = string.match(raw, '"browser_binding":"([^"]+)"')
local verifier = string.match(raw, '"code_verifier":"([^"]+)"')
if not binding or not verifier then
  return {"corrupt", ""}
end
if binding ~= ARGV[1] then
  return {"mismatch", ""}
end
redis.call("DEL", KEYS[1])
return {"ok", verifier}
`

const (
	consumeMissing  = "missing"
	consumeMismatch = "mismatch"
	consumeCorrupt  = "corrupt"
	consumeOK       = "ok"
)

var (
	errStateExists   = errors.New("oauth state exists")
	errStateMissing  = errors.New("oauth state missing")
	errStateMismatch = errors.New("oauth state mismatch")
	errStateCorrupt  = errors.New("oauth state corrupt")
	errStateReply    = errors.New("oauth state reply invalid")
)

// transaction is the temporary authorization record stored under one state.
type transaction struct {
	BrowserBinding string `json:"browser_binding"`
	CodeVerifier   string `json:"code_verifier"`
}

// RedisStore keeps authorization transactions in Redis.
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore stores authorization transactions in client.
func NewRedisStore(client *redis.Client) *RedisStore {
	return &RedisStore{client: client}
}

// Save writes record when the state key is absent.
// An existing key is left unchanged.
func (s *RedisStore) Save(ctx context.Context, state string, record transaction, ttl time.Duration) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	saved, err := s.client.SetNX(ctx, stateKey(state), payload, ttl).Result()
	if err != nil {
		return err
	}
	if !saved {
		return errStateExists
	}
	return nil
}

// Consume returns the PKCE verifier and deletes the state when bindingHash matches.
// A missing or different binding leaves the key in place.
func (s *RedisStore) Consume(ctx context.Context, state, bindingHash string) (string, error) {
	parts, err := s.client.Eval(ctx, consumeStateScript, []string{stateKey(state)}, bindingHash).StringSlice()
	if err != nil {
		return "", err
	}
	return decodeConsume(parts)
}

func stateKey(state string) string {
	return stateKeyPrefix + state
}

func decodeConsume(parts []string) (string, error) {
	if len(parts) != 2 {
		return "", errStateReply
	}
	switch parts[0] {
	case consumeMissing:
		if parts[1] != "" {
			return "", errStateReply
		}
		return "", errStateMissing
	case consumeMismatch:
		if parts[1] != "" {
			return "", errStateReply
		}
		return "", errStateMismatch
	case consumeCorrupt:
		return "", errStateCorrupt
	case consumeOK:
		if parts[1] == "" {
			return "", errStateCorrupt
		}
		return parts[1], nil
	default:
		return "", errStateReply
	}
}
