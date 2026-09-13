package domain

import (
	"errors"
	"time"
)

var (
	// ErrNoAccess means the user has no paid access right now, either because
	// they never paid or because it has run out.
	ErrNoAccess = errors.New("no active paid access")
	// ErrKeyLimit means the user already holds the maximum number of live keys.
	ErrKeyLimit = errors.New("api key limit reached")
	// ErrNoActiveKey means the user has not chosen a key for the bot to act
	// with, so it cannot shorten links on their behalf.
	ErrNoActiveKey = errors.New("no active key selected")
	// ErrNoDomainSlot means the user has used every custom domain they paid
	// for and must buy another slot before adding one more.
	ErrNoDomainSlot = errors.New("no custom domain slot available")
	// ErrDomainTaken means the hostname is already registered, by this user or
	// by somebody else. The two cases are not distinguished on purpose: telling
	// a stranger which domains another customer serves leaks the customer list.
	ErrDomainTaken = errors.New("domain already registered")
	// ErrDomainNotPointed means the hostname does not resolve to this server,
	// so it cannot be served and ownership cannot be taken on trust.
	ErrDomainNotPointed = errors.New("domain does not point at this server")
)

// Subscription is a Telegram user's paid access window.
type Subscription struct {
	TelegramUserID int64
	PaidUntil      time.Time
	// DomainSlots is how many custom domains the user has paid to register.
	DomainSlots int
	// ActiveDomainID is the domain new links are stamped with, or nil for the
	// operator's own domain.
	ActiveDomainID *int64
}

// IsActive reports whether the paid window covers the given instant.
func (s *Subscription) IsActive(now time.Time) bool {
	return now.Before(s.PaidUntil)
}

// PaymentStatus is where an invoice is in its life.
type PaymentStatus string

// The states a payment moves through. It starts pending and ends in exactly
// one of the other two; paid is final.
const (
	PaymentPending PaymentStatus = "pending"
	PaymentPaid    PaymentStatus = "paid"
	PaymentExpired PaymentStatus = "expired"
)

// Product is what an invoice sells.
type Product string

// The two things on sale. A payment is for exactly one of them, and the
// product decides what confirming it grants.
const (
	// ProductAccess buys a window of API access, measured in days.
	ProductAccess Product = "access"
	// ProductDomain buys the right to register one custom domain. It grants no
	// days: a registered domain only serves while the buyer's access is live.
	ProductDomain Product = "domain"
)

// Payment is one invoice issued to one user.
type Payment struct {
	ID             int64
	InvoiceID      int64
	TelegramUserID int64
	Product        Product
	Amount         string
	Currency       string
	// AccessDays is set for ProductAccess and nil for ProductDomain.
	AccessDays *int
	Status     PaymentStatus
	PayURL     string
	CreatedAt  time.Time
	PaidAt     *time.Time
}

// Confirmation is the outcome of settling one payment. Confirmed is false when
// the payment was already settled or was never issued here, which is what lets
// a retried webhook and the reconciler both call into the same code safely.
type Confirmation struct {
	Confirmed      bool
	Product        Product
	TelegramUserID int64
	// PaidUntil is the new end of the access window, set for ProductAccess.
	PaidUntil time.Time
	// DomainSlots is the buyer's slot total after the purchase, set for
	// ProductDomain.
	DomainSlots int
}

// CustomDomain is a hostname a buyer brought of their own.
type CustomDomain struct {
	ID             int64
	TelegramUserID int64
	Hostname       string
	CreatedAt      time.Time
	// VerifiedAt is nil until the hostname was seen resolving to this server.
	VerifiedAt *time.Time
}

// IsVerified reports whether the domain may be served.
func (d *CustomDomain) IsVerified() bool { return d.VerifiedAt != nil }
