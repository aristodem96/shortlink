package postgres

import (
	"context"
	"io"
	"log/slog"
	"strings"
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

type stubProvider struct{}

func (stubProvider) CreateInvoice(context.Context, cryptopay.InvoiceRequest) (cryptopay.Invoice, error) {
	return cryptopay.Invoice{InvoiceID: 4242, Status: cryptopay.StatusActive, BotInvoiceURL: "https://t.me/CryptoBot?start=IV4242"}, nil
}

func (stubProvider) GetInvoices(context.Context, []int64) ([]cryptopay.Invoice, error) {
	return nil, nil
}

type stubNotifier struct{ paid int }

func (n *stubNotifier) PaymentConfirmed(context.Context, int64, time.Time) error {
	n.paid++
	return nil
}

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
	svc := billing.New(store, stubProvider{}, notifier, billing.Config{
		PriceUSD: "10", AccessDays: 5, MaxKeys: 5, AcceptedAssets: []string{"USDT"}, InvoiceTTL: time.Hour,
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
	svc := billing.New(store, stubProvider{}, &stubNotifier{}, billing.Config{
		PriceUSD: "10", AccessDays: 5, MaxKeys: 5, InvoiceTTL: time.Hour,
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
	svc := billing.New(store, stubProvider{}, &stubNotifier{}, billing.Config{
		PriceUSD: "10", AccessDays: 5, MaxKeys: 5, InvoiceTTL: time.Hour,
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
