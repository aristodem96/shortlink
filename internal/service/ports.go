// Package service holds the application logic: it orchestrates storage, cache
// and analytics, and it owns every rule that is not pure domain validation.
//
// The interfaces below are declared here, on the consumer side, rather than
// next to their implementations. That is the idiomatic Go direction: the
// service states what it needs, and internal/storage happens to satisfy it,
// so storage packages never import the service and the dependency graph stays
// acyclic.
package service

import (
	"context"
	"time"

	"github.com/aristodem96/shortlink/internal/domain"
)

// ErrCacheMiss is re-exported for readability at the call sites in this
// package; the canonical value lives in domain so cache implementations can
// return it without importing the service.
var ErrCacheMiss = domain.ErrCacheMiss

// LinkRepository is the durable store of links.
type LinkRepository interface {
	// Create persists a link and fills in its ID. It returns
	// domain.ErrAlreadyExists if the code is taken.
	Create(ctx context.Context, link *domain.Link) error
	// GetByCode returns domain.ErrNotFound when no such code exists.
	GetByCode(ctx context.Context, code string) (*domain.Link, error)
	// GetOwnedByCode scopes the lookup to one owner.
	GetOwnedByCode(ctx context.Context, ownerID int64, code string) (*domain.Link, error)
	// ListByOwner returns the owner's links, newest first, plus the total count.
	ListByOwner(ctx context.Context, ownerID int64, limit, offset int) ([]domain.Link, int64, error)
	// DeleteOwnedByCode returns domain.ErrNotFound when the owner has no such link.
	DeleteOwnedByCode(ctx context.Context, ownerID int64, code string) error
	// Stats aggregates clicks for a link within [since, now].
	Stats(ctx context.Context, linkID int64, since time.Time) (*domain.LinkStats, error)
	// ClickTotals returns all-time click counts for the owner's links, keyed
	// by link id. Links nobody has clicked are absent from the map.
	ClickTotals(ctx context.Context, ownerID int64, linkIDs []int64) (map[int64]int64, error)
}

// LinkCache is the read-through cache in front of LinkRepository.
type LinkCache interface {
	// Get returns the cached link, domain.ErrCacheMiss when the key is absent,
	// or domain.ErrNotFound when the key is negatively cached.
	Get(ctx context.Context, code string) (*domain.Link, error)
	Set(ctx context.Context, link *domain.Link, ttl time.Duration) error
	SetMissing(ctx context.Context, code string, ttl time.Duration) error
	Delete(ctx context.Context, code string) error
}

// ClickRecorder accepts redirect events. Implementations must not block: the
// redirect path calls this before writing the response.
type ClickRecorder interface {
	Record(click domain.Click)
}

// CodeGenerator produces candidate short codes.
type CodeGenerator interface {
	Generate() (string, error)
}

// IPHasher turns a client address into a stable pseudonymous identifier, so
// unique-visitor counts survive without the raw address being stored.
type IPHasher interface {
	HashIP(ip string) []byte
}

// APIKeyRepository is the durable store of API keys.
type APIKeyRepository interface {
	// GetByHash returns domain.ErrNotFound when the hash is unknown.
	GetByHash(ctx context.Context, hash []byte) (*domain.APIKey, error)
	Create(ctx context.Context, name string, hash []byte) (*domain.APIKey, error)
}
