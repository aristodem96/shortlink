package domain

import "time"

// APIKey identifies the owner of a set of links. Only the hash of the secret is
// persisted, so a database dump does not leak usable credentials.
type APIKey struct {
	ID        int64
	Name      string
	CreatedAt time.Time
	RevokedAt *time.Time
	// ExpiresAt is nil for operator keys issued with shortlinkctl, which never
	// expire, and set for keys bought through the Telegram bot.
	ExpiresAt *time.Time
	// TelegramUserID is the buyer of a key issued through the bot, nil otherwise.
	TelegramUserID *int64
}

// IsActive reports whether the key may be used at the given instant.
func (k *APIKey) IsActive(now time.Time) bool {
	if k.RevokedAt != nil {
		return false
	}
	return k.ExpiresAt == nil || now.Before(*k.ExpiresAt)
}
