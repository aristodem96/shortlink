package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"shortlink/internal/cryptopay"
	"shortlink/internal/domain"
)

// --- doubles ------------------------------------------------------------------

// fakeStore keeps only what these tests inspect. The locking and exactly-once
// behaviour of the real store are tested against Postgres, not here.
type fakeStore struct {
	mu          sync.Mutex
	payments    map[int64]*domain.Payment
	confirmed   map[int64]bool
	confirmFn   func(invoiceID int64) (domain.Confirmation, error)
	expired     []int64
	issued      []string
	sub         *domain.Subscription
	activeKey   *domain.APIKey
	hashLookups [][]byte

	domains      []domain.CustomDomain
	activeDomain *domain.CustomDomain
	addErr       error
	slotsGranted int
}

func newFakeStore() *fakeStore {
	return &fakeStore{payments: map[int64]*domain.Payment{}, confirmed: map[int64]bool{}}
}

func (s *fakeStore) RecentPendingPayment(_ context.Context, user int64, product domain.Product, after time.Time) (*domain.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.payments {
		if p.TelegramUserID == user && p.Product == product &&
			p.Status == domain.PaymentPending && p.CreatedAt.After(after) {
			c := *p
			return &c, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (s *fakeStore) CreatePayment(_ context.Context, p domain.Payment) (*domain.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.CreatedAt = time.Now()
	s.payments[p.InvoiceID] = &p
	return &p, nil
}

func (s *fakeStore) ConfirmPayment(_ context.Context, invoiceID int64) (domain.Confirmation, error) {
	if s.confirmFn != nil {
		return s.confirmFn(invoiceID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.payments[invoiceID]
	if !ok || s.confirmed[invoiceID] {
		return domain.Confirmation{}, nil
	}
	s.confirmed[invoiceID] = true

	c := domain.Confirmation{Confirmed: true, Product: p.Product, TelegramUserID: p.TelegramUserID}
	if p.Product == domain.ProductDomain {
		s.slotsGranted++
		c.DomainSlots = s.slotsGranted
		return c, nil
	}
	c.PaidUntil = time.Now().Add(5 * 24 * time.Hour)
	return c, nil
}

func (s *fakeStore) AddDomain(_ context.Context, user int64, hostname string) (*domain.CustomDomain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.addErr != nil {
		return nil, s.addErr
	}
	d := domain.CustomDomain{
		ID: int64(len(s.domains) + 1), TelegramUserID: user, Hostname: hostname,
		VerifiedAt: ptr(time.Now()),
	}
	s.domains = append(s.domains, d)
	return &d, nil
}

func (s *fakeStore) Domains(_ context.Context, _ int64) ([]domain.CustomDomain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.domains, nil
}

func (s *fakeStore) RemoveDomain(_ context.Context, _ int64, hostname string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, d := range s.domains {
		if d.Hostname == hostname {
			s.domains = append(s.domains[:i], s.domains[i+1:]...)
			return nil
		}
	}
	return domain.ErrNotFound
}

func (s *fakeStore) SetActiveDomain(_ context.Context, _ int64, hostname string) (*domain.CustomDomain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.domains {
		if d.Hostname == hostname {
			s.activeDomain = &d
			return &d, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (s *fakeStore) ClearActiveDomain(_ context.Context, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeDomain = nil
	return nil
}

func (s *fakeStore) ActiveDomain(_ context.Context, _ int64) (*domain.CustomDomain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeDomain == nil {
		return nil, domain.ErrNotFound
	}
	return s.activeDomain, nil
}

func (s *fakeStore) ServableDomain(_ context.Context, hostname string) (*domain.CustomDomain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.domains {
		if d.Hostname == hostname {
			return &d, nil
		}
	}
	return nil, domain.ErrNotFound
}

func ptr[T any](v T) *T { return &v }

// stubResolver answers domain verification without touching DNS.
type stubResolver struct {
	addrs map[string][]string
	err   error
}

func (r stubResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if r.err != nil {
		return nil, r.err
	}
	addrs, ok := r.addrs[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return addrs, nil
}

func (s *fakeStore) ExpirePayment(_ context.Context, invoiceID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expired = append(s.expired, invoiceID)
	return nil
}

func (s *fakeStore) PendingPayments(_ context.Context, _ time.Time, _ int) ([]domain.Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Payment
	for id, p := range s.payments {
		if !s.confirmed[id] {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (s *fakeStore) Subscription(context.Context, int64) (*domain.Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sub == nil {
		return nil, domain.ErrNotFound
	}
	return s.sub, nil
}

func (s *fakeStore) IssueKey(_ context.Context, _ int64, name string, _ []byte, _ int) (*domain.APIKey, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issued = append(s.issued, name)
	first := len(s.issued) == 1
	return &domain.APIKey{ID: int64(len(s.issued)), Name: name}, first, nil
}

func (s *fakeStore) ActiveKey(context.Context, int64) (*domain.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeKey == nil {
		return nil, domain.ErrNotFound
	}
	return s.activeKey, nil
}

func (s *fakeStore) SetActiveKey(_ context.Context, _ int64, keyID int64) (*domain.APIKey, error) {
	if keyID != 7 {
		return nil, domain.ErrNotFound
	}
	return &domain.APIKey{ID: 7, Name: "chosen"}, nil
}

func (s *fakeStore) SetActiveKeyByHash(_ context.Context, _ int64, hash []byte) (*domain.APIKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hashLookups = append(s.hashLookups, hash)
	return &domain.APIKey{ID: 8, Name: "by-secret"}, nil
}

func (s *fakeStore) ListKeys(context.Context, int64) ([]domain.APIKey, error) { return nil, nil }
func (s *fakeStore) RevokeKey(context.Context, int64, int64) error            { return nil }

type fakeProvider struct {
	mu       sync.Mutex
	created  int
	statuses map[int64]string
	nextID   int64
}

func (p *fakeProvider) CreateInvoice(context.Context, cryptopay.InvoiceRequest) (cryptopay.Invoice, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.created++
	p.nextID++
	// A distinct URL per invoice, as the real provider returns, so a test can
	// tell "the same invoice back" from "a new one".
	return cryptopay.Invoice{
		InvoiceID:     p.nextID,
		Status:        cryptopay.StatusActive,
		BotInvoiceURL: fmt.Sprintf("https://t.me/pay/%d", p.nextID),
	}, nil
}

func (p *fakeProvider) GetInvoices(_ context.Context, ids []int64) ([]cryptopay.Invoice, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []cryptopay.Invoice
	for _, id := range ids {
		if st, ok := p.statuses[id]; ok {
			out = append(out, cryptopay.Invoice{InvoiceID: id, Status: st})
		}
	}
	return out, nil
}

type fakeNotifier struct {
	mu        sync.Mutex
	sent      []int64
	slotsSent []int
	fails     bool
}

func (n *fakeNotifier) DomainSlotGranted(_ context.Context, user int64, slots int) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, user)
	n.slotsSent = append(n.slotsSent, slots)
	if n.fails {
		return errors.New("telegram is down")
	}
	return nil
}

func (n *fakeNotifier) PaymentConfirmed(_ context.Context, user int64, _ time.Time) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, user)
	if n.fails {
		return errors.New("telegram is down")
	}
	return nil
}

func (n *fakeNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.sent)
}

func newService(t *testing.T) (*Service, *fakeStore, *fakeProvider, *fakeNotifier) {
	t.Helper()
	store := newFakeStore()
	provider := &fakeProvider{statuses: map[int64]string{}}
	notifier := &fakeNotifier{}
	resolver := stubResolver{addrs: map[string][]string{"links.example.com": {"203.0.113.45"}}}
	svc := New(store, provider, notifier, resolver, []string{"203.0.113.45"}, Config{
		PriceUSD: "10", DomainPriceUSD: "5", AccessDays: 5, MaxKeys: 5,
		AcceptedAssets: []string{"USDT"}, InvoiceTTL: time.Hour,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, store, provider, notifier
}

// --- service ------------------------------------------------------------------

func TestCheckout_ReusesAStillPayableInvoice(t *testing.T) {
	t.Parallel()
	svc, _, provider, _ := newService(t)
	ctx := context.Background()

	first, err := svc.Checkout(ctx, 42, domain.ProductAccess)
	require.NoError(t, err)
	second, err := svc.Checkout(ctx, 42, domain.ProductAccess)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Equal(t, 1, provider.created, "tapping buy twice must not open a second invoice")
}

func TestCheckout_DoesNotShareInvoicesBetweenUsers(t *testing.T) {
	t.Parallel()
	svc, _, provider, _ := newService(t)
	ctx := context.Background()

	_, err := svc.Checkout(ctx, 1, domain.ProductAccess)
	require.NoError(t, err)
	_, err = svc.Checkout(ctx, 2, domain.ProductAccess)
	require.NoError(t, err)

	assert.Equal(t, 2, provider.created)
}

// TestCheckout_DoesNotReuseAnInvoiceAcrossProducts guards the mis-billing bug
// that scoping RecentPendingPayment by product exists to prevent: a buyer who
// asks for access and then for a domain, inside the reuse window, must not be
// handed the first invoice back and charged $10 for a $5 product.
func TestCheckout_DoesNotReuseAnInvoiceAcrossProducts(t *testing.T) {
	t.Parallel()
	svc, store, provider, _ := newService(t)
	ctx := context.Background()

	accessURL, err := svc.Checkout(ctx, 1, domain.ProductAccess)
	require.NoError(t, err)

	domainURL, err := svc.Checkout(ctx, 1, domain.ProductDomain)
	require.NoError(t, err)

	assert.Equal(t, 2, provider.created, "each product needs its own invoice")
	assert.NotEqual(t, accessURL, domainURL)

	// And the recorded rows describe what was actually sold.
	var access, custom *domain.Payment
	for _, p := range store.payments {
		switch p.Product {
		case domain.ProductAccess:
			access = p
		case domain.ProductDomain:
			custom = p
		}
	}
	require.NotNil(t, access)
	require.NotNil(t, custom)
	assert.Equal(t, "10", access.Amount)
	require.NotNil(t, access.AccessDays)
	assert.Equal(t, 5, *access.AccessDays)
	assert.Equal(t, "5", custom.Amount)
	assert.Nil(t, custom.AccessDays, "a domain slot buys no days")
}

func TestCheckout_StillReusesAPendingInvoiceOfTheSameProduct(t *testing.T) {
	t.Parallel()
	svc, _, provider, _ := newService(t)
	ctx := context.Background()

	first, err := svc.Checkout(ctx, 1, domain.ProductDomain)
	require.NoError(t, err)
	second, err := svc.Checkout(ctx, 1, domain.ProductDomain)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.Equal(t, 1, provider.created)
}

func TestConfirm_GrantsADomainSlotInsteadOfDays(t *testing.T) {
	t.Parallel()
	svc, store, _, notifier := newService(t)
	ctx := context.Background()

	_, err := svc.Checkout(ctx, 1, domain.ProductDomain)
	require.NoError(t, err)

	var invoice int64
	for id := range store.payments {
		invoice = id
	}
	require.NoError(t, svc.Confirm(ctx, invoice))

	assert.Equal(t, []int{1}, notifier.slotsSent, "the buyer is told about the slot, not an expiry date")

	// Confirming again changes nothing, exactly as for access.
	require.NoError(t, svc.Confirm(ctx, invoice))
	assert.Equal(t, []int{1}, notifier.slotsSent)
}

func TestAddDomain_RequiresTheDomainToPointHere(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newService(t)
	ctx := context.Background()

	_, err := svc.AddDomain(ctx, 1, "elsewhere.example.com")
	assert.ErrorIs(t, err, domain.ErrDomainNotPointed,
		"a domain that does not resolve here cannot be served, so it is refused on arrival")

	d, err := svc.AddDomain(ctx, 1, "  HTTPS://Links.Example.com/path  ")
	require.NoError(t, err)
	assert.Equal(t, "links.example.com", d.Hostname, "the hostname is canonicalised before it is stored")
}

func TestMayServe_FailsClosed(t *testing.T) {
	t.Parallel()
	svc, store, _, _ := newService(t)
	ctx := context.Background()

	// Nothing registered yet.
	ok, err := svc.MayServe(ctx, "links.example.com")
	require.NoError(t, err)
	assert.False(t, ok)

	// Malformed names are refused without consulting the store at all, so a
	// certificate is never requested for something shaped like an IP address.
	ok, err = svc.MayServe(ctx, "203.0.113.45")
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = svc.AddDomain(ctx, 1, "links.example.com")
	require.NoError(t, err)

	ok, err = svc.MayServe(ctx, "links.example.com")
	require.NoError(t, err)
	assert.True(t, ok)

	// A store failure must not be read as permission to issue.
	store.mu.Lock()
	store.domains = nil
	store.mu.Unlock()
	ok, err = svc.MayServe(ctx, "links.example.com")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestConfirm_NotifiesExactlyOnce(t *testing.T) {
	t.Parallel()
	svc, _, _, notifier := newService(t)
	ctx := context.Background()

	_, err := svc.Checkout(ctx, 42, domain.ProductAccess)
	require.NoError(t, err)

	// A webhook retried three times.
	for i := 0; i < 3; i++ {
		require.NoError(t, svc.Confirm(ctx, 1))
	}

	assert.Equal(t, 1, notifier.count(), "the buyer must get one confirmation, not three")
}

func TestConfirm_AFailedNotificationDoesNotFailThePayment(t *testing.T) {
	t.Parallel()
	svc, _, _, notifier := newService(t)
	notifier.fails = true
	ctx := context.Background()

	_, err := svc.Checkout(ctx, 42, domain.ProductAccess)
	require.NoError(t, err)

	// The access is already recorded; an error here would make the provider
	// retry a payment that is settled.
	assert.NoError(t, svc.Confirm(ctx, 1))
}

func TestConfirm_PropagatesStoreFailures(t *testing.T) {
	t.Parallel()
	svc, store, _, notifier := newService(t)
	store.confirmFn = func(int64) (domain.Confirmation, error) {
		return domain.Confirmation{}, errors.New("connection refused")
	}

	err := svc.Confirm(context.Background(), 1)
	require.Error(t, err, "an unrecorded payment must surface, so the webhook answers 500 and is retried")
	assert.Zero(t, notifier.count())
}

func TestReconcile_SettlesPaidAndExpiredInvoices(t *testing.T) {
	t.Parallel()
	svc, store, provider, notifier := newService(t)
	ctx := context.Background()

	_, err := svc.Checkout(ctx, 1, domain.ProductAccess) // invoice 1
	require.NoError(t, err)
	_, err = svc.Checkout(ctx, 2, domain.ProductAccess) // invoice 2
	require.NoError(t, err)
	_, err = svc.Checkout(ctx, 3, domain.ProductAccess) // invoice 3
	require.NoError(t, err)

	provider.statuses[1] = cryptopay.StatusPaid
	provider.statuses[2] = cryptopay.StatusExpired
	provider.statuses[3] = cryptopay.StatusActive

	require.NoError(t, svc.Reconcile(ctx))

	assert.Equal(t, 1, notifier.count(), "only the paid invoice grants access")
	assert.Equal(t, []int64{2}, store.expired)
}

func TestIssueKey_DefaultsAndLimitsTheName(t *testing.T) {
	t.Parallel()
	svc, store, _, _ := newService(t)
	ctx := context.Background()

	issued, err := svc.IssueKey(ctx, 42, "   ")
	require.NoError(t, err)
	assert.Equal(t, "telegram", issued.Key.Name, "an empty name gets a default rather than an empty label")
	assert.Contains(t, issued.Secret, "sl_")
	assert.True(t, issued.Activated, "a user's first key becomes the one the bot uses")

	issued, err = svc.IssueKey(ctx, 42, "ci\x00\nbuild")
	require.NoError(t, err)
	assert.Equal(t, "cibuild", issued.Key.Name, "control characters from a chat message are stripped")
	assert.False(t, issued.Activated, "a later key leaves the choice alone")

	// 41 visible characters. (string(make([]rune, 41)) would be 41 NULs, which
	// are control characters and get stripped to nothing.)
	_, err = svc.IssueKey(ctx, 42, strings.Repeat("a", 41))
	var ve *domain.ValidationError
	require.ErrorAs(t, err, &ve)
	assert.Len(t, store.issued, 2, "an invalid name must not reach the store")
}

// --- webhook ------------------------------------------------------------------

const webhookToken = "12345:AAwebhookToken"

func signed(t *testing.T, body string) *http.Request {
	t.Helper()
	secret := sha256.Sum256([]byte(webhookToken))
	mac := hmac.New(sha256.New, secret[:])
	mac.Write([]byte(body))

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cryptopay", bytes.NewBufferString(body))
	req.Header.Set(cryptopay.SignatureHeader, hex.EncodeToString(mac.Sum(nil)))
	return req
}

const paidUpdate = `{"update_id":1,"update_type":"invoice_paid","payload":{"invoice_id":1,"status":"paid"}}`

func TestWebhook_RejectsAForgedPayment(t *testing.T) {
	t.Parallel()
	svc, _, _, notifier := newService(t)
	_, err := svc.Checkout(context.Background(), 42, domain.ProductAccess)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cryptopay", bytes.NewBufferString(paidUpdate))
	req.Header.Set(cryptopay.SignatureHeader, "deadbeef")

	rec := httptest.NewRecorder()
	svc.WebhookHandler(webhookToken).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Zero(t, notifier.count(), "a forged webhook must never grant access")
}

func TestWebhook_ConfirmsAGenuinePayment(t *testing.T) {
	t.Parallel()
	svc, _, _, notifier := newService(t)
	_, err := svc.Checkout(context.Background(), 42, domain.ProductAccess)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	svc.WebhookHandler(webhookToken).ServeHTTP(rec, signed(t, paidUpdate))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, notifier.count())
}

func TestWebhook_AcknowledgesButIgnoresOtherUpdates(t *testing.T) {
	t.Parallel()
	svc, _, _, notifier := newService(t)

	rec := httptest.NewRecorder()
	svc.WebhookHandler(webhookToken).ServeHTTP(rec,
		signed(t, `{"update_id":2,"update_type":"something_else","payload":{"invoice_id":1}}`))

	// 200, not an error: anything else makes the provider retry forever.
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Zero(t, notifier.count())
}

func TestWebhook_AsksForARetryWhenThePaymentCannotBeRecorded(t *testing.T) {
	t.Parallel()
	svc, store, _, _ := newService(t)
	store.confirmFn = func(int64) (domain.Confirmation, error) {
		return domain.Confirmation{}, errors.New("database is down")
	}

	rec := httptest.NewRecorder()
	svc.WebhookHandler(webhookToken).ServeHTTP(rec, signed(t, paidUpdate))

	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"a 500 makes the provider redeliver, which is safe because confirming is idempotent")
}

func TestWebhook_RejectsOtherMethods(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newService(t)

	rec := httptest.NewRecorder()
	svc.WebhookHandler(webhookToken).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhooks/cryptopay", nil))

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestUsableKey_RefusesWhenThereIsNothingToUse(t *testing.T) {
	t.Parallel()

	t.Run("paid but no key created yet", func(t *testing.T) {
		t.Parallel()
		svc, store, _, _ := newService(t)
		store.sub = &domain.Subscription{PaidUntil: time.Now().Add(time.Hour)}

		_, err := svc.UsableKey(context.Background(), 42)
		assert.ErrorIs(t, err, domain.ErrNoActiveKey, "the advice should be /newkey")
	})

	t.Run("never paid", func(t *testing.T) {
		t.Parallel()
		svc, _, _, _ := newService(t)

		// Not ErrNoActiveKey: telling this user to run /newkey would only get
		// them refused again.
		_, err := svc.UsableKey(context.Background(), 42)
		assert.ErrorIs(t, err, domain.ErrNoAccess, "the advice should be /buy")
	})

	t.Run("access ran out and no key is chosen", func(t *testing.T) {
		t.Parallel()
		svc, store, _, _ := newService(t)
		store.sub = &domain.Subscription{PaidUntil: time.Now().Add(-time.Hour)}

		_, err := svc.UsableKey(context.Background(), 42)
		assert.ErrorIs(t, err, domain.ErrNoAccess)
	})

	t.Run("key has lapsed", func(t *testing.T) {
		t.Parallel()
		svc, store, _, _ := newService(t)
		past := time.Now().Add(-time.Hour)
		store.activeKey = &domain.APIKey{ID: 1, ExpiresAt: &past}

		// The bot creates links in-process, bypassing the API's auth
		// middleware, so this check is the only thing standing between an
		// expired key and free use of the service.
		_, err := svc.UsableKey(context.Background(), 42)
		assert.ErrorIs(t, err, domain.ErrNoAccess)
	})

	t.Run("key is live", func(t *testing.T) {
		t.Parallel()
		svc, store, _, _ := newService(t)
		future := time.Now().Add(time.Hour)
		store.activeKey = &domain.APIKey{ID: 1, ExpiresAt: &future}

		key, err := svc.UsableKey(context.Background(), 42)
		require.NoError(t, err)
		assert.Equal(t, int64(1), key.ID)
	})
}

func TestUseKey_AcceptsANumberOrAPastedSecret(t *testing.T) {
	t.Parallel()
	svc, store, _, _ := newService(t)
	ctx := context.Background()

	key, err := svc.UseKey(ctx, 42, " 7 ")
	require.NoError(t, err)
	assert.Equal(t, int64(7), key.ID)

	key, err = svc.UseKey(ctx, 42, "sl_pastedSecretValue")
	require.NoError(t, err)
	assert.Equal(t, int64(8), key.ID)
	require.Len(t, store.hashLookups, 1)
	assert.NotContains(t, string(store.hashLookups[0]), "sl_pastedSecretValue",
		"a pasted secret is hashed before it reaches the store")

	_, err = svc.UseKey(ctx, 42, "not-a-key")
	var ve *domain.ValidationError
	assert.ErrorAs(t, err, &ve)
}
