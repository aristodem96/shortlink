// Package bot is the Telegram front end of billing: a user buys API access and
// then mints their own keys, all inside a private chat.
package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/aristodem96/shortlink/internal/billing"
	"github.com/aristodem96/shortlink/internal/domain"
	"github.com/aristodem96/shortlink/internal/telegram"
)

// Messenger sends messages to a chat.
type Messenger interface {
	SendMessage(ctx context.Context, chatID int64, html string, buttons ...telegram.Button) error
}

// UpdateSource yields incoming events.
type UpdateSource interface {
	GetUpdates(ctx context.Context, offset int64) ([]telegram.Update, error)
}

// Billing is what the bot needs from the billing service.
type Billing interface {
	Terms() billing.Config
	Checkout(ctx context.Context, telegramUserID int64) (string, error)
	Status(ctx context.Context, telegramUserID int64) (*domain.Subscription, error)
	IssueKey(ctx context.Context, telegramUserID int64, name string) (string, *domain.APIKey, error)
	Keys(ctx context.Context, telegramUserID int64) ([]domain.APIKey, error)
	RevokeKey(ctx context.Context, telegramUserID, keyID int64) error
}

// Commands is the menu registered with Telegram.
var Commands = []telegram.Command{
	{Command: "buy", Description: "Оплатити доступ до API"},
	{Command: "newkey", Description: "Створити API-ключ"},
	{Command: "keys", Description: "Мої ключі"},
	{Command: "status", Description: "Скільки доступу лишилось"},
	{Command: "revoke", Description: "Відкликати ключ"},
	{Command: "help", Description: "Довідка"},
}

// Bot handles chat commands.
type Bot struct {
	msg        Messenger
	updates    UpdateSource
	billing    Billing
	apiBaseURL string
	loc        *time.Location
	log        *slog.Logger
	now        func() time.Time
}

// New wires a Bot. loc is the time zone dates are shown in.
func New(msg Messenger, updates UpdateSource, b Billing, apiBaseURL string, loc *time.Location, log *slog.Logger) *Bot {
	return &Bot{msg: msg, updates: updates, billing: b, apiBaseURL: apiBaseURL, loc: loc, log: log, now: time.Now}
}

// Run long-polls for updates until ctx is cancelled.
//
// Long polling rather than a webhook means the bot needs no public URL of its
// own, so it runs unchanged on a laptop and on a server.
func (b *Bot) Run(ctx context.Context) error {
	var (
		offset  int64
		backoff = time.Second
	)

	for ctx.Err() == nil {
		updates, err := b.updates.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				// Shutting down: the in-flight long poll was cancelled, which
				// is how it is supposed to end rather than a failure.
				break
			}

			// Telegram being unreachable is not fatal: wait and try again,
			// backing off so an outage is not met with a request storm.
			b.log.WarnContext(ctx, "could not fetch updates, retrying",
				slog.Duration("backoff", backoff), slog.Any("error", err))

			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second

		for _, u := range updates {
			// Advancing the offset is what acknowledges an update: Telegram
			// only forgets it once a later getUpdates asks for offset+1.
			offset = u.UpdateID + 1
			b.handleSafely(ctx, u)
		}
	}
	// Cancellation is how a long-running worker is told to stop, so reaching
	// here is a clean exit. Returning ctx.Err() instead would make every
	// graceful shutdown surface in errgroup as a failure.
	return nil //nolint:nilerr // the only error on this path is the expected cancellation
}

// handleSafely contains a panic to the one update that caused it. Without the
// recover, a single malformed message would take the bot down for everyone.
func (b *Bot) handleSafely(ctx context.Context, u telegram.Update) {
	defer func() {
		if rec := recover(); rec != nil {
			b.log.ErrorContext(ctx, "update handler panicked",
				slog.Int64("update_id", u.UpdateID),
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())))
		}
	}()
	b.Handle(ctx, u)
}

// Handle processes one update.
func (b *Bot) Handle(ctx context.Context, u telegram.Update) {
	m := u.Message
	if m == nil || m.From == nil {
		return
	}

	// Keys are secrets. In a group, /newkey would publish one to every member,
	// so everything except a pointer to a private chat is refused there.
	if m.Chat.Type != telegram.ChatPrivate {
		b.reply(ctx, m.Chat.ID, msgPrivateOnly)
		return
	}

	command, args := parseCommand(m.Text)
	user := m.From.ID

	b.log.InfoContext(ctx, "command", slog.String("command", command), slog.Int64("telegram_user_id", user))

	switch command {
	case "/start":
		t := b.billing.Terms()
		b.reply(ctx, user, fmt.Sprintf(tplStart, t.PriceUSD, t.AccessDays, t.MaxKeys, t.AccessDays))
	case "/help", "":
		b.reply(ctx, user, tplHelp)
	case "/buy":
		b.buy(ctx, user)
	case "/status":
		b.status(ctx, user)
	case "/newkey":
		b.newKey(ctx, user, args)
	case "/keys":
		b.keys(ctx, user)
	case "/revoke":
		b.revoke(ctx, user, args)
	default:
		b.reply(ctx, user, msgUnknownCommand)
	}
}

func (b *Bot) buy(ctx context.Context, user int64) {
	url, err := b.billing.Checkout(ctx, user)
	if err != nil {
		b.fail(ctx, user, "checkout", err)
		return
	}
	t := b.billing.Terms()
	b.reply(ctx, user, fmt.Sprintf(tplCheckout, t.PriceUSD, t.AccessDays), telegram.Button{Text: msgPayButton, URL: url})
}

func (b *Bot) status(ctx context.Context, user int64) {
	sub, err := b.billing.Status(ctx, user)
	if errors.Is(err, domain.ErrNotFound) {
		b.reply(ctx, user, msgNoAccess)
		return
	}
	if err != nil {
		b.fail(ctx, user, "status", err)
		return
	}

	if !sub.IsActive(b.now()) {
		b.reply(ctx, user, fmt.Sprintf(tplStatusExpired, b.date(sub.PaidUntil)))
		return
	}

	keys, err := b.billing.Keys(ctx, user)
	if err != nil {
		b.fail(ctx, user, "status keys", err)
		return
	}
	b.reply(ctx, user, fmt.Sprintf(tplStatusActive, b.date(sub.PaidUntil), len(keys), b.billing.Terms().MaxKeys))
}

func (b *Bot) newKey(ctx context.Context, user int64, name string) {
	secret, key, err := b.billing.IssueKey(ctx, user, name)

	var validation *domain.ValidationError
	switch {
	case errors.Is(err, domain.ErrNoAccess):
		b.reply(ctx, user, msgNoAccess)
		return
	case errors.Is(err, domain.ErrKeyLimit):
		b.reply(ctx, user, fmt.Sprintf(tplKeyLimit, b.billing.Terms().MaxKeys))
		return
	case errors.As(err, &validation):
		b.reply(ctx, user, fmt.Sprintf(tplBadName, html.EscapeString(validation.Reason)))
		return
	case err != nil:
		b.fail(ctx, user, "issue key", err)
		return
	}

	expires := ""
	if key.ExpiresAt != nil {
		expires = b.date(*key.ExpiresAt)
	}

	b.reply(ctx, user,
		fmt.Sprintf(tplKeyIssued, html.EscapeString(key.Name), secret, expires, html.EscapeString(b.apiBaseURL)),
		telegram.Button{Text: msgDocsButton, URL: b.apiBaseURL + "/docs"})
}

func (b *Bot) keys(ctx context.Context, user int64) {
	keys, err := b.billing.Keys(ctx, user)
	if err != nil {
		b.fail(ctx, user, "list keys", err)
		return
	}
	if len(keys) == 0 {
		b.reply(ctx, user, msgNoKeys)
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, tplKeysHeader, len(keys), b.billing.Terms().MaxKeys)
	for _, k := range keys {
		expires := "—"
		if k.ExpiresAt != nil {
			expires = b.date(*k.ExpiresAt)
		}
		fmt.Fprintf(&sb, tplKeyLine, k.ID, html.EscapeString(k.Name), expires)
	}
	sb.WriteString(tplKeysFooter)

	b.reply(ctx, user, sb.String())
}

func (b *Bot) revoke(ctx context.Context, user int64, args string) {
	id, err := strconv.ParseInt(strings.TrimSpace(args), 10, 64)
	if err != nil || id <= 0 {
		b.reply(ctx, user, msgRevokeUsage)
		return
	}

	switch err := b.billing.RevokeKey(ctx, user, id); {
	case errors.Is(err, domain.ErrNotFound):
		b.reply(ctx, user, msgKeyNotFound)
	case err != nil:
		b.fail(ctx, user, "revoke key", err)
	default:
		b.reply(ctx, user, msgKeyRevoked)
	}
}

// fail logs the real cause and shows the user a generic message: internal
// errors carry table names and provider responses that are no business of a
// chat.
func (b *Bot) fail(ctx context.Context, chatID int64, op string, err error) {
	b.log.ErrorContext(ctx, "command failed", slog.String("op", op), slog.Any("error", err))
	b.reply(ctx, chatID, msgInternalError)
}

func (b *Bot) reply(ctx context.Context, chatID int64, text string, buttons ...telegram.Button) {
	if err := b.msg.SendMessage(ctx, chatID, text, buttons...); err != nil {
		b.log.WarnContext(ctx, "could not send a message", slog.Int64("chat_id", chatID), slog.Any("error", err))
	}
}

func (b *Bot) date(t time.Time) string {
	return t.In(b.loc).Format("02.01.2006 15:04")
}

// parseCommand splits "/newkey@ShortlinkBot my app" into "/newkey" and
// "my app". The @botname suffix is what Telegram appends when a command is
// picked from the menu in a group, and it has to go before matching.
func parseCommand(text string) (command, args string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", ""
	}

	command, args, _ = strings.Cut(text, " ")
	command, _, _ = strings.Cut(command, "@")
	return strings.ToLower(command), strings.TrimSpace(args)
}

// Notifier tells a buyer their payment went through. It is separate from Bot
// so billing can depend on it without depending on the bot, which itself
// depends on billing.
type Notifier struct {
	msg Messenger
	loc *time.Location
}

// NewNotifier wires a Notifier.
func NewNotifier(msg Messenger, loc *time.Location) *Notifier {
	return &Notifier{msg: msg, loc: loc}
}

// PaymentConfirmed implements billing.Notifier. In a private chat the chat id
// is the user id, so the buyer can be messaged without any stored chat state.
func (n *Notifier) PaymentConfirmed(ctx context.Context, telegramUserID int64, paidUntil time.Time) error {
	return n.msg.SendMessage(ctx, telegramUserID, fmt.Sprintf(tplPaid, paidUntil.In(n.loc).Format("02.01.2006 15:04")))
}
