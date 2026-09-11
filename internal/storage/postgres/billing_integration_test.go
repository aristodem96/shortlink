package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aristodem96/shortlink/internal/domain"
)

// These run against the real PostgreSQL from TestMain. The properties they
// check -- exactly-once confirmation under concurrency, a key limit that holds
// when requests race -- are properties of the SQL and its locking, so a fake
// could only ever agree with whatever it was told.

func setupBilling(t *testing.T) *BillingRepository {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test: -short")
	}
	require.NotNil(t, testPool, "the postgres container failed to start")

	_, err := testPool.Exec(context.Background(),
		`TRUNCATE payments, subscriptions, api_keys RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	return NewBillingRepository(testPool)
}

var nextInvoice atomic.Int64

func pendingPayment(t *testing.T, repo *BillingRepository, user int64, days int) int64 {
	t.Helper()
	invoice := 100_000 + nextInvoice.Add(1)
	_, err := repo.CreatePayment(context.Background(), domain.Payment{
		InvoiceID: invoice, TelegramUserID: user, Amount: "10", Currency: "USD",
		AccessDays: days, Status: domain.PaymentPending, PayURL: fmt.Sprintf("https://t.me/pay/%d", invoice),
	})
	require.NoError(t, err)
	return invoice
}

func TestBilling_PaymentRoundTrip(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	invoice := pendingPayment(t, repo, 42, 5)

	got, err := repo.RecentPendingPayment(ctx, 42, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, invoice, got.InvoiceID)
	assert.Equal(t, "10.00", got.Amount, "NUMERIC(12,2) must come back exact, not as a float")
	assert.Equal(t, domain.PaymentPending, got.Status)

	_, err = repo.RecentPendingPayment(ctx, 42, time.Now().Add(time.Hour))
	assert.ErrorIs(t, err, domain.ErrNotFound, "a payment older than the cut-off is not reused")
}

func TestBilling_ConfirmGrantsAccessOnce(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()
	invoice := pendingPayment(t, repo, 42, 5)

	sub, confirmed, err := repo.ConfirmPayment(ctx, invoice)
	require.NoError(t, err)
	require.True(t, confirmed)
	assert.WithinDuration(t, time.Now().Add(5*24*time.Hour), sub.PaidUntil, time.Minute)

	// A retried webhook for the same invoice.
	_, confirmed, err = repo.ConfirmPayment(ctx, invoice)
	require.NoError(t, err)
	assert.False(t, confirmed, "a second confirmation must not grant anything")

	stored, err := repo.Subscription(ctx, 42)
	require.NoError(t, err)
	assert.WithinDuration(t, sub.PaidUntil, stored.PaidUntil, time.Second, "access must not have been extended twice")
}

// TestBilling_ConcurrentConfirmationsGrantAccessExactlyOnce is the test the
// whole design rests on: a webhook retried while the reconciler is running
// means several confirmations for one invoice arriving at the same moment.
func TestBilling_ConcurrentConfirmationsGrantAccessExactlyOnce(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()
	invoice := pendingPayment(t, repo, 42, 5)

	const attempts = 20
	var (
		wg        sync.WaitGroup
		confirmed atomic.Int64
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := repo.ConfirmPayment(ctx, invoice)
			assert.NoError(t, err)
			if ok {
				confirmed.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(1), confirmed.Load(), "%d concurrent confirmations must grant access exactly once", attempts)

	sub, err := repo.Subscription(ctx, 42)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(5*24*time.Hour), sub.PaidUntil, time.Minute,
		"one $10 payment must buy 5 days, not 100")
}

func TestBilling_RenewalStacksOnRemainingTime(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, _, err := repo.ConfirmPayment(ctx, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)

	// Paying again before the first period ends must not forfeit what is left.
	sub, confirmed, err := repo.ConfirmPayment(ctx, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)
	require.True(t, confirmed)
	assert.WithinDuration(t, time.Now().Add(10*24*time.Hour), sub.PaidUntil, time.Minute)
}

func TestBilling_RenewalRevivesLapsedKeys(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, _, err := repo.ConfirmPayment(ctx, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)
	key, _, err := repo.IssueKey(ctx, 42, "ci", []byte("hash-ci"), 5)
	require.NoError(t, err)

	// Let the period lapse.
	_, err = testPool.Exec(ctx, `UPDATE subscriptions SET paid_until = now() - interval '1 hour' WHERE telegram_user_id = 42`)
	require.NoError(t, err)
	_, err = testPool.Exec(ctx, `UPDATE api_keys SET expires_at = now() - interval '1 hour' WHERE id = $1`, key.ID)
	require.NoError(t, err)

	sub, _, err := repo.ConfirmPayment(ctx, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)

	keys, err := repo.ListKeys(ctx, 42)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	require.NotNil(t, keys[0].ExpiresAt)
	assert.WithinDuration(t, sub.PaidUntil, *keys[0].ExpiresAt, time.Second,
		"renewing must bring the existing key back, so no integration needs a new secret")
	// A lapsed period restarts from now rather than stacking on a past date.
	assert.WithinDuration(t, time.Now().Add(5*24*time.Hour), sub.PaidUntil, time.Minute)
}

func TestBilling_RenewalLeavesRevokedKeysAlone(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, _, err := repo.ConfirmPayment(ctx, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)
	key, _, err := repo.IssueKey(ctx, 42, "old", []byte("hash-old"), 5)
	require.NoError(t, err)
	require.NoError(t, repo.RevokeKey(ctx, 42, key.ID))

	_, _, err = repo.ConfirmPayment(ctx, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)

	keys, err := repo.ListKeys(ctx, 42)
	require.NoError(t, err)
	assert.Empty(t, keys, "paying again must not resurrect a key the user revoked")
}

func TestBilling_ProviderPaidOutranksLocalExpiry(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()
	invoice := pendingPayment(t, repo, 42, 5)

	require.NoError(t, repo.ExpirePayment(ctx, invoice))

	_, confirmed, err := repo.ConfirmPayment(ctx, invoice)
	require.NoError(t, err)
	assert.True(t, confirmed, "if the provider says an invoice was paid, the buyer gets what they paid for")
}

func TestBilling_UnknownInvoiceGrantsNothing(t *testing.T) {
	repo := setupBilling(t)

	sub, confirmed, err := repo.ConfirmPayment(context.Background(), 999_999)
	require.NoError(t, err)
	assert.False(t, confirmed)
	assert.Nil(t, sub)
}

func TestBilling_IssueKeyRequiresActiveAccess(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, _, err := repo.IssueKey(ctx, 42, "ci", []byte("h1"), 5)
	assert.ErrorIs(t, err, domain.ErrNoAccess, "never paid")

	_, _, err = repo.ConfirmPayment(ctx, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)
	_, err = testPool.Exec(ctx, `UPDATE subscriptions SET paid_until = now() - interval '1 second' WHERE telegram_user_id = 42`)
	require.NoError(t, err)

	_, _, err = repo.IssueKey(ctx, 42, "ci", []byte("h2"), 5)
	assert.ErrorIs(t, err, domain.ErrNoAccess, "paid, but the period is over")
}

func TestBilling_IssuedKeyExpiresWithAccess(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	sub, _, err := repo.ConfirmPayment(ctx, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)

	key, _, err := repo.IssueKey(ctx, 42, "ci", []byte("hash"), 5)
	require.NoError(t, err)
	require.NotNil(t, key.ExpiresAt)
	require.NotNil(t, key.TelegramUserID)
	assert.Equal(t, int64(42), *key.TelegramUserID)
	assert.WithinDuration(t, sub.PaidUntil, *key.ExpiresAt, time.Second)
	assert.True(t, key.IsActive(time.Now()))
	assert.False(t, key.IsActive(sub.PaidUntil.Add(time.Second)))
}

// TestBilling_KeyLimitHoldsUnderConcurrency is why IssueKey locks the
// subscription row: without FOR UPDATE, requests racing each other all count
// the same number of keys and all insert.
func TestBilling_KeyLimitHoldsUnderConcurrency(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, _, err := repo.ConfirmPayment(ctx, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)

	const (
		limit    = 3
		attempts = 20
	)
	var (
		wg      sync.WaitGroup
		issued  atomic.Int64
		limited atomic.Int64
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := repo.IssueKey(ctx, 42, "k", []byte(fmt.Sprintf("hash-%d", i)), limit)
			switch {
			case err == nil:
				issued.Add(1)
			case errors.Is(err, domain.ErrKeyLimit):
				limited.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int64(limit), issued.Load(), "concurrent requests must never exceed the key limit")
	assert.Equal(t, int64(attempts-limit), limited.Load())
}

func TestBilling_RevokeIsScopedToTheOwner(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, _, err := repo.ConfirmPayment(ctx, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)
	key, _, err := repo.IssueKey(ctx, 42, "mine", []byte("hash"), 5)
	require.NoError(t, err)

	assert.ErrorIs(t, repo.RevokeKey(ctx, 7, key.ID), domain.ErrNotFound, "another user cannot revoke it")
	require.NoError(t, repo.RevokeKey(ctx, 42, key.ID))
	assert.ErrorIs(t, repo.RevokeKey(ctx, 42, key.ID), domain.ErrNotFound, "revoking twice reports not found")

	// A revoked key frees a slot.
	keys, err := repo.ListKeys(ctx, 42)
	require.NoError(t, err)
	assert.Empty(t, keys)
}

func TestBilling_PendingPaymentsSkipsSettledOnes(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	open := pendingPayment(t, repo, 1, 5)
	paid := pendingPayment(t, repo, 2, 5)
	expired := pendingPayment(t, repo, 3, 5)
	_, _, err := repo.ConfirmPayment(ctx, paid)
	require.NoError(t, err)
	require.NoError(t, repo.ExpirePayment(ctx, expired))

	pending, err := repo.PendingPayments(ctx, time.Now().Add(-time.Hour), 100)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, open, pending[0].InvoiceID)
}

func TestBilling_TheFirstKeyBecomesTheBotKey(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, _, err := repo.ConfirmPayment(ctx, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)

	_, err = repo.ActiveKey(ctx, 42)
	assert.ErrorIs(t, err, domain.ErrNotFound, "nothing is chosen before there are keys")

	first, activated, err := repo.IssueKey(ctx, 42, "first", []byte("h1"), 5)
	require.NoError(t, err)
	assert.True(t, activated, "so that /buy then /newkey is all it takes before sending a URL")

	second, activated, err := repo.IssueKey(ctx, 42, "second", []byte("h2"), 5)
	require.NoError(t, err)
	assert.False(t, activated, "a later key -- say, one made for CI -- must not hijack the choice")

	active, err := repo.ActiveKey(ctx, 42)
	require.NoError(t, err)
	assert.Equal(t, first.ID, active.ID)
	assert.NotEqual(t, second.ID, active.ID)
}

func TestBilling_ChoosingTheBotKeyIsScopedToTheOwner(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, _, err := repo.ConfirmPayment(ctx, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)
	_, _, err = repo.ConfirmPayment(ctx, pendingPayment(t, repo, 7, 5))
	require.NoError(t, err)

	mine, _, err := repo.IssueKey(ctx, 42, "mine", []byte("h-mine"), 5)
	require.NoError(t, err)
	theirs, _, err := repo.IssueKey(ctx, 7, "theirs", []byte("h-theirs"), 5)
	require.NoError(t, err)

	_, err = repo.SetActiveKey(ctx, 42, theirs.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound, "another user's key id must not be selectable")

	chosen, err := repo.SetActiveKey(ctx, 42, mine.ID)
	require.NoError(t, err)
	assert.Equal(t, mine.ID, chosen.ID)

	// By pasted secret, matched on its hash.
	chosen, err = repo.SetActiveKeyByHash(ctx, 42, []byte("h-mine"))
	require.NoError(t, err)
	assert.Equal(t, mine.ID, chosen.ID)

	_, err = repo.SetActiveKeyByHash(ctx, 42, []byte("h-theirs"))
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// TestBilling_RevokingTheBotKeyHandsItAnother keeps the bot from being left
// pointing at a key that no longer works.
func TestBilling_RevokingTheBotKeyHandsItAnother(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, _, err := repo.ConfirmPayment(ctx, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)

	first, _, err := repo.IssueKey(ctx, 42, "first", []byte("h1"), 5)
	require.NoError(t, err)
	second, _, err := repo.IssueKey(ctx, 42, "second", []byte("h2"), 5)
	require.NoError(t, err)

	require.NoError(t, repo.RevokeKey(ctx, 42, first.ID))

	active, err := repo.ActiveKey(ctx, 42)
	require.NoError(t, err)
	assert.Equal(t, second.ID, active.ID, "the newest remaining key takes over")

	require.NoError(t, repo.RevokeKey(ctx, 42, second.ID))
	_, err = repo.ActiveKey(ctx, 42)
	assert.ErrorIs(t, err, domain.ErrNotFound, "with no keys left, nothing is chosen")
}

func TestBilling_ClickTotalsAreScopedToTheOwner(t *testing.T) {
	links, clicks, _, owner := setup(t)
	ctx := context.Background()

	mine := newLink(owner, "totals1", "https://example.com")
	require.NoError(t, links.Create(ctx, mine))
	quiet := newLink(owner, "totals2", "https://example.com/quiet")
	require.NoError(t, links.Create(ctx, quiet))

	now := time.Now().UTC()
	require.NoError(t, clicks.InsertClicks(ctx, []domain.Click{
		{LinkID: mine.ID, ClickedAt: now},
		{LinkID: mine.ID, ClickedAt: now},
	}))

	totals, err := links.ClickTotals(ctx, owner.ID, []int64{mine.ID, quiet.ID})
	require.NoError(t, err)
	assert.Equal(t, int64(2), totals[mine.ID])
	assert.NotContains(t, totals, quiet.ID, "a link nobody clicked is simply absent")

	// Another owner asking about the same ids gets nothing back.
	totals, err = links.ClickTotals(ctx, owner.ID+999, []int64{mine.ID})
	require.NoError(t, err)
	assert.Empty(t, totals)
}
