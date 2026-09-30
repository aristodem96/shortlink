// Package rediscache implements the link cache and the rate limiter on Redis.
package rediscache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/aristodem96/shortlink/internal/domain"
)

// missingSentinel marks a negatively cached code. It is not valid JSON, so it
// can never be confused with a serialised link.
const missingSentinel = "\x00missing"

// keyPrefix is versioned. Entries written before custom domains existed carry
// no hostname, and decoding one would produce a link that looks like it belongs
// on the operator's domain -- serving a customer's branded link on the wrong
// host until the entry expired. Changing the prefix retires that whole
// keyspace at once; the stale entries fall out on their own TTL.
const keyPrefix = "link2:"

// LinkCache caches resolved links in Redis. It satisfies service.LinkCache.
type LinkCache struct {
	rdb redis.UniversalClient
}

// NewLinkCache wires a LinkCache.
func NewLinkCache(rdb redis.UniversalClient) *LinkCache {
	return &LinkCache{rdb: rdb}
}

// cachedLink is the wire format. It is deliberately separate from domain.Link:
// the cache is a persisted representation, and pinning its shape here means a
// later change to the domain struct cannot silently break entries that are
// already in Redis.
type cachedLink struct {
	ID        int64      `json:"i"`
	Code      string     `json:"c"`
	TargetURL string     `json:"u"`
	OwnerID   int64      `json:"o"`
	DomainID  *int64     `json:"d,omitempty"`
	Host      string     `json:"h,omitempty"`
	ExpiresAt *time.Time `json:"e,omitempty"`
	CreatedAt time.Time  `json:"t"`
}

// Get returns the cached link, domain.ErrCacheMiss when nothing is stored, or
// domain.ErrNotFound when the code is negatively cached.
func (c *LinkCache) Get(ctx context.Context, code string) (*domain.Link, error) {
	raw, err := c.rdb.Get(ctx, keyPrefix+code).Result()
	if errors.Is(err, redis.Nil) {
		return nil, domain.ErrCacheMiss
	}
	if err != nil {
		return nil, fmt.Errorf("rediscache: get link: %w", err)
	}
	if raw == missingSentinel {
		return nil, domain.ErrNotFound
	}

	var cl cachedLink
	if err := json.Unmarshal([]byte(raw), &cl); err != nil {
		// A corrupt or stale-format entry is treated as a miss so the request
		// still succeeds through the database.
		return nil, domain.ErrCacheMiss
	}
	return &domain.Link{
		ID:        cl.ID,
		Code:      cl.Code,
		TargetURL: cl.TargetURL,
		OwnerID:   cl.OwnerID,
		DomainID:  cl.DomainID,
		Host:      cl.Host,
		ExpiresAt: cl.ExpiresAt,
		CreatedAt: cl.CreatedAt,
	}, nil
}

// Set caches a link.
//
// When the link expires sooner than the configured TTL the entry is shortened
// to match, so an expired link cannot keep being served from cache.
func (c *LinkCache) Set(ctx context.Context, link *domain.Link, ttl time.Duration) error {
	if link.ExpiresAt != nil {
		if remaining := time.Until(*link.ExpiresAt); remaining < ttl {
			if remaining <= 0 {
				return nil // Already expired; nothing worth caching.
			}
			ttl = remaining
		}
	}

	payload, err := json.Marshal(cachedLink{
		ID:        link.ID,
		Code:      link.Code,
		TargetURL: link.TargetURL,
		OwnerID:   link.OwnerID,
		DomainID:  link.DomainID,
		Host:      link.Host,
		ExpiresAt: link.ExpiresAt,
		CreatedAt: link.CreatedAt,
	})
	if err != nil {
		return fmt.Errorf("rediscache: encode link: %w", err)
	}

	if err := c.rdb.Set(ctx, keyPrefix+link.Code, payload, ttl).Err(); err != nil {
		return fmt.Errorf("rediscache: set link: %w", err)
	}
	return nil
}

// SetMissing negatively caches a code, which is what stops a flood of requests
// for codes that do not exist from reaching Postgres.
func (c *LinkCache) SetMissing(ctx context.Context, code string, ttl time.Duration) error {
	if err := c.rdb.Set(ctx, keyPrefix+code, missingSentinel, ttl).Err(); err != nil {
		return fmt.Errorf("rediscache: set missing marker: %w", err)
	}
	return nil
}

// Delete drops an entry, positive or negative.
func (c *LinkCache) Delete(ctx context.Context, code string) error {
	if err := c.rdb.Del(ctx, keyPrefix+code).Err(); err != nil {
		return fmt.Errorf("rediscache: delete link: %w", err)
	}
	return nil
}
