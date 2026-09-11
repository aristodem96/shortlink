package postgres

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/aristodem96/shortlink/internal/domain"
)

// These tests run against a real PostgreSQL in Docker rather than a mock,
// because what they verify is the SQL itself: the ON CONFLICT insert, the
// window-function pagination, the batched stats and the unnest write that has
// to survive a click whose link was deleted underneath it. None of that can
// fail in a fake.
//
// `go test -short` skips them, so the unit suite stays runnable without Docker.

var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	// testing.Short panics if the flags have not been parsed yet, and TestMain
	// runs before the framework does it.
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}

	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("shortlink"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not start postgres: %v\n", err)
		os.Exit(1)
	}

	code := func() int {
		defer func() {
			if err := testcontainers.TerminateContainer(container); err != nil {
				fmt.Fprintf(os.Stderr, "could not terminate postgres: %v\n", err)
			}
		}()

		dsn, err := container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not build dsn: %v\n", err)
			return 1
		}

		// Running the real migrations is part of the test: a broken migration
		// should fail here rather than in production.
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		if err := Migrate(ctx, dsn, log); err != nil {
			fmt.Fprintf(os.Stderr, "could not migrate: %v\n", err)
			return 1
		}

		testPool, err = NewPool(ctx, dsn, 8)
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not open pool: %v\n", err)
			return 1
		}
		defer testPool.Close()

		return m.Run()
	}()

	os.Exit(code)
}

// setup truncates the schema and returns wired repositories plus an owner.
func setup(t *testing.T) (*LinkRepository, *ClickRepository, *APIKeyRepository, *domain.APIKey) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test: -short")
	}
	require.NotNil(t, testPool, "the postgres container failed to start")

	ctx := context.Background()
	// CASCADE covers links and clicks through their foreign keys.
	_, err := testPool.Exec(ctx, `TRUNCATE api_keys RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	keys := NewAPIKeyRepository(testPool)
	owner, err := keys.Create(ctx, "test-owner", []byte("hash-"+t.Name()))
	require.NoError(t, err)

	return NewLinkRepository(testPool), NewClickRepository(testPool), keys, owner
}

func newLink(owner *domain.APIKey, code, target string) *domain.Link {
	return &domain.Link{
		Code:      code,
		TargetURL: target,
		OwnerID:   owner.ID,
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
}

func TestLinkRepository_CreateAndGet(t *testing.T) {
	links, _, _, owner := setup(t)
	ctx := context.Background()

	link := newLink(owner, "abc1234", "https://go.dev/blog/context")
	require.NoError(t, links.Create(ctx, link))
	assert.NotZero(t, link.ID, "Create must fill in the generated identity")

	got, err := links.GetByCode(ctx, "abc1234")
	require.NoError(t, err)
	assert.Equal(t, link.ID, got.ID)
	assert.Equal(t, "https://go.dev/blog/context", got.TargetURL)
	assert.Equal(t, owner.ID, got.OwnerID)
	assert.Nil(t, got.ExpiresAt)
}

func TestLinkRepository_CreateRejectsADuplicateCode(t *testing.T) {
	links, _, _, owner := setup(t)
	ctx := context.Background()

	require.NoError(t, links.Create(ctx, newLink(owner, "dupe123", "https://a.example")))

	// The unique index is the real guarantee; ON CONFLICT DO NOTHING turns it
	// into a clean sentinel instead of a driver error.
	err := links.Create(ctx, newLink(owner, "dupe123", "https://b.example"))
	assert.ErrorIs(t, err, domain.ErrAlreadyExists)
}

func TestLinkRepository_RoundTripsAnExpiry(t *testing.T) {
	links, _, _, owner := setup(t)
	ctx := context.Background()

	expiry := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Microsecond)
	link := newLink(owner, "expiry1", "https://example.com")
	link.ExpiresAt = &expiry
	require.NoError(t, links.Create(ctx, link))

	got, err := links.GetByCode(ctx, "expiry1")
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt)
	assert.True(t, expiry.Equal(*got.ExpiresAt), "want %s, got %s", expiry, *got.ExpiresAt)
}

func TestLinkRepository_GetByCodeReportsAMissingCode(t *testing.T) {
	links, _, _, _ := setup(t)

	_, err := links.GetByCode(context.Background(), "nosuch1")
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestLinkRepository_ScopesReadsAndDeletesToTheOwner(t *testing.T) {
	links, _, keys, owner := setup(t)
	ctx := context.Background()

	stranger, err := keys.Create(ctx, "stranger", []byte("stranger-hash"))
	require.NoError(t, err)

	require.NoError(t, links.Create(ctx, newLink(owner, "owned12", "https://example.com")))

	_, err = links.GetOwnedByCode(ctx, stranger.ID, "owned12")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	err = links.DeleteOwnedByCode(ctx, stranger.ID, "owned12")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// The owner can still see it: the stranger's attempts changed nothing.
	_, err = links.GetOwnedByCode(ctx, owner.ID, "owned12")
	assert.NoError(t, err)
}

func TestLinkRepository_ListByOwnerPaginates(t *testing.T) {
	links, _, _, owner := setup(t)
	ctx := context.Background()

	const total = 25
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < total; i++ {
		link := newLink(owner, fmt.Sprintf("page%03d", i), fmt.Sprintf("https://example.com/%d", i))
		// Distinct timestamps make the newest-first ordering deterministic.
		link.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		require.NoError(t, links.Create(ctx, link))
	}

	firstPage, reported, err := links.ListByOwner(ctx, owner.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, firstPage, 10)
	assert.Equal(t, int64(total), reported, "COUNT(*) OVER() must report the full total, not the page size")
	assert.Equal(t, "page024", firstPage[0].Code, "newest first")

	lastPage, reported, err := links.ListByOwner(ctx, owner.ID, 10, 20)
	require.NoError(t, err)
	assert.Len(t, lastPage, 5)
	assert.Equal(t, int64(total), reported)

	beyondEnd, reported, err := links.ListByOwner(ctx, owner.ID, 10, 100)
	require.NoError(t, err)
	assert.Empty(t, beyondEnd)
	assert.Zero(t, reported, "an empty page carries no window function row to read the total from")
}

func TestLinkRepository_DeleteCascadesToClicks(t *testing.T) {
	links, clicks, _, owner := setup(t)
	ctx := context.Background()

	link := newLink(owner, "cascade", "https://example.com")
	require.NoError(t, links.Create(ctx, link))
	require.NoError(t, clicks.InsertClicks(ctx, []domain.Click{
		{LinkID: link.ID, ClickedAt: time.Now().UTC()},
	}))

	require.NoError(t, links.DeleteOwnedByCode(ctx, owner.ID, "cascade"))

	var remaining int64
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM clicks WHERE link_id = $1`, link.ID).Scan(&remaining))
	assert.Zero(t, remaining, "clicks must go away with their link")
}

// TestClickRepository_SkipsClicksForDeletedLinks is the reason the insert uses
// unnest with an EXISTS filter instead of CopyFrom: the collector buffers for
// seconds, so a link can disappear between the redirect and the flush, and a
// foreign key violation would throw away the whole batch.
func TestClickRepository_SkipsClicksForDeletedLinks(t *testing.T) {
	links, clicks, _, owner := setup(t)
	ctx := context.Background()

	live := newLink(owner, "live123", "https://example.com")
	require.NoError(t, links.Create(ctx, live))

	doomed := newLink(owner, "doomed1", "https://example.com/gone")
	require.NoError(t, links.Create(ctx, doomed))
	require.NoError(t, links.DeleteOwnedByCode(ctx, owner.ID, "doomed1"))

	now := time.Now().UTC()
	batch := []domain.Click{
		{LinkID: live.ID, ClickedAt: now},
		{LinkID: doomed.ID, ClickedAt: now}, // Orphan: its link is already gone.
		{LinkID: live.ID, ClickedAt: now},
	}

	require.NoError(t, clicks.InsertClicks(ctx, batch),
		"one orphan must not fail the whole batch")

	var stored int64
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM clicks`).Scan(&stored))
	assert.Equal(t, int64(2), stored, "the two valid clicks are kept, the orphan is dropped")
}

func TestClickRepository_InsertClicksHandlesAnEmptyBatch(t *testing.T) {
	_, clicks, _, _ := setup(t)

	assert.NoError(t, clicks.InsertClicks(context.Background(), nil))
}

func TestLinkRepository_Stats(t *testing.T) {
	links, clicks, _, owner := setup(t)
	ctx := context.Background()

	link := newLink(owner, "stats01", "https://example.com")
	require.NoError(t, links.Create(ctx, link))

	now := time.Now().UTC()
	batch := []domain.Click{
		{LinkID: link.ID, ClickedAt: now.Add(-1 * time.Hour), Referer: "https://news.example.com", IPHash: []byte("visitor-a")},
		{LinkID: link.ID, ClickedAt: now.Add(-2 * time.Hour), Referer: "https://news.example.com", IPHash: []byte("visitor-a")},
		{LinkID: link.ID, ClickedAt: now.Add(-3 * time.Hour), Referer: "https://blog.example.com", IPHash: []byte("visitor-b")},
		{LinkID: link.ID, ClickedAt: now.Add(-49 * time.Hour), Referer: "", IPHash: []byte("visitor-c")},
		// Outside the reporting window below, so it must not be counted.
		{LinkID: link.ID, ClickedAt: now.Add(-40 * 24 * time.Hour), IPHash: []byte("visitor-d")},
	}
	require.NoError(t, clicks.InsertClicks(ctx, batch))

	stats, err := links.Stats(ctx, link.ID, now.Add(-30*24*time.Hour))
	require.NoError(t, err)

	assert.Equal(t, int64(4), stats.TotalClicks)
	assert.Equal(t, int64(3), stats.UniqueVisitors, "visitor-a clicked twice and counts once")
	require.NotNil(t, stats.LastClickAt)
	assert.WithinDuration(t, now.Add(-time.Hour), *stats.LastClickAt, time.Second)

	require.NotEmpty(t, stats.Daily)
	var daily int64
	for _, d := range stats.Daily {
		assert.Regexp(t, `^\d{4}-\d{2}-\d{2}$`, d.Day)
		daily += d.Clicks
	}
	assert.Equal(t, int64(4), daily, "the daily histogram must add up to the total")

	require.NotEmpty(t, stats.TopReferers)
	assert.Equal(t, "https://news.example.com", stats.TopReferers[0].Referer, "ordered by click count")
	assert.Equal(t, int64(2), stats.TopReferers[0].Clicks)

	var sawDirect bool
	for _, r := range stats.TopReferers {
		if r.Referer == "(direct)" {
			sawDirect = true
		}
	}
	assert.True(t, sawDirect, "an empty referer should be reported as (direct), not as an empty string")
}

func TestLinkRepository_StatsForALinkWithNoClicks(t *testing.T) {
	links, _, _, owner := setup(t)
	ctx := context.Background()

	link := newLink(owner, "silent1", "https://example.com")
	require.NoError(t, links.Create(ctx, link))

	stats, err := links.Stats(ctx, link.ID, time.Now().UTC().Add(-24*time.Hour))
	require.NoError(t, err)

	assert.Zero(t, stats.TotalClicks)
	assert.Zero(t, stats.UniqueVisitors)
	assert.Nil(t, stats.LastClickAt, "max() over no rows is NULL and must decode as nil")
	assert.Empty(t, stats.Daily)
	assert.Empty(t, stats.TopReferers)
}

func TestAPIKeyRepository(t *testing.T) {
	_, _, keys, owner := setup(t)
	ctx := context.Background()

	t.Run("finds a key by hash", func(t *testing.T) {
		created, err := keys.Create(ctx, "findable", []byte("findable-hash"))
		require.NoError(t, err)

		got, err := keys.GetByHash(ctx, []byte("findable-hash"))
		require.NoError(t, err)
		assert.Equal(t, created.ID, got.ID)
		assert.Equal(t, "findable", got.Name)
		assert.NotEqual(t, owner.ID, got.ID)
	})

	t.Run("reports an unknown hash", func(t *testing.T) {
		_, err := keys.GetByHash(ctx, []byte("no-such-hash"))
		assert.ErrorIs(t, err, domain.ErrNotFound)
	})

	t.Run("rejects a duplicate hash", func(t *testing.T) {
		_, err := keys.Create(ctx, "another", []byte("shared-hash"))
		require.NoError(t, err)

		_, err = keys.Create(ctx, "clash", []byte("shared-hash"))
		assert.ErrorIs(t, err, domain.ErrAlreadyExists)
	})

	t.Run("round-trips revocation", func(t *testing.T) {
		key, err := keys.Create(ctx, "revocable", []byte("revocable-hash"))
		require.NoError(t, err)
		assert.True(t, key.IsActive(time.Now()))

		_, err = testPool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE id = $1`, key.ID)
		require.NoError(t, err)

		got, err := keys.GetByHash(ctx, []byte("revocable-hash"))
		require.NoError(t, err)
		assert.False(t, got.IsActive(time.Now()))
	})
}

func TestMigrate_IsIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test: -short")
	}
	require.NotNil(t, testPool)

	// Migrations already ran in TestMain; running them again must be a no-op,
	// because the server applies them on every start.
	assert.NoError(t, Migrate(context.Background(), testPool.Config().ConnString(),
		slog.New(slog.NewTextHandler(io.Discard, nil))))
}
