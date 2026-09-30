package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aristodem96/shortlink/internal/domain"
)

// ClickRepository writes redirect events. It satisfies analytics.ClickWriter.
type ClickRepository struct {
	pool *pgxpool.Pool
}

// NewClickRepository wires a ClickRepository.
func NewClickRepository(pool *pgxpool.Pool) *ClickRepository {
	return &ClickRepository{pool: pool}
}

// InsertClicks writes a batch of clicks in one statement.
//
// The rows travel as parallel arrays through unnest rather than through
// CopyFrom, for one reason: a click can outlive its link. The collector buffers
// events for a couple of seconds, and if the link is deleted in that window a
// COPY would fail the foreign key and lose the entire batch. The EXISTS filter
// drops exactly those orphan rows and commits the rest.
func (r *ClickRepository) InsertClicks(ctx context.Context, clicks []domain.Click) error {
	if len(clicks) == 0 {
		return nil
	}

	const query = `
		INSERT INTO clicks (link_id, clicked_at, referer, user_agent, ip_hash)
		SELECT t.link_id, t.clicked_at, t.referer, t.user_agent, t.ip_hash
		FROM unnest($1::bigint[], $2::timestamptz[], $3::text[], $4::text[], $5::bytea[])
		     AS t(link_id, clicked_at, referer, user_agent, ip_hash)
		WHERE EXISTS (SELECT 1 FROM links l WHERE l.id = t.link_id)`

	n := len(clicks)
	linkIDs := make([]int64, n)
	timestamps := make([]time.Time, n)
	referers := make([]string, n)
	userAgents := make([]string, n)
	ipHashes := make([][]byte, n)

	for i, c := range clicks {
		linkIDs[i] = c.LinkID
		timestamps[i] = c.ClickedAt
		referers[i] = c.Referer
		userAgents[i] = c.UserAgent
		ipHashes[i] = c.IPHash
	}

	if _, err := r.pool.Exec(ctx, query, linkIDs, timestamps, referers, userAgents, ipHashes); err != nil {
		return fmt.Errorf("postgres: insert clicks: %w", err)
	}
	return nil
}
