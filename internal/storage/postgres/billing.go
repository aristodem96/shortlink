package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"shortlink/internal/domain"
)

// BillingRepository stores payments, subscriptions and user-issued keys. It
// satisfies billing.Store.
type BillingRepository struct {
	pool *pgxpool.Pool
}

// NewBillingRepository wires a BillingRepository.
func NewBillingRepository(pool *pgxpool.Pool) *BillingRepository {
	return &BillingRepository{pool: pool}
}

// amount is read as text: NUMERIC has no exact Go counterpart, and a float
// would turn 10.00 into 9.999999.
const paymentColumns = `id, invoice_id, telegram_user_id, product, amount::text, currency,
	access_days, status, pay_url, created_at, paid_at`

func scanPayment(row pgx.Row) (*domain.Payment, error) {
	var (
		p       domain.Payment
		product string
		status  string
	)
	err := row.Scan(&p.ID, &p.InvoiceID, &p.TelegramUserID, &product, &p.Amount, &p.Currency,
		&p.AccessDays, &status, &p.PayURL, &p.CreatedAt, &p.PaidAt)
	if err != nil {
		return nil, err
	}
	p.Product = domain.Product(product)
	p.Status = domain.PaymentStatus(status)
	return &p, nil
}

// RecentPendingPayment returns the user's newest pending payment for one
// product, created after the given instant.
//
// Scoping by product is not a refinement, it is a correctness requirement. The
// caller reuses whatever this returns as the invoice to send the buyer, so an
// unscoped lookup would hand somebody who tapped "buy access" and then "buy a
// domain" the first invoice again -- charging them the wrong amount for the
// wrong thing.
func (r *BillingRepository) RecentPendingPayment(ctx context.Context, telegramUserID int64, product domain.Product, after time.Time) (*domain.Payment, error) {
	p, err := scanPayment(r.pool.QueryRow(ctx, `
		SELECT `+paymentColumns+`
		FROM payments
		WHERE telegram_user_id = $1 AND product = $2 AND status = 'pending' AND created_at > $3
		ORDER BY id DESC
		LIMIT 1`, telegramUserID, string(product), after))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: recent pending payment: %w", err)
	}
	return p, nil
}

// CreatePayment records a newly issued invoice.
func (r *BillingRepository) CreatePayment(ctx context.Context, p domain.Payment) (*domain.Payment, error) {
	created, err := scanPayment(r.pool.QueryRow(ctx, `
		INSERT INTO payments (invoice_id, telegram_user_id, product, amount, currency, access_days, status, pay_url)
		VALUES ($1, $2, $3, $4::text::numeric, $5, $6, $7, $8)
		RETURNING `+paymentColumns,
		p.InvoiceID, p.TelegramUserID, string(p.Product), p.Amount, p.Currency,
		p.AccessDays, string(p.Status), p.PayURL))
	if isUniqueViolation(err) {
		return nil, domain.ErrAlreadyExists
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: create payment: %w", err)
	}
	return created, nil
}

// ConfirmPayment marks the invoice paid and extends access, in one transaction.
//
// The guard is the WHERE clause of the first UPDATE. Two deliveries for the
// same invoice -- a retried webhook, or a webhook racing the reconciler -- both
// reach that statement; the second one blocks on the row lock the first holds,
// and once the first commits it sees status = 'paid' and updates nothing. There
// is no window in which both can grant access, and no application-level lock is
// needed to get there.
//
// A payment the reconciler already marked expired is still confirmed here: if
// the provider says an invoice is paid, the buyer paid, and the provider's word
// outranks our own bookkeeping.
func (r *BillingRepository) ConfirmPayment(ctx context.Context, invoiceID int64) (domain.Confirmation, error) {
	var out domain.Confirmation

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("postgres: begin confirm: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so this is safe to defer.
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		telegramUserID int64
		product        string
		accessDays     *int
	)
	err = tx.QueryRow(ctx, `
		UPDATE payments
		SET status = 'paid', paid_at = now()
		WHERE invoice_id = $1 AND status <> 'paid'
		RETURNING telegram_user_id, product, access_days`, invoiceID).
		Scan(&telegramUserID, &product, &accessDays)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already confirmed, or not an invoice we issued. Either way there is
		// nothing to grant.
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("postgres: mark payment paid: %w", err)
	}

	out = domain.Confirmation{
		Confirmed:      true,
		Product:        domain.Product(product),
		TelegramUserID: telegramUserID,
	}

	switch out.Product {
	case domain.ProductAccess:
		if accessDays == nil {
			return domain.Confirmation{}, fmt.Errorf("postgres: access payment %d has no access_days", invoiceID)
		}
		if err := extendAccess(ctx, tx, telegramUserID, *accessDays, &out); err != nil {
			return domain.Confirmation{}, err
		}
	case domain.ProductDomain:
		// A domain slot buys no days, so the subscription row may not exist
		// yet. Inserting with paid_until = now() creates a lapsed row rather
		// than granting free access: the slot is recorded, but the buyer still
		// has to hold live access for the domain to serve anything.
		err = tx.QueryRow(ctx, `
			INSERT INTO subscriptions (telegram_user_id, paid_until, domain_slots)
			VALUES ($1, now(), 1)
			ON CONFLICT (telegram_user_id) DO UPDATE
			SET domain_slots = subscriptions.domain_slots + 1,
			    updated_at = now()
			RETURNING domain_slots`, telegramUserID).Scan(&out.DomainSlots)
		if err != nil {
			return domain.Confirmation{}, fmt.Errorf("postgres: grant domain slot: %w", err)
		}
	default:
		return domain.Confirmation{}, fmt.Errorf("postgres: payment %d has unknown product %q", invoiceID, product)
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.Confirmation{}, fmt.Errorf("postgres: commit confirm: %w", err)
	}
	return out, nil
}

// extendAccess moves the buyer's access window forward and drags every live key
// along with it.
func extendAccess(ctx context.Context, tx pgx.Tx, telegramUserID int64, accessDays int, out *domain.Confirmation) error {
	// GREATEST stacks a renewal on top of the time still left, so paying early
	// never forfeits the remainder of the current period.
	err := tx.QueryRow(ctx, `
		INSERT INTO subscriptions (telegram_user_id, paid_until)
		VALUES ($1, now() + make_interval(days => $2::int))
		ON CONFLICT (telegram_user_id) DO UPDATE
		SET paid_until = GREATEST(subscriptions.paid_until, now()) + make_interval(days => $2::int),
		    updated_at = now()
		RETURNING paid_until`, telegramUserID, accessDays).Scan(&out.PaidUntil)
	if err != nil {
		return fmt.Errorf("postgres: extend subscription: %w", err)
	}

	// Every live key follows the subscription, including ones that had already
	// lapsed: renewing brings the user's existing integrations back to life
	// without making them paste new secrets everywhere.
	if _, err := tx.Exec(ctx, `
		UPDATE api_keys SET expires_at = $2
		WHERE telegram_user_id = $1 AND revoked_at IS NULL`, telegramUserID, out.PaidUntil); err != nil {
		return fmt.Errorf("postgres: extend keys: %w", err)
	}
	return nil
}

// ExpirePayment marks a pending invoice as expired.
func (r *BillingRepository) ExpirePayment(ctx context.Context, invoiceID int64) error {
	if _, err := r.pool.Exec(ctx, `
		UPDATE payments SET status = 'expired'
		WHERE invoice_id = $1 AND status = 'pending'`, invoiceID); err != nil {
		return fmt.Errorf("postgres: expire payment: %w", err)
	}
	return nil
}

// PendingPayments returns pending payments created after the given instant,
// oldest first.
func (r *BillingRepository) PendingPayments(ctx context.Context, createdAfter time.Time, limit int) ([]domain.Payment, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+paymentColumns+`
		FROM payments
		WHERE status = 'pending' AND created_at > $1
		ORDER BY created_at
		LIMIT $2`, createdAfter, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: pending payments: %w", err)
	}
	defer rows.Close()

	var out []domain.Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan payment: %w", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// Subscription returns a user's access window.
func (r *BillingRepository) Subscription(ctx context.Context, telegramUserID int64) (*domain.Subscription, error) {
	sub := domain.Subscription{TelegramUserID: telegramUserID}
	err := r.pool.QueryRow(ctx,
		`SELECT paid_until, domain_slots, active_domain_id FROM subscriptions WHERE telegram_user_id = $1`,
		telegramUserID).Scan(&sub.PaidUntil, &sub.DomainSlots, &sub.ActiveDomainID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: subscription: %w", err)
	}
	return &sub, nil
}

// IssueKey stores a new key for a user with active access.
//
// SELECT ... FOR UPDATE on the subscription row serialises this per user.
// Without it, two /newkey messages arriving together would both count the
// user's keys, both see room for one more, and both insert -- walking straight
// past the limit. Locking the one row that every key issue and every renewal
// for that user touches makes the check and the insert behave as one step.
func (r *BillingRepository) IssueKey(ctx context.Context, telegramUserID int64, name string, hash []byte, maxKeys int) (*domain.APIKey, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("postgres: begin issue key: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		paidUntil time.Time
		active    bool
	)
	err = tx.QueryRow(ctx, `
		SELECT paid_until, paid_until > now()
		FROM subscriptions
		WHERE telegram_user_id = $1
		FOR UPDATE`, telegramUserID).Scan(&paidUntil, &active)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
		return nil, false, domain.ErrNoAccess
	}
	if err != nil {
		return nil, false, fmt.Errorf("postgres: lock subscription: %w", err)
	}

	var live int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM api_keys
		WHERE telegram_user_id = $1 AND revoked_at IS NULL`, telegramUserID).Scan(&live); err != nil {
		return nil, false, fmt.Errorf("postgres: count keys: %w", err)
	}
	if live >= maxKeys {
		return nil, false, domain.ErrKeyLimit
	}

	var key domain.APIKey
	err = tx.QueryRow(ctx, `
		INSERT INTO api_keys (name, key_hash, telegram_user_id, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING `+apiKeyColumns, name, hash, telegramUserID, paidUntil).Scan(apiKeyDest(&key)...)
	if err != nil {
		return nil, false, fmt.Errorf("postgres: insert key: %w", err)
	}

	// The first key a user creates becomes the one the bot shortens links with,
	// so /buy then /newkey is all it takes before sending a URL. A later key --
	// perhaps made for a CI job -- leaves the choice alone; /usekey changes it.
	tag, err := tx.Exec(ctx, `
		UPDATE subscriptions SET active_key_id = $2
		WHERE telegram_user_id = $1 AND active_key_id IS NULL`, telegramUserID, key.ID)
	if err != nil {
		return nil, false, fmt.Errorf("postgres: activate key: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("postgres: commit issue key: %w", err)
	}
	return &key, tag.RowsAffected() == 1, nil
}

// ListKeys returns the user's keys that have not been revoked.
func (r *BillingRepository) ListKeys(ctx context.Context, telegramUserID int64) ([]domain.APIKey, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+apiKeyColumns+`
		FROM api_keys
		WHERE telegram_user_id = $1 AND revoked_at IS NULL
		ORDER BY id`, telegramUserID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list keys: %w", err)
	}
	defer rows.Close()

	var out []domain.APIKey
	for rows.Next() {
		var k domain.APIKey
		if err := rows.Scan(apiKeyDest(&k)...); err != nil {
			return nil, fmt.Errorf("postgres: scan key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey disables one of the user's keys. A key that belongs to somebody
// else reports as not found, the same way links do, so key ids cannot be probed.
//
// If the revoked key was the one the bot acts with, the bot moves on to the
// user's newest remaining key -- or to none -- in the same transaction, so it can
// never be left pointing at a key that no longer works.
func (r *BillingRepository) RevokeKey(ctx context.Context, telegramUserID, keyID int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin revoke key: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE api_keys SET revoked_at = now()
		WHERE id = $2 AND telegram_user_id = $1 AND revoked_at IS NULL`, telegramUserID, keyID)
	if err != nil {
		return fmt.Errorf("postgres: revoke key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}

	if _, err := tx.Exec(ctx, `
		UPDATE subscriptions
		SET active_key_id = (
			SELECT id FROM api_keys
			WHERE telegram_user_id = $1 AND revoked_at IS NULL
			ORDER BY id DESC
			LIMIT 1
		)
		WHERE telegram_user_id = $1 AND active_key_id = $2`, telegramUserID, keyID); err != nil {
		return fmt.Errorf("postgres: reassign active key: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit revoke key: %w", err)
	}
	return nil
}

// ActiveKey returns the key the bot acts with for this user, whatever its
// state; deciding whether it is still usable is the service's job. It returns
// domain.ErrNotFound when no key is chosen.
func (r *BillingRepository) ActiveKey(ctx context.Context, telegramUserID int64) (*domain.APIKey, error) {
	var key domain.APIKey
	err := r.pool.QueryRow(ctx, `
		SELECT `+prefixed("k", apiKeyColumns)+`
		FROM subscriptions s
		JOIN api_keys k ON k.id = s.active_key_id
		WHERE s.telegram_user_id = $1`, telegramUserID).Scan(apiKeyDest(&key)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: active key: %w", err)
	}
	return &key, nil
}

// SetActiveKey points the bot at one of the user's live keys, by id.
func (r *BillingRepository) SetActiveKey(ctx context.Context, telegramUserID, keyID int64) (*domain.APIKey, error) {
	return r.setActive(ctx, telegramUserID, `k.id = $2`, keyID)
}

// SetActiveKeyByHash points the bot at one of the user's live keys, identified
// by the hash of a secret the user pasted. The secret itself never reaches the
// database, so choosing a key this way stores nothing sensitive.
func (r *BillingRepository) SetActiveKeyByHash(ctx context.Context, telegramUserID int64, hash []byte) (*domain.APIKey, error) {
	return r.setActive(ctx, telegramUserID, `k.key_hash = $2`, hash)
}

// setActive is shared by both lookups. Ownership is part of the WHERE clause,
// not a separate check, so a user cannot activate somebody else's key by
// guessing its id -- or an operator key, whose telegram_user_id is NULL.
func (r *BillingRepository) setActive(ctx context.Context, telegramUserID int64, match string, arg any) (*domain.APIKey, error) {
	var key domain.APIKey
	err := r.pool.QueryRow(ctx, `
		UPDATE subscriptions s
		SET active_key_id = k.id
		FROM api_keys k
		WHERE s.telegram_user_id = $1
		  AND k.telegram_user_id = $1
		  AND k.revoked_at IS NULL
		  AND `+match+`
		RETURNING `+prefixed("k", apiKeyColumns), telegramUserID, arg).Scan(apiKeyDest(&key)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: set active key: %w", err)
	}
	return &key, nil
}

// prefixed qualifies each column in a comma-separated list with a table alias,
// for queries that join api_keys to another table with overlapping names.
func prefixed(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, c := range parts {
		parts[i] = alias + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
