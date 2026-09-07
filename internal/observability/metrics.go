// Package observability holds the logger and the Prometheus metrics shared by
// the rest of the service.
package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const namespace = "shortlink"

var (
	// HTTPRequests counts requests by route template, method and status class.
	// Labelling by the *template* rather than the raw path is deliberate: the
	// redirect route would otherwise create one time series per short code and
	// blow up cardinality.
	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "http",
		Name:      "requests_total",
		Help:      "Total HTTP requests handled, by route template, method and status code.",
	}, []string{"route", "method", "status"})

	// HTTPDuration observes request latency per route.
	HTTPDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "http",
		Name:      "request_duration_seconds",
		Help:      "HTTP request latency in seconds, by route template and method.",
		Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"route", "method"})

	// CacheLookups counts link cache outcomes: hit, negative_hit, miss,
	// coalesced (served by another goroutine's fetch) and error.
	CacheLookups = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "cache",
		Name:      "lookups_total",
		Help:      "Link cache lookups by outcome.",
	}, []string{"outcome"})

	// LinksCreated counts successfully shortened URLs.
	LinksCreated = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "links_created_total",
		Help:      "Total links created.",
	})

	// CodeCollisions counts random short codes that were already taken. A
	// rising rate means the keyspace is getting crowded and CODE_LENGTH should
	// grow.
	CodeCollisions = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "code_collisions_total",
		Help:      "Generated short codes that collided with an existing one.",
	})

	// Redirects counts redirect attempts by outcome: ok, not_found, expired.
	Redirects = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "redirects_total",
		Help:      "Redirect attempts by outcome.",
	}, []string{"outcome"})

	// ClicksEnqueued counts click events accepted into the collector buffer.
	ClicksEnqueued = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "clicks",
		Name:      "enqueued_total",
		Help:      "Click events accepted into the in-memory buffer.",
	})

	// ClicksDropped counts click events discarded because the buffer was full.
	// This is the signal that analytics is shedding load; the redirect itself
	// still succeeded.
	ClicksDropped = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "clicks",
		Name:      "dropped_total",
		Help:      "Click events dropped because the buffer was full.",
	})

	// ClicksPersisted counts click rows written to Postgres.
	ClicksPersisted = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "clicks",
		Name:      "persisted_total",
		Help:      "Click rows successfully written to the database.",
	})

	// ClickBatchSize observes how many rows each flush wrote.
	ClickBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "clicks",
		Name:      "batch_size",
		Help:      "Rows per click batch flush.",
		Buckets:   []float64{1, 10, 50, 100, 250, 500, 1000},
	})

	// RateLimited counts requests rejected by the token bucket.
	RateLimited = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "rate_limited_total",
		Help:      "Requests rejected by the rate limiter.",
	})
)
