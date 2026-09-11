package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aristodem96/shortlink/internal/domain"
)

// LinkRepository stores links in PostgreSQL. It satisfies
// service.LinkRepository.
type LinkRepository struct {
	pool *pgxpool.Pool
}

// NewLinkRepository wires a LinkRepository.
func NewLinkRepository(pool *pgxpool.Pool) *LinkRepository {
	return &LinkRepository{pool: pool}
}

const linkColumns = `id, code, target_url, owner_id, expires_at, created_at`

// Create inserts a link and fills in its generated ID.
//
// ON CONFLICT DO NOTHING combined with RETURNING turns a taken code into "zero
// rows returned", which pgx reports as ErrNoRows. That keeps the insert to a
// single statement: a SELECT-then-INSERT would still race two concurrent
// callers claiming the same alias.
func (r *LinkRepository) Create(ctx context.Context, link *domain.Link) error {
	const query = `
		INSERT INTO links (code, target_url, owner_id, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (code) DO NOTHING
		RETURNING id, created_at`

	err := r.pool.QueryRow(ctx, query,
		link.Code, link.TargetURL, link.OwnerID, link.ExpiresAt, link.CreatedAt,
	).Scan(&link.ID, &link.CreatedAt)

	switch {
	case errors.Is(err, pgx.ErrNoRows), isUniqueViolation(err):
		return domain.ErrAlreadyExists
	case err != nil:
		return fmt.Errorf("postgres: insert link: %w", err)
	}
	return nil
}

// GetByCode loads a link by its short code.
func (r *LinkRepository) GetByCode(ctx context.Context, code string) (*domain.Link, error) {
	const query = `SELECT ` + linkColumns + ` FROM links WHERE code = $1`

	link, err := r.queryOne(ctx, query, code)
	if err != nil {
		return nil, fmt.Errorf("postgres: get link by code: %w", err)
	}
	return link, nil
}

// GetOwnedByCode loads a link scoped to one owner.
func (r *LinkRepository) GetOwnedByCode(ctx context.Context, ownerID int64, code string) (*domain.Link, error) {
	const query = `SELECT ` + linkColumns + ` FROM links WHERE owner_id = $1 AND code = $2`

	link, err := r.queryOne(ctx, query, ownerID, code)
	if err != nil {
		return nil, fmt.Errorf("postgres: get owned link: %w", err)
	}
	return link, nil
}

func (r *LinkRepository) queryOne(ctx context.Context, query string, args ...any) (*domain.Link, error) {
	var l domain.Link
	err := r.pool.QueryRow(ctx, query, args...).
		Scan(&l.ID, &l.Code, &l.TargetURL, &l.OwnerID, &l.ExpiresAt, &l.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Wrapped by the caller, so errors.Is still finds the sentinel.
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// ListByOwner returns a page of the owner's links, newest first, along with the
// total number of links they own.
//
// COUNT(*) OVER() rides along with the page, so pagination costs one query
// instead of a SELECT plus a separate COUNT that could disagree with it.
func (r *LinkRepository) ListByOwner(ctx context.Context, ownerID int64, limit, offset int) ([]domain.Link, int64, error) {
	const query = `
		SELECT ` + linkColumns + `, COUNT(*) OVER() AS total
		FROM links
		WHERE owner_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3`

	rows, err := r.pool.Query(ctx, query, ownerID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("postgres: list links: %w", err)
	}
	defer rows.Close()

	links := make([]domain.Link, 0, limit)
	var total int64
	for rows.Next() {
		var l domain.Link
		if err := rows.Scan(&l.ID, &l.Code, &l.TargetURL, &l.OwnerID, &l.ExpiresAt, &l.CreatedAt, &total); err != nil {
			return nil, 0, fmt.Errorf("postgres: scan link: %w", err)
		}
		links = append(links, l)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("postgres: list links: %w", err)
	}
	return links, total, nil
}

// DeleteOwnedByCode removes one of the owner's links. Clicks are removed with
// it by the ON DELETE CASCADE on clicks.link_id.
func (r *LinkRepository) DeleteOwnedByCode(ctx context.Context, ownerID int64, code string) error {
	const query = `DELETE FROM links WHERE owner_id = $1 AND code = $2`

	tag, err := r.pool.Exec(ctx, query, ownerID, code)
	if err != nil {
		return fmt.Errorf("postgres: delete link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Stats aggregates a link's clicks since a point in time.
//
// The three aggregations travel as one pipelined batch: a single network round
// trip instead of three, which matters more than the query cost itself.
func (r *LinkRepository) Stats(ctx context.Context, linkID int64, since time.Time) (*domain.LinkStats, error) {
	const (
		totalsQuery = `
			SELECT count(*), count(DISTINCT ip_hash), max(clicked_at)
			FROM clicks
			WHERE link_id = $1 AND clicked_at >= $2`

		dailyQuery = `
			SELECT to_char(date_trunc('day', clicked_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD') AS day,
			       count(*)
			FROM clicks
			WHERE link_id = $1 AND clicked_at >= $2
			GROUP BY 1
			ORDER BY 1`

		referersQuery = `
			SELECT coalesce(nullif(referer, ''), '(direct)') AS referer, count(*)
			FROM clicks
			WHERE link_id = $1 AND clicked_at >= $2
			GROUP BY 1
			ORDER BY 2 DESC, 1
			LIMIT 5`
	)

	batch := &pgx.Batch{}
	batch.Queue(totalsQuery, linkID, since)
	batch.Queue(dailyQuery, linkID, since)
	batch.Queue(referersQuery, linkID, since)

	results := r.pool.SendBatch(ctx, batch)
	defer func() { _ = results.Close() }()

	stats := &domain.LinkStats{
		Daily:       []domain.DailyCount{},
		TopReferers: []domain.RefererHit{},
	}

	if err := results.QueryRow().Scan(&stats.TotalClicks, &stats.UniqueVisitors, &stats.LastClickAt); err != nil {
		return nil, fmt.Errorf("postgres: click totals: %w", err)
	}

	daily, err := collect(results, func(rows pgx.Rows) (domain.DailyCount, error) {
		var d domain.DailyCount
		err := rows.Scan(&d.Day, &d.Clicks)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: daily clicks: %w", err)
	}
	stats.Daily = daily

	referers, err := collect(results, func(rows pgx.Rows) (domain.RefererHit, error) {
		var h domain.RefererHit
		err := rows.Scan(&h.Referer, &h.Clicks)
		return h, err
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: top referers: %w", err)
	}
	stats.TopReferers = referers

	// Closing here rather than only in the deferred call surfaces an error
	// from the last query in the batch instead of swallowing it.
	if err := results.Close(); err != nil {
		return nil, fmt.Errorf("postgres: close stats batch: %w", err)
	}
	return stats, nil
}

// collect reads the next result set of a batch into a slice.
func collect[T any](results pgx.BatchResults, scan func(pgx.Rows) (T, error)) ([]T, error) {
	rows, err := results.Query()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]T, 0, 8)
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ClickTotals counts clicks for several links at once.
//
// The join on links.owner_id is what scopes it: the ids come from the caller,
// and filtering by owner in the same statement means ids belonging to another
// tenant simply produce no rows, instead of leaking their counts.
func (r *LinkRepository) ClickTotals(ctx context.Context, ownerID int64, linkIDs []int64) (map[int64]int64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT c.link_id, count(*)
		FROM clicks c
		JOIN links l ON l.id = c.link_id
		WHERE l.owner_id = $1 AND c.link_id = ANY($2)
		GROUP BY c.link_id`, ownerID, linkIDs)
	if err != nil {
		return nil, fmt.Errorf("postgres: click totals: %w", err)
	}
	defer rows.Close()

	totals := make(map[int64]int64, len(linkIDs))
	for rows.Next() {
		var id, n int64
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("postgres: scan click total: %w", err)
		}
		totals[id] = n
	}
	return totals, rows.Err()
}
