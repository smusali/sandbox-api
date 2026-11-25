// Package ratelimit admits sandbox creation attempts with a fixed window per project.
// The window starts on the first admitted attempt, and later attempts do not move its expiration.
// A fixed window can admit a burst at the boundary between two windows.
// Redis data loss can reset this temporary allowance.
// An admitted attempt is not returned after quota or persistence failure.
package ratelimit

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const creationKeyPrefix = "sandbox-api:ratelimit:create:"

const (
	decisionAllowed int64 = 1
	decisionDenied  int64 = 0
)

const (
	invalidLimitPolicy    = "INVALID_LIMIT_POLICY"
	invalidRateLimitState = "INVALID_RATE_LIMIT_STATE"
)

// ErrUnavailable means Redis did not produce a trustworthy admission decision.
// Callers must not insert a sandbox after this error.
var ErrUnavailable = errors.New("rate limit unavailable")

var (
	errNilClient     = errors.New("rate limit client missing")
	errInvalidWindow = errors.New("rate limit window invalid")
)

// Decision is one completed admission.
// Allowed is false when the project window is full.
// Remaining is the time left in that window.
type Decision struct {
	Allowed   bool
	Remaining time.Duration
}

// Limiter counts admitted sandbox creation attempts in Redis.
// It has no release operation. An admitted unit stays consumed.
type Limiter struct {
	client       *redis.Client
	windowMillis int64
}

// New prepares admission for window.
// window is the process-wide fixed window shared by every project.
func New(client *redis.Client, window time.Duration) (*Limiter, error) {
	if client == nil {
		return nil, errNilClient
	}
	if window < time.Millisecond {
		return nil, errInvalidWindow
	}
	return &Limiter{client: client, windowMillis: window.Milliseconds()}, nil
}

// ClientOptions returns Redis options for admission.
// MaxRetries is negative so a lost reply is not executed again.
// The zero value enables three retries, which can apply the script twice.
func ClientOptions(addr string, timeout time.Duration) *redis.Options {
	return &redis.Options{
		Addr:         addr,
		DialTimeout:  timeout,
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
		MaxRetries:   -1,
	}
}

// Allow admits one sandbox creation attempt for projectID when the stored count is below limit.
// limit is that project's persisted allowance.
// A full window returns Allowed set to false and a nil error.
// The zero project id and a non-positive limit fail closed and do not change Redis.
func (l *Limiter) Allow(ctx context.Context, projectID uuid.UUID, limit int32) (Decision, error) {
	if projectID == uuid.Nil || limit < 1 {
		return Decision{}, ErrUnavailable
	}
	values, err := l.client.Eval(
		ctx,
		admitScript,
		[]string{creationKey(projectID)},
		int64(limit),
		l.windowMillis,
	).Int64Slice()
	if err != nil {
		return Decision{}, closed(err)
	}
	return decisionFrom(values)
}

func creationKey(projectID uuid.UUID) string {
	return creationKeyPrefix + projectID.String()
}

func closed(err error) error {
	if redis.HasErrorPrefix(err, invalidLimitPolicy) || redis.HasErrorPrefix(err, invalidRateLimitState) {
		return ErrUnavailable
	}
	return ErrUnavailable
}

func decisionFrom(values []int64) (Decision, error) {
	if len(values) != 2 {
		return Decision{}, ErrUnavailable
	}
	remaining, err := remainingFrom(values[1])
	if err != nil {
		return Decision{}, err
	}
	switch values[0] {
	case decisionAllowed:
		return Decision{Allowed: true, Remaining: remaining}, nil
	case decisionDenied:
		return Decision{Allowed: false, Remaining: remaining}, nil
	default:
		return Decision{}, ErrUnavailable
	}
}

func remainingFrom(millis int64) (time.Duration, error) {
	if millis < 0 {
		return 0, ErrUnavailable
	}
	remaining := time.Duration(millis) * time.Millisecond
	if remaining < 0 || int64(remaining/time.Millisecond) != millis {
		return 0, ErrUnavailable
	}
	return remaining, nil
}

// admitScript applies one fixed-window admission.
// A stored count must be a positive integer of at most 15 digits.
// Longer digit strings are not exact Lua numbers, so they are invalid state.
// Invalid state is deleted in this script. This attempt is not admitted.
const admitScript = `
local limit = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
if not limit or limit < 1 or limit ~= math.floor(limit)
    or not window or window < 1 or window ~= math.floor(window) then
  return redis.error_reply("INVALID_LIMIT_POLICY")
end

local current = redis.pcall("GET", KEYS[1])
if type(current) == "table" then
  redis.call("DEL", KEYS[1])
  return redis.error_reply("INVALID_RATE_LIMIT_STATE")
end
if not current then
  redis.call("SET", KEYS[1], "1", "PX", window)
  return {1, window}
end

local ttl = redis.call("PTTL", KEYS[1])
if ttl < 0 then
  redis.call("DEL", KEYS[1])
  return redis.error_reply("INVALID_RATE_LIMIT_STATE")
end
if type(current) ~= "string"
    or not string.match(current, "^[1-9]%d*$")
    or #current > 15 then
  redis.call("DEL", KEYS[1])
  return redis.error_reply("INVALID_RATE_LIMIT_STATE")
end

local count = tonumber(current)
if not count or count < 1 or count ~= math.floor(count) then
  redis.call("DEL", KEYS[1])
  return redis.error_reply("INVALID_RATE_LIMIT_STATE")
end
if count >= limit then
  return {0, ttl}
end

redis.call("INCR", KEYS[1])
return {1, ttl}
`
