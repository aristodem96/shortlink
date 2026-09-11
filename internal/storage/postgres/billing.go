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
const paymentColumns = `id, invoice_id, telegram_user_id, amount::text, currency,
	access_days, status, pay_url, created_at, paid_at`

func scanPayment(row pgx.Row) (*domain.Payment, error) {
	var (
		p      domain.Payment
		status string
	)
	err := row.Scan(&p.ID, &p.InvoiceID, &p.TelegramUserID, &p.Amount, &p.Currency,
		&p.AccessDays, &status, &p.PayURL, &p.CreatedAt, &p.PaidAt)
	if err != nil {
		return nil, err
	}
	p.Status = domain.PaymentStatus(status)
	return &p, nil
}

// RecentPendingPayment returns the user's newest pending payment created after
// the given instant.
func (r *BillingRepository) RecentPendingPayment(ctx context.Context, telegramUserID int64, after time.Time) (*domain.Payment, error) {
	p, err := scanPayment(r.pool.QueryRow(ctx, `
		SELECT `+paymentColumns+`
		FROM payments
		WHERE telegram_user_id = $1 AND status = 'pending' AND created_at > $2
		ORDER BY id DESC
		LIMIT 1`, telegramUserID, after))
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
		INSERT INTO payments (invoice_id, telegram_user_id, amount, currency, access_days, status, pay_url)
		VALUES ($1, $2, $3::text::numeric, $4, $5, $6, $7)
		RETURNING `+paymentColumns,
		p.InvoiceID, p.TelegramUserID, p.Amount, p.Currency, p.AccessDays, string(p.Status), p.PayURL))
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
func (r *BillingRepository) ConfirmPayment(ctx context.Context, invoiceID int64) (*domain.Subscription, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("postgres: begin confirm: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so this is safe to defer.
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		telegramUserID int64
		accessDays     int
	)
	err = tx.QueryRow(ctx, `
		UPDATE payments
		SET status = 'paid', paid_at = now()
		WHERE invoice_id = $1 AND status <> 'paid'
		RETURNING telegram_user_id, access_days`, invoiceID).Scan(&telegramUserID, &accessDays)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already confirmed, or not an invoice we issued. Either way there is
		// nothing to grant.
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("postgres: mark payment paid: %w", err)
	}

	// GREATEST stacks a renewal on top of the time still left, so paying early
	// never forfeits the remainder of the current period.
	sub := domain.Subscription{TelegramUserID: telegramUserID}
	err = tx.QueryRow(ctx, `
		INSERT INTO subscriptions (telegram_user_id, paid_until)
		VALUES ($1, now() + make_interval(days => $2::int))
		ON CONFLICT (telegram_user_id) DO UPDATE
		SET paid_until = GREATEST(subscriptions.paid_until, now()) + make_interval(days => $2::int),
		    updated_at = now()
		RETURNING paid_until`, telegramUserID, accessDays).Scan(&sub.PaidUntil)
	if err != nil {
		return nil, false, fmt.Errorf("postgres: extend subscription: %w", err)
	}

	// Every live key follows the subscription, including ones that had already
	// lapsed: renewing brings the user's existing integrations back to life
	// without making them paste new secrets everywhere.
	if _, err := tx.Exec(ctx, `
		UPDATE api_keys SET expires_at = $2
		WHERE telegram_user_id = $1 AND revoked_at IS NULL`, telegramUserID, sub.PaidUntil); err != nil {
		return nil, false, fmt.Errorf("postgres: extend keys: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("postgres: commit confirm: %w", err)
	}
	return &sub, true, nil
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
		`SELECT paid_until FROM subscriptions WHERE telegram_user_id = $1`, telegramUserID).Scan(&sub.PaidUntil)
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
func (r *BillingRepository) IssueKey(ctx context.Context, telegramUserID int64, name string, hash []byte, maxKeys int) (*domain.APIKey, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: begin issue key: %w", err)
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
		return nil, domain.ErrNoAccess
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: lock subscription: %w", err)
	}

	var live int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM api_keys
		WHERE telegram_user_id = $1 AND revoked_at IS NULL`, telegramUserID).Scan(&live); err != nil {
		return nil, fmt.Errorf("postgres: count keys: %w", err)
	}
	if live >= maxKeys {
		return nil, domain.ErrKeyLimit
	}

	var key domain.APIKey
	err = tx.QueryRow(ctx, `
		INSERT INTO api_keys (name, key_hash, telegram_user_id, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING `+apiKeyColumns, name, hash, telegramUserID, paidUntil).Scan(apiKeyDest(&key)...)
	if err != nil {
		return nil, fmt.Errorf("postgres: insert key: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: commit issue key: %w", err)
	}
	return &key, nil
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
func (r *BillingRepository) RevokeKey(ctx context.Context, telegramUserID, keyID int64) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE api_keys SET revoked_at = now()
		WHERE id = $2 AND telegram_user_id = $1 AND revoked_at IS NULL`, telegramUserID, keyID)
	if err != nil {
		return fmt.Errorf("postgres: revoke key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
