// Package httpapi is the HTTP transport: routing, middleware and the
// translation between JSON requests and the service layer.
//
// It is named httpapi rather than http so that net/http needs no import alias
// anywhere in the package.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"shortlink/internal/domain"
)

// errorResponse is the single error envelope of the API. Having exactly one
// shape means clients can parse failures without branching per endpoint.
type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	// Code is a stable machine-readable identifier, e.g. "not_found".
	Code string `json:"code"`
	// Message is a human-readable explanation.
	Message string `json:"message"`
	// Field names the offending request field, for validation failures.
	Field string `json:"field,omitempty"`
}

func writeJSON(ctx context.Context, w http.ResponseWriter, log *slog.Logger, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already on the wire, so this can only be logged.
		log.WarnContext(ctx, "could not write response body", slog.Any("error", err))
	}
}

func writeErrorDetail(ctx context.Context, w http.ResponseWriter, log *slog.Logger, status int, detail errorDetail) {
	writeJSON(ctx, w, log, status, errorResponse{Error: detail})
}

// writeError maps a domain error onto an HTTP response.
//
// This is the only place that knows both vocabularies, which is what keeps
// status codes out of the service and the domain.
func writeError(ctx context.Context, w http.ResponseWriter, log *slog.Logger, err error) {
	var validationErr *domain.ValidationError
	if errors.As(err, &validationErr) {
		writeErrorDetail(ctx, w, log, http.StatusBadRequest, errorDetail{
			Code:    "validation_error",
			Message: validationErr.Reason,
			Field:   validationErr.Field,
		})
		return
	}

	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeErrorDetail(ctx, w, log, http.StatusNotFound, errorDetail{
			Code: "not_found", Message: "no link with this code",
		})
	case errors.Is(err, domain.ErrAlreadyExists):
		writeErrorDetail(ctx, w, log, http.StatusConflict, errorDetail{
			Code: "already_exists", Message: "this alias is already taken",
		})
	case errors.Is(err, domain.ErrGone):
		writeErrorDetail(ctx, w, log, http.StatusGone, errorDetail{
			Code: "expired", Message: "this link has expired",
		})
	case errors.Is(err, domain.ErrUnauthorized):
		writeErrorDetail(ctx, w, log, http.StatusUnauthorized, errorDetail{
			Code: "unauthorized", Message: "a valid API key is required",
		})
	case errors.Is(err, domain.ErrForbidden):
		writeErrorDetail(ctx, w, log, http.StatusForbidden, errorDetail{
			Code: "forbidden", Message: "this key may not access that resource",
		})
	case errors.Is(err, domain.ErrRateLimited):
		writeErrorDetail(ctx, w, log, http.StatusTooManyRequests, errorDetail{
			Code: "rate_limited", Message: "too many requests, slow down",
		})
	case errors.Is(err, context.Canceled):
		// The client hung up; nothing useful can be written back.
		log.DebugContext(ctx, "client cancelled the request")
	default:
		// The cause is logged, never returned: internal errors routinely carry
		// table names, DSNs and query fragments.
		log.ErrorContext(ctx, "unhandled error", slog.Any("error", err))
		writeErrorDetail(ctx, w, log, http.StatusInternalServerError, errorDetail{
			Code: "internal_error", Message: "something went wrong on our side",
		})
	}
}
