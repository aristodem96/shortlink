package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aristodem96/shortlink/internal/billing"
	"github.com/aristodem96/shortlink/internal/domain"
	"github.com/aristodem96/shortlink/internal/service"
	"github.com/aristodem96/shortlink/internal/telegram"
)

type sent struct {
	chatID  int64
	text    string
	buttons []telegram.Button
}

type fakeMessenger struct {
	mu   sync.Mutex
	msgs []sent
}

func (m *fakeMessenger) SendMessage(_ context.Context, chatID int64, text string, buttons ...telegram.Button) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.msgs = append(m.msgs, sent{chatID, text, buttons})
	return nil
}

func (m *fakeMessenger) last(t *testing.T) sent {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.NotEmpty(t, m.msgs, "the bot sent nothing")
	return m.msgs[len(m.msgs)-1]
}

var liveUntil = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func liveKey() *domain.APIKey {
	return &domain.APIKey{ID: 3, Name: "ci", ExpiresAt: &liveUntil}
}

type fakeBilling struct {
	sub       *domain.Subscription
	subErr    error
	issueErr  error
	issuedAs  string
	activated bool
	keys      []domain.APIKey
	active    *domain.APIKey
	activeErr error
	usable    *domain.APIKey
	usableErr error
	useKeyErr error
	usedRef   string
	revokeErr error
	revoked   int64
	checkout  string
}

func (f *fakeBilling) Terms() billing.Config {
	return billing.Config{PriceUSD: "10", AccessDays: 5, MaxKeys: 5}
}

func (f *fakeBilling) Checkout(context.Context, int64) (string, error) { return f.checkout, nil }

func (f *fakeBilling) Status(context.Context, int64) (*domain.Subscription, error) {
	return f.sub, f.subErr
}

func (f *fakeBilling) IssueKey(_ context.Context, _ int64, name string) (*billing.IssuedKey, error) {
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	f.issuedAs = name
	return &billing.IssuedKey{
		Secret:    "sl_SECRET",
		Key:       &domain.APIKey{ID: 3, Name: name, ExpiresAt: &liveUntil},
		Activated: f.activated,
	}, nil
}

func (f *fakeBilling) Keys(context.Context, int64) ([]domain.APIKey, error) { return f.keys, nil }

func (f *fakeBilling) ActiveKey(context.Context, int64) (*domain.APIKey, error) {
	if f.active == nil && f.activeErr == nil {
		return nil, domain.ErrNotFound
	}
	return f.active, f.activeErr
}

func (f *fakeBilling) UsableKey(context.Context, int64) (*domain.APIKey, error) {
	if f.usable == nil && f.usableErr == nil {
		return nil, domain.ErrNoActiveKey
	}
	return f.usable, f.usableErr
}

func (f *fakeBilling) UseKey(_ context.Context, _ int64, ref string) (*domain.APIKey, error) {
	f.usedRef = ref
	if f.useKeyErr != nil {
		return nil, f.useKeyErr
	}
	return &domain.APIKey{ID: 9, Name: "chosen"}, nil
}

func (f *fakeBilling) RevokeKey(_ context.Context, _, id int64) error {
	f.revoked = id
	return f.revokeErr
}

type fakeLinks struct {
	created   service.CreateLinkInput
	createErr error
	list      []domain.Link
	total     int64
	totals    map[int64]int64
	stats     *domain.LinkStats
	statsErr  error
}

func (f *fakeLinks) Create(_ context.Context, in service.CreateLinkInput) (*domain.Link, error) {
	f.created = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	code := in.Alias
	if code == "" {
		code = "gen1234"
	}
	return &domain.Link{ID: 1, Code: code, TargetURL: in.TargetURL, OwnerID: in.OwnerID}, nil
}

func (f *fakeLinks) List(context.Context, int64, int, int) ([]domain.Link, int64, error) {
	return f.list, f.total, nil
}

func (f *fakeLinks) Stats(context.Context, int64, string, time.Duration) (*domain.LinkStats, error) {
	return f.stats, f.statsErr
}

func (f *fakeLinks) ClickTotals(context.Context, int64, []int64) (map[int64]int64, error) {
	return f.totals, nil
}

func newBot(t *testing.T, fb *fakeBilling, fl *fakeLinks) (*Bot, *fakeMessenger) {
	t.Helper()
	if fl == nil {
		fl = &fakeLinks{}
	}
	m := &fakeMessenger{}
	b := New(m, nil, fb, fl, "https://sho.rt", time.UTC, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return b, m
}

func private(text string) telegram.Update {
	return telegram.Update{UpdateID: 1, Message: &telegram.Message{
		From: &telegram.User{ID: 42}, Chat: telegram.Chat{ID: 42, Type: telegram.ChatPrivate}, Text: text,
	}}
}

// --- access control -----------------------------------------------------------

// TestHandle_RefusesToWorkInGroups is the reason keys stay secret: /newkey in
// a group would post one to every member.
func TestHandle_RefusesToWorkInGroups(t *testing.T) {
	t.Parallel()
	fb := &fakeBilling{}
	b, m := newBot(t, fb, nil)

	b.Handle(context.Background(), telegram.Update{Message: &telegram.Message{
		From: &telegram.User{ID: 42}, Chat: telegram.Chat{ID: -100, Type: "group"}, Text: "/newkey",
	}})

	msg := m.last(t)
	assert.Equal(t, int64(-100), msg.chatID)
	assert.Equal(t, msgPrivateOnly, msg.text)
	assert.Empty(t, fb.issuedAs, "no key may be issued outside a private chat")
}

// TestShorten_NeedsALiveKey: the bot creates links in-process, bypassing the
// API's auth middleware, so it has to refuse the same cases the middleware
// would -- otherwise it becomes a way to use the service without paying.
func TestShorten_NeedsALiveKey(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		billing *fakeBilling
		want    string
	}{
		"no key chosen":  {&fakeBilling{usableErr: domain.ErrNoActiveKey}, msgNoKey},
		"access expired": {&fakeBilling{usableErr: domain.ErrNoAccess}, msgNoAccess},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fl := &fakeLinks{}
			b, m := newBot(t, tc.billing, fl)

			b.Handle(context.Background(), private("https://go.dev"))

			assert.Equal(t, tc.want, m.last(t).text)
			assert.Empty(t, fl.created.TargetURL, "no link may be created")
		})
	}
}

// --- shortening ---------------------------------------------------------------

func TestShorten_APlainURLIsEnough(t *testing.T) {
	t.Parallel()
	fl := &fakeLinks{}
	b, m := newBot(t, &fakeBilling{usable: liveKey()}, fl)

	b.Handle(context.Background(), private("https://go.dev/blog/context"))

	assert.Equal(t, "https://go.dev/blog/context", fl.created.TargetURL)
	assert.Equal(t, int64(3), fl.created.OwnerID, "the link belongs to the bot's key")
	assert.Empty(t, fl.created.Alias)

	msg := m.last(t)
	assert.Contains(t, msg.text, "https://sho.rt/gen1234")
	require.Len(t, msg.buttons, 1)
	assert.Equal(t, "https://sho.rt/gen1234", msg.buttons[0].URL)
}

func TestShorten_AcceptsAnAliasAfterTheURL(t *testing.T) {
	t.Parallel()
	fl := &fakeLinks{}
	b, m := newBot(t, &fakeBilling{usable: liveKey()}, fl)

	b.Handle(context.Background(), private("https://go.dev my-alias"))

	assert.Equal(t, "my-alias", fl.created.Alias)
	assert.Contains(t, m.last(t).text, "https://sho.rt/my-alias")
}

func TestShorten_WorksAsACommandToo(t *testing.T) {
	t.Parallel()
	fl := &fakeLinks{}
	b, _ := newBot(t, &fakeBilling{usable: liveKey()}, fl)

	b.Handle(context.Background(), private("/short https://go.dev alias-x"))

	assert.Equal(t, "https://go.dev", fl.created.TargetURL)
	assert.Equal(t, "alias-x", fl.created.Alias)
}

func TestShorten_ExplainsARejection(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  error
		want string
	}{
		// The domain refuses private addresses; the reason is worth repeating
		// to the user rather than hiding behind a generic failure.
		"private target": {&domain.ValidationError{Field: "target_url", Reason: "must not point at a private or loopback address"}, "private or loopback"},
		"alias taken":    {domain.ErrAlreadyExists, msgAliasTaken},
		"internal":       {errors.New("connection refused"), msgInternalError},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, m := newBot(t, &fakeBilling{usable: liveKey()}, &fakeLinks{createErr: tc.err})

			b.Handle(context.Background(), private("https://10.0.0.1"))

			msg := m.last(t)
			assert.Contains(t, msg.text, tc.want)
			assert.NotContains(t, msg.text, "connection refused", "internal errors stay in the logs")
		})
	}
}

func TestShorten_EscapesTheTargetURL(t *testing.T) {
	t.Parallel()
	b, m := newBot(t, &fakeBilling{usable: liveKey()}, &fakeLinks{})

	b.Handle(context.Background(), private("https://example.com/?q=<b>x</b>"))

	msg := m.last(t).text
	assert.Contains(t, msg, "&lt;b&gt;")
	assert.NotContains(t, msg, "<b>x</b>", "an unbalanced tag would make Telegram reject the whole message")
}

func TestPlainTextThatIsNotALinkGetsHelp(t *testing.T) {
	t.Parallel()
	b, m := newBot(t, &fakeBilling{}, nil)

	b.Handle(context.Background(), private("hello there"))

	assert.Equal(t, tplHelp, m.last(t).text)
}

// --- links and clicks ---------------------------------------------------------

func TestLinks_ListsWithClickCounts(t *testing.T) {
	t.Parallel()
	fl := &fakeLinks{
		list: []domain.Link{
			{ID: 1, Code: "abc1234", TargetURL: "https://go.dev/blog/context"},
			{ID: 2, Code: "xyz9999", TargetURL: "https://example.com"},
		},
		total:  2,
		totals: map[int64]int64{1: 12},
	}
	b, m := newBot(t, &fakeBilling{usable: liveKey()}, fl)

	b.Handle(context.Background(), private("/links"))

	msg := m.last(t).text
	assert.Contains(t, msg, "<code>abc1234</code> · 12 clicks")
	assert.Contains(t, msg, "<code>xyz9999</code> · 0 clicks", "a link nobody clicked still shows")
	assert.Contains(t, msg, "https://go.dev/blog/context")
}

func TestLinks_SaysSoWhenThereAreNone(t *testing.T) {
	t.Parallel()
	b, m := newBot(t, &fakeBilling{usable: liveKey()}, &fakeLinks{})

	b.Handle(context.Background(), private("/links"))

	assert.Equal(t, msgNoLinks, m.last(t).text)
}

func TestStats_ShowsClicksVisitorsAndReferrers(t *testing.T) {
	t.Parallel()
	last := time.Date(2026, 9, 12, 9, 30, 0, 0, time.UTC)
	fl := &fakeLinks{stats: &domain.LinkStats{
		Code: "abc1234", TotalClicks: 12, UniqueVisitors: 7, LastClickAt: &last,
		Daily:       []domain.DailyCount{{Day: "2026-09-11", Clicks: 3}, {Day: "2026-09-12", Clicks: 9}},
		TopReferers: []domain.RefererHit{{Referer: "https://news.example.com", Clicks: 5}, {Referer: "(direct)", Clicks: 7}},
	}}
	b, m := newBot(t, &fakeBilling{usable: liveKey()}, fl)

	b.Handle(context.Background(), private("/stats abc1234"))

	msg := m.last(t).text
	assert.Contains(t, msg, "Clicks: <b>12</b>")
	assert.Contains(t, msg, "Unique visitors: <b>7</b>")
	assert.Contains(t, msg, "12 Sep 2026, 09:30 UTC")
	assert.Contains(t, msg, "https://news.example.com — 5")
	assert.Contains(t, msg, "2026-09-12 — 9")
}

// TestStats_EscapesReferrers: a referrer is whatever a visitor put in the
// Referer header, so it is attacker-controlled text being shown to the owner.
func TestStats_EscapesReferrers(t *testing.T) {
	t.Parallel()
	fl := &fakeLinks{stats: &domain.LinkStats{
		Code: "abc1234", TopReferers: []domain.RefererHit{{Referer: "<b>evil</b>", Clicks: 1}},
	}}
	b, m := newBot(t, &fakeBilling{usable: liveKey()}, fl)

	b.Handle(context.Background(), private("/stats abc1234"))

	msg := m.last(t).text
	assert.Contains(t, msg, "&lt;b&gt;evil")
	assert.NotContains(t, msg, "<b>evil</b>")
}

func TestStats_AcceptsAWholeShortURL(t *testing.T) {
	t.Parallel()
	fl := &fakeLinks{stats: &domain.LinkStats{Code: "abc1234"}}
	b, m := newBot(t, &fakeBilling{usable: liveKey()}, fl)

	b.Handle(context.Background(), private("/stats https://sho.rt/abc1234"))

	assert.Contains(t, m.last(t).text, "abc1234", "users paste whichever they have to hand")
}

func TestStats_ReportsAnUnknownCode(t *testing.T) {
	t.Parallel()
	b, m := newBot(t, &fakeBilling{usable: liveKey()}, &fakeLinks{statsErr: domain.ErrNotFound})

	b.Handle(context.Background(), private("/stats nosuch1"))

	assert.Equal(t, msgLinkNotFound, m.last(t).text)
}

func TestStats_NeedsACode(t *testing.T) {
	t.Parallel()
	b, m := newBot(t, &fakeBilling{usable: liveKey()}, &fakeLinks{})

	b.Handle(context.Background(), private("/stats"))

	assert.Equal(t, msgStatsUsage, m.last(t).text)
}

// --- keys ---------------------------------------------------------------------

func TestNewKey_ShowsTheSecretOnceAndSaysItIsTheBotKey(t *testing.T) {
	t.Parallel()
	b, m := newBot(t, &fakeBilling{activated: true}, nil)

	b.Handle(context.Background(), private("/newkey ci"))

	msg := m.last(t)
	assert.Contains(t, msg.text, "<code>sl_SECRET</code>", "the secret is tap-to-copy")
	assert.Contains(t, msg.text, "17 Sep 2026, 12:00 UTC")
	assert.Contains(t, msg.text, msgKeyActivated)
	require.Len(t, msg.buttons, 1)
	assert.Equal(t, "https://sho.rt/docs", msg.buttons[0].URL)
}

func TestNewKey_ALaterKeyDoesNotHijackTheChoice(t *testing.T) {
	t.Parallel()
	b, m := newBot(t, &fakeBilling{activated: false, active: &domain.APIKey{ID: 3, Name: "first"}}, nil)

	b.Handle(context.Background(), private("/newkey ci"))

	assert.Contains(t, m.last(t).text, "keeps using key <code>3</code>")
}

func TestNewKey_EscapesTheName(t *testing.T) {
	t.Parallel()
	b, m := newBot(t, &fakeBilling{}, nil)

	b.Handle(context.Background(), private("/newkey <b>evil"))

	msg := m.last(t)
	assert.Contains(t, msg.text, "&lt;b&gt;evil")
	assert.NotContains(t, msg.text, "<b>evil")
}

func TestNewKey_ExplainsWhyItRefused(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err  error
		want string
	}{
		"no paid access": {domain.ErrNoAccess, "/buy"},
		"at the limit":   {domain.ErrKeyLimit, "/revoke"},
		"bad name":       {&domain.ValidationError{Field: "name", Reason: "too long"}, "too long"},
		"internal error": {errors.New("connection refused"), msgInternalError},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, m := newBot(t, &fakeBilling{issueErr: tc.err}, nil)

			b.Handle(context.Background(), private("/newkey"))

			msg := m.last(t)
			assert.Contains(t, msg.text, tc.want)
			assert.NotContains(t, msg.text, "connection refused")
		})
	}
}

func TestKeys_MarksTheBotKeyAndHidesSecrets(t *testing.T) {
	t.Parallel()
	b, m := newBot(t, &fakeBilling{
		keys: []domain.APIKey{
			{ID: 7, Name: "ci", ExpiresAt: &liveUntil},
			{ID: 9, Name: "<prod>", ExpiresAt: &liveUntil},
		},
		active: &domain.APIKey{ID: 9},
	}, nil)

	b.Handle(context.Background(), private("/keys"))

	msg := m.last(t).text
	assert.Contains(t, msg, "<code>7</code> · ci")
	assert.Contains(t, msg, "&lt;prod&gt;", "names are escaped in the list too")
	assert.Contains(t, msg, keyBotMarker)
	assert.NotContains(t, msg, "sl_", "only hashes are stored; no secret can appear here")
}

func TestUseKey_AcceptsANumberOrAPastedKey(t *testing.T) {
	t.Parallel()

	t.Run("by number", func(t *testing.T) {
		t.Parallel()
		fb := &fakeBilling{}
		b, m := newBot(t, fb, nil)
		b.Handle(context.Background(), private("/usekey 9"))
		assert.Equal(t, "9", fb.usedRef)
		assert.Contains(t, m.last(t).text, "now uses key <code>9</code>")
	})

	// The literal reading of "set this key": paste the key itself.
	t.Run("by pasting the key as a plain message", func(t *testing.T) {
		t.Parallel()
		fb := &fakeBilling{}
		b, m := newBot(t, fb, nil)
		b.Handle(context.Background(), private("sl_pastedSecret"))
		assert.Equal(t, "sl_pastedSecret", fb.usedRef)
		assert.Contains(t, m.last(t).text, "now uses key")
	})

	t.Run("not one of theirs", func(t *testing.T) {
		t.Parallel()
		b, m := newBot(t, &fakeBilling{useKeyErr: domain.ErrNotFound}, nil)
		b.Handle(context.Background(), private("/usekey 9"))
		assert.Equal(t, msgKeyNotFound, m.last(t).text)
	})

	t.Run("no argument", func(t *testing.T) {
		t.Parallel()
		fb := &fakeBilling{}
		b, m := newBot(t, fb, nil)
		b.Handle(context.Background(), private("/usekey"))
		assert.Equal(t, msgUseKeyUsage, m.last(t).text)
		assert.Empty(t, fb.usedRef)
	})
}

func TestStatus_DistinguishesNeverPaidActiveAndExpired(t *testing.T) {
	t.Parallel()

	future := &domain.Subscription{PaidUntil: time.Now().Add(48 * time.Hour)}
	past := &domain.Subscription{PaidUntil: time.Now().Add(-time.Hour)}

	tests := map[string]struct {
		billing *fakeBilling
		want    string
	}{
		"never paid": {&fakeBilling{subErr: domain.ErrNotFound}, "/buy"},
		"active":     {&fakeBilling{sub: future, keys: []domain.APIKey{{ID: 1}}, active: &domain.APIKey{ID: 1, Name: "ci"}}, "Keys: 1 of 5"},
		"no bot key": {&fakeBilling{sub: future}, msgBotKeyNone},
		"expired":    {&fakeBilling{sub: past}, "access ended"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, m := newBot(t, tc.billing, nil)
			b.Handle(context.Background(), private("/status"))
			assert.Contains(t, m.last(t).text, tc.want)
		})
	}
}

func TestBuy_SendsAPayButton(t *testing.T) {
	t.Parallel()
	b, m := newBot(t, &fakeBilling{checkout: "https://t.me/CryptoBot?start=IV1"}, nil)

	b.Handle(context.Background(), private("/buy"))

	msg := m.last(t)
	assert.Contains(t, msg.text, "$10")
	require.Len(t, msg.buttons, 1)
	assert.Equal(t, "https://t.me/CryptoBot?start=IV1", msg.buttons[0].URL)
}

func TestRevoke_ParsesTheIDAndReportsTheOutcome(t *testing.T) {
	t.Parallel()

	t.Run("revokes", func(t *testing.T) {
		t.Parallel()
		fb := &fakeBilling{}
		b, m := newBot(t, fb, nil)
		b.Handle(context.Background(), private("/revoke 12"))
		assert.Equal(t, int64(12), fb.revoked)
		assert.Equal(t, msgKeyRevoked, m.last(t).text)
	})

	for _, bad := range []string{"/revoke", "/revoke abc", "/revoke -3", "/revoke 0"} {
		t.Run("usage for "+bad, func(t *testing.T) {
			t.Parallel()
			fb := &fakeBilling{}
			b, m := newBot(t, fb, nil)
			b.Handle(context.Background(), private(bad))
			assert.Equal(t, msgRevokeUsage, m.last(t).text)
			assert.Zero(t, fb.revoked)
		})
	}
}

func TestParseCommand(t *testing.T) {
	t.Parallel()

	tests := map[string]struct{ in, cmd, args string }{
		"plain":                     {"/buy", "/buy", ""},
		"with args":                 {"/newkey my app", "/newkey", "my app"},
		"addressed in a group menu": {"/newkey@ShortlinkBot ci", "/newkey", "ci"},
		"mixed case":                {"/NewKey", "/newkey", ""},
		"not a command":             {"hello", "", ""},
		"surrounding space":         {"  /keys  ", "/keys", ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cmd, args := parseCommand(tc.in)
			assert.Equal(t, tc.cmd, cmd)
			assert.Equal(t, tc.args, args)
		})
	}
}

func TestLinkCode(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "abc1234", linkCode("abc1234"))
	assert.Equal(t, "abc1234", linkCode("https://sho.rt/abc1234"))
	assert.Equal(t, "abc1234", linkCode("  sho.rt/abc1234 "))
	assert.Empty(t, linkCode(""))
}

func TestHandle_SurvivesAPanickingHandler(t *testing.T) {
	t.Parallel()
	b, _ := newBot(t, nil, nil) // nil billing: any command that touches it panics

	assert.NotPanics(t, func() { b.handleSafely(context.Background(), private("/buy")) },
		"one bad update must not take the bot down for everyone")
}

func TestNotifier_TellsTheBuyerWhenAccessEnds(t *testing.T) {
	t.Parallel()
	m := &fakeMessenger{}
	until := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

	require.NoError(t, NewNotifier(m, time.UTC).PaymentConfirmed(context.Background(), 42, until))

	msg := m.last(t)
	assert.Equal(t, int64(42), msg.chatID, "in a private chat the user id is the chat id")
	assert.True(t, strings.Contains(msg.text, "17 Sep 2026, 12:00 UTC"))
}
