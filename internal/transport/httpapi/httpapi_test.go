package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aristodem96/shortlink/internal/domain"
	"github.com/aristodem96/shortlink/internal/service"
)

const testKeySecret = "sl_test-key"

// --- test doubles ------------------------------------------------------------

type stubLinkService struct {
	mu sync.Mutex

	created   *domain.Link
	createErr error

	resolved   *domain.Link
	resolveErr error

	got    *domain.Link
	getErr error

	listItems []domain.Link
	listTotal int64
	listErr   error

	deleteErr error

	stats    *domain.LinkStats
	statsErr error

	lastCreateInput service.CreateLinkInput
	lastListLimit   int
	lastStatsWindow time.Duration
	clicks          []service.ClickMeta
}

func (s *stubLinkService) Create(_ context.Context, in service.CreateLinkInput) (*domain.Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastCreateInput = in
	return s.created, s.createErr
}

func (s *stubLinkService) Resolve(context.Context, string) (*domain.Link, error) {
	return s.resolved, s.resolveErr
}

func (s *stubLinkService) RecordClick(_ *domain.Link, meta service.ClickMeta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clicks = append(s.clicks, meta)
}

func (s *stubLinkService) Get(context.Context, int64, string) (*domain.Link, error) {
	return s.got, s.getErr
}

func (s *stubLinkService) List(_ context.Context, _ int64, limit, _ int) ([]domain.Link, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastListLimit = limit
	return s.listItems, s.listTotal, s.listErr
}

func (s *stubLinkService) Delete(context.Context, int64, string) error { return s.deleteErr }

func (s *stubLinkService) Stats(_ context.Context, _ int64, _ string, window time.Duration) (*domain.LinkStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastStatsWindow = window
	return s.stats, s.statsErr
}

func (s *stubLinkService) recordedClicks() []service.ClickMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]service.ClickMeta(nil), s.clicks...)
}

// stubAuth accepts exactly one secret.
type stubAuth struct{ key *domain.APIKey }

func (a stubAuth) Authenticate(_ context.Context, secret string) (*domain.APIKey, error) {
	if secret != testKeySecret {
		return nil, domain.ErrUnauthorized
	}
	return a.key, nil
}

// stubLimiter allows or denies everything.
type stubLimiter struct {
	deny bool
	keys []string
	mu   sync.Mutex
}

func (l *stubLimiter) Allow(_ context.Context, key string) (bool, int, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.keys = append(l.keys, key)
	if l.deny {
		return false, 0, 3 * time.Second
	}
	return true, 42, 0
}

func (l *stubLimiter) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.keys...)
}

// --- helpers -----------------------------------------------------------------

type testServer struct {
	handler http.Handler
	svc     *stubLinkService
	limiter *stubLimiter
}

func newTestServer(t *testing.T, customise ...func(*Deps)) *testServer {
	t.Helper()

	svc := &stubLinkService{}
	limiter := &stubLimiter{}

	deps := Deps{
		Links:   svc,
		Auth:    stubAuth{key: &domain.APIKey{ID: 7, Name: "test"}},
		Limiter: limiter,
		ReadinessChecks: []ReadinessCheck{
			{Name: "postgres", Probe: func(context.Context) error { return nil }},
		},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		BaseURL: "https://sho.rt",
		Version: "test",
	}
	for _, fn := range customise {
		fn(&deps)
	}

	return &testServer{handler: NewRouter(deps), svc: svc, limiter: limiter}
}

func (ts *testServer) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = "203.0.113.7:54321"
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)
	return rec
}

func (ts *testServer) authed(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return ts.do(t, method, path, body, map[string]string{
		"Authorization": "Bearer " + testKeySecret,
		"Content-Type":  "application/json",
	})
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorDetail {
	t.Helper()

	var body errorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body: %s", rec.Body.String())
	return body.Error
}

// --- create ------------------------------------------------------------------

func TestCreateLink(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	ts.svc.created = &domain.Link{
		Code: "abc1234", TargetURL: "https://go.dev", CreatedAt: time.Now().UTC(),
	}

	rec := ts.authed(t, http.MethodPost, "/api/v1/links", `{"target_url":"https://go.dev"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var got linkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "abc1234", got.Code)
	assert.Equal(t, "https://sho.rt/abc1234", got.ShortURL, "the short URL must be built from BaseURL")
	assert.Equal(t, "https://sho.rt/abc1234", rec.Header().Get("Location"))
	assert.Equal(t, int64(7), ts.svc.lastCreateInput.OwnerID, "the authenticated key must become the owner")
}

func TestCreateLink_PropagatesValidationErrors(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	ts.svc.createErr = &domain.ValidationError{Field: "target_url", Reason: "must use the http or https scheme"}

	rec := ts.authed(t, http.MethodPost, "/api/v1/links", `{"target_url":"ftp://example.com"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	detail := decodeError(t, rec)
	assert.Equal(t, "validation_error", detail.Code)
	assert.Equal(t, "target_url", detail.Field, "the client needs to know which field to fix")
}

func TestCreateLink_ReportsAConflictForATakenAlias(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	ts.svc.createErr = domain.ErrAlreadyExists

	rec := ts.authed(t, http.MethodPost, "/api/v1/links", `{"target_url":"https://go.dev","alias":"taken"}`)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "already_exists", decodeError(t, rec).Code)
}

func TestCreateLink_RejectsMalformedBodies(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"empty body":     "",
		"broken json":    `{"target_url":`,
		"wrong type":     `{"target_url":42}`,
		"unknown field":  `{"target_url":"https://go.dev","colour":"red"}`,
		"two objects":    `{"target_url":"https://go.dev"}{"target_url":"https://go.dev"}`,
		"oversized body": `{"target_url":"https://go.dev/` + strings.Repeat("x", maxRequestBody) + `"}`,
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ts := newTestServer(t)
			rec := ts.authed(t, http.MethodPost, "/api/v1/links", body)

			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			assert.Equal(t, "validation_error", decodeError(t, rec).Code)
		})
	}
}

func TestCreateLink_HidesInternalErrorDetails(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	ts.svc.createErr = assertUnexpected{}

	rec := ts.authed(t, http.MethodPost, "/api/v1/links", `{"target_url":"https://go.dev"}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	// Internal errors routinely carry query fragments and DSNs; none of that
	// may reach the client.
	assert.NotContains(t, rec.Body.String(), "postgres")
	assert.Equal(t, "internal_error", decodeError(t, rec).Code)
}

type assertUnexpected struct{}

func (assertUnexpected) Error() string { return "postgres: insert link: connection refused" }

// --- auth --------------------------------------------------------------------

func TestAuth_RejectsMissingAndBadCredentials(t *testing.T) {
	t.Parallel()

	tests := map[string]map[string]string{
		"no header":     {},
		"wrong scheme":  {"Authorization": "Basic dXNlcjpwYXNz"},
		"empty bearer":  {"Authorization": "Bearer "},
		"unknown key":   {"Authorization": "Bearer sl_not-a-real-key"},
		"scheme only":   {"Authorization": "Bearer"},
		"lowercase ok?": {"Authorization": "bearer sl_not-a-real-key"},
	}

	for name, headers := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ts := newTestServer(t)
			rec := ts.do(t, http.MethodGet, "/api/v1/links", "", headers)

			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, "unauthorized", decodeError(t, rec).Code)
		})
	}
}

func TestAuth_AdvertisesTheSchemeWhenCredentialsAreMissing(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	rec := ts.do(t, http.MethodGet, "/api/v1/links", "", nil)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer",
		"a 401 is only actionable if it says which scheme to use")
}

func TestAuth_AcceptsALowercaseBearerScheme(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	rec := ts.do(t, http.MethodGet, "/api/v1/links", "", map[string]string{
		"Authorization": "bearer " + testKeySecret,
	})

	// RFC 7235 says the scheme is case-insensitive.
	assert.Equal(t, http.StatusOK, rec.Code)
}

// --- list, get, delete, stats ------------------------------------------------

func TestListLinks_RendersAnEmptyPageAsAnArray(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	rec := ts.authed(t, http.MethodGet, "/api/v1/links", "")
	require.Equal(t, http.StatusOK, rec.Code)

	// `null` would force every client into a nil check.
	assert.Contains(t, rec.Body.String(), `"items":[]`)
}

func TestListLinks_RejectsANonNumericLimit(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	rec := ts.authed(t, http.MethodGet, "/api/v1/links?limit=lots", "")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "limit", decodeError(t, rec).Field)
}

func TestGetLink_ReportsAMissingCodeAsNotFound(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	ts.svc.getErr = domain.ErrNotFound

	rec := ts.authed(t, http.MethodGet, "/api/v1/links/nosuch1", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDeleteLink(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	rec := ts.authed(t, http.MethodDelete, "/api/v1/links/abc1234", "")

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String(), "204 must carry no body")
}

func TestStats_ValidatesTheWindow(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	ts.svc.stats = &domain.LinkStats{Code: "abc1234"}

	t.Run("defaults to 30 days", func(t *testing.T) {
		rec := ts.authed(t, http.MethodGet, "/api/v1/links/abc1234/stats", "")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, 30*24*time.Hour, ts.svc.lastStatsWindow)
	})

	t.Run("honours days", func(t *testing.T) {
		rec := ts.authed(t, http.MethodGet, "/api/v1/links/abc1234/stats?days=7", "")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, 7*24*time.Hour, ts.svc.lastStatsWindow)
	})

	for _, days := range []string{"0", "-3", "400"} {
		t.Run("rejects days="+days, func(t *testing.T) {
			rec := ts.authed(t, http.MethodGet, "/api/v1/links/abc1234/stats?days="+days, "")
			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "days", decodeError(t, rec).Field)
		})
	}
}

// --- redirect ----------------------------------------------------------------

func TestRedirect(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	ts.svc.resolved = &domain.Link{ID: 3, Code: "abc1234", TargetURL: "https://go.dev/blog/context"}

	rec := ts.do(t, http.MethodGet, "/abc1234", "", map[string]string{
		"Referer":    "https://news.example.com/thread",
		"User-Agent": "Mozilla/5.0 (test)",
	})

	// 302, not 301: a permanent redirect would be cached by the browser and
	// both freeze the target and stop the click statistics.
	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "https://go.dev/blog/context", rec.Header().Get("Location"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"),
		"the short code must not leak to the destination")

	clicks := ts.svc.recordedClicks()
	require.Len(t, clicks, 1)
	assert.Equal(t, "https://news.example.com/thread", clicks[0].Referer)
	assert.Equal(t, "Mozilla/5.0 (test)", clicks[0].UserAgent)
	assert.Equal(t, "203.0.113.7", clicks[0].IP)
}

func TestRedirect_MapsDomainErrorsOntoStatusCodes(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		"unknown code": {domain.ErrNotFound, http.StatusNotFound, "not_found"},
		"expired link": {domain.ErrGone, http.StatusGone, "expired"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ts := newTestServer(t)
			ts.svc.resolveErr = tc.err

			rec := ts.do(t, http.MethodGet, "/abc1234", "", nil)
			require.Equal(t, tc.wantStatus, rec.Code)
			assert.Equal(t, tc.wantCode, decodeError(t, rec).Code)
			assert.Empty(t, ts.svc.recordedClicks(), "a failed redirect is not a click")
		})
	}
}

func TestRedirect_IgnoresForwardedHeadersWhenProxiesAreNotTrusted(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	ts.svc.resolved = &domain.Link{ID: 1, Code: "abc1234", TargetURL: "https://go.dev"}

	rec := ts.do(t, http.MethodGet, "/abc1234", "", map[string]string{
		"X-Forwarded-For": "198.51.100.99",
	})
	require.Equal(t, http.StatusFound, rec.Code)

	clicks := ts.svc.recordedClicks()
	require.Len(t, clicks, 1)
	// Trusting the header unconditionally would let anyone forge an address and
	// walk around the per-IP rate limit.
	assert.Equal(t, "203.0.113.7", clicks[0].IP)
}

func TestRedirect_UsesForwardedHeadersWhenProxiesAreTrusted(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t, func(d *Deps) { d.TrustProxyHeaders = true })
	ts.svc.resolved = &domain.Link{ID: 1, Code: "abc1234", TargetURL: "https://go.dev"}

	rec := ts.do(t, http.MethodGet, "/abc1234", "", map[string]string{
		"X-Forwarded-For": "198.51.100.99, 10.0.0.1",
	})
	require.Equal(t, http.StatusFound, rec.Code)

	clicks := ts.svc.recordedClicks()
	require.Len(t, clicks, 1)
	assert.Equal(t, "198.51.100.99", clicks[0].IP, "the left-most entry is the original client")
}

// --- rate limiting -----------------------------------------------------------

func TestRateLimit_RejectsWithRetryAfter(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	ts.limiter.deny = true

	rec := ts.authed(t, http.MethodGet, "/api/v1/links", "")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	assert.Equal(t, "3", rec.Header().Get("Retry-After"), "clients need to know how long to back off")
	assert.Equal(t, "rate_limited", decodeError(t, rec).Code)
}

func TestRateLimit_KeysByAPIKeyWhenAuthenticated(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	ts.authed(t, http.MethodGet, "/api/v1/links", "")

	// Keying by API key means one tenant cannot spend the quota of everyone
	// else behind the same NAT.
	assert.Contains(t, ts.limiter.seen(), "key:7")
}

func TestRateLimit_KeysByIPForAnonymousRedirects(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)
	ts.svc.resolved = &domain.Link{ID: 1, Code: "abc1234", TargetURL: "https://go.dev"}
	ts.do(t, http.MethodGet, "/abc1234", "", nil)

	assert.Contains(t, ts.limiter.seen(), "ip:203.0.113.7")
}

// --- operational endpoints and middleware ------------------------------------

func TestHealthz_DoesNotProbeDependencies(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t, func(d *Deps) {
		d.ReadinessChecks = []ReadinessCheck{
			{Name: "postgres", Probe: func(context.Context) error { return assertUnexpected{} }},
		}
	})

	// Liveness must stay green during a database outage, otherwise the
	// orchestrator restarts every replica and turns a blip into a cold start.
	rec := ts.do(t, http.MethodGet, "/healthz", "", nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestReadyz(t *testing.T) {
	t.Parallel()

	t.Run("reports ok when dependencies are healthy", func(t *testing.T) {
		t.Parallel()

		ts := newTestServer(t)
		rec := ts.do(t, http.MethodGet, "/readyz", "", nil)

		require.Equal(t, http.StatusOK, rec.Code)
		var body healthResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "ok", body.Status)
		assert.Equal(t, "ok", body.Checks["postgres"])
	})

	t.Run("reports 503 when a dependency is down", func(t *testing.T) {
		t.Parallel()

		ts := newTestServer(t, func(d *Deps) {
			d.ReadinessChecks = []ReadinessCheck{
				{Name: "postgres", Probe: func(context.Context) error { return nil }},
				{Name: "redis", Probe: func(context.Context) error { return assertUnexpected{} }},
			}
		})

		rec := ts.do(t, http.MethodGet, "/readyz", "", nil)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)

		var body healthResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "degraded", body.Status)
		assert.Equal(t, "ok", body.Checks["postgres"])
		assert.Equal(t, "unavailable", body.Checks["redis"])
	})
}

func TestMetricsAndDocsAreServed(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)

	metrics := ts.do(t, http.MethodGet, "/metrics", "", nil)
	assert.Equal(t, http.StatusOK, metrics.Code)
	assert.Contains(t, metrics.Body.String(), "shortlink_")

	spec := ts.do(t, http.MethodGet, "/openapi.yaml", "", nil)
	require.Equal(t, http.StatusOK, spec.Code)
	assert.Contains(t, spec.Body.String(), "openapi: 3.0.3")

	docs := ts.do(t, http.MethodGet, "/docs", "", nil)
	assert.Equal(t, http.StatusOK, docs.Code)
}

func TestRequestID_IsEchoedAndGenerated(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)

	t.Run("generates one when absent", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/healthz", "", nil)
		assert.NotEmpty(t, rec.Header().Get("X-Request-Id"))
	})

	t.Run("honours a sane inbound value", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/healthz", "", map[string]string{"X-Request-Id": "trace-abc-123"})
		assert.Equal(t, "trace-abc-123", rec.Header().Get("X-Request-Id"))
	})

	t.Run("replaces a hostile value", func(t *testing.T) {
		// A client-supplied newline would let an attacker forge log entries.
		rec := ts.do(t, http.MethodGet, "/healthz", "", map[string]string{
			"X-Request-Id": "abc\ndef INFO fake log line",
		})
		got := rec.Header().Get("X-Request-Id")
		assert.NotContains(t, got, "fake log line")
		assert.NotEmpty(t, got)
	})
}

func TestRecoverer_TurnsAPanicIntoA500(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t, func(d *Deps) { d.Links = &panickingService{} })

	var rec *httptest.ResponseRecorder
	require.NotPanics(t, func() {
		rec = ts.do(t, http.MethodGet, "/abc1234", "", nil)
	})

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "internal_error", decodeError(t, rec).Code)
}

type panickingService struct{ stubLinkService }

func (*panickingService) Resolve(context.Context, string) (*domain.Link, error) {
	panic("boom")
}

func TestUnknownRoutesAndMethodsReturnJSON(t *testing.T) {
	t.Parallel()

	ts := newTestServer(t)

	t.Run("unknown endpoint", func(t *testing.T) {
		rec := ts.authed(t, http.MethodGet, "/api/v1/nope", "")
		require.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "not_found", decodeError(t, rec).Code)
	})

	t.Run("unknown endpoint without credentials answers 401 first", func(t *testing.T) {
		// Authentication runs before routing inside /api/v1, so an anonymous
		// caller cannot map the API surface by probing for 404s.
		rec := ts.do(t, http.MethodGet, "/api/v1/nope", "", nil)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("wrong method", func(t *testing.T) {
		rec := ts.authed(t, http.MethodPatch, "/api/v1/links/abc1234", "")
		require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
		assert.Equal(t, "method_not_allowed", decodeError(t, rec).Code)
	})
}
