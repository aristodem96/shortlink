// Package billing sells time-limited API access through Telegram: it issues
// invoices, confirms payments, and lets a paying user mint their own keys.
package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/aristodem96/shortlink/internal/cryptopay"
	"github.com/aristodem96/shortlink/internal/domain"
	"github.com/aristodem96/shortlink/internal/service"
)

// Store is the durable side of billing. Every method that changes more than
// one row does so in a single transaction.
type Store interface {
	// RecentPendingPayment returns the user's newest pending payment for one
	// product, created after the given instant, or domain.ErrNotFound. The
	// product is part of the lookup because the result is reused as the invoice
	// the buyer is sent: unscoped, somebody who asked for access and then a
	// domain would be handed the wrong invoice and charged the wrong amount.
	RecentPendingPayment(ctx context.Context, telegramUserID int64, product domain.Product, after time.Time) (*domain.Payment, error)
	CreatePayment(ctx context.Context, p domain.Payment) (*domain.Payment, error)
	// ConfirmPayment marks a pending payment paid and grants whatever it sold.
	// Confirmation.Confirmed is false when the payment was already confirmed or
	// does not exist -- which is what makes it safe to call from a retried
	// webhook and the reconciler at the same time.
	ConfirmPayment(ctx context.Context, invoiceID int64) (domain.Confirmation, error)
	ExpirePayment(ctx context.Context, invoiceID int64) error
	PendingPayments(ctx context.Context, createdAfter time.Time, limit int) ([]domain.Payment, error)
	Subscription(ctx context.Context, telegramUserID int64) (*domain.Subscription, error)
	// IssueKey stores a key for a user with active access, returning
	// domain.ErrNoAccess or domain.ErrKeyLimit otherwise. activated reports
	// whether the new key became the one the bot acts with.
	IssueKey(ctx context.Context, telegramUserID int64, name string, hash []byte, maxKeys int) (key *domain.APIKey, activated bool, err error)
	ListKeys(ctx context.Context, telegramUserID int64) ([]domain.APIKey, error)
	RevokeKey(ctx context.Context, telegramUserID, keyID int64) error
	// ActiveKey returns the key the bot acts with, whatever its state, or
	// domain.ErrNotFound when none is chosen.
	ActiveKey(ctx context.Context, telegramUserID int64) (*domain.APIKey, error)
	// SetActiveKey and SetActiveKeyByHash choose one of the user's own live
	// keys, returning domain.ErrNotFound for anything else.
	SetActiveKey(ctx context.Context, telegramUserID, keyID int64) (*domain.APIKey, error)
	SetActiveKeyByHash(ctx context.Context, telegramUserID int64, hash []byte) (*domain.APIKey, error)

	// AddDomain registers a verified hostname against one of the user's paid
	// slots, returning domain.ErrNoDomainSlot or domain.ErrDomainTaken.
	AddDomain(ctx context.Context, telegramUserID int64, hostname string) (*domain.CustomDomain, error)
	Domains(ctx context.Context, telegramUserID int64) ([]domain.CustomDomain, error)
	RemoveDomain(ctx context.Context, telegramUserID int64, hostname string) error
	SetActiveDomain(ctx context.Context, telegramUserID int64, hostname string) (*domain.CustomDomain, error)
	// ClearActiveDomain puts new links back on the operator's domain, leaving
	// the user's registered domains alone.
	ClearActiveDomain(ctx context.Context, telegramUserID int64) error
	// ActiveDomain returns the domain new links are stamped with, or
	// domain.ErrNotFound when the user has none.
	ActiveDomain(ctx context.Context, telegramUserID int64) (*domain.CustomDomain, error)
	// ServableDomain returns a hostname that may be served right now:
	// registered, verified, and owned by somebody with live access.
	ServableDomain(ctx context.Context, hostname string) (*domain.CustomDomain, error)
}

// Resolver looks up a hostname's addresses. It exists so domain verification
// can be driven by a stub in tests instead of real DNS.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// PaymentProvider issues and looks up invoices.
type PaymentProvider interface {
	CreateInvoice(ctx context.Context, req cryptopay.InvoiceRequest) (cryptopay.Invoice, error)
	GetInvoices(ctx context.Context, ids []int64) ([]cryptopay.Invoice, error)
}

// Notifier tells a user that their payment went through. It is an interface
// rather than a message string so billing stays free of user-facing copy.
type Notifier interface {
	PaymentConfirmed(ctx context.Context, telegramUserID int64, paidUntil time.Time) error
	// DomainSlotGranted announces a custom-domain purchase. It is separate from
	// PaymentConfirmed because a domain slot buys no days, so there is no new
	// access window to report.
	DomainSlotGranted(ctx context.Context, telegramUserID int64, slots int) error
}

// Config holds the commercial terms.
type Config struct {
	// PriceUSD is the price of one access period, e.g. "10".
	PriceUSD string
	// DomainPriceUSD is the one-off price of one custom domain slot.
	DomainPriceUSD string
	// AccessDays is how long one payment buys.
	AccessDays int
	// MaxKeys caps the live keys one user may hold, so a single purchase
	// cannot be turned into an unbounded number of tenants.
	MaxKeys int
	// AcceptedAssets are the coins a buyer may pay with.
	AcceptedAssets []string
	// InvoiceTTL is how long an invoice stays payable.
	InvoiceTTL time.Duration
}

// Service implements the billing use cases.
type Service struct {
	store    Store
	pay      PaymentProvider
	notify   Notifier
	resolve  Resolver
	serverIP []string
	cfg      Config
	log      *slog.Logger
	now      func() time.Time
}

// New wires a Service. serverIP is the set of addresses a customer's domain
// must resolve to before it is accepted; an empty set disables the check, which
// is only appropriate in development.
func New(store Store, pay PaymentProvider, notify Notifier, resolve Resolver, serverIP []string, cfg Config, log *slog.Logger) *Service {
	if resolve == nil {
		resolve = net.DefaultResolver
	}
	return &Service{
		store: store, pay: pay, notify: notify, resolve: resolve, serverIP: serverIP,
		cfg: cfg, log: log, now: time.Now,
	}
}

// Terms exposes the commercial terms for display.
func (s *Service) Terms() Config { return s.cfg }

// Checkout returns a payment link for the user.
//
// A user who taps "buy" repeatedly gets the same still-payable invoice back
// rather than a new one each time. Otherwise every tap would open another
// invoice at the provider, and a user who paid an older one would be confused
// by the rest.
func (s *Service) Checkout(ctx context.Context, telegramUserID int64, product domain.Product) (string, error) {
	price, description, accessDays, err := s.offer(product)
	if err != nil {
		return "", err
	}

	// Reuse only invoices with comfortable time left, so nobody is handed a
	// link that expires while they are finding their wallet. The lookup is
	// scoped to the product: reusing across products would charge the buyer
	// the other product's price.
	reuseAfter := s.now().Add(-s.cfg.InvoiceTTL / 2)

	existing, err := s.store.RecentPendingPayment(ctx, telegramUserID, product, reuseAfter)
	switch {
	case err == nil:
		return existing.PayURL, nil
	case !errors.Is(err, domain.ErrNotFound):
		return "", err
	}

	inv, err := s.pay.CreateInvoice(ctx, cryptopay.InvoiceRequest{
		Amount:         price,
		Fiat:           "USD",
		AcceptedAssets: s.cfg.AcceptedAssets,
		Description:    description,
		Payload:        strconv.FormatInt(telegramUserID, 10),
		ExpiresIn:      s.cfg.InvoiceTTL,
	})
	if err != nil {
		return "", fmt.Errorf("create invoice: %w", err)
	}
	if inv.InvoiceID == 0 || inv.PayURL() == "" {
		return "", fmt.Errorf("create invoice: provider returned no invoice id or link")
	}

	if _, err := s.store.CreatePayment(ctx, domain.Payment{
		InvoiceID:      inv.InvoiceID,
		TelegramUserID: telegramUserID,
		Product:        product,
		Amount:         price,
		Currency:       "USD",
		AccessDays:     accessDays,
		Status:         domain.PaymentPending,
		PayURL:         inv.PayURL(),
	}); err != nil {
		return "", fmt.Errorf("record invoice %d: %w", inv.InvoiceID, err)
	}

	return inv.PayURL(), nil
}

// offer turns a product into what the invoice should say and what the payment
// row should record. accessDays is nil for anything that buys no time.
func (s *Service) offer(product domain.Product) (price, description string, accessDays *int, err error) {
	switch product {
	case domain.ProductAccess:
		days := s.cfg.AccessDays
		return s.cfg.PriceUSD, fmt.Sprintf("shortlink API access, %d days", days), &days, nil
	case domain.ProductDomain:
		return s.cfg.DomainPriceUSD, "shortlink custom domain", nil, nil
	default:
		return "", "", nil, fmt.Errorf("unknown product %q", product)
	}
}

// Confirm grants access for a paid invoice. It is idempotent, and it notifies
// the buyer only on the call that actually made the change -- so a webhook
// retried three times, racing the reconciler, still sends exactly one message.
func (s *Service) Confirm(ctx context.Context, invoiceID int64) error {
	c, err := s.store.ConfirmPayment(ctx, invoiceID)
	if err != nil {
		return fmt.Errorf("confirm invoice %d: %w", invoiceID, err)
	}
	if !c.Confirmed {
		return nil
	}

	s.log.InfoContext(ctx, "payment confirmed",
		slog.Int64("invoice_id", invoiceID),
		slog.Int64("telegram_user_id", c.TelegramUserID),
		slog.String("product", string(c.Product)))

	// The payment is recorded before this point. A failed notification is
	// logged, never returned: the user has what they paid for, and /status
	// shows it, whereas returning an error would make the webhook retry a
	// payment that is already settled.
	var notifyErr error
	switch c.Product {
	case domain.ProductAccess:
		notifyErr = s.notify.PaymentConfirmed(ctx, c.TelegramUserID, c.PaidUntil)
	case domain.ProductDomain:
		notifyErr = s.notify.DomainSlotGranted(ctx, c.TelegramUserID, c.DomainSlots)
	}
	if notifyErr != nil {
		s.log.WarnContext(ctx, "could not notify buyer",
			slog.Int64("telegram_user_id", c.TelegramUserID), slog.Any("error", notifyErr))
	}
	return nil
}

// Reconcile asks the provider about every recent pending invoice and settles
// the ones that changed.
//
// Webhooks are the fast path, but they can be lost: the service was down, the
// network dropped, the webhook URL was never configured. Reconciling closes
// that gap, and it is also what makes the whole flow work in local development,
// where the provider has no public URL to deliver a webhook to.
func (s *Service) Reconcile(ctx context.Context) error {
	// Look back a little further than an invoice can live, so one that expired
	// while the reconciler was down still gets marked.
	pending, err := s.store.PendingPayments(ctx, s.now().Add(-2*s.cfg.InvoiceTTL), 100)
	if err != nil {
		return fmt.Errorf("list pending payments: %w", err)
	}
	if len(pending) == 0 {
		return nil
	}

	ids := make([]int64, len(pending))
	for i, p := range pending {
		ids[i] = p.InvoiceID
	}

	invoices, err := s.pay.GetInvoices(ctx, ids)
	if err != nil {
		return fmt.Errorf("look up invoices: %w", err)
	}

	for _, inv := range invoices {
		switch inv.Status {
		case cryptopay.StatusPaid:
			if err := s.Confirm(ctx, inv.InvoiceID); err != nil {
				s.log.ErrorContext(ctx, "could not confirm a paid invoice",
					slog.Int64("invoice_id", inv.InvoiceID), slog.Any("error", err))
			}
		case cryptopay.StatusExpired:
			if err := s.store.ExpirePayment(ctx, inv.InvoiceID); err != nil {
				s.log.WarnContext(ctx, "could not expire an invoice",
					slog.Int64("invoice_id", inv.InvoiceID), slog.Any("error", err))
			}
		}
	}
	return nil
}

// Status returns the user's access window, or domain.ErrNotFound if they never
// paid.
func (s *Service) Status(ctx context.Context, telegramUserID int64) (*domain.Subscription, error) {
	return s.store.Subscription(ctx, telegramUserID)
}

// IssuedKey is a freshly minted key. Secret is shown to the user once and
// never stored.
type IssuedKey struct {
	Secret string
	Key    *domain.APIKey
	// Activated is true when this key became the one the bot shortens links
	// with, which happens for a user's first key.
	Activated bool
}

// IssueKey mints a key for a user with active access.
func (s *Service) IssueKey(ctx context.Context, telegramUserID int64, name string) (*IssuedKey, error) {
	name, err := normalizeKeyName(name)
	if err != nil {
		return nil, err
	}

	secret, err := service.NewSecret()
	if err != nil {
		return nil, err
	}

	key, activated, err := s.store.IssueKey(ctx, telegramUserID, name, service.HashKey(secret), s.cfg.MaxKeys)
	if err != nil {
		return nil, err
	}
	return &IssuedKey{Secret: secret, Key: key, Activated: activated}, nil
}

// ActiveKey returns the key the bot acts with, even if it has lapsed, for
// display. domain.ErrNotFound means none is chosen.
func (s *Service) ActiveKey(ctx context.Context, telegramUserID int64) (*domain.APIKey, error) {
	return s.store.ActiveKey(ctx, telegramUserID)
}

// UsableKey returns the key the bot may act with right now.
//
// The bot creates links in-process rather than through the HTTP API, which
// means it bypasses the API's authentication middleware. Every check that
// middleware would make -- the key exists, it is not revoked, it has not
// expired -- therefore has to be made here instead, or the bot would become a
// way to use the service without paying.
func (s *Service) UsableKey(ctx context.Context, telegramUserID int64) (*domain.APIKey, error) {
	key, err := s.store.ActiveKey(ctx, telegramUserID)
	if errors.Is(err, domain.ErrNotFound) {
		// No key chosen has two very different causes, and sending the wrong
		// advice wastes the user's time: somebody who never paid would be told
		// to run /newkey, which then refuses them as well.
		sub, subErr := s.store.Subscription(ctx, telegramUserID)
		switch {
		case errors.Is(subErr, domain.ErrNotFound):
			return nil, domain.ErrNoAccess
		case subErr != nil:
			return nil, subErr
		case !sub.IsActive(s.now()):
			return nil, domain.ErrNoAccess
		}
		return nil, domain.ErrNoActiveKey
	}
	if err != nil {
		return nil, err
	}
	if !key.IsActive(s.now()) {
		return nil, domain.ErrNoAccess
	}
	return key, nil
}

// UseKey chooses the key the bot acts with. ref is either a key number from
// /keys or a pasted secret; a secret is hashed on arrival and only the hash is
// used to find the key.
func (s *Service) UseKey(ctx context.Context, telegramUserID int64, ref string) (*domain.APIKey, error) {
	ref = strings.TrimSpace(ref)

	if strings.HasPrefix(ref, service.KeyPrefix) {
		return s.store.SetActiveKeyByHash(ctx, telegramUserID, service.HashKey(ref))
	}

	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil || id <= 0 {
		return nil, &domain.ValidationError{Field: "key", Reason: "give a key number from /keys, or paste the key itself"}
	}
	return s.store.SetActiveKey(ctx, telegramUserID, id)
}

// Keys lists the user's live keys. Secrets are not included -- only hashes are
// stored, so they could not be shown again even if that were wanted.
func (s *Service) Keys(ctx context.Context, telegramUserID int64) ([]domain.APIKey, error) {
	return s.store.ListKeys(ctx, telegramUserID)
}

// RevokeKey disables one of the user's keys.
func (s *Service) RevokeKey(ctx context.Context, telegramUserID, keyID int64) error {
	return s.store.RevokeKey(ctx, telegramUserID, keyID)
}

// AddDomain validates a customer-supplied hostname, checks that it really
// points at this server, and registers it against one of their paid slots.
//
// The DNS check is the ownership proof. It is weaker than a TXT challenge --
// anyone may point a domain they own at somebody else's IP -- but it is the
// condition that has to hold anyway for the domain to serve, and pointing a
// domain here gains an attacker nothing except a hostname that redirects to
// links they cannot see or control.
func (s *Service) AddDomain(ctx context.Context, telegramUserID int64, raw string) (*domain.CustomDomain, error) {
	hostname, err := domain.NormalizeHostname(raw)
	if err != nil {
		return nil, err
	}

	if err := s.checkPointsHere(ctx, hostname); err != nil {
		return nil, err
	}
	return s.store.AddDomain(ctx, telegramUserID, hostname)
}

// checkPointsHere resolves the hostname and requires at least one address to be
// one of this server's. An empty serverIP set skips the check, which is how the
// local development stack works without public DNS.
func (s *Service) checkPointsHere(ctx context.Context, hostname string) error {
	if len(s.serverIP) == 0 {
		return nil
	}

	addrs, err := s.resolve.LookupHost(ctx, hostname)
	if err != nil {
		// A name that does not resolve yet is the ordinary case right after
		// somebody adds the record, so this is not an internal error.
		s.log.InfoContext(ctx, "custom domain does not resolve",
			slog.String("hostname", hostname), slog.Any("error", err))
		return domain.ErrDomainNotPointed
	}
	for _, a := range addrs {
		if slices.Contains(s.serverIP, a) {
			return nil
		}
	}
	return domain.ErrDomainNotPointed
}

// Domains lists the user's registered domains.
func (s *Service) Domains(ctx context.Context, telegramUserID int64) ([]domain.CustomDomain, error) {
	return s.store.Domains(ctx, telegramUserID)
}

// RemoveDomain deletes one of the user's domains. Links stamped with it fall
// back to the operator's domain instead of breaking.
func (s *Service) RemoveDomain(ctx context.Context, telegramUserID int64, raw string) error {
	hostname, err := domain.NormalizeHostname(raw)
	if err != nil {
		return err
	}
	return s.store.RemoveDomain(ctx, telegramUserID, hostname)
}

// UseDomain chooses which domain new links are stamped with.
func (s *Service) UseDomain(ctx context.Context, telegramUserID int64, raw string) (*domain.CustomDomain, error) {
	hostname, err := domain.NormalizeHostname(raw)
	if err != nil {
		return nil, err
	}
	return s.store.SetActiveDomain(ctx, telegramUserID, hostname)
}

// UseDefaultDomain sends new links back to the operator's domain. The user's
// own domains stay registered and their slots stay spent, so this is reversible
// with UseDomain -- unlike removing a domain, which also unstamps every link
// already created on it.
func (s *Service) UseDefaultDomain(ctx context.Context, telegramUserID int64) error {
	return s.store.ClearActiveDomain(ctx, telegramUserID)
}

// ActiveDomain returns the domain new links are stamped with, or
// domain.ErrNotFound when the user has none.
func (s *Service) ActiveDomain(ctx context.Context, telegramUserID int64) (*domain.CustomDomain, error) {
	return s.store.ActiveDomain(ctx, telegramUserID)
}

// MayServe reports whether a hostname may be served, and is what gates
// certificate issuance. It answers false for anything it does not recognise.
func (s *Service) MayServe(ctx context.Context, hostname string) (bool, error) {
	hostname, err := domain.NormalizeHostname(hostname)
	if err != nil {
		return false, nil //nolint:nilerr // a malformed hostname is a refusal, not a failure
	}

	_, err = s.store.ServableDomain(ctx, hostname)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, domain.ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

const maxKeyNameRunes = 40

// normalizeKeyName tidies a user-supplied key label. It arrives from a chat
// message, so it can hold anything -- newlines, control characters, a
// thousand emoji -- and it is shown back to the user later.
func normalizeKeyName(name string) (string, error) {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(name))

	if name == "" {
		return "telegram", nil
	}
	if len([]rune(name)) > maxKeyNameRunes {
		return "", &domain.ValidationError{Field: "name", Reason: fmt.Sprintf("must be at most %d characters", maxKeyNameRunes)}
	}
	return name, nil
}
