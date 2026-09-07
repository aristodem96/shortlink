package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aristodem96/shortlink/internal/domain"
	"github.com/aristodem96/shortlink/internal/observability"
	"github.com/aristodem96/shortlink/internal/service"
)

// LinkService is the subset of the service layer this transport needs.
type LinkService interface {
	Create(ctx context.Context, in service.CreateLinkInput) (*domain.Link, error)
	Resolve(ctx context.Context, code string) (*domain.Link, error)
	RecordClick(link *domain.Link, meta service.ClickMeta)
	Get(ctx context.Context, ownerID int64, code string) (*domain.Link, error)
	List(ctx context.Context, ownerID int64, limit, offset int) ([]domain.Link, int64, error)
	Delete(ctx context.Context, ownerID int64, code string) error
	Stats(ctx context.Context, ownerID int64, code string, window time.Duration) (*domain.LinkStats, error)
}

// maxRequestBody caps how much of a request body is read. Without it a client
// could stream gigabytes into json.Decode.
const maxRequestBody = 16 << 10 // 16 KiB

type linkHandler struct {
	svc        LinkService
	log        *slog.Logger
	baseURL    string
	trustProxy bool
}

// createLinkRequest is the body of POST /api/v1/links.
type createLinkRequest struct {
	TargetURL string     `json:"target_url"`
	Alias     string     `json:"alias,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// linkResponse is how a link is rendered to clients.
type linkResponse struct {
	Code      string     `json:"code"`
	ShortURL  string     `json:"short_url"`
	TargetURL string     `json:"target_url"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type listLinksResponse struct {
	Items  []linkResponse `json:"items"`
	Total  int64          `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

func (h *linkHandler) toResponse(l *domain.Link) linkResponse {
	return linkResponse{
		Code:      l.Code,
		ShortURL:  h.baseURL + "/" + l.Code,
		TargetURL: l.TargetURL,
		ExpiresAt: l.ExpiresAt,
		CreatedAt: l.CreatedAt,
	}
}

// create handles POST /api/v1/links.
func (h *linkHandler) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	key, ok := apiKeyFromContext(ctx)
	if !ok {
		writeError(ctx, w, h.log, domain.ErrUnauthorized)
		return
	}

	var req createLinkRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(ctx, w, h.log, err)
		return
	}

	link, err := h.svc.Create(ctx, service.CreateLinkInput{
		OwnerID:   key.ID,
		TargetURL: req.TargetURL,
		Alias:     req.Alias,
		ExpiresAt: req.ExpiresAt,
	})
	if err != nil {
		writeError(ctx, w, h.log, err)
		return
	}

	resp := h.toResponse(link)
	w.Header().Set("Location", resp.ShortURL)
	writeJSON(ctx, w, h.log, http.StatusCreated, resp)
}

// list handles GET /api/v1/links.
func (h *linkHandler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	key, ok := apiKeyFromContext(ctx)
	if !ok {
		writeError(ctx, w, h.log, domain.ErrUnauthorized)
		return
	}

	limit, err := intQuery(r, "limit", 20)
	if err != nil {
		writeError(ctx, w, h.log, err)
		return
	}
	offset, err := intQuery(r, "offset", 0)
	if err != nil {
		writeError(ctx, w, h.log, err)
		return
	}

	links, total, err := h.svc.List(ctx, key.ID, limit, offset)
	if err != nil {
		writeError(ctx, w, h.log, err)
		return
	}

	// Rendered as an empty array rather than null, so clients can iterate the
	// result without a nil check.
	items := make([]linkResponse, 0, len(links))
	for i := range links {
		items = append(items, h.toResponse(&links[i]))
	}

	writeJSON(ctx, w, h.log, http.StatusOK, listLinksResponse{
		Items: items, Total: total, Limit: limit, Offset: offset,
	})
}

// get handles GET /api/v1/links/{code}.
func (h *linkHandler) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	key, ok := apiKeyFromContext(ctx)
	if !ok {
		writeError(ctx, w, h.log, domain.ErrUnauthorized)
		return
	}

	link, err := h.svc.Get(ctx, key.ID, chi.URLParam(r, "code"))
	if err != nil {
		writeError(ctx, w, h.log, err)
		return
	}
	writeJSON(ctx, w, h.log, http.StatusOK, h.toResponse(link))
}

// remove handles DELETE /api/v1/links/{code}.
func (h *linkHandler) remove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	key, ok := apiKeyFromContext(ctx)
	if !ok {
		writeError(ctx, w, h.log, domain.ErrUnauthorized)
		return
	}

	if err := h.svc.Delete(ctx, key.ID, chi.URLParam(r, "code")); err != nil {
		writeError(ctx, w, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// stats handles GET /api/v1/links/{code}/stats.
func (h *linkHandler) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	key, ok := apiKeyFromContext(ctx)
	if !ok {
		writeError(ctx, w, h.log, domain.ErrUnauthorized)
		return
	}

	days, err := intQuery(r, "days", 30)
	if err != nil {
		writeError(ctx, w, h.log, err)
		return
	}
	if days < 1 || days > 365 {
		writeError(ctx, w, h.log, &domain.ValidationError{
			Field: "days", Reason: "must be between 1 and 365",
		})
		return
	}

	stats, err := h.svc.Stats(ctx, key.ID, chi.URLParam(r, "code"), time.Duration(days)*24*time.Hour)
	if err != nil {
		writeError(ctx, w, h.log, err)
		return
	}
	writeJSON(ctx, w, h.log, http.StatusOK, stats)
}

// redirect handles GET /{code}: the hot path.
func (h *linkHandler) redirect(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := chi.URLParam(r, "code")

	link, err := h.svc.Resolve(ctx, code)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		observability.Redirects.WithLabelValues("not_found").Inc()
		writeError(ctx, w, h.log, err)
		return
	case errors.Is(err, domain.ErrGone):
		observability.Redirects.WithLabelValues("expired").Inc()
		writeError(ctx, w, h.log, err)
		return
	case err != nil:
		writeError(ctx, w, h.log, err)
		return
	}

	h.svc.RecordClick(link, service.ClickMeta{
		Referer:   r.Referer(),
		UserAgent: r.UserAgent(),
		IP:        clientIP(r, h.trustProxy),
	})

	// 302 rather than 301: a permanent redirect is cached by browsers
	// indefinitely, which would make the link impossible to retarget or delete
	// and would stop click statistics after the first visit.
	w.Header().Set("Cache-Control", "no-store")
	// Short links are shared publicly; do not leak the short code to the
	// destination through the Referer header.
	w.Header().Set("Referrer-Policy", "no-referrer")

	observability.Redirects.WithLabelValues("ok").Inc()
	// Redirecting to a user-supplied target is the entire purpose of this
	// service, so the open-redirect warning does not apply. The target was
	// validated by domain.NormalizeTargetURL when the link was created: an
	// http(s) scheme, and never a private, loopback or link-local address.
	//nolint:gosec // G710: intentional redirect to a previously validated target.
	http.Redirect(w, r, link.TargetURL, http.StatusFound)
}

// decodeJSON reads a bounded, strictly typed JSON body.
//
// DisallowUnknownFields turns a typo in a client's payload into a clear 400
// instead of a silently ignored field.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var maxBytesErr *http.MaxBytesError
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError

		switch {
		case errors.As(err, &maxBytesErr):
			return &domain.ValidationError{Field: "body", Reason: "request body is too large"}
		case errors.As(err, &syntaxErr):
			return &domain.ValidationError{
				Field:  "body",
				Reason: fmt.Sprintf("malformed JSON at byte %d", syntaxErr.Offset),
			}
		case errors.As(err, &typeErr):
			return &domain.ValidationError{
				Field:  typeErr.Field,
				Reason: fmt.Sprintf("must be a %s", typeErr.Type),
			}
		case errors.Is(err, io.EOF):
			return &domain.ValidationError{Field: "body", Reason: "must not be empty"}
		default:
			return &domain.ValidationError{Field: "body", Reason: "is not valid JSON"}
		}
	}

	// A second value in the stream means the client sent something like
	// `{...}{...}`; accepting it silently would hide a bug on their side.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &domain.ValidationError{Field: "body", Reason: "must contain a single JSON object"}
	}
	return nil
}

func intQuery(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}

	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, &domain.ValidationError{Field: name, Reason: "must be an integer"}
	}
	return n, nil
}
