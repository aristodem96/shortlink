package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/aristodem96/shortlink/api"
)

// apiRequestTimeout bounds a management API request. The redirect path is left
// out of it: it is served from cache in the common case and its own handlers
// already carry tighter budgets.
const apiRequestTimeout = 10 * time.Second

// Deps is everything the router needs. Passing one struct rather than nine
// positional arguments keeps the call site at the composition root readable and
// makes adding a dependency a non-breaking change.
type Deps struct {
	Links           LinkService
	Auth            Authenticator
	Limiter         RateLimiter
	ReadinessChecks []ReadinessCheck
	Logger          *slog.Logger

	// BaseURL is the public origin used to render short URLs.
	BaseURL string
	// Version is reported by the health endpoints.
	Version string
	// TrustProxyHeaders enables X-Forwarded-For parsing. Enable it only when
	// the service really does sit behind a proxy that overwrites the header.
	TrustProxyHeaders bool
}

// NewRouter builds the HTTP handler for the whole service.
func NewRouter(deps Deps) http.Handler {
	links := &linkHandler{
		svc:        deps.Links,
		log:        deps.Logger,
		baseURL:    deps.BaseURL,
		trustProxy: deps.TrustProxyHeaders,
	}
	health := &healthHandler{
		checks:  deps.ReadinessChecks,
		version: deps.Version,
		log:     deps.Logger,
	}

	r := chi.NewRouter()

	// Order matters. requestID runs first so every later log line carries it;
	// recoverer sits above the handlers but below the logger, so a panic is
	// still recorded as a completed 500 request.
	r.Use(requestID)
	r.Use(requestLogger(deps.Logger, deps.TrustProxyHeaders))
	r.Use(recoverer(deps.Logger))
	r.Use(metrics)
	r.Use(middleware.CleanPath)

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeErrorDetail(r.Context(), w, deps.Logger, http.StatusNotFound, errorDetail{
			Code: "not_found", Message: "no such endpoint",
		})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeErrorDetail(r.Context(), w, deps.Logger, http.StatusMethodNotAllowed, errorDetail{
			Code: "method_not_allowed", Message: "this method is not supported here",
		})
	})

	// Operational endpoints, deliberately unauthenticated so a probe does not
	// need a credential, and deliberately not rate limited so a limiter outage
	// cannot make the service look unhealthy.
	r.Get("/healthz", health.live)
	r.Get("/readyz", health.ready)
	r.Handle("/metrics", promhttp.Handler())

	r.Get("/openapi.yaml", serveSpec)
	r.Get("/docs", serveDocs)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(middleware.Timeout(apiRequestTimeout))
		r.Use(authenticate(deps.Auth, deps.Logger))
		// Placed after authentication so the bucket can be keyed by API key
		// rather than by IP, which is both fairer and harder to evade.
		r.Use(rateLimit(deps.Limiter, deps.Logger, deps.TrustProxyHeaders))

		r.Route("/links", func(r chi.Router) {
			r.Post("/", links.create)
			r.Get("/", links.list)
			r.Get("/{code}", links.get)
			r.Delete("/{code}", links.remove)
			r.Get("/{code}/stats", links.stats)
		})
	})

	// The redirect route is mounted last and at the root, so it can only ever
	// receive paths no other route claimed. domain.ValidateAlias refuses the
	// reserved words above, so a user cannot register an alias that would
	// shadow one of them.
	r.With(rateLimit(deps.Limiter, deps.Logger, deps.TrustProxyHeaders)).
		Get("/{code}", links.redirect)

	return r
}

func serveSpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	http.ServeContent(w, r, "openapi.yaml", time.Time{}, bytes.NewReader(api.OpenAPISpec))
}

// docsPage renders the embedded spec with Swagger UI loaded from a CDN. Keeping
// the HTML here rather than vendoring the whole of Swagger UI keeps the repo
// small; the spec itself is served locally, so the docs describe exactly the
// build that is running.
const docsPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>shortlink API</title>
  <link rel="stylesheet" href="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui.min.css">
</head>
<body>
  <div id="swagger"></div>
  <script src="https://cdnjs.cloudflare.com/ajax/libs/swagger-ui/5.17.14/swagger-ui-bundle.min.js"></script>
  <script>
    window.onload = () => SwaggerUIBundle({ url: "/openapi.yaml", dom_id: "#swagger" });
  </script>
</body>
</html>`

func serveDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(docsPage))
}
