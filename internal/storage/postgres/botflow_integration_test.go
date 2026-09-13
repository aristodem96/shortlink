package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"shortlink/internal/billing"
	"shortlink/internal/bot"
	"shortlink/internal/cryptopay"
	"shortlink/internal/domain"
	"shortlink/internal/service"
	"shortlink/internal/shortid"
	"shortlink/internal/telegram"
)

// This drives the bot the way a person does -- one chat message at a time --
// against the real database, through the real billing store, the real link
// service and the real SQL. Everything between the message and Postgres is
// production code; only Telegram and the payment provider are stand-ins,
// because neither can be reached from a test.

type captureMessenger struct{ msgs []string }

func (m *captureMessenger) SendMessage(_ context.Context, _ int64, text string, _ ...telegram.Button) error {
	m.msgs = append(m.msgs, text)
	return nil
}

func (m *captureMessenger) last(t *testing.T) string {
	t.Helper()
	require.NotEmpty(t, m.msgs)
	return m.msgs[len(m.msgs)-1]
}

// noopCache keeps the link service honest without a Redis container: caching is
// covered by its own tests, and what matters here is the SQL underneath.
type noopCache struct{}

func (noopCache) Get(context.Context, string) (*domain.Link, error)       { return nil, domain.ErrCacheMiss }
func (noopCache) Set(context.Context, *domain.Link, time.Duration) error  { return nil }
func (noopCache) SetMissing(context.Context, string, time.Duration) error { return nil }
func (noopCache) Delete(context.Context, string) error                    { return nil }

type noopClicks struct{}

func (noopClicks) Record(domain.Click) {}

type noopHasher struct{}

func (noopHasher) HashIP(string) []byte { return nil }

// stubProvider mints one invoice id per call, starting at 4242. Ids have to be
// distinct because payments.invoice_id is UNIQUE: a provider that reused one
// would make the second purchase fail, which is a property of the stub rather
// than of the code under test.
type stubProvider struct{ issued atomic.Int64 }

const firstInvoice = 4242

func (p *stubProvider) CreateInvoice(context.Context, cryptopay.InvoiceRequest) (cryptopay.Invoice, error) {
	id := firstInvoice + p.issued.Add(1) - 1
	return cryptopay.Invoice{
		InvoiceID:     id,
		Status:        cryptopay.StatusActive,
		BotInvoiceURL: fmt.Sprintf("https://t.me/CryptoBot?start=IV%d", id),
	}, nil
}

func (p *stubProvider) GetInvoices(context.Context, []int64) ([]cryptopay.Invoice, error) {
	return nil, nil
}

type stubNotifier struct {
	paid  int
	slots int
}

func (n *stubNotifier) PaymentConfirmed(context.Context, int64, time.Time) error {
	n.paid++
	return nil
}

func (n *stubNotifier) DomainSlotGranted(_ context.Context, _ int64, slots int) error {
	n.slots = slots
	return nil
}

// stubResolver answers domain verification without touching DNS, so the flow
// can be walked end to end without a public hostname.
type stubResolver struct{ addrs map[string][]string }

func (r stubResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if a, ok := r.addrs[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

const testServerIP = "203.0.113.45"

func chat(text string) telegram.Update {
	return telegram.Update{Message: &telegram.Message{
		From: &telegram.User{ID: 4242}, Chat: telegram.Chat{ID: 4242, Type: telegram.ChatPrivate}, Text: text,
	}}
}

func TestBotFlow_BuyThenShortenThenSeeClicks(t *testing.T) {
	store := setupBilling(t)
	ctx := context.Background()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	codes, err := shortid.NewGenerator(7)
	require.NoError(t, err)

	links := service.NewLinkService(
		NewLinkRepository(testPool), noopCache{}, noopClicks{}, codes, noopHasher{},
		service.LinkServiceConfig{CacheTTL: time.Hour, NegativeCacheTTL: 30 * time.Second}, discard,
	)
	notifier := &stubNotifier{}
	svc := billing.New(store, &stubProvider{}, notifier,
		stubResolver{addrs: map[string][]string{"links.example.com": {testServerIP}}},
		[]string{testServerIP},
		billing.Config{
			PriceUSD: "10", DomainPriceUSD: "5", AccessDays: 5, MaxKeys: 5,
			AcceptedAssets: []string{"USDT"}, InvoiceTTL: time.Hour,
		}, discard)

	msgs := &captureMessenger{}
	b := bot.New(msgs, nil, svc, links, "https://sho.rt", time.UTC, discard)

	// 1. Before paying, there is nothing to shorten with.
	b.Handle(ctx, chat("https://go.dev/blog/context"))
	assert.Contains(t, msgs.last(t), "/buy", "an unpaid user is pointed at the till")

	// 2. Buy, and let the payment land.
	b.Handle(ctx, chat("/buy"))
	assert.Contains(t, msgs.last(t), "$10")
	require.NoError(t, svc.Confirm(ctx, 4242))
	assert.Equal(t, 1, notifier.paid)

	// 3. A key: the first one becomes the key the bot shortens with.
	b.Handle(ctx, chat("/newkey ci"))
	issued := msgs.last(t)
	assert.Contains(t, issued, "sl_")
	assert.Contains(t, issued, "The bot will use this key")

	// 4. Shorten a link by sending it, with an alias.
	b.Handle(ctx, chat("https://go.dev/blog/context go-ctx"))
	assert.Contains(t, msgs.last(t), "https://sho.rt/go-ctx")

	// The link really is in the database, owned by the key the bot issued.
	link, err := NewLinkRepository(testPool).GetByCode(ctx, "go-ctx")
	require.NoError(t, err)
	assert.Equal(t, "https://go.dev/blog/context", link.TargetURL)

	// 5. Clicks arrive the way redirects record them.
	now := time.Now().UTC()
	require.NoError(t, NewClickRepository(testPool).InsertClicks(ctx, []domain.Click{
		{LinkID: link.ID, ClickedAt: now, Referer: "https://news.example.com", IPHash: []byte("visitor-a")},
		{LinkID: link.ID, ClickedAt: now, Referer: "https://news.example.com", IPHash: []byte("visitor-a")},
		{LinkID: link.ID, ClickedAt: now, IPHash: []byte("visitor-b")},
	}))

	// 6. The user sees them in the chat.
	b.Handle(ctx, chat("/links"))
	assert.Contains(t, msgs.last(t), "<code>go-ctx</code> · 3 clicks")

	b.Handle(ctx, chat("/stats go-ctx"))
	stats := msgs.last(t)
	assert.Contains(t, stats, "Clicks: <b>3</b>")
	assert.Contains(t, stats, "Unique visitors: <b>2</b>", "one visitor clicked twice")
	assert.Contains(t, stats, "https://news.example.com — 2")
	assert.Contains(t, stats, "(direct) — 1")
}

func TestBotFlow_TheDomainRulesApplyToLinksMadeInChat(t *testing.T) {
	store := setupBilling(t)
	ctx := context.Background()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	codes, err := shortid.NewGenerator(7)
	require.NoError(t, err)
	links := service.NewLinkService(
		NewLinkRepository(testPool), noopCache{}, noopClicks{}, codes, noopHasher{},
		service.LinkServiceConfig{CacheTTL: time.Hour, NegativeCacheTTL: 30 * time.Second}, discard,
	)
	svc := billing.New(store, &stubProvider{}, &stubNotifier{}, nil, nil, billing.Config{
		PriceUSD: "10", DomainPriceUSD: "5", AccessDays: 5, MaxKeys: 5, InvoiceTTL: time.Hour,
	}, discard)

	msgs := &captureMessenger{}
	b := bot.New(msgs, nil, svc, links, "https://sho.rt", time.UTC, discard)

	b.Handle(ctx, chat("/buy"))
	require.NoError(t, svc.Confirm(ctx, 4242))
	b.Handle(ctx, chat("/newkey"))

	// The chat is not a way around the checks the HTTP API makes.
	b.Handle(ctx, chat("http://169.254.169.254/latest/meta-data/"))
	assert.Contains(t, msgs.last(t), "private or loopback",
		"the cloud metadata endpoint is refused in the chat exactly as it is over HTTP")

	b.Handle(ctx, chat("https://example.com api"))
	assert.Contains(t, msgs.last(t), "reserved", "a reserved alias would shadow the API routes")

	b.Handle(ctx, chat("https://example.com taken-once"))
	require.Contains(t, msgs.last(t), "taken-once")
	b.Handle(ctx, chat("https://example.com taken-once"))
	assert.Contains(t, strings.ToLower(msgs.last(t)), "already taken")
}

// TestBotFlow_ExpiredAccessStopsTheChatToo: the bot creates links in-process,
// so it does not pass through the API's authentication middleware. If it did
// not check for itself, it would be a way to keep using the service after the
// access paid for had run out.
func TestBotFlow_ExpiredAccessStopsTheChatToo(t *testing.T) {
	store := setupBilling(t)
	ctx := context.Background()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	codes, err := shortid.NewGenerator(7)
	require.NoError(t, err)
	links := service.NewLinkService(
		NewLinkRepository(testPool), noopCache{}, noopClicks{}, codes, noopHasher{},
		service.LinkServiceConfig{CacheTTL: time.Hour, NegativeCacheTTL: 30 * time.Second}, discard,
	)
	svc := billing.New(store, &stubProvider{}, &stubNotifier{}, nil, nil, billing.Config{
		PriceUSD: "10", DomainPriceUSD: "5", AccessDays: 5, MaxKeys: 5, InvoiceTTL: time.Hour,
	}, discard)

	msgs := &captureMessenger{}
	b := bot.New(msgs, nil, svc, links, "https://sho.rt", time.UTC, discard)

	b.Handle(ctx, chat("/buy"))
	require.NoError(t, svc.Confirm(ctx, 4242))
	b.Handle(ctx, chat("/newkey"))
	b.Handle(ctx, chat("https://example.com while-paid"))
	require.Contains(t, msgs.last(t), "while-paid")

	// Let the paid period run out.
	_, err = testPool.Exec(ctx, `
		UPDATE subscriptions SET paid_until = now() - interval '1 minute' WHERE telegram_user_id = 4242;
		UPDATE api_keys SET expires_at = now() - interval '1 minute' WHERE telegram_user_id = 4242;`)
	require.NoError(t, err)

	b.Handle(ctx, chat("https://example.com after-lapse"))
	assert.Contains(t, msgs.last(t), "/buy")

	b.Handle(ctx, chat("/links"))
	assert.Contains(t, msgs.last(t), "/buy")

	_, err = NewLinkRepository(testPool).GetByCode(ctx, "after-lapse")
	assert.ErrorIs(t, err, domain.ErrNotFound, "nothing was created")
}

// TestBotFlow_BuyADomainThenShortenOnIt walks the whole add-on the way a buyer
// does: pay for access, pay for a domain, point DNS, register it, and see the
// next link come back on their own hostname.
func TestBotFlow_BuyADomainThenShortenOnIt(t *testing.T) {
	store := setupBilling(t)
	ctx := context.Background()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	codes, err := shortid.NewGenerator(7)
	require.NoError(t, err)
	links := service.NewLinkService(
		NewLinkRepository(testPool), noopCache{}, noopClicks{}, codes, noopHasher{},
		service.LinkServiceConfig{CacheTTL: time.Hour, NegativeCacheTTL: 30 * time.Second}, discard,
	)
	notifier := &stubNotifier{}
	svc := billing.New(store, &stubProvider{}, notifier,
		stubResolver{addrs: map[string][]string{"links.example.com": {testServerIP}}},
		[]string{testServerIP},
		billing.Config{
			PriceUSD: "10", DomainPriceUSD: "5", AccessDays: 5, MaxKeys: 5,
			AcceptedAssets: []string{"USDT"}, InvoiceTTL: time.Hour,
		}, discard)

	msgs := &captureMessenger{}
	b := bot.New(msgs, nil, svc, links, "https://sho.rt", time.UTC, discard)

	// Access first, so there is a key to shorten with.
	b.Handle(ctx, chat("/buy"))
	require.NoError(t, svc.Confirm(ctx, 4242))
	b.Handle(ctx, chat("/newkey"))

	// Without a slot, registering a domain is refused and points at the till.
	b.Handle(ctx, chat("/adddomain links.example.com"))
	assert.Contains(t, msgs.last(t), "/buydomain")

	// Buy the slot. The stub provider reuses invoice 4242, so the pending
	// access payment must not be what gets confirmed here -- the products are
	// separate rows.
	b.Handle(ctx, chat("/buydomain"))
	assert.Contains(t, msgs.last(t), "$5")

	sub, err := store.Subscription(ctx, 4242)
	require.NoError(t, err)
	require.Equal(t, 0, sub.DomainSlots, "nothing is granted until the invoice is paid")

	// Settle the domain invoice directly: the stub provider cannot mint a
	// second id, so take the pending domain payment as it was recorded.
	pending, err := store.RecentPendingPayment(ctx, 4242, domain.ProductDomain, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.NoError(t, svc.Confirm(ctx, pending.InvoiceID))
	assert.Equal(t, 1, notifier.slots)

	// A domain that does not point here is refused.
	b.Handle(ctx, chat("/adddomain elsewhere.example.com"))
	assert.Contains(t, msgs.last(t), "does not point at this server")

	// The real one is accepted and becomes the active domain.
	b.Handle(ctx, chat("/adddomain links.example.com"))
	assert.Contains(t, msgs.last(t), "links.example.com")

	b.Handle(ctx, chat("/domains"))
	assert.Contains(t, msgs.last(t), "links.example.com")

	// And the next link is rendered on it, not on the operator's domain.
	b.Handle(ctx, chat("https://go.dev/blog/context my-promo"))
	assert.Contains(t, msgs.last(t), "https://links.example.com/my-promo")
	assert.NotContains(t, msgs.last(t), "https://sho.rt/my-promo")

	// The stamp is durable: it is stored on the row, not derived at render time.
	stored, err := NewLinkRepository(testPool).GetByCode(ctx, "my-promo")
	require.NoError(t, err)
	require.NotNil(t, stored.DomainID)
	assert.Equal(t, "links.example.com", stored.Host)
}
