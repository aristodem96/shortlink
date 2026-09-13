package domain

import "time"

// Click is a single redirect event. It is written asynchronously and in
// batches, so the redirect hot path never waits on analytics.
//
// The visitor IP is stored as a salted hash: aggregate "unique visitors" stays
// possible while the raw address never lands in the database.
type Click struct {
	LinkID    int64
	ClickedAt time.Time
	Referer   string
	UserAgent string
	IPHash    []byte
}

// LinkStats is the aggregated view of a link's traffic over a time window.
type LinkStats struct {
	Code string `json:"code"`
	// Host is the custom domain the link is served on, empty for the
	// operator's own. It rides along so callers can render the link's public
	// address without loading it a second time.
	Host           string       `json:"-"`
	TotalClicks    int64        `json:"total_clicks"`
	UniqueVisitors int64        `json:"unique_visitors"`
	LastClickAt    *time.Time   `json:"last_click_at,omitempty"`
	Daily          []DailyCount `json:"daily"`
	TopReferers    []RefererHit `json:"top_referers"`
}

// DailyCount is one bucket of the per-day click histogram.
type DailyCount struct {
	Day    string `json:"day"` // YYYY-MM-DD, UTC
	Clicks int64  `json:"clicks"`
}

// RefererHit counts clicks coming from one referring host.
type RefererHit struct {
	Referer string `json:"referer"`
	Clicks  int64  `json:"clicks"`
}
