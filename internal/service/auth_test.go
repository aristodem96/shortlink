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

func (r *fakeKeyRepo) Create(_ context.Context, name string, hash []byte, expiresAt *time.Time) (*domain.APIKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextID++
	key := &domain.APIKey{ID: r.nextID, Name: name, CreatedAt: time.Now(), ExpiresAt: expiresAt}
	r.byHash[string(hash)] = key

	copied := *key
	return &copied, nil
}

func (r *fakeKeyRepo) setExpiry(secret string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if key, ok := r.byHash[string(HashKey(secret))]; ok {
		key.ExpiresAt = &at
	}
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

	secret, key, err := auth.Issue(ctx, "ci", nil)
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
	secret, _, err := NewAuthenticator(repo).Issue(context.Background(), "ci", nil)
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

	_, _, err := NewAuthenticator(newFakeKeyRepo()).Issue(context.Background(), "   ", nil)

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

	secret, _, err := auth.Issue(ctx, "temporary", nil)
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

	secret, _, err := auth.Issue(ctx, "ci", nil)
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

	secret, _, err := auth.Issue(ctx, "ci", nil)
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

	secret, _, err := auth.Issue(ctx, "ci", nil)
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

func TestAuthenticate_RejectsAnExpiredKey(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	auth := NewAuthenticator(repo)
	ctx := context.Background()

	secret, _, err := auth.Issue(ctx, "bought", nil)
	require.NoError(t, err)
	repo.setExpiry(secret, time.Now().Add(-time.Minute))

	_, err = auth.Authenticate(ctx, secret)
	assert.ErrorIs(t, err, domain.ErrUnauthorized)
}

// TestAuthenticate_ExpiryIsExactDespiteTheCache checks that a cached key stops
// working at its expiry, not up to one cache TTL later.
func TestAuthenticate_ExpiryIsExactDespiteTheCache(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	auth := NewAuthenticator(repo)
	ctx := context.Background()

	secret, _, err := auth.Issue(ctx, "bought", nil)
	require.NoError(t, err)

	now := time.Now()
	repo.setExpiry(secret, now.Add(10*time.Second))

	_, err = auth.Authenticate(ctx, secret)
	require.NoError(t, err, "valid now, and now cached")

	// Well inside the cache TTL, but past the key's expiry.
	auth.now = func() time.Time { return now.Add(11 * time.Second) }

	_, err = auth.Authenticate(ctx, secret)
	assert.ErrorIs(t, err, domain.ErrUnauthorized)
}

// TestAuthenticate_RenewalTakesEffectImmediately covers the other direction.
// A cached copy that looks expired is re-read rather than refused: the owner
// may have paid again, which moves expires_at in the database but not in the
// cached copy.
func TestAuthenticate_RenewalTakesEffectImmediately(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	auth := NewAuthenticator(repo)
	ctx := context.Background()

	secret, _, err := auth.Issue(ctx, "bought", nil)
	require.NoError(t, err)

	now := time.Now()
	repo.setExpiry(secret, now.Add(10*time.Second))
	_, err = auth.Authenticate(ctx, secret)
	require.NoError(t, err)

	auth.now = func() time.Time { return now.Add(11 * time.Second) }
	repo.setExpiry(secret, now.Add(5*24*time.Hour)) // the user renewed

	_, err = auth.Authenticate(ctx, secret)
	assert.NoError(t, err, "a renewed key must work at once, not after the cache TTL")
}

func TestAuthenticate_OperatorKeysNeverExpire(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	auth := NewAuthenticator(repo)

	secret, key, err := auth.Issue(context.Background(), "operator", nil)
	require.NoError(t, err)
	assert.Nil(t, key.ExpiresAt, "keys from shortlinkctl carry no expiry")

	auth.now = func() time.Time { return time.Now().Add(10 * 365 * 24 * time.Hour) }
	_, err = auth.Authenticate(context.Background(), secret)
	assert.NoError(t, err)
}

// TestAuthenticate_RenewalAfterARefusalIsNotBlockedForLong covers the moment
// right after paying. The user tried their lapsed key (refused, and the
// refusal cached), then paid, then tried again. A 30-second negative cache
// would greet a customer who has just been told "payment received" with 401.
func TestAuthenticate_RenewalAfterARefusalIsNotBlockedForLong(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	auth := NewAuthenticator(repo)
	ctx := context.Background()

	secret, _, err := auth.Issue(ctx, "bought", nil)
	require.NoError(t, err)

	now := time.Now()
	repo.setExpiry(secret, now.Add(-time.Minute)) // lapsed

	_, err = auth.Authenticate(ctx, secret)
	require.ErrorIs(t, err, domain.ErrUnauthorized, "refused, and the refusal is now cached")

	repo.setExpiry(secret, now.Add(5*24*time.Hour)) // the user paid

	auth.now = func() time.Time { return now.Add(expiredRecheckTTL + time.Second) }
	_, err = auth.Authenticate(ctx, secret)
	assert.NoError(t, err, "within seconds of renewing, not after the full cache TTL")
}

// TestAuthenticate_RevokedKeysStayRefusedForTheFullTTL: the short recheck is
// only for keys that can come back. A revoked key cannot, so there is no
// reason to let it hit the database every few seconds.
func TestAuthenticate_RevokedKeysStayRefusedForTheFullTTL(t *testing.T) {
	t.Parallel()

	repo := newFakeKeyRepo()
	auth := NewAuthenticator(repo)
	ctx := context.Background()

	secret, _, err := auth.Issue(ctx, "gone", nil)
	require.NoError(t, err)
	repo.revoke(secret)

	_, err = auth.Authenticate(ctx, secret)
	require.ErrorIs(t, err, domain.ErrUnauthorized)
	lookups := repo.lookups.Load()

	auth.now = func() time.Time { return time.Now().Add(expiredRecheckTTL + time.Second) }
	_, err = auth.Authenticate(ctx, secret)
	require.ErrorIs(t, err, domain.ErrUnauthorized)
	assert.Equal(t, lookups, repo.lookups.Load(), "a revoked key is served from the negative cache, not re-read")
}

// TestIssue_ExpiryIsOptionalAndHonoured: the CLI and the bot are the two ways a
// key is born, and the only difference between them is this argument. Both end
// up as the same row, judged by the same IsActive.
func TestIssue_ExpiryIsOptionalAndHonoured(t *testing.T) {
	t.Parallel()

	auth := NewAuthenticator(newFakeKeyRepo())
	ctx := context.Background()

	_, operator, err := auth.Issue(ctx, "operator", nil)
	require.NoError(t, err)
	assert.Nil(t, operator.ExpiresAt, "no expiry is a real state, not a date far in the future")
	assert.True(t, operator.IsActive(time.Now().Add(10*365*24*time.Hour)))

	until := time.Now().Add(5 * 24 * time.Hour)
	secret, limited, err := auth.Issue(ctx, "temporary", &until)
	require.NoError(t, err)
	require.NotNil(t, limited.ExpiresAt)
	assert.True(t, limited.IsActive(time.Now()))
	assert.False(t, limited.IsActive(until.Add(time.Second)))

	// And it is refused at the door once it lapses, exactly as a bot key is.
	auth.now = func() time.Time { return until.Add(time.Second) }
	_, err = auth.Authenticate(ctx, secret)
	assert.ErrorIs(t, err, domain.ErrUnauthorized)
}
