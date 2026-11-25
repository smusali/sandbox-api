package ratelimit

import (
	"context"
	_ "embed"
)

// admitScript applies one fixed-window admission.
// A stored count must be a positive integer of at most 15 digits.
// Longer digit strings are not exact Lua numbers, so they are invalid state.
// Invalid state is deleted. This attempt is not admitted.
//
//go:embed admit.lua
var admitScript string

func (l *Limiter) admit(ctx context.Context, key string, limit int64) ([]int64, error) {
	return l.client.Eval(ctx, admitScript, []string{key}, limit, l.windowMillis).Int64Slice()
}
