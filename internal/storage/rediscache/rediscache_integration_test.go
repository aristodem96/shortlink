package rediscache

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/aristodem96/shortlink/internal/domain"
)

// These tests run against a real Redis. The cache is mostly serialisation, but
// the limiter is a Lua script whose whole point is atomicity: only a real
// server can show that concurrent callers cannot overspend the bucket.
//
// `go test -short` skips them.

var testClient *redis.Client

func TestMain(m *testing.M) {
	// testing.Short panics if the flags have not been parsed yet, and TestMain
	// runs before the framework does it.
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}

	ctx := context.Background()

	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not start redis: %v\n", err)
		os.Exit(1)
	}

	code := func() int {
		defer func() {
			if err := testcontainers.TerminateContainer(container); err != nil {
				fmt.Fprintf(os.Stderr, "could not terminate redis: %v\n", err)
			}
		}()

		uri, err := container.ConnectionString(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not build redis uri: %v\n", err)
			return 1
		}
		opts, err := redis.ParseURL(uri)
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not parse redis uri: %v\n", err)
			return 1
		}

		testClient = redis.NewClient(opts)
		defer func() { _ = testClient.Close() }()

		return m.Run()
	}()

	os.Exit(code)
}

func setup(t *testing.T) *redis.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test: -short")
	}
	require.NotNil(t, testClient, "the redis container failed to start")
	require.NoError(t, testClient.FlushDB(context.Background()).Err())

	return testClient
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLinkCache_RoundTrip(t *testing.T) {
	cache := NewLinkCache(setup(t))
	ctx := context.Background()

	expiry := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	link := &domain.Link{
		ID: 7, Code: "abc1234", TargetURL: "https://go.dev", OwnerID: 3,
		ExpiresAt: &expiry, CreatedAt: time.Now().UTC().Truncate(time.Second),
	}

	require.NoError(t, cache.Set(ctx, link, time.Hour))

	got, err := cache.Get(ctx, "abc1234")
	require.NoError(t, err)
	assert.Equal(t, link.ID, got.ID)
	assert.Equal(t, link.TargetURL, got.TargetURL)
	assert.Equal(t, link.OwnerID, got.OwnerID)
	require.NotNil(t, got.ExpiresAt)
	assert.True(t, expiry.Equal(*got.ExpiresAt))
}

func TestLinkCache_ReportsAMissDistinctlyFromANegativeEntry(t *testing.T) {
	cache := NewLinkCache(setup(t))
	ctx := context.Background()

	// Nothing stored at all.
	_, err := cache.Get(ctx, "unknown")
	assert.ErrorIs(t, err, domain.ErrCacheMiss)

	// Stored as "known to be missing". The two must not be confused: a miss
	// means "ask the database", a negative entry means "do not".
	require.NoError(t, cache.SetMissing(ctx, "unknown", time.Minute))

	_, err = cache.Get(ctx, "unknown")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestLinkCache_Delete(t *testing.T) {
	cache := NewLinkCache(setup(t))
	ctx := context.Background()

	require.NoError(t, cache.SetMissing(ctx, "gone123", time.Minute))
	require.NoError(t, cache.Delete(ctx, "gone123"))

	_, err := cache.Get(ctx, "gone123")
	assert.ErrorIs(t, err, domain.ErrCacheMiss)

	assert.NoError(t, cache.Delete(ctx, "never-existed"), "deleting an absent key is not an error")
}

// TestLinkCache_ShortensTheTTLToTheLinkExpiry stops an expired link from being
// served out of a cache entry that outlives it.
func TestLinkCache_ShortensTheTTLToTheLinkExpiry(t *testing.T) {
	client := setup(t)
	cache := NewLinkCache(client)
	ctx := context.Background()

	expiry := time.Now().UTC().Add(30 * time.Second)
	link := &domain.Link{ID: 1, Code: "soon123", TargetURL: "https://example.com", ExpiresAt: &expiry}

	require.NoError(t, cache.Set(ctx, link, time.Hour))

	ttl, err := client.TTL(ctx, keyPrefix+"soon123").Result()
	require.NoError(t, err)
	assert.LessOrEqual(t, ttl, 30*time.Second, "the entry must not outlive the link")
	assert.Greater(t, ttl, time.Duration(0))
}

func TestLinkCache_DoesNotCacheAnAlreadyExpiredLink(t *testing.T) {
	client := setup(t)
	cache := NewLinkCache(client)
	ctx := context.Background()

	past := time.Now().UTC().Add(-time.Minute)
	link := &domain.Link{ID: 1, Code: "past123", TargetURL: "https://example.com", ExpiresAt: &past}

	require.NoError(t, cache.Set(ctx, link, time.Hour))

	exists, err := client.Exists(ctx, keyPrefix+"past123").Result()
	require.NoError(t, err)
	assert.Zero(t, exists)
}

func TestLinkCache_TreatsCorruptEntriesAsMisses(t *testing.T) {
	client := setup(t)
	cache := NewLinkCache(client)
	ctx := context.Background()

	require.NoError(t, client.Set(ctx, keyPrefix+"junk123", "not json at all", time.Minute).Err())

	// A stale serialisation format must degrade to a database read, not a 500.
	_, err := cache.Get(ctx, "junk123")
	assert.ErrorIs(t, err, domain.ErrCacheMiss)
}

func TestLimiter_AllowsUpToTheBurstThenRejects(t *testing.T) {
	client := setup(t)
	ctx := context.Background()

	// A rate of 1/s means refills are far too slow to interfere within the test.
	limiter := NewLimiter(client, 1, 5, discardLogger())

	for i := 0; i < 5; i++ {
		allowed, remaining, _ := limiter.Allow(ctx, "tenant")
		require.True(t, allowed, "request %d should fit inside the burst of 5", i+1)
		assert.Equal(t, 4-i, remaining)
	}

	allowed, _, retryAfter := limiter.Allow(ctx, "tenant")
	assert.False(t, allowed, "the sixth request must exhaust the bucket")
	assert.Greater(t, retryAfter, time.Duration(0), "a rejection must say how long to wait")
}

func TestLimiter_KeepsBucketsSeparate(t *testing.T) {
	client := setup(t)
	ctx := context.Background()
	limiter := NewLimiter(client, 1, 2, discardLogger())

	for i := 0; i < 2; i++ {
		allowed, _, _ := limiter.Allow(ctx, "noisy")
		require.True(t, allowed)
	}
	exhausted, _, _ := limiter.Allow(ctx, "noisy")
	require.False(t, exhausted)

	// One tenant running out must not affect another.
	allowed, _, _ := limiter.Allow(ctx, "quiet")
	assert.True(t, allowed)
}

func TestLimiter_RefillsOverTime(t *testing.T) {
	client := setup(t)
	ctx := context.Background()

	// 20 tokens per second: a 100ms wait is worth about two tokens.
	limiter := NewLimiter(client, 20, 2, discardLogger())

	for i := 0; i < 2; i++ {
		allowed, _, _ := limiter.Allow(ctx, "refill")
		require.True(t, allowed)
	}
	exhausted, _, _ := limiter.Allow(ctx, "refill")
	require.False(t, exhausted)

	time.Sleep(150 * time.Millisecond)

	allowed, _, _ := limiter.Allow(ctx, "refill")
	assert.True(t, allowed, "tokens should have been refilled by the elapsed time")
}

// TestLimiter_IsAtomicUnderConcurrency is why the bucket is a Lua script: a
// read-modify-write from Go would let concurrent callers each read the same
// token count and all decide they may proceed.
func TestLimiter_IsAtomicUnderConcurrency(t *testing.T) {
	client := setup(t)
	ctx := context.Background()

	const burst = 20
	// A refill rate low enough that no meaningful number of tokens is added
	// while the test runs.
	limiter := NewLimiter(client, 0.001, burst, discardLogger())

	const attempts = 200
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if allowed, _, _ := limiter.Allow(ctx, "race"); allowed {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, burst, granted,
		"%d concurrent callers must be granted exactly the burst size, never more", attempts)
}

func TestLimiter_ExpiresIdleBuckets(t *testing.T) {
	client := setup(t)
	ctx := context.Background()

	limiter := NewLimiter(client, 10, 10, discardLogger())
	limiter.Allow(ctx, "idle")

	ttl, err := client.PTTL(ctx, "ratelimit:idle").Result()
	require.NoError(t, err)
	// An idle bucket refills to full and becomes indistinguishable from a fresh
	// one, so keeping it around would only waste memory.
	assert.Greater(t, ttl, time.Duration(0), "the bucket must carry an expiry")
	assert.LessOrEqual(t, ttl, 3*time.Second)
}

func TestLimiter_FailsOpenWhenRedisIsUnreachable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test: -short")
	}

	// A client pointed at nothing: the limiter must not turn a Redis outage
	// into a total outage.
	dead := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 100 * time.Millisecond,
		MaxRetries:  -1,
	})
	defer func() { _ = dead.Close() }()

	allowed, _, _ := NewLimiter(dead, 1, 1, discardLogger()).Allow(context.Background(), "any")
	assert.True(t, allowed)
}
