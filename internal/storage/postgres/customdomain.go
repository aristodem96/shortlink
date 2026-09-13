package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"shortlink/internal/domain"
)

const customDomainColumns = `id, telegram_user_id, hostname, created_at, verified_at`

func scanCustomDomain(row pgx.Row) (*domain.CustomDomain, error) {
	var d domain.CustomDomain
	if err := row.Scan(&d.ID, &d.TelegramUserID, &d.Hostname, &d.CreatedAt, &d.VerifiedAt); err != nil {
		return nil, err
	}
	return &d, nil
}

// AddDomain registers a verified hostname against one of the buyer's paid
// slots, and makes it the active domain if they had none.
//
// SELECT ... FOR UPDATE on the subscription row serialises this per user, the
// same way IssueKey does. Without it two /adddomain messages arriving together
// would both count the user's domains, both see a free slot, and both insert --
// handing out more domains than were paid for.
func (r *BillingRepository) AddDomain(ctx context.Context, telegramUserID int64, hostname string) (*domain.CustomDomain, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: begin add domain: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var slots int
	err = tx.QueryRow(ctx, `
		SELECT domain_slots FROM subscriptions
		WHERE telegram_user_id = $1
		FOR UPDATE`, telegramUserID).Scan(&slots)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNoDomainSlot
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: lock subscription: %w", err)
	}

	var used int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM custom_domains WHERE telegram_user_id = $1`, telegramUserID).Scan(&used); err != nil {
		return nil, fmt.Errorf("postgres: count domains: %w", err)
	}
	if used >= slots {
		return nil, domain.ErrNoDomainSlot
	}

	d, err := scanCustomDomain(tx.QueryRow(ctx, `
		INSERT INTO custom_domains (telegram_user_id, hostname, verified_at)
		VALUES ($1, $2, now())
		RETURNING `+customDomainColumns, telegramUserID, hostname))
	if isUniqueViolation(err) {
		// Registered already, by this user or another. The caller reports both
		// the same way: naming the owner would leak the customer list.
		return nil, domain.ErrDomainTaken
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: insert domain: %w", err)
	}

	// The first domain becomes the one new links are stamped with, mirroring
	// how a user's first API key becomes the key the bot acts with.
	if _, err := tx.Exec(ctx, `
		UPDATE subscriptions SET active_domain_id = $2
		WHERE telegram_user_id = $1 AND active_domain_id IS NULL`, telegramUserID, d.ID); err != nil {
		return nil, fmt.Errorf("postgres: activate first domain: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: commit add domain: %w", err)
	}
	return d, nil
}

// Domains lists a user's registered domains, newest last.
func (r *BillingRepository) Domains(ctx context.Context, telegramUserID int64) ([]domain.CustomDomain, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+customDomainColumns+`
		FROM custom_domains
		WHERE telegram_user_id = $1
		ORDER BY id`, telegramUserID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list domains: %w", err)
	}
	defer rows.Close()

	var out []domain.CustomDomain
	for rows.Next() {
		d, err := scanCustomDomain(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan domain: %w", err)
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// RemoveDomain deletes one of the user's own domains. Links stamped with it
// fall back to the operator's domain rather than breaking, because
// links.domain_id is ON DELETE SET NULL.
//
// The slot is not returned to the pool here -- it stays spent, so removing and
// re-adding is not a way to accumulate domains without paying.
func (r *BillingRepository) RemoveDomain(ctx context.Context, telegramUserID int64, hostname string) error {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM custom_domains WHERE telegram_user_id = $1 AND hostname = $2`,
		telegramUserID, hostname)
	if err != nil {
		return fmt.Errorf("postgres: remove domain: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Includes somebody else's domain: not found, never forbidden, so the
		// answer does not confirm that the hostname is registered at all.
		return domain.ErrNotFound
	}
	return nil
}

// SetActiveDomain chooses which of the user's domains new links are stamped
// with. Ownership is part of the WHERE clause rather than a separate check, so
// there is no window between reading and writing.
func (r *BillingRepository) SetActiveDomain(ctx context.Context, telegramUserID int64, hostname string) (*domain.CustomDomain, error) {
	d, err := scanCustomDomain(r.pool.QueryRow(ctx, `
		UPDATE subscriptions s SET active_domain_id = d.id
		FROM custom_domains d
		WHERE s.telegram_user_id = $1
		  AND d.telegram_user_id = $1
		  AND d.hostname = $2
		  AND d.verified_at IS NOT NULL
		RETURNING `+prefixed("d", customDomainColumns), telegramUserID, hostname))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: set active domain: %w", err)
	}
	return d, nil
}

// ClearActiveDomain puts new links back on the operator's domain without
// touching the user's registered domains.
//
// It is the counterpart to SetActiveDomain, and the only non-destructive way
// back: deleting a domain would also unstamp every link already created on it.
// Clearing is idempotent, so a user who has no subscription row yet, or who is
// already on the default, gets the same answer as one who really changed.
func (r *BillingRepository) ClearActiveDomain(ctx context.Context, telegramUserID int64) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE subscriptions SET active_domain_id = NULL WHERE telegram_user_id = $1`,
		telegramUserID); err != nil {
		return fmt.Errorf("postgres: clear active domain: %w", err)
	}
	return nil
}

// ActiveDomain returns the domain new links are stamped with, or
// domain.ErrNotFound when the user has none.
func (r *BillingRepository) ActiveDomain(ctx context.Context, telegramUserID int64) (*domain.CustomDomain, error) {
	d, err := scanCustomDomain(r.pool.QueryRow(ctx, `
		SELECT `+prefixed("d", customDomainColumns)+`
		FROM subscriptions s
		JOIN custom_domains d ON d.id = s.active_domain_id
		WHERE s.telegram_user_id = $1`, telegramUserID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: active domain: %w", err)
	}
	return d, nil
}

// ServableDomain reports whether a hostname may be served right now: it must be
// registered, verified, and owned by somebody whose access has not lapsed.
//
// This is the query behind Caddy's on-demand TLS gate, so it decides which
// hostnames this server will ask a certificate authority about. It must stay
// strict: every hostname it approves consumes part of a rate-limited quota that
// the operator's own domains also draw on.
func (r *BillingRepository) ServableDomain(ctx context.Context, hostname string) (*domain.CustomDomain, error) {
	d, err := scanCustomDomain(r.pool.QueryRow(ctx, `
		SELECT `+prefixed("d", customDomainColumns)+`
		FROM custom_domains d
		JOIN subscriptions s ON s.telegram_user_id = d.telegram_user_id
		WHERE d.hostname = $1
		  AND d.verified_at IS NOT NULL
		  AND s.paid_until > now()`, hostname))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: servable domain: %w", err)
	}
	return d, nil
}
