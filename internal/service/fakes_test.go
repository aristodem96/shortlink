package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aristodem96/shortlink/internal/domain"
)

// The doubles below are written by hand rather than generated.
//
// The ports are small (five methods at most) and the tests need behaviour, not
// just recorded calls: a repository that really stores links, a cache that can
// be told to fail. Generated mocks would express that as a pile of expectation
// setup, and would add a code-generation step to the build for no gain.

// fakeLinkRepo is an in-memory LinkRepository.
type fakeLinkRepo struct {
	mu     sync.Mutex
	byCode map[string]*domain.Link
	nextID int64

	getCalls    atomic.Int64
	createCalls atomic.Int64

	// createErr, when set, is returned by the next Create call and then cleared.
	createErr error
	// getErr, when set, is returned by every GetByCode call.
	getErr error
	// beforeGet, when set, runs at the start of GetByCode. Tests use it to
	// block the repository and exercise concurrent behaviour.
	beforeGet func()
}

func newFakeLinkRepo() *fakeLinkRepo {
	return &fakeLinkRepo{byCode: make(map[string]*domain.Link)}
}

func (r *fakeLinkRepo) Create(_ context.Context, link *domain.Link) error {
	r.createCalls.Add(1)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.createErr != nil {
		err := r.createErr
		r.createErr = nil
		return err
	}
	if _, taken := r.byCode[link.Code]; taken {
		return domain.ErrAlreadyExists
	}

	r.nextID++
	link.ID = r.nextID

	stored := *link
	r.byCode[link.Code] = &stored
	return nil
}

func (r *fakeLinkRepo) GetByCode(_ context.Context, code string) (*domain.Link, error) {
	r.getCalls.Add(1)
	if r.beforeGet != nil {
		r.beforeGet()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.getErr != nil {
		return nil, r.getErr
	}
	link, ok := r.byCode[code]
	if !ok {
		return nil, domain.ErrNotFound
	}

	copied := *link
	return &copied, nil
}

func (r *fakeLinkRepo) GetOwnedByCode(_ context.Context, ownerID int64, code string) (*domain.Link, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	link, ok := r.byCode[code]
	if !ok || link.OwnerID != ownerID {
		return nil, domain.ErrNotFound
	}

	copied := *link
	return &copied, nil
}

func (r *fakeLinkRepo) ListByOwner(_ context.Context, ownerID int64, limit, offset int) ([]domain.Link, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var owned []domain.Link
	for _, link := range r.byCode {
		if link.OwnerID == ownerID {
			owned = append(owned, *link)
		}
	}

	total := int64(len(owned))
	if offset >= len(owned) {
		return nil, total, nil
	}
	end := min(offset+limit, len(owned))
	return owned[offset:end], total, nil
}

func (r *fakeLinkRepo) DeleteOwnedByCode(_ context.Context, ownerID int64, code string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	link, ok := r.byCode[code]
	if !ok || link.OwnerID != ownerID {
		return domain.ErrNotFound
	}
	delete(r.byCode, code)
	return nil
}

func (r *fakeLinkRepo) Stats(_ context.Context, linkID int64, _ time.Time) (*domain.LinkStats, error) {
	return &domain.LinkStats{TotalClicks: linkID}, nil
}

// fakeCache is an in-memory LinkCache. A nil value marks a negative entry.
type fakeCache struct {
	mu      sync.Mutex
	entries map[string]*domain.Link

	getCalls atomic.Int64
	setCalls atomic.Int64

	// failWith, when set, is returned by every operation. Tests use it to check
	// that the service degrades to the database instead of failing.
	failWith error
}

func newFakeCache() *fakeCache {
	return &fakeCache{entries: make(map[string]*domain.Link)}
}

func (c *fakeCache) Get(_ context.Context, code string) (*domain.Link, error) {
	c.getCalls.Add(1)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.failWith != nil {
		return nil, c.failWith
	}
	link, ok := c.entries[code]
	switch {
	case !ok:
		return nil, domain.ErrCacheMiss
	case link == nil:
		return nil, domain.ErrNotFound
	default:
		copied := *link
		return &copied, nil
	}
}

func (c *fakeCache) Set(_ context.Context, link *domain.Link, _ time.Duration) error {
	c.setCalls.Add(1)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.failWith != nil {
		return c.failWith
	}
	stored := *link
	c.entries[link.Code] = &stored
	return nil
}

func (c *fakeCache) SetMissing(_ context.Context, code string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.failWith != nil {
		return c.failWith
	}
	c.entries[code] = nil
	return nil
}

func (c *fakeCache) Delete(_ context.Context, code string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.failWith != nil {
		return c.failWith
	}
	delete(c.entries, code)
	return nil
}

// has reports whether the cache holds an entry, and whether it is negative.
func (c *fakeCache) has(code string) (present, negative bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	link, ok := c.entries[code]
	return ok, ok && link == nil
}

// fakeRecorder collects clicks.
type fakeRecorder struct {
	mu     sync.Mutex
	clicks []domain.Click
}

func (r *fakeRecorder) Record(click domain.Click) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clicks = append(r.clicks, click)
}

func (r *fakeRecorder) recorded() []domain.Click {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Click(nil), r.clicks...)
}

// scriptedGenerator returns codes from a fixed list, so collision handling can
// be tested deterministically.
type scriptedGenerator struct {
	mu    sync.Mutex
	codes []string
	err   error
}

func (g *scriptedGenerator) Generate() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.err != nil {
		return "", g.err
	}
	if len(g.codes) == 0 {
		return "", errors.New("scriptedGenerator: out of codes")
	}

	code := g.codes[0]
	g.codes = g.codes[1:]
	return code, nil
}

// fakeHasher hashes without a salt; the salting itself is tested in the
// analytics package.
type fakeHasher struct{}

func (fakeHasher) HashIP(ip string) []byte {
	if ip == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(ip))
	return sum[:8]
}
