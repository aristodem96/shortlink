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

	"shortlink/internal/domain"
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

	// custom_domains is listed explicitly: nothing references it with a foreign
	// key, so CASCADE from the other tables does not reach it, and leftover
	// rows would spend the next test's domain slots.
	_, err := testPool.Exec(context.Background(),
		`TRUNCATE payments, subscriptions, custom_domains, api_keys RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	return NewBillingRepository(testPool)
}

var nextInvoice atomic.Int64

// confirmPayment keeps the (subscription, confirmed, error) shape these tests
// were written against, now that ConfirmPayment returns a Confirmation covering
// both products. The assertions below are about access, which is what the
// subscription describes.
func confirmPayment(ctx context.Context, repo *BillingRepository, invoice int64) (*domain.Subscription, bool, error) {
	c, err := repo.ConfirmPayment(ctx, invoice)
	if err != nil || !c.Confirmed {
		return nil, c.Confirmed, err
	}
	return &domain.Subscription{TelegramUserID: c.TelegramUserID, PaidUntil: c.PaidUntil}, true, nil
}

// domainPayment records a pending custom-domain purchase, which buys a slot and
// no days at all.
func domainPayment(t *testing.T, repo *BillingRepository, user int64) int64 {
	t.Helper()
	invoice := 100_000 + nextInvoice.Add(1)
	_, err := repo.CreatePayment(context.Background(), domain.Payment{
		InvoiceID: invoice, TelegramUserID: user, Product: domain.ProductDomain,
		Amount: "5", Currency: "USD", Status: domain.PaymentPending,
		PayURL: fmt.Sprintf("https://t.me/pay/%d", invoice),
	})
	require.NoError(t, err)
	return invoice
}

func pendingPayment(t *testing.T, repo *BillingRepository, user int64, days int) int64 {
	t.Helper()
	invoice := 100_000 + nextInvoice.Add(1)
	_, err := repo.CreatePayment(context.Background(), domain.Payment{
		InvoiceID: invoice, TelegramUserID: user, Product: domain.ProductAccess,
		Amount: "10", Currency: "USD", AccessDays: &days, Status: domain.PaymentPending, PayURL: fmt.Sprintf("https://t.me/pay/%d", invoice),
	})
	require.NoError(t, err)
	return invoice
}

func TestBilling_PaymentRoundTrip(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	invoice := pendingPayment(t, repo, 42, 5)

	got, err := repo.RecentPendingPayment(ctx, 42, domain.ProductAccess, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, invoice, got.InvoiceID)
	assert.Equal(t, "10.00", got.Amount, "NUMERIC(12,2) must come back exact, not as a float")
	assert.Equal(t, domain.PaymentPending, got.Status)

	_, err = repo.RecentPendingPayment(ctx, 42, domain.ProductAccess, time.Now().Add(time.Hour))
	assert.ErrorIs(t, err, domain.ErrNotFound, "a payment older than the cut-off is not reused")
}

func TestBilling_ConfirmGrantsAccessOnce(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()
	invoice := pendingPayment(t, repo, 42, 5)

	sub, confirmed, err := confirmPayment(ctx, repo, invoice)
	require.NoError(t, err)
	require.True(t, confirmed)
	assert.WithinDuration(t, time.Now().Add(5*24*time.Hour), sub.PaidUntil, time.Minute)

	// A retried webhook for the same invoice.
	_, confirmed, err = confirmPayment(ctx, repo, invoice)
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
			_, ok, err := confirmPayment(ctx, repo, invoice)
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

	_, _, err := confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)

	// Paying again before the first period ends must not forfeit what is left.
	sub, confirmed, err := confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)
	require.True(t, confirmed)
	assert.WithinDuration(t, time.Now().Add(10*24*time.Hour), sub.PaidUntil, time.Minute)
}

func TestBilling_RenewalRevivesLapsedKeys(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, _, err := confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)
	key, _, err := repo.IssueKey(ctx, 42, "ci", []byte("hash-ci"), 5)
	require.NoError(t, err)

	// Let the period lapse.
	_, err = testPool.Exec(ctx, `UPDATE subscriptions SET paid_until = now() - interval '1 hour' WHERE telegram_user_id = 42`)
	require.NoError(t, err)
	_, err = testPool.Exec(ctx, `UPDATE api_keys SET expires_at = now() - interval '1 hour' WHERE id = $1`, key.ID)
	require.NoError(t, err)

	sub, _, err := confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
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

	_, _, err := confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)
	key, _, err := repo.IssueKey(ctx, 42, "old", []byte("hash-old"), 5)
	require.NoError(t, err)
	require.NoError(t, repo.RevokeKey(ctx, 42, key.ID))

	_, _, err = confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
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

	_, confirmed, err := confirmPayment(ctx, repo, invoice)
	require.NoError(t, err)
	assert.True(t, confirmed, "if the provider says an invoice was paid, the buyer gets what they paid for")
}

func TestBilling_UnknownInvoiceGrantsNothing(t *testing.T) {
	repo := setupBilling(t)

	sub, confirmed, err := confirmPayment(context.Background(), repo, 999_999)
	require.NoError(t, err)
	assert.False(t, confirmed)
	assert.Nil(t, sub)
}

func TestBilling_IssueKeyRequiresActiveAccess(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, _, err := repo.IssueKey(ctx, 42, "ci", []byte("h1"), 5)
	assert.ErrorIs(t, err, domain.ErrNoAccess, "never paid")

	_, _, err = confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)
	_, err = testPool.Exec(ctx, `UPDATE subscriptions SET paid_until = now() - interval '1 second' WHERE telegram_user_id = 42`)
	require.NoError(t, err)

	_, _, err = repo.IssueKey(ctx, 42, "ci", []byte("h2"), 5)
	assert.ErrorIs(t, err, domain.ErrNoAccess, "paid, but the period is over")
}

func TestBilling_IssuedKeyExpiresWithAccess(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	sub, _, err := confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
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

	_, _, err := confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
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

	_, _, err := confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
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
	_, _, err := confirmPayment(ctx, repo, paid)
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

	_, _, err := confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
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

	_, _, err := confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)
	_, _, err = confirmPayment(ctx, repo, pendingPayment(t, repo, 7, 5))
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

	_, _, err := confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
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

// --- custom domains -----------------------------------------------------------

// TestBilling_ConcurrentDomainConfirmationsGrantOneSlot mirrors the access-side
// guarantee: a webhook retried while the reconciler runs must buy one slot, not
// twenty.
func TestBilling_ConcurrentDomainConfirmationsGrantOneSlot(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()
	invoice := domainPayment(t, repo, 42)

	const attempts = 20
	var (
		wg        sync.WaitGroup
		confirmed atomic.Int64
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := repo.ConfirmPayment(ctx, invoice)
			assert.NoError(t, err)
			if c.Confirmed {
				confirmed.Add(1)
				assert.Equal(t, domain.ProductDomain, c.Product)
				assert.Equal(t, 1, c.DomainSlots)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(1), confirmed.Load())

	sub, err := repo.Subscription(ctx, 42)
	require.NoError(t, err)
	assert.Equal(t, 1, sub.DomainSlots, "one $5 payment must buy one slot, not %d", attempts)
}

// TestBilling_ADomainPurchaseGrantsNoAccess is the rule that makes the add-on an
// add-on: buying a domain must not smuggle in free API access.
func TestBilling_ADomainPurchaseGrantsNoAccess(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, err := repo.ConfirmPayment(ctx, domainPayment(t, repo, 42))
	require.NoError(t, err)

	sub, err := repo.Subscription(ctx, 42)
	require.NoError(t, err)
	assert.False(t, sub.IsActive(time.Now()), "a domain slot buys no days")
	assert.Equal(t, 1, sub.DomainSlots)

	// And no key can be minted off the back of it.
	_, _, err = repo.IssueKey(ctx, 42, "ci", []byte("hash-nokey"), 5)
	assert.ErrorIs(t, err, domain.ErrNoAccess)
}

func TestBilling_DomainSlotsStack(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := repo.ConfirmPayment(ctx, domainPayment(t, repo, 42))
		require.NoError(t, err)
	}

	sub, err := repo.Subscription(ctx, 42)
	require.NoError(t, err)
	assert.Equal(t, 3, sub.DomainSlots)
}

func TestBilling_AddDomainHonoursTheSlotCountUnderConcurrency(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	// Two slots paid for, ten domains attempted at once.
	for i := 0; i < 2; i++ {
		_, err := repo.ConfirmPayment(ctx, domainPayment(t, repo, 42))
		require.NoError(t, err)
	}

	const attempts = 10
	var (
		wg       sync.WaitGroup
		added    atomic.Int64
		refused  atomic.Int64
		otherErr atomic.Int64
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := repo.AddDomain(ctx, 42, fmt.Sprintf("links%d.example.com", n))
			switch {
			case err == nil:
				added.Add(1)
			case errors.Is(err, domain.ErrNoDomainSlot):
				refused.Add(1)
			default:
				otherErr.Add(1)
			}
		}(i)
	}
	wg.Wait()

	assert.Zero(t, otherErr.Load())
	assert.Equal(t, int64(2), added.Load(), "only the paid-for slots may be used")
	assert.Equal(t, int64(attempts-2), refused.Load())
}

func TestBilling_ADomainBelongsToExactlyOneUser(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	for _, user := range []int64{42, 7} {
		_, err := repo.ConfirmPayment(ctx, domainPayment(t, repo, user))
		require.NoError(t, err)
	}

	_, err := repo.AddDomain(ctx, 42, "links.example.com")
	require.NoError(t, err)

	_, err = repo.AddDomain(ctx, 7, "links.example.com")
	assert.ErrorIs(t, err, domain.ErrDomainTaken,
		"two customers must never both serve the same hostname")
}

func TestBilling_TheFirstDomainBecomesTheActiveOne(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_, err := repo.ConfirmPayment(ctx, domainPayment(t, repo, 42))
		require.NoError(t, err)
	}

	first, err := repo.AddDomain(ctx, 42, "one.example.com")
	require.NoError(t, err)
	_, err = repo.AddDomain(ctx, 42, "two.example.com")
	require.NoError(t, err)

	active, err := repo.ActiveDomain(ctx, 42)
	require.NoError(t, err)
	assert.Equal(t, first.ID, active.ID, "the first domain is chosen, as the first key is")

	chosen, err := repo.SetActiveDomain(ctx, 42, "two.example.com")
	require.NoError(t, err)
	active, err = repo.ActiveDomain(ctx, 42)
	require.NoError(t, err)
	assert.Equal(t, chosen.ID, active.ID)

	// Another user cannot point at it.
	_, err = repo.SetActiveDomain(ctx, 7, "two.example.com")
	assert.ErrorIs(t, err, domain.ErrNotFound, "not forbidden: the answer must not confirm it exists")
}

func TestBilling_ServableDomainRequiresLiveAccess(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, err := repo.ConfirmPayment(ctx, domainPayment(t, repo, 42))
	require.NoError(t, err)
	_, err = repo.AddDomain(ctx, 42, "links.example.com")
	require.NoError(t, err)

	// The slot alone leaves paid_until at now(), so access is already lapsed.
	_, err = repo.ServableDomain(ctx, "links.example.com")
	assert.ErrorIs(t, err, domain.ErrNotFound,
		"a domain must not be served, nor a certificate requested, once access lapses")

	_, _, err = confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)

	got, err := repo.ServableDomain(ctx, "links.example.com")
	require.NoError(t, err)
	assert.Equal(t, "links.example.com", got.Hostname)
}

func TestBilling_RemovingADomainKeepsItsLinks(t *testing.T) {
	repo := setupBilling(t)
	ctx := context.Background()

	_, err := repo.ConfirmPayment(ctx, domainPayment(t, repo, 42))
	require.NoError(t, err)
	_, _, err = confirmPayment(ctx, repo, pendingPayment(t, repo, 42, 5))
	require.NoError(t, err)
	key, _, err := repo.IssueKey(ctx, 42, "ci", []byte("hash-rm"), 5)
	require.NoError(t, err)

	d, err := repo.AddDomain(ctx, 42, "links.example.com")
	require.NoError(t, err)

	linkRepo := NewLinkRepository(testPool)
	link := &domain.Link{
		Code: "keepme", TargetURL: "https://example.com", OwnerID: key.ID,
		DomainID: &d.ID, CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, linkRepo.Create(ctx, link))

	stored, err := linkRepo.GetByCode(ctx, "keepme")
	require.NoError(t, err)
	assert.Equal(t, "links.example.com", stored.Host, "the hostname rides along with the link")

	require.NoError(t, repo.RemoveDomain(ctx, 42, "links.example.com"))

	stored, err = linkRepo.GetByCode(ctx, "keepme")
	require.NoError(t, err, "removing a domain must not delete the links that used it")
	assert.Empty(t, stored.Host, "they fall back to the operator's domain")
	assert.Nil(t, stored.DomainID)
}
