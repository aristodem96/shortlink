package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// readinessTimeout bounds the dependency probes so a hung database cannot make
// the readiness endpoint itself hang, which would look like a liveness failure
// to an orchestrator and trigger a pointless restart.
const readinessTimeout = 2 * time.Second

// ReadinessCheck is one named dependency probe.
type ReadinessCheck struct {
	Name  string
	Probe func(ctx context.Context) error
}

type healthHandler struct {
	checks  []ReadinessCheck
	version string
	log     *slog.Logger
}

type healthResponse struct {
	Status  string            `json:"status"`
	Version string            `json:"version,omitempty"`
	Checks  map[string]string `json:"checks,omitempty"`
}

// live handles GET /healthz.
//
// Liveness answers "is this process still running", nothing more. Probing
// dependencies here would be a mistake: a database blip would restart every
// replica at once and turn a recoverable outage into a cold start.
func (h *healthHandler) live(w http.ResponseWriter, r *http.Request) {
	writeJSON(r.Context(), w, h.log, http.StatusOK, healthResponse{
		Status: "ok", Version: h.version,
	})
}

// ready handles GET /readyz: may this instance take traffic?
func (h *healthHandler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	results := make([]string, len(h.checks))

	// The probes are independent, so they run concurrently and the endpoint
	// costs the slowest one rather than their sum.
	var wg sync.WaitGroup
	for i, check := range h.checks {
		wg.Add(1)
		go func(i int, check ReadinessCheck) {
			defer wg.Done()
			if err := check.Probe(ctx); err != nil {
				h.log.WarnContext(ctx, "readiness probe failed",
					slog.String("dependency", check.Name), slog.Any("error", err))
				results[i] = "unavailable"
				return
			}
			results[i] = "ok"
		}(i, check)
	}
	wg.Wait()

	status := http.StatusOK
	body := healthResponse{
		Status:  "ok",
		Version: h.version,
		Checks:  make(map[string]string, len(h.checks)),
	}
	for i, check := range h.checks {
		body.Checks[check.Name] = results[i]
		if results[i] != "ok" {
			status = http.StatusServiceUnavailable
			body.Status = "degraded"
		}
	}

	writeJSON(ctx, w, h.log, status, body)
}
