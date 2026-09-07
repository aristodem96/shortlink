package service

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aristodem96/shortlink/internal/domain"
)

// fakeKeyRepo is an in-memory APIKeyRepository.
type fakeKeyRepo struct {
	mu     sync.Mutex
	byHash map[string]*domain.APIKey
	nextID int64

	lookups atomic.Int64
}

func newFakeKeyRepo() *fakeKeyRepo {
	return &fakeKeyRepo{byHash: make(map[string]*domain.APIKey)}
}

func (r *fakeKeyRepo) GetByHash(_ context.Context, hash []byte) (*domain.APIKey, error) {
	r.lookups.Add(1)

	r.mu.Lock()
	defer r.mu.Unlock()

	key, ok := r.byHash[string(hash)]
	if !ok {
		return nil, domain.ErrNotFound
	}

	copied := *key
	return &copied, nil
}

func (r *fakeKeyRepo) Create(_ context.Context, name string, hash []byte) (*domain.APIKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextID++
	key := &domain.APIKey{ID: r.nextID, Name: name, CreatedAt: time.Now()}
	r.byHash[string(hash)] = key

	copied := *key
	return &copied, nil
}

func (r *fakeKeyRepo) revoke(secret string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if key, ok := r.byHash[string(HashKey(secret))]; ok {
		now := time.Now()
		key.RevokedAt = &now
	}
}

func TestIssue_ProducesAUsableKey(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	auth := NewAuthenticator(repo)
	ctx := context.Background()

	secret, key, err := auth.Issue(ctx, "ci")
	require.NoError(t, err)

	assert.True(t, strings.HasPrefix(secret, KeyPrefix), "secrets should be recognisable by their prefix")
	assert.Greater(t, len(secret), 40, "the secret should carry the full 32 bytes of entropy")
	assert.Equal(t, "ci", key.Name)

	got, err := auth.Authenticate(ctx, secret)
	require.NoError(t, err)
	assert.Equal(t, key.ID, got.ID)
}

func TestIssue_StoresOnlyTheHash(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	secret, _, err := NewAuthenticator(repo).Issue(context.Background(), "ci")
	require.NoError(t, err)

	repo.mu.Lock()
	defer repo.mu.Unlock()

	for storedHash := range repo.byHash {
		assert.NotContains(t, storedHash, secret, "the plaintext secret must never be persisted")
	}
	assert.Contains(t, repo.byHash, string(HashKey(secret)))
}

func TestIssue_RequiresAName(t *testing.T) {
	t.Parallel()

	_, _, err := NewAuthenticator(newFakeKeyRepo()).Issue(context.Background(), "   ")

	var ve *domain.ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Equal(t, "name", ve.Field)
}

func TestAuthenticate_RejectsBadSecrets(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	auth := NewAuthenticator(repo)
	ctx := context.Background()

	for name, secret := range map[string]string{
		"empty":                       "",
		"whitespace":                  "   ",
		"wrong shape":                 "not-an-api-key",
		"right prefix, unknown value": KeyPrefix + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := auth.Authenticate(ctx, secret)
			assert.ErrorIs(t, err, domain.ErrUnauthorized)
		})
	}
}

func TestAuthenticate_RejectsARevokedKey(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	auth := NewAuthenticator(repo)
	ctx := context.Background()

	secret, _, err := auth.Issue(ctx, "temporary")
	require.NoError(t, err)
	repo.revoke(secret)

	_, err = auth.Authenticate(ctx, secret)
	assert.ErrorIs(t, err, domain.ErrUnauthorized)
}

func TestAuthenticate_CachesSuccessfulLookups(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	auth := NewAuthenticator(repo)
	ctx := context.Background()

	secret, _, err := auth.Issue(ctx, "ci")
	require.NoError(t, err)

	for i := 0; i < 20; i++ {
		_, err := auth.Authenticate(ctx, secret)
		require.NoError(t, err)
	}

	assert.Equal(t, int64(1), repo.lookups.Load(),
		"repeated requests with the same key must not each cost a database round trip")
}

// TestAuthenticate_CachesFailedLookups is the brute-force defence: without a
// negative cache, an attacker spraying random keys would get one free database
// lookup per guess.
func TestAuthenticate_CachesFailedLookups(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	auth := NewAuthenticator(repo)
	ctx := context.Background()

	const guess = KeyPrefix + "wrongwrongwrongwrongwrongwrongwr"
	for i := 0; i < 20; i++ {
		_, err := auth.Authenticate(ctx, guess)
		require.ErrorIs(t, err, domain.ErrUnauthorized)
	}

	assert.Equal(t, int64(1), repo.lookups.Load())
}

func TestAuthenticate_ReChecksAfterTheCacheExpires(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	auth := NewAuthenticator(repo)
	ctx := context.Background()

	secret, _, err := auth.Issue(ctx, "ci")
	require.NoError(t, err)

	_, err = auth.Authenticate(ctx, secret)
	require.NoError(t, err)
	require.Equal(t, int64(1), repo.lookups.Load())

	// Jump past the cache TTL. This is what bounds how long a revoked key keeps
	// working after it is revoked.
	auth.now = func() time.Time { return time.Now().Add(authCacheTTL + time.Second) }
	repo.revoke(secret)

	_, err = auth.Authenticate(ctx, secret)
	assert.ErrorIs(t, err, domain.ErrUnauthorized)
	assert.Equal(t, int64(2), repo.lookups.Load(), "an expired entry must be re-verified")
}

func TestAuthenticate_IsSafeUnderConcurrency(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	auth := NewAuthenticator(repo)
	ctx := context.Background()

	secret, _, err := auth.Issue(ctx, "ci")
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = auth.Authenticate(ctx, secret)
				return
			}
			_, _ = auth.Authenticate(ctx, KeyPrefix+"unknown-key-value-padding-abcdef")
		}(i)
	}
	wg.Wait()
}

func TestTTLCache_EvictsWhenFull(t *testing.T) {
	t.Parallel()

	const capacity = 4
	cache := newTTLCache(capacity)
	now := time.Now()

	// Fill past the cap with live entries; the cache must stay bounded so key
	// spraying cannot grow it without limit.
	for i := 0; i < capacity*10; i++ {
		cache.set(string(rune('a'+i%26))+string(rune(i)), &domain.APIKey{ID: int64(i)}, now.Add(time.Minute))
	}

	cache.mu.RLock()
	defer cache.mu.RUnlock()
	assert.LessOrEqual(t, len(cache.entries), capacity)
}

func TestTTLCache_TreatsExpiredEntriesAsAbsent(t *testing.T) {
	t.Parallel()

	cache := newTTLCache(16)
	now := time.Now()
	cache.set("k", &domain.APIKey{ID: 1}, now.Add(time.Second))

	_, ok := cache.get("k", now)
	assert.True(t, ok)

	_, ok = cache.get("k", now.Add(2*time.Second))
	assert.False(t, ok)
}
