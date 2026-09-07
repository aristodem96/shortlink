package domain

import "time"

// APIKey identifies the owner of a set of links. Only the hash of the secret is
// persisted, so a database dump does not leak usable credentials.
type APIKey struct {
	ID        int64
	Name      string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// IsActive reports whether the key may still be used.
func (k *APIKey) IsActive() bool { return k.RevokedAt == nil }
