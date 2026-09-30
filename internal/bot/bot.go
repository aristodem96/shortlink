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
	"github.com/aristodem96/shortlink/internal/service"
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
	Checkout(ctx context.Context, telegramUserID int64, product domain.Product) (string, error)
	Status(ctx context.Context, telegramUserID int64) (*domain.Subscription, error)
	IssueKey(ctx context.Context, telegramUserID int64, name string) (*billing.IssuedKey, error)
	Keys(ctx context.Context, telegramUserID int64) ([]domain.APIKey, error)
	RevokeKey(ctx context.Context, telegramUserID, keyID int64) error
	ActiveKey(ctx context.Context, telegramUserID int64) (*domain.APIKey, error)
	UsableKey(ctx context.Context, telegramUserID int64) (*domain.APIKey, error)
	UseKey(ctx context.Context, telegramUserID int64, ref string) (*domain.APIKey, error)
	AddDomain(ctx context.Context, telegramUserID int64, hostname string) (*domain.CustomDomain, error)
	Domains(ctx context.Context, telegramUserID int64) ([]domain.CustomDomain, error)
	RemoveDomain(ctx context.Context, telegramUserID int64, hostname string) error
	UseDomain(ctx context.Context, telegramUserID int64, hostname string) (*domain.CustomDomain, error)
	UseDefaultDomain(ctx context.Context, telegramUserID int64) error
	ActiveDomain(ctx context.Context, telegramUserID int64) (*domain.CustomDomain, error)
}

// Links is what the bot needs from the link service. It is the same
// service.LinkService the API uses, so a link made in the chat goes through
// exactly the same validation -- the SSRF check, the reserved aliases, the
// collision retry -- as one made over HTTP.
type Links interface {
	Create(ctx context.Context, in service.CreateLinkInput) (*domain.Link, error)
	List(ctx context.Context, ownerID int64, limit, offset int) ([]domain.Link, int64, error)
	Stats(ctx context.Context, ownerID int64, code string, window time.Duration) (*domain.LinkStats, error)
	ClickTotals(ctx context.Context, ownerID int64, linkIDs []int64) (map[int64]int64, error)
}

// Commands is the menu registered with Telegram.
var Commands = []telegram.Command{
	{Command: "links", Description: "Your links with their clicks"},
	{Command: "stats", Description: "Detailed clicks for one link"},
	{Command: "buy", Description: "Pay for API access"},
	{Command: "newkey", Description: "Create an API key"},
	{Command: "keys", Description: "Your keys"},
	{Command: "usekey", Description: "Choose the key the bot uses for links"},
	{Command: "status", Description: "How much access is left"},
	{Command: "revoke", Description: "Revoke a key"},
	{Command: "buydomain", Description: "Pay for a custom domain"},
	{Command: "domains", Description: "Your custom domains"},
	{Command: "adddomain", Description: "Register a domain you own"},
	{Command: "help", Description: "All commands"},
}

// Bot handles chat commands.
type Bot struct {
	msg        Messenger
	updates    UpdateSource
	billing    Billing
	links      Links
	apiBaseURL string
	loc        *time.Location
	log        *slog.Logger
	now        func() time.Time
}

// New wires a Bot. loc is the time zone dates are shown in.
func New(msg Messenger, updates UpdateSource, b Billing, links Links, apiBaseURL string, loc *time.Location, log *slog.Logger) *Bot {
	return &Bot{msg: msg, updates: updates, billing: b, links: links, apiBaseURL: apiBaseURL, loc: loc, log: log, now: time.Now}
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

	user := m.From.ID
	command, args := parseCommand(m.Text)

	// A plain message is the common case: a link to shorten, or a key being
	// pasted in to choose it. Anything else gets the help text.
	if command == "" {
		text := strings.TrimSpace(m.Text)
		switch {
		case looksLikeURL(text):
			b.log.InfoContext(ctx, "shorten", slog.Int64("telegram_user_id", user))
			b.shorten(ctx, user, text)
		case strings.HasPrefix(text, service.KeyPrefix):
			b.log.InfoContext(ctx, "use key by secret", slog.Int64("telegram_user_id", user))
			b.useKey(ctx, user, text)
		default:
			b.reply(ctx, user, tplHelp)
		}
		return
	}

	// The command is logged, its arguments are not: /usekey can carry a secret.
	b.log.InfoContext(ctx, "command", slog.String("command", command), slog.Int64("telegram_user_id", user))

	switch command {
	case "/start":
		t := b.billing.Terms()
		b.reply(ctx, user, fmt.Sprintf(tplStart, t.PriceUSD, t.AccessDays, t.MaxKeys))
	case "/help":
		b.reply(ctx, user, tplHelp)
	case "/buy":
		b.buy(ctx, user)
	case "/status":
		b.status(ctx, user)
	case "/newkey":
		b.newKey(ctx, user, args)
	case "/keys":
		b.keys(ctx, user)
	case "/usekey":
		b.useKey(ctx, user, args)
	case "/revoke":
		b.revoke(ctx, user, args)
	case "/short":
		b.shorten(ctx, user, args)
	case "/links":
		b.listLinks(ctx, user)
	case "/stats":
		b.stats(ctx, user, args)
	case "/buydomain":
		b.buyDomain(ctx, user)
	case "/domains":
		b.domains(ctx, user)
	case "/adddomain":
		b.addDomain(ctx, user, args)
	case "/usedomain":
		b.useDomain(ctx, user, args)
	case "/deldomain":
		b.delDomain(ctx, user, args)
	default:
		b.reply(ctx, user, msgUnknownCommand)
	}
}

func (b *Bot) buy(ctx context.Context, user int64) {
	url, err := b.billing.Checkout(ctx, user, domain.ProductAccess)
	if err != nil {
		b.fail(ctx, user, "checkout", err)
		return
	}
	t := b.billing.Terms()
	b.reply(ctx, user, fmt.Sprintf(tplCheckout, t.PriceUSD, t.AccessDays), telegram.Button{Text: msgPayButton, URL: url})
}

func (b *Bot) buyDomain(ctx context.Context, user int64) {
	url, err := b.billing.Checkout(ctx, user, domain.ProductDomain)
	if err != nil {
		b.fail(ctx, user, "checkout domain", err)
		return
	}
	t := b.billing.Terms()
	b.reply(ctx, user, fmt.Sprintf(tplBuyDomain, t.DomainPriceUSD),
		telegram.Button{Text: msgPayButton, URL: url})
}

func (b *Bot) addDomain(ctx context.Context, user int64, args string) {
	host := strings.TrimSpace(args)
	if host == "" {
		b.reply(ctx, user, msgAddDomainUsage)
		return
	}

	d, err := b.billing.AddDomain(ctx, user, host)
	var validation *domain.ValidationError
	switch {
	case errors.Is(err, domain.ErrNoDomainSlot):
		b.reply(ctx, user, fmt.Sprintf(tplNoDomainSlot, b.billing.Terms().DomainPriceUSD))
		return
	case errors.Is(err, domain.ErrDomainTaken):
		b.reply(ctx, user, msgDomainTaken)
		return
	case errors.Is(err, domain.ErrDomainNotPointed):
		b.reply(ctx, user, fmt.Sprintf(tplDomainNotPointed, html.EscapeString(host)))
		return
	case errors.As(err, &validation):
		b.reply(ctx, user, fmt.Sprintf(tplBadDomain, html.EscapeString(validation.Reason)))
		return
	case err != nil:
		b.fail(ctx, user, "add domain", err)
		return
	}
	b.reply(ctx, user, fmt.Sprintf(tplDomainAdded, html.EscapeString(d.Hostname)))
}

func (b *Bot) domains(ctx context.Context, user int64) {
	list, err := b.billing.Domains(ctx, user)
	if err != nil {
		b.fail(ctx, user, "list domains", err)
		return
	}
	if len(list) == 0 {
		b.reply(ctx, user, fmt.Sprintf(tplNoDomains, b.billing.Terms().DomainPriceUSD))
		return
	}

	// A missing active domain is not an error worth showing: the list is still
	// useful, it just has nothing marked.
	var activeID int64
	if active, err := b.billing.ActiveDomain(ctx, user); err == nil {
		activeID = active.ID
	}

	var sb strings.Builder
	sb.WriteString(tplDomainsHeader)
	for _, d := range list {
		marker := ""
		if d.ID == activeID {
			marker = domainActiveMarker
		}
		fmt.Fprintf(&sb, tplDomainLine, html.EscapeString(d.Hostname), marker)
	}
	sb.WriteString(tplDomainsFooter)
	b.reply(ctx, user, sb.String())
}

func (b *Bot) useDomain(ctx context.Context, user int64, args string) {
	host := strings.TrimSpace(args)
	if host == "" {
		b.reply(ctx, user, msgUseDomainUsage)
		return
	}

	// "default" can never collide with a real hostname: NormalizeHostname
	// requires at least one dot, so no registered domain can be named this.
	if strings.EqualFold(host, defaultDomainKeyword) {
		if err := b.billing.UseDefaultDomain(ctx, user); err != nil {
			b.fail(ctx, user, "use default domain", err)
			return
		}
		b.reply(ctx, user, fmt.Sprintf(tplDefaultDomainInUse, html.EscapeString(b.apiBaseURL)))
		return
	}

	d, err := b.billing.UseDomain(ctx, user, host)
	var validation *domain.ValidationError
	switch {
	case errors.Is(err, domain.ErrNotFound):
		b.reply(ctx, user, msgDomainUnknown)
		return
	case errors.As(err, &validation):
		b.reply(ctx, user, fmt.Sprintf(tplBadDomain, html.EscapeString(validation.Reason)))
		return
	case err != nil:
		b.fail(ctx, user, "use domain", err)
		return
	}
	b.reply(ctx, user, fmt.Sprintf(tplDomainInUse, html.EscapeString(d.Hostname)))
}

func (b *Bot) delDomain(ctx context.Context, user int64, args string) {
	host := strings.TrimSpace(args)
	if host == "" {
		b.reply(ctx, user, msgDelDomainUsage)
		return
	}

	err := b.billing.RemoveDomain(ctx, user, host)
	var validation *domain.ValidationError
	switch {
	case errors.Is(err, domain.ErrNotFound):
		b.reply(ctx, user, msgDomainUnknown)
		return
	case errors.As(err, &validation):
		b.reply(ctx, user, fmt.Sprintf(tplBadDomain, html.EscapeString(validation.Reason)))
		return
	case err != nil:
		b.fail(ctx, user, "remove domain", err)
		return
	}
	b.reply(ctx, user, fmt.Sprintf(tplDomainRemoved, html.EscapeString(host)))
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

	botKey := msgBotKeyNone
	if active, err := b.billing.ActiveKey(ctx, user); err == nil {
		botKey = fmt.Sprintf(tplBotKeyLine, active.ID, html.EscapeString(active.Name))
	}

	b.reply(ctx, user, fmt.Sprintf(tplStatusActive, b.date(sub.PaidUntil), len(keys), b.billing.Terms().MaxKeys, botKey))
}

func (b *Bot) newKey(ctx context.Context, user int64, name string) {
	issued, err := b.billing.IssueKey(ctx, user, name)

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
	if issued.Key.ExpiresAt != nil {
		expires = b.date(*issued.Key.ExpiresAt)
	}

	note := msgKeyActivated
	if !issued.Activated {
		note = ""
		if active, err := b.billing.ActiveKey(ctx, user); err == nil {
			note = fmt.Sprintf(tplKeyNotActivated, active.ID)
		}
	}

	b.reply(ctx, user,
		fmt.Sprintf(tplKeyIssued, html.EscapeString(issued.Key.Name), issued.Secret, expires,
			html.EscapeString(b.apiBaseURL), note),
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

	var activeID int64
	if active, err := b.billing.ActiveKey(ctx, user); err == nil {
		activeID = active.ID
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, tplKeysHeader, len(keys), b.billing.Terms().MaxKeys)
	for _, k := range keys {
		expires := "—"
		if k.ExpiresAt != nil {
			expires = b.date(*k.ExpiresAt)
		}
		marker := ""
		if k.ID == activeID {
			marker = keyBotMarker
		}
		fmt.Fprintf(&sb, tplKeyLine, k.ID, html.EscapeString(k.Name), expires, marker)
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

func (b *Bot) useKey(ctx context.Context, user int64, ref string) {
	if strings.TrimSpace(ref) == "" {
		b.reply(ctx, user, msgUseKeyUsage)
		return
	}

	key, err := b.billing.UseKey(ctx, user, ref)

	var validation *domain.ValidationError
	switch {
	case errors.As(err, &validation):
		b.reply(ctx, user, msgUseKeyUsage)
	case errors.Is(err, domain.ErrNotFound):
		b.reply(ctx, user, msgKeyNotFound)
	case err != nil:
		b.fail(ctx, user, "use key", err)
	default:
		b.reply(ctx, user, fmt.Sprintf(tplKeySelected, key.ID, html.EscapeString(key.Name)))
	}
}

// usableKey resolves the key the bot acts with, or tells the user why there
// isn't one and returns nil.
func (b *Bot) usableKey(ctx context.Context, user int64) *domain.APIKey {
	key, err := b.billing.UsableKey(ctx, user)
	switch {
	case errors.Is(err, domain.ErrNoActiveKey):
		b.reply(ctx, user, msgNoKey)
		return nil
	case errors.Is(err, domain.ErrNoAccess):
		b.reply(ctx, user, msgNoAccess)
		return nil
	case err != nil:
		b.fail(ctx, user, "resolve key", err)
		return nil
	}
	return key
}

// shorten turns "URL [alias]" into a short link owned by the bot's key.
func (b *Bot) shorten(ctx context.Context, user int64, args string) {
	target, alias, _ := strings.Cut(strings.TrimSpace(args), " ")
	if target == "" {
		b.reply(ctx, user, msgShortenUsage)
		return
	}

	key := b.usableKey(ctx, user)
	if key == nil {
		return
	}

	// A failure to look up the active domain is not worth refusing the link
	// over: falling back to the default host still produces a working link.
	var (
		domainID   *int64
		domainHost string
	)
	if active, err := b.billing.ActiveDomain(ctx, user); err == nil {
		domainID, domainHost = &active.ID, active.Hostname
	} else if !errors.Is(err, domain.ErrNotFound) {
		b.log.WarnContext(ctx, "could not read the active domain",
			slog.Int64("telegram_user_id", user), slog.Any("error", err))
	}

	link, err := b.links.Create(ctx, service.CreateLinkInput{
		OwnerID:   key.ID,
		DomainID:  domainID,
		Host:      domainHost,
		TargetURL: target,
		Alias:     strings.TrimSpace(alias),
	})

	var validation *domain.ValidationError
	switch {
	case errors.As(err, &validation):
		b.reply(ctx, user, fmt.Sprintf(tplCannotShorten, html.EscapeString(validation.Reason)))
		return
	case errors.Is(err, domain.ErrAlreadyExists):
		b.reply(ctx, user, msgAliasTaken)
		return
	case err != nil:
		b.fail(ctx, user, "create link", err)
		return
	}

	short := b.shortURL(link)
	b.reply(ctx, user,
		fmt.Sprintf(tplShortened, html.EscapeString(short), html.EscapeString(link.TargetURL)),
		telegram.Button{Text: msgOpenButton, URL: short})
}

const linksPageSize = 10

func (b *Bot) listLinks(ctx context.Context, user int64) {
	key := b.usableKey(ctx, user)
	if key == nil {
		return
	}

	links, total, err := b.links.List(ctx, key.ID, linksPageSize, 0)
	if err != nil {
		b.fail(ctx, user, "list links", err)
		return
	}
	if len(links) == 0 {
		b.reply(ctx, user, msgNoLinks)
		return
	}

	ids := make([]int64, len(links))
	for i, l := range links {
		ids[i] = l.ID
	}
	// One query for the whole page, not one per link.
	totals, err := b.links.ClickTotals(ctx, key.ID, ids)
	if err != nil {
		b.fail(ctx, user, "click totals", err)
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, tplLinksHeader, len(links), total)
	for _, l := range links {
		fmt.Fprintf(&sb, tplLinkLine, html.EscapeString(l.Code), clicks(totals[l.ID]),
			html.EscapeString(ellipsize(l.TargetURL, 60)))
	}
	sb.WriteString(tplLinksFooter)

	b.reply(ctx, user, sb.String())
}

const statsWindow = 30 * 24 * time.Hour

func (b *Bot) stats(ctx context.Context, user int64, args string) {
	code := linkCode(args)
	if code == "" {
		b.reply(ctx, user, msgStatsUsage)
		return
	}

	key := b.usableKey(ctx, user)
	if key == nil {
		return
	}

	st, err := b.links.Stats(ctx, key.ID, code, statsWindow)
	if errors.Is(err, domain.ErrNotFound) {
		b.reply(ctx, user, msgLinkNotFound)
		return
	}
	if err != nil {
		b.fail(ctx, user, "link stats", err)
		return
	}

	last := msgNever
	if st.LastClickAt != nil {
		last = b.date(*st.LastClickAt)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, tplStats, html.EscapeString(st.Code), st.TotalClicks, st.UniqueVisitors, last)

	if len(st.TopReferers) > 0 {
		sb.WriteString(tplStatsReferrersHeader)
		for _, r := range st.TopReferers {
			// Referrers come from a request header any visitor controls.
			fmt.Fprintf(&sb, tplStatsLine, html.EscapeString(ellipsize(r.Referer, 50)), r.Clicks)
		}
	}

	if len(st.Daily) > 0 {
		sb.WriteString(tplStatsDaysHeader)
		days := st.Daily
		if len(days) > 7 {
			days = days[len(days)-7:] // the most recent week keeps the message short
		}
		for _, d := range days {
			fmt.Fprintf(&sb, tplStatsLine, d.Day, d.Clicks)
		}
	}

	b.reply(ctx, user, sb.String(),
		telegram.Button{Text: msgOpenButton, URL: b.shortURL(&domain.Link{Code: st.Code, Host: st.Host})})
}

// shortURL renders a link's public address. A link stamped with a custom domain
// is always shown on that domain, even after the owner switches to another one:
// the hostname was fixed when the link was created, and anything already shared
// still points there.
func (b *Bot) shortURL(l *domain.Link) string {
	if l.Host != "" {
		return "https://" + l.Host + "/" + l.Code
	}
	return b.apiBaseURL + "/" + l.Code
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

// dateLayout names the zone, so a date is never ambiguous to a reader in
// another one.
const dateLayout = "02 Jan 2006, 15:04 MST"

func (b *Bot) date(t time.Time) string {
	return t.In(b.loc).Format(dateLayout)
}

// looksLikeURL is deliberately loose. Real validation -- scheme, host, the
// private-network check -- happens in the link service, which then explains a
// rejection; this only decides that a plain message was meant as a link.
func looksLikeURL(text string) bool {
	lower := strings.ToLower(text)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// linkCode accepts a bare code or a whole short URL, since users will paste
// whichever they have to hand.
func linkCode(arg string) string {
	arg = strings.TrimSpace(arg)
	if i := strings.LastIndex(arg, "/"); i >= 0 {
		arg = arg[i+1:]
	}
	return arg
}

func clicks(n int64) string {
	if n == 1 {
		return "1 click"
	}
	return strconv.FormatInt(n, 10) + " clicks"
}

func ellipsize(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
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
	return n.msg.SendMessage(ctx, telegramUserID, fmt.Sprintf(tplPaid, paidUntil.In(n.loc).Format(dateLayout)))
}

// DomainSlotGranted tells the buyer a custom domain slot is theirs, and what to
// do next: the slot is useless until they point DNS at this server.
func (n *Notifier) DomainSlotGranted(ctx context.Context, telegramUserID int64, slots int) error {
	return n.msg.SendMessage(ctx, telegramUserID, fmt.Sprintf(tplDomainSlotGranted, slots))
}
