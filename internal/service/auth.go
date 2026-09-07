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

	"github.com/aristodem96/shortlink/internal/domain"
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

	if entry, ok := a.cache.get(cacheKey, a.now()); ok {
		if entry == nil {
			return nil, domain.ErrUnauthorized
		}
		return entry, nil
	}

	key, err := a.repo.GetByHash(ctx, hash)
	if errors.Is(err, domain.ErrNotFound) {
		a.cache.set(cacheKey, nil, a.now().Add(authCacheTTL))
		return nil, domain.ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("look up api key: %w", err)
	}
	if !key.IsActive() {
		a.cache.set(cacheKey, nil, a.now().Add(authCacheTTL))
		return nil, domain.ErrUnauthorized
	}

	a.cache.set(cacheKey, key, a.now().Add(authCacheTTL))
	return key, nil
}

// Issue creates a new API key and returns the secret. The secret is returned
// exactly once; only its hash is stored.
func (a *Authenticator) Issue(ctx context.Context, name string) (string, *domain.APIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil, &domain.ValidationError{Field: "name", Reason: "must not be empty"}
	}

	buf := make([]byte, keySecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate api key: %w", err)
	}
	secret := KeyPrefix + base64.RawURLEncoding.EncodeToString(buf)

	key, err := a.repo.Create(ctx, name, HashKey(secret))
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
