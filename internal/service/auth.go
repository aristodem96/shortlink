package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"shortlink/internal/domain"
)

const (
	// KeyPrefix makes issued secrets recognisable in logs and in secret
	// scanners, which is why GitHub asks vendors to use one.
	KeyPrefix = "sl_"

	// keySecretBytes is the entropy behind a key: 32 bytes is far beyond
	// brute-force reach.
	keySecretBytes = 32

	// authCacheTTL bounds how long a revoked key keeps working. Verifying every
	// request against Postgres would put a database round trip on the critical
	// path of the whole management API; caching for a few seconds is the usual
	// trade, and the window is short enough to be operationally honest.
	authCacheTTL = 30 * time.Second

	// expiredRecheckTTL is how long the refusal of an *expired* key is cached.
	//
	// It is much shorter than authCacheTTL on purpose. An unknown or revoked
	// key never comes back, so its refusal can be cached for the full TTL. An
	// expired key comes back the moment its owner pays -- and the moment right
	// after paying, having just been told "payment received", is exactly when
	// they will try it. Caching that refusal for 30 seconds would answer a
	// brand-new customer with 401. Five seconds still bounds the database load
	// an expired key can cause to one lookup per interval.
	expiredRecheckTTL = 5 * time.Second

	// authCacheMaxEntries caps memory. The cache is a plain map, so an
	// attacker spraying random keys must not be able to grow it without bound.
	authCacheMaxEntries = 4096
)

// Authenticator verifies API keys and issues new ones.
type Authenticator struct {
	repo  APIKeyRepository
	cache *ttlCache
	now   func() time.Time
}

// NewAuthenticator wires an Authenticator.
func NewAuthenticator(repo APIKeyRepository) *Authenticator {
	return &Authenticator{
		repo:  repo,
		cache: newTTLCache(authCacheMaxEntries),
		now:   time.Now,
	}
}

// HashKey returns the digest stored for a presented secret. Exported so the
// CLI and the tests derive the hash the same way the server does.
func HashKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// Authenticate resolves a presented secret to its owner.
//
// Unknown keys are cached negatively too: without that, a brute-force sprayer
// would get one database lookup per guess for free.
func (a *Authenticator) Authenticate(ctx context.Context, secret string) (*domain.APIKey, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" || !strings.HasPrefix(secret, KeyPrefix) {
		return nil, domain.ErrUnauthorized
	}

	hash := HashKey(secret)
	cacheKey := string(hash)

	now := a.now()
	if entry, ok := a.cache.get(cacheKey, now); ok {
		if entry == nil {
			return nil, domain.ErrUnauthorized
		}
		if entry.IsActive(now) {
			return entry, nil
		}
		// The cached copy has expired. That is not a reason to refuse: the
		// owner may have renewed since it was cached, which moves expires_at
		// in the database but not in this copy. Fall through and re-read it,
		// so a renewal takes effect immediately instead of after the TTL.
	}

	key, err := a.repo.GetByHash(ctx, hash)
	if errors.Is(err, domain.ErrNotFound) {
		a.cache.set(cacheKey, nil, now.Add(authCacheTTL))
		return nil, domain.ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("look up api key: %w", err)
	}
	if key.RevokedAt != nil {
		a.cache.set(cacheKey, nil, now.Add(authCacheTTL))
		return nil, domain.ErrUnauthorized
	}
	if !key.IsActive(now) {
		// Expired rather than revoked: its owner can renew it, so recheck soon.
		a.cache.set(cacheKey, nil, now.Add(expiredRecheckTTL))
		return nil, domain.ErrUnauthorized
	}

	a.cache.set(cacheKey, key, now.Add(authCacheTTL))
	return key, nil
}

// NewSecret generates a fresh API key secret. It is exported so every place
// that mints keys -- the operator CLI and the Telegram bot -- produces them the
// same way, with the same prefix and the same entropy.
func NewSecret() (string, error) {
	buf := make([]byte, keySecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return KeyPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// Issue creates a new API key and returns the secret. The secret is returned
// exactly once; only its hash is stored.
//
// expiresAt is optional and is the one thing that distinguishes the two ways a
// key can come into being. The bot sets it to the end of a buyer's paid access;
// the operator CLI leaves it nil for a key that never expires, and may set it
// to hand out time-limited access by hand. Both end up as the same row, checked
// by the same IsActive, so there is no second notion of a key to keep in step.
func (a *Authenticator) Issue(ctx context.Context, name string, expiresAt *time.Time) (string, *domain.APIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil, &domain.ValidationError{Field: "name", Reason: "must not be empty"}
	}

	secret, err := NewSecret()
	if err != nil {
		return "", nil, err
	}

	key, err := a.repo.Create(ctx, name, HashKey(secret), expiresAt)
	if err != nil {
		return "", nil, err
	}
	return secret, key, nil
}

// ttlCache is a small, bounded map with per-entry expiry. A nil value marks a
// negatively cached key.
type ttlCache struct {
	mu         sync.RWMutex
	entries    map[string]ttlEntry
	maxEntries int
}

type ttlEntry struct {
	key       *domain.APIKey
	expiresAt time.Time
}

func newTTLCache(maxEntries int) *ttlCache {
	return &ttlCache{entries: make(map[string]ttlEntry), maxEntries: maxEntries}
}

func (c *ttlCache) get(k string, now time.Time) (*domain.APIKey, bool) {
	c.mu.RLock()
	e, ok := c.entries[k]
	c.mu.RUnlock()

	if !ok || now.After(e.expiresAt) {
		return nil, false
	}
	return e.key, true
}

func (c *ttlCache) set(k string, v *domain.APIKey, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= c.maxEntries {
		// Cheap eviction: drop everything that has already expired, and if that
		// frees nothing, reset. The cache is a latency optimisation, so losing
		// it costs one database lookup per key, never correctness.
		now := time.Now()
		for key, e := range c.entries {
			if now.After(e.expiresAt) {
				delete(c.entries, key)
			}
		}
		if len(c.entries) >= c.maxEntries {
			c.entries = make(map[string]ttlEntry, c.maxEntries)
		}
	}

	c.entries[k] = ttlEntry{key: v, expiresAt: expiresAt}
}
