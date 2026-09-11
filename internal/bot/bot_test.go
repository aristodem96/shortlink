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

type fakeBilling struct {
	sub       *domain.Subscription
	subErr    error
	issueErr  error
	issuedAs  string
	keys      []domain.APIKey
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

func (f *fakeBilling) IssueKey(_ context.Context, _ int64, name string) (string, *domain.APIKey, error) {
	if f.issueErr != nil {
		return "", nil, f.issueErr
	}
	f.issuedAs = name
	exp := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	return "sl_SECRET", &domain.APIKey{ID: 3, Name: name, ExpiresAt: &exp}, nil
}

func (f *fakeBilling) Keys(context.Context, int64) ([]domain.APIKey, error) { return f.keys, nil }

func (f *fakeBilling) RevokeKey(_ context.Context, _, id int64) error {
	f.revoked = id
	return f.revokeErr
}

func newBot(t *testing.T, fb *fakeBilling) (*Bot, *fakeMessenger) {
	t.Helper()
	m := &fakeMessenger{}
	b := New(m, nil, fb, "https://sho.rt", time.UTC, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return b, m
}

func private(text string) telegram.Update {
	return telegram.Update{UpdateID: 1, Message: &telegram.Message{
		From: &telegram.User{ID: 42}, Chat: telegram.Chat{ID: 42, Type: telegram.ChatPrivate}, Text: text,
	}}
}

// TestHandle_RefusesToWorkInGroups is the reason keys stay secret: /newkey in
// a group would post the key to every member.
func TestHandle_RefusesToWorkInGroups(t *testing.T) {
	t.Parallel()
	fb := &fakeBilling{}
	b, m := newBot(t, fb)

	b.Handle(context.Background(), telegram.Update{Message: &telegram.Message{
		From: &telegram.User{ID: 42}, Chat: telegram.Chat{ID: -100, Type: "group"}, Text: "/newkey",
	}})

	msg := m.last(t)
	assert.Equal(t, int64(-100), msg.chatID)
	assert.Equal(t, msgPrivateOnly, msg.text)
	assert.Empty(t, fb.issuedAs, "no key may be issued outside a private chat")
}

func TestNewKey_ShowsTheSecretOnce(t *testing.T) {
	t.Parallel()
	b, m := newBot(t, &fakeBilling{})

	b.Handle(context.Background(), private("/newkey ci"))

	msg := m.last(t)
	assert.Contains(t, msg.text, "<code>sl_SECRET</code>", "the secret is tap-to-copy")
	assert.Contains(t, msg.text, "17.09.2026 12:00")
	require.Len(t, msg.buttons, 1)
	assert.Equal(t, "https://sho.rt/docs", msg.buttons[0].URL)
}

// TestNewKey_EscapesTheName guards against a user-supplied name being read as
// markup: "<b>" would be rendered, and an unbalanced tag would make Telegram
// reject the whole message -- including the secret the user just paid for.
func TestNewKey_EscapesTheName(t *testing.T) {
	t.Parallel()
	b, m := newBot(t, &fakeBilling{})

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
			b, m := newBot(t, &fakeBilling{issueErr: tc.err})

			b.Handle(context.Background(), private("/newkey"))

			msg := m.last(t)
			assert.Contains(t, msg.text, tc.want)
			assert.NotContains(t, msg.text, "connection refused", "internal errors stay in the logs")
		})
	}
}

func TestBuy_SendsAPayButton(t *testing.T) {
	t.Parallel()
	b, m := newBot(t, &fakeBilling{checkout: "https://t.me/CryptoBot?start=IV1"})

	b.Handle(context.Background(), private("/buy"))

	msg := m.last(t)
	assert.Contains(t, msg.text, "$10")
	require.Len(t, msg.buttons, 1)
	assert.Equal(t, "https://t.me/CryptoBot?start=IV1", msg.buttons[0].URL)
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
		"active":     {&fakeBilling{sub: future, keys: []domain.APIKey{{ID: 1}}}, "Ключів: 1 з 5"},
		"expired":    {&fakeBilling{sub: past}, "закінчився"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, m := newBot(t, tc.billing)
			b.Handle(context.Background(), private("/status"))
			assert.Contains(t, m.last(t).text, tc.want)
		})
	}
}

func TestKeys_ListsWithoutSecrets(t *testing.T) {
	t.Parallel()
	exp := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	b, m := newBot(t, &fakeBilling{keys: []domain.APIKey{
		{ID: 7, Name: "ci", ExpiresAt: &exp},
		{ID: 9, Name: "<prod>", ExpiresAt: &exp},
	}})

	b.Handle(context.Background(), private("/keys"))

	msg := m.last(t).text
	assert.Contains(t, msg, "<code>7</code> · ci")
	assert.Contains(t, msg, "&lt;prod&gt;", "names are escaped in the list too")
	assert.NotContains(t, msg, "sl_", "only hashes are stored; no secret can appear here")
}

func TestRevoke_ParsesTheIDAndReportsTheOutcome(t *testing.T) {
	t.Parallel()

	t.Run("revokes", func(t *testing.T) {
		t.Parallel()
		fb := &fakeBilling{}
		b, m := newBot(t, fb)
		b.Handle(context.Background(), private("/revoke 12"))
		assert.Equal(t, int64(12), fb.revoked)
		assert.Equal(t, msgKeyRevoked, m.last(t).text)
	})

	t.Run("not one of theirs", func(t *testing.T) {
		t.Parallel()
		b, m := newBot(t, &fakeBilling{revokeErr: domain.ErrNotFound})
		b.Handle(context.Background(), private("/revoke 12"))
		assert.Equal(t, msgKeyNotFound, m.last(t).text)
	})

	for _, bad := range []string{"/revoke", "/revoke abc", "/revoke -3", "/revoke 0"} {
		t.Run("usage for "+bad, func(t *testing.T) {
			t.Parallel()
			fb := &fakeBilling{}
			b, m := newBot(t, fb)
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

func TestHandle_SurvivesAPanickingHandler(t *testing.T) {
	t.Parallel()
	b, _ := newBot(t, nil) // nil billing: any command that touches it panics

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
	assert.True(t, strings.Contains(msg.text, "17.09.2026 12:00"))
}
