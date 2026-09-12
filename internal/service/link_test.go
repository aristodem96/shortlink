package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"shortlink/internal/domain"
)

// fixedNow is the instant every test runs at, so expiry is deterministic.
var fixedNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

type harness struct {
	svc      *LinkService
	repo     *fakeLinkRepo
	cache    *fakeCache
	recorder *fakeRecorder
	codes    *scriptedGenerator
}

func newHarness(t *testing.T, codes ...string) *harness {
	t.Helper()

	if len(codes) == 0 {
		codes = []string{"code001", "code002", "code003", "code004"}
	}

	h := &harness{
		repo:     newFakeLinkRepo(),
		cache:    newFakeCache(),
		recorder: &fakeRecorder{},
		codes:    &scriptedGenerator{codes: codes},
	}

	h.svc = NewLinkService(
		h.repo, h.cache, h.recorder, h.codes, fakeHasher{},
		LinkServiceConfig{CacheTTL: time.Hour, NegativeCacheTTL: 30 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithClock(func() time.Time { return fixedNow }),
	)
	return h
}

func TestCreate_GeneratesACode(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	link, err := h.svc.Create(context.Background(), CreateLinkInput{
		OwnerID: 1, TargetURL: "https://go.dev/blog/context",
	})
	require.NoError(t, err)

	assert.Equal(t, "code001", link.Code)
	assert.Equal(t, "https://go.dev/blog/context", link.TargetURL)
	assert.Equal(t, int64(1), link.OwnerID)
	assert.Equal(t, fixedNow, link.CreatedAt)
	assert.NotZero(t, link.ID, "the repository must fill in the generated ID")
}

func TestCreate_UsesTheAliasWhenGiven(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	link, err := h.svc.Create(context.Background(), CreateLinkInput{
		OwnerID: 1, TargetURL: "https://example.com", Alias: "my-link",
	})
	require.NoError(t, err)

	assert.Equal(t, "my-link", link.Code)
	assert.Len(t, h.codes.codes, 4, "the generator must not be consumed when an alias is supplied")
}

func TestCreate_RejectsInvalidInput(t *testing.T) {
	t.Parallel()

	past := fixedNow.Add(-time.Hour)

	tests := map[string]struct {
		in    CreateLinkInput
		field string
	}{
		"bad url":        {CreateLinkInput{TargetURL: "not a url"}, "target_url"},
		"private target": {CreateLinkInput{TargetURL: "http://10.0.0.1/"}, "target_url"},
		"bad alias":      {CreateLinkInput{TargetURL: "https://example.com", Alias: "a"}, "alias"},
		"reserved alias": {CreateLinkInput{TargetURL: "https://example.com", Alias: "api"}, "alias"},
		"past expiry":    {CreateLinkInput{TargetURL: "https://example.com", ExpiresAt: &past}, "expires_at"},
		"expiry equals now": {
			CreateLinkInput{TargetURL: "https://example.com", ExpiresAt: &fixedNow}, "expires_at",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)

			_, err := h.svc.Create(context.Background(), tc.in)

			var ve *domain.ValidationError
			require.ErrorAs(t, err, &ve)
			assert.Equal(t, tc.field, ve.Field)
			assert.Zero(t, h.repo.createCalls.Load(), "invalid input must not reach the repository")
		})
	}
}

func TestCreate_ReportsATakenAlias(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	_, err := h.svc.Create(ctx, CreateLinkInput{OwnerID: 1, TargetURL: "https://example.com", Alias: "taken"})
	require.NoError(t, err)

	_, err = h.svc.Create(ctx, CreateLinkInput{OwnerID: 2, TargetURL: "https://other.com", Alias: "taken"})
	assert.ErrorIs(t, err, domain.ErrAlreadyExists)
}

func TestCreate_RetriesOnAGeneratedCodeCollision(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "dupe", "fresh")
	ctx := context.Background()

	// Occupy the first code the generator will hand out.
	_, err := h.svc.Create(ctx, CreateLinkInput{OwnerID: 1, TargetURL: "https://example.com", Alias: "dupe"})
	require.NoError(t, err)

	link, err := h.svc.Create(ctx, CreateLinkInput{OwnerID: 1, TargetURL: "https://example.com/second"})
	require.NoError(t, err)
	assert.Equal(t, "fresh", link.Code, "the service should have retried past the collision")
}

func TestCreate_GivesUpAfterRepeatedCollisions(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "dupe", "dupe", "dupe")
	ctx := context.Background()

	_, err := h.svc.Create(ctx, CreateLinkInput{OwnerID: 1, TargetURL: "https://example.com", Alias: "dupe"})
	require.NoError(t, err)

	_, err = h.svc.Create(ctx, CreateLinkInput{OwnerID: 1, TargetURL: "https://example.com/second"})
	require.Error(t, err)
	assert.ErrorIs(t, err, domain.ErrAlreadyExists)
	assert.Contains(t, err.Error(), "no free short code")
}

func TestCreate_ClearsAStaleNegativeCacheEntry(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	// Someone probes the code before it exists, which caches the miss.
	_, err := h.svc.Resolve(ctx, "my-link")
	require.ErrorIs(t, err, domain.ErrNotFound)

	present, negative := h.cache.has("my-link")
	require.True(t, present && negative, "the miss should have been cached")

	_, err = h.svc.Create(ctx, CreateLinkInput{OwnerID: 1, TargetURL: "https://example.com", Alias: "my-link"})
	require.NoError(t, err)

	// Without the invalidation the new link would 404 until the negative entry
	// expired.
	link, err := h.svc.Resolve(ctx, "my-link")
	require.NoError(t, err)
	assert.Equal(t, "https://example.com", link.TargetURL)
}

func TestResolve_ServesFromCacheWithoutTouchingTheRepository(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	created, err := h.svc.Create(ctx, CreateLinkInput{OwnerID: 1, TargetURL: "https://example.com"})
	require.NoError(t, err)

	// First resolve is a miss and populates the cache.
	_, err = h.svc.Resolve(ctx, created.Code)
	require.NoError(t, err)
	require.Equal(t, int64(1), h.repo.getCalls.Load())

	for i := 0; i < 5; i++ {
		link, err := h.svc.Resolve(ctx, created.Code)
		require.NoError(t, err)
		assert.Equal(t, "https://example.com", link.TargetURL)
	}
	assert.Equal(t, int64(1), h.repo.getCalls.Load(), "cached resolves must not reach the repository")
}

func TestResolve_NegativelyCachesAMissingCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, err := h.svc.Resolve(ctx, "nosuch1")
		require.ErrorIs(t, err, domain.ErrNotFound)
	}

	// This is the cache-penetration defence: without it, a flood of random
	// codes would be a flood of database reads.
	assert.Equal(t, int64(1), h.repo.getCalls.Load(),
		"a missing code must be looked up once and then served from the negative cache")
}

func TestResolve_RejectsJunkPathsWithoutTouchingAnyStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	for _, path := range []string{"", "ab", "wp-login.php", "favicon.ico", strings.Repeat("x", 64), "../../etc/passwd"} {
		_, err := h.svc.Resolve(ctx, path)
		assert.ErrorIs(t, err, domain.ErrNotFound, "path %q", path)
	}

	assert.Zero(t, h.repo.getCalls.Load())
	assert.Zero(t, h.cache.getCalls.Load())
}

func TestResolve_ReportsExpiredLinksAsGone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	expiry := fixedNow.Add(time.Hour)
	created, err := h.svc.Create(ctx, CreateLinkInput{
		OwnerID: 1, TargetURL: "https://example.com", ExpiresAt: &expiry,
	})
	require.NoError(t, err)

	// Still valid at the current clock.
	_, err = h.svc.Resolve(ctx, created.Code)
	require.NoError(t, err)

	// Move past the expiry. 410 rather than 404 tells the caller the link was
	// real, which is the difference between "wrong link" and "too late".
	h.svc.now = func() time.Time { return expiry.Add(time.Second) }

	_, err = h.svc.Resolve(ctx, created.Code)
	assert.ErrorIs(t, err, domain.ErrGone)
}

func TestResolve_FallsBackToTheRepositoryWhenTheCacheIsBroken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	created, err := h.svc.Create(ctx, CreateLinkInput{OwnerID: 1, TargetURL: "https://example.com"})
	require.NoError(t, err)

	h.cache.mu.Lock()
	h.cache.failWith = errors.New("redis is down")
	h.cache.mu.Unlock()

	// A cache outage must degrade latency, not availability.
	link, err := h.svc.Resolve(ctx, created.Code)
	require.NoError(t, err)
	assert.Equal(t, "https://example.com", link.TargetURL)
}

// TestResolve_CoalescesConcurrentMisses is the stampede test: when a popular
// link falls out of cache, every in-flight request for it must share one
// database read rather than each issuing their own.
func TestResolve_CoalescesConcurrentMisses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	created, err := h.svc.Create(ctx, CreateLinkInput{OwnerID: 1, TargetURL: "https://example.com"})
	require.NoError(t, err)

	// Hold the repository open until every goroutine has piled up behind the
	// same singleflight key.
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	h.repo.beforeGet = func() {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}

	const concurrent = 50
	var wg sync.WaitGroup
	results := make([]*domain.Link, concurrent)
	errs := make([]error, concurrent)

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = h.svc.Resolve(ctx, created.Code)
		}(i)
	}

	<-entered
	// Give the remaining goroutines room to reach the singleflight group.
	time.Sleep(250 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int64(1), h.repo.getCalls.Load(),
		"%d concurrent misses should collapse into a single database read", concurrent)

	for i := 0; i < concurrent; i++ {
		require.NoError(t, errs[i])
		assert.Equal(t, "https://example.com", results[i].TargetURL)
	}
}

// TestResolve_SurvivesCallerCancellation covers the reason the shared fetch
// runs on a detached context: the goroutine that wins the singleflight race
// may be the first to disconnect, and that must not fail everyone waiting.
func TestResolve_SurvivesCallerCancellation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	created, err := h.svc.Create(context.Background(), CreateLinkInput{
		OwnerID: 1, TargetURL: "https://example.com",
	})
	require.NoError(t, err)

	cancelCtx, cancel := context.WithCancel(context.Background())

	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	h.repo.beforeGet = func() {
		once.Do(func() { close(entered) })
		<-release
	}

	var (
		wg      sync.WaitGroup
		gotLink *domain.Link
		gotErr  error
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = h.svc.Resolve(cancelCtx, created.Code) // The caller that gives up.
	}()

	<-entered
	time.Sleep(100 * time.Millisecond)

	wg.Add(1)
	go func() {
		defer wg.Done()
		gotLink, gotErr = h.svc.Resolve(context.Background(), created.Code)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel() // The first caller disconnects while the fetch is still running.
	close(release)
	wg.Wait()

	require.NoError(t, gotErr, "a cancelled caller must not poison the shared fetch")
	require.NotNil(t, gotLink)
	assert.Equal(t, "https://example.com", gotLink.TargetURL)
}

func TestRecordClick_PseudonymisesAndTruncates(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	link := &domain.Link{ID: 42}
	h.svc.RecordClick(link, ClickMeta{
		Referer:   strings.Repeat("r", maxRefererLength+100),
		UserAgent: strings.Repeat("u", maxUserAgentLength+100),
		IP:        "203.0.113.7",
	})

	recorded := h.recorder.recorded()
	require.Len(t, recorded, 1)

	click := recorded[0]
	assert.Equal(t, int64(42), click.LinkID)
	assert.Equal(t, fixedNow, click.ClickedAt)
	assert.Len(t, click.Referer, maxRefererLength, "an oversized referer must be truncated, not stored whole")
	assert.Len(t, click.UserAgent, maxUserAgentLength)
	assert.NotEmpty(t, click.IPHash)
	assert.NotContains(t, string(click.IPHash), "203.0.113.7", "the raw address must never be stored")
}

func TestDelete_NegativelyCachesTheRemovedCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	created, err := h.svc.Create(ctx, CreateLinkInput{OwnerID: 1, TargetURL: "https://example.com"})
	require.NoError(t, err)

	_, err = h.svc.Resolve(ctx, created.Code)
	require.NoError(t, err)

	require.NoError(t, h.svc.Delete(ctx, 1, created.Code))

	present, negative := h.cache.has(created.Code)
	assert.True(t, present && negative,
		"a deleted code should be negatively cached so its traffic does not hit the database")

	_, err = h.svc.Resolve(ctx, created.Code)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestDelete_RefusesSomeoneElsesLink(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	created, err := h.svc.Create(ctx, CreateLinkInput{OwnerID: 1, TargetURL: "https://example.com"})
	require.NoError(t, err)

	// Not found rather than forbidden: a 403 would confirm the code exists.
	err = h.svc.Delete(ctx, 999, created.Code)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestGet_ScopesToTheOwner(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	created, err := h.svc.Create(ctx, CreateLinkInput{OwnerID: 1, TargetURL: "https://example.com"})
	require.NoError(t, err)

	got, err := h.svc.Get(ctx, 1, created.Code)
	require.NoError(t, err)
	assert.Equal(t, created.Code, got.Code)

	_, err = h.svc.Get(ctx, 2, created.Code)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestList_ClampsPagination(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	_, _, err := h.svc.List(ctx, 1, 0, -5)
	require.NoError(t, err)

	// The clamping itself is asserted through the repository's view of the
	// arguments, so build a repo that records them.
	recorder := &argRecordingRepo{fakeLinkRepo: newFakeLinkRepo()}
	h.svc.repo = recorder

	for _, tc := range []struct{ in, want int }{
		{in: 0, want: defaultPageSize},
		{in: -1, want: defaultPageSize},
		{in: 50, want: 50},
		{in: 1000, want: maxPageSize},
	} {
		_, _, err := h.svc.List(ctx, 1, tc.in, 0)
		require.NoError(t, err)
		assert.Equal(t, tc.want, recorder.lastLimit, "limit %d", tc.in)
	}
}

type argRecordingRepo struct {
	*fakeLinkRepo
	lastLimit  int
	lastOffset int
}

func (r *argRecordingRepo) ListByOwner(ctx context.Context, ownerID int64, limit, offset int) ([]domain.Link, int64, error) {
	r.lastLimit, r.lastOffset = limit, offset
	return r.fakeLinkRepo.ListByOwner(ctx, ownerID, limit, offset)
}
