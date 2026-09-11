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
)

// Subscription is a Telegram user's paid access window.
type Subscription struct {
	TelegramUserID int64
	PaidUntil      time.Time
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

// Payment is one invoice issued to one user.
type Payment struct {
	ID             int64
	InvoiceID      int64
	TelegramUserID int64
	Amount         string
	Currency       string
	AccessDays     int
	Status         PaymentStatus
	PayURL         string
	CreatedAt      time.Time
	PaidAt         *time.Time
}
