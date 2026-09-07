package rediscache

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucketScript is a token bucket evaluated atomically inside Redis.
//
// Running it as a script matters: read-modify-write from Go would need either a
// WATCH/MULTI retry loop or a lock, and under contention both cost more than
// the limiter saves. A single EVALSHA is one round trip and cannot interleave.
//
// A token bucket rather than a fixed window because a fixed window lets a
// client spend its whole quota in the last millisecond of one window and again
// in the first of the next -- twice the intended rate at the boundary.
//
//	KEYS[1] bucket key
//	ARGV[1] refill rate, tokens per second
//	ARGV[2] burst, the bucket capacity
//	ARGV[3] current time in milliseconds
//	ARGV[4] tokens requested
//
// Returns {allowed, tokens_remaining, retry_after_ms}.
const tokenBucketScript = `
local rate     = tonumber(ARGV[1])
local burst    = tonumber(ARGV[2])
local now      = tonumber(ARGV[3])
local cost     = tonumber(ARGV[4])

local bucket = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(bucket[1])
local ts     = tonumber(bucket[2])

if tokens == nil or ts == nil then
  tokens = burst
  ts     = now
end

-- Refill for the time that passed, capped at the burst size.
local elapsed = math.max(0, now - ts) / 1000.0
tokens = math.min(burst, tokens + elapsed * rate)

local allowed     = 0
local retry_after = 0
if tokens >= cost then
  allowed = 1
  tokens  = tokens - cost
else
  retry_after = math.ceil(((cost - tokens) / rate) * 1000)
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
-- Expire once a full refill would have happened anyway: an idle bucket is
-- indistinguishable from a fresh one, so keeping it wastes memory.
redis.call('PEXPIRE', KEYS[1], math.ceil((burst / rate) * 1000) + 1000)

return {allowed, math.floor(tokens), retry_after}
`

// Limiter is a distributed token bucket.
type Limiter struct {
	rdb    redis.UniversalClient
	script *redis.Script
	rate   float64
	burst  int
	log    *slog.Logger
}

// NewLimiter wires a Limiter allowing `rate` requests per second per key, with
// bursts of up to `burst`.
func NewLimiter(rdb redis.UniversalClient, rate float64, burst int, log *slog.Logger) *Limiter {
	return &Limiter{
		rdb:    rdb,
		script: redis.NewScript(tokenBucketScript),
		rate:   rate,
		burst:  burst,
		log:    log,
	}
}

// Allow consumes one token for key. It satisfies httpapi.RateLimiter.
//
// If Redis is unreachable the request is allowed. That is a deliberate choice:
// a rate limiter is a protection mechanism, not a correctness one, and failing
// closed would turn a cache outage into a total outage.
func (l *Limiter) Allow(ctx context.Context, key string) (allowed bool, remaining int, retryAfter time.Duration) {
	res, err := l.script.Run(ctx, l.rdb, []string{"ratelimit:" + key},
		l.rate, l.burst, time.Now().UnixMilli(), 1,
	).Slice()
	if err != nil {
		l.log.WarnContext(ctx, "rate limiter unavailable, failing open",
			slog.String("key", key), slog.Any("error", err))
		return true, l.burst, 0
	}

	allowed, remaining, retryAfterMS, err := parseDecision(res)
	if err != nil {
		l.log.WarnContext(ctx, "unexpected rate limiter reply, failing open", slog.Any("error", err))
		return true, l.burst, 0
	}
	return allowed, remaining, time.Duration(retryAfterMS) * time.Millisecond
}

// parseDecision unpacks the script's three-element reply. Redis returns Lua
// numbers as int64.
func parseDecision(res []any) (allowed bool, remaining, retryAfterMS int, err error) {
	if len(res) != 3 {
		return false, 0, 0, fmt.Errorf("rediscache: expected 3 values from the limiter script, got %d", len(res))
	}

	values := make([]int64, 3)
	for i, v := range res {
		n, ok := v.(int64)
		if !ok {
			return false, 0, 0, fmt.Errorf("rediscache: limiter value %d is %T, want int64", i, v)
		}
		values[i] = n
	}
	return values[0] == 1, int(values[1]), int(values[2]), nil
}
