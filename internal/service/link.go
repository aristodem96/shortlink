package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/aristodem96/shortlink/internal/domain"
	"github.com/aristodem96/shortlink/internal/observability"
)

const (
	// maxCodeAttempts bounds the retry loop for random code collisions. With a
	// 7 character base62 code the keyspace is 62^7 ≈ 3.5e12, so even at a
	// billion links the chance of three consecutive collisions is negligible;
	// exhausting the attempts means something else is wrong.
	maxCodeAttempts = 3

	// loadTimeout caps the database read behind a cache miss. It is applied to
	// a context detached from the caller so a stampede's shared fetch cannot be
	// cancelled by whichever client happens to disconnect first.
	loadTimeout = 3 * time.Second

	maxRefererLength   = 512
	maxUserAgentLength = 512

	defaultPageSize = 20
	maxPageSize     = 100
)

// LinkService implements the link use cases.
type LinkService struct {
	repo   LinkRepository
	cache  LinkCache
	clicks ClickRecorder
	codes  CodeGenerator
	hasher IPHasher
	log    *slog.Logger

	// group collapses concurrent cache misses for the same code into a single
	// database read.
	group singleflight.Group

	cacheTTL    time.Duration
	negativeTTL time.Duration

	// now is injectable so tests can control expiry without sleeping.
	now func() time.Time
}

// LinkServiceConfig carries the tunables of LinkService.
type LinkServiceConfig struct {
	CacheTTL         time.Duration
	NegativeCacheTTL time.Duration
}

// LinkServiceOption customises a LinkService after construction.
type LinkServiceOption func(*LinkService)

// WithClock overrides the time source. Tests use it to make expiry
// deterministic.
func WithClock(now func() time.Time) LinkServiceOption {
	return func(s *LinkService) { s.now = now }
}

// NewLinkService wires a LinkService.
func NewLinkService(
	repo LinkRepository,
	cache LinkCache,
	clicks ClickRecorder,
	codes CodeGenerator,
	hasher IPHasher,
	cfg LinkServiceConfig,
	log *slog.Logger,
	opts ...LinkServiceOption,
) *LinkService {
	s := &LinkService{
		repo:        repo,
		cache:       cache,
		clicks:      clicks,
		codes:       codes,
		hasher:      hasher,
		log:         log,
		cacheTTL:    cfg.CacheTTL,
		negativeTTL: cfg.NegativeCacheTTL,
		now:         time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// CreateLinkInput is the request to shorten a URL.
type CreateLinkInput struct {
	OwnerID   int64
	TargetURL string
	// DomainID stamps the link with one of the owner's custom domains. It is
	// fixed at creation so a link already shared keeps the hostname it was
	// shared with, even if the owner later switches domains. nil means the
	// operator's own domain.
	DomainID *int64
	// Host is DomainID's hostname. The caller has already loaded the domain to
	// get the id, so passing the name along saves the insert a join just to
	// render the link that is about to be returned.
	Host string
	// Alias, when non-empty, is used verbatim instead of a random code.
	Alias string
	// ExpiresAt, when set, makes the link stop redirecting at that instant.
	ExpiresAt *time.Time
}

// Create shortens a URL.
func (s *LinkService) Create(ctx context.Context, in CreateLinkInput) (*domain.Link, error) {
	target, err := domain.NormalizeTargetURL(in.TargetURL)
	if err != nil {
		return nil, err
	}

	now := s.now().UTC()
	if in.ExpiresAt != nil && !in.ExpiresAt.After(now) {
		return nil, &domain.ValidationError{Field: "expires_at", Reason: "must be in the future"}
	}

	link := &domain.Link{
		TargetURL: target,
		OwnerID:   in.OwnerID,
		DomainID:  in.DomainID,
		Host:      in.Host,
		ExpiresAt: in.ExpiresAt,
		CreatedAt: now,
	}

	if in.Alias != "" {
		if err := domain.ValidateAlias(in.Alias); err != nil {
			return nil, err
		}
		link.Code = in.Alias
		if err := s.repo.Create(ctx, link); err != nil {
			return nil, err // domain.ErrAlreadyExists reaches the caller as a 409.
		}
	} else if err := s.createWithGeneratedCode(ctx, link); err != nil {
		return nil, err
	}

	// The code may have been probed while it did not exist yet and therefore
	// negatively cached. Drop that entry so the new link resolves immediately.
	if err := s.cache.Delete(ctx, link.Code); err != nil {
		s.log.WarnContext(ctx, "could not invalidate cache after create",
			slog.String("code", link.Code), slog.Any("error", err))
	}

	observability.LinksCreated.Inc()
	return link, nil
}

func (s *LinkService) createWithGeneratedCode(ctx context.Context, link *domain.Link) error {
	var lastErr error
	for attempt := 1; attempt <= maxCodeAttempts; attempt++ {
		code, err := s.codes.Generate()
		if err != nil {
			return fmt.Errorf("generate short code: %w", err)
		}
		link.Code = code

		lastErr = s.repo.Create(ctx, link)
		if lastErr == nil {
			return nil
		}
		if !errors.Is(lastErr, domain.ErrAlreadyExists) {
			return lastErr
		}

		observability.CodeCollisions.Inc()
		s.log.WarnContext(ctx, "short code collision, retrying",
			slog.String("code", code), slog.Int("attempt", attempt))
	}
	return fmt.Errorf("no free short code after %d attempts: %w", maxCodeAttempts, lastErr)
}

// Resolve returns the link a code points at. It is the redirect hot path.
//
// Callers get domain.ErrNotFound for an unknown code and domain.ErrGone for an
// expired one; the distinction lets the transport answer 404 versus 410.
func (s *LinkService) Resolve(ctx context.Context, code string) (*domain.Link, error) {
	if !domain.IsPossibleCode(code) {
		// Junk paths (scanners, favicons, /wp-admin) never reach the stores.
		return nil, domain.ErrNotFound
	}

	link, err := s.load(ctx, code)
	if err != nil {
		return nil, err
	}
	if link.IsExpired(s.now()) {
		return nil, domain.ErrGone
	}
	return link, nil
}

// load implements cache-aside with negative caching and stampede protection.
func (s *LinkService) load(ctx context.Context, code string) (*domain.Link, error) {
	switch cached, err := s.cache.Get(ctx, code); {
	case err == nil:
		observability.CacheLookups.WithLabelValues("hit").Inc()
		return cached, nil
	case errors.Is(err, domain.ErrNotFound):
		observability.CacheLookups.WithLabelValues("negative_hit").Inc()
		return nil, domain.ErrNotFound
	case errors.Is(err, ErrCacheMiss):
		observability.CacheLookups.WithLabelValues("miss").Inc()
	default:
		// A broken cache must not take the service down: log it and read
		// through to Postgres.
		observability.CacheLookups.WithLabelValues("error").Inc()
		s.log.WarnContext(ctx, "link cache read failed, falling back to database",
			slog.String("code", code), slog.Any("error", err))
	}

	// context.WithoutCancel keeps the shared fetch alive when the caller that
	// happened to win the singleflight race disconnects: every other waiter
	// would otherwise receive that caller's cancellation.
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadTimeout)
	defer cancel()

	v, err, shared := s.group.Do(code, func() (any, error) {
		link, err := s.repo.GetByCode(fetchCtx, code)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			s.cacheMissing(fetchCtx, code)
			return nil, domain.ErrNotFound
		case err != nil:
			return nil, err
		}
		if err := s.cache.Set(fetchCtx, link, s.cacheTTL); err != nil {
			s.log.WarnContext(fetchCtx, "could not populate link cache",
				slog.String("code", code), slog.Any("error", err))
		}
		return link, nil
	})
	if shared {
		observability.CacheLookups.WithLabelValues("coalesced").Inc()
	}
	if err != nil {
		return nil, err
	}

	link, ok := v.(*domain.Link)
	if !ok || link == nil {
		return nil, fmt.Errorf("link loader returned %T, want *domain.Link", v)
	}
	return link, nil
}

func (s *LinkService) cacheMissing(ctx context.Context, code string) {
	if err := s.cache.SetMissing(ctx, code, s.negativeTTL); err != nil {
		s.log.WarnContext(ctx, "could not negatively cache a missing code",
			slog.String("code", code), slog.Any("error", err))
	}
}

// ClickMeta is what the transport knows about a redirect request.
type ClickMeta struct {
	Referer   string
	UserAgent string
	IP        string
}

// RecordClick hands a redirect event to the analytics collector. It never
// blocks and never fails the redirect: analytics is strictly best effort.
func (s *LinkService) RecordClick(link *domain.Link, meta ClickMeta) {
	s.clicks.Record(domain.Click{
		LinkID:    link.ID,
		ClickedAt: s.now().UTC(),
		Referer:   truncate(meta.Referer, maxRefererLength),
		UserAgent: truncate(meta.UserAgent, maxUserAgentLength),
		IPHash:    s.hasher.HashIP(meta.IP),
	})
}

// Get returns one of the owner's links. A link owned by somebody else reports
// as not found rather than forbidden, so the API does not confirm which codes
// are taken.
func (s *LinkService) Get(ctx context.Context, ownerID int64, code string) (*domain.Link, error) {
	if !domain.IsPossibleCode(code) {
		return nil, domain.ErrNotFound
	}
	return s.repo.GetOwnedByCode(ctx, ownerID, code)
}

// List returns a page of the owner's links, newest first, with the total count.
func (s *LinkService) List(ctx context.Context, ownerID int64, limit, offset int) ([]domain.Link, int64, error) {
	switch {
	case limit <= 0:
		limit = defaultPageSize
	case limit > maxPageSize:
		limit = maxPageSize
	}
	if offset < 0 {
		offset = 0
	}
	return s.repo.ListByOwner(ctx, ownerID, limit, offset)
}

// Delete removes one of the owner's links.
func (s *LinkService) Delete(ctx context.Context, ownerID int64, code string) error {
	if !domain.IsPossibleCode(code) {
		return domain.ErrNotFound
	}
	if err := s.repo.DeleteOwnedByCode(ctx, ownerID, code); err != nil {
		return err
	}

	// Negatively cache instead of merely deleting: a popular link that has just
	// been removed would otherwise send its whole traffic to Postgres.
	s.cacheMissing(ctx, code)
	return nil
}

// Stats aggregates the click history of one of the owner's links over the last
// `window`.
func (s *LinkService) Stats(ctx context.Context, ownerID int64, code string, window time.Duration) (*domain.LinkStats, error) {
	link, err := s.Get(ctx, ownerID, code)
	if err != nil {
		return nil, err
	}
	if window <= 0 {
		window = 30 * 24 * time.Hour
	}

	stats, err := s.repo.Stats(ctx, link.ID, s.now().UTC().Add(-window))
	if err != nil {
		return nil, err
	}
	stats.Code = link.Code
	stats.Host = link.Host
	return stats, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// ClickTotals returns all-time click counts for a page of the owner's links in
// one query, rather than one Stats call per link.
func (s *LinkService) ClickTotals(ctx context.Context, ownerID int64, linkIDs []int64) (map[int64]int64, error) {
	if len(linkIDs) == 0 {
		return map[int64]int64{}, nil
	}
	return s.repo.ClickTotals(ctx, ownerID, linkIDs)
}
