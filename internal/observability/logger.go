package observability

import (
	"context"
	"fmt"
	"log/slog"
	"os"
)

// contextKey is unexported so no other package can collide with these keys.
type contextKey int

const requestIDKey contextKey = iota

// WithRequestID returns a context carrying the request identifier.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFromContext returns the request identifier, or "" if unset.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// NewLogger builds the process logger. format is "json" or "text".
func NewLogger(level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("observability: unknown log level %q", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}

	var handler slog.Handler
	switch format {
	case "json":
		handler = slog.NewJSONHandler(os.Stdout, opts)
	case "text":
		handler = slog.NewTextHandler(os.Stdout, opts)
	default:
		return nil, fmt.Errorf("observability: unknown log format %q", format)
	}

	// contextHandler copies the request ID onto every record, so a single log
	// line is enough to pull the whole request out of a log aggregator.
	return slog.New(&contextHandler{Handler: handler}), nil
}

// contextHandler decorates another handler with attributes taken from the
// context.
type contextHandler struct{ slog.Handler }

// Handle adds the request ID, when present, and delegates to the wrapped
// handler.
func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestIDFromContext(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs returns a handler that keeps the context decoration. Delegating
// without re-wrapping would silently drop the request ID from every logger
// derived with slog.With.
func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup returns a grouped handler that keeps the context decoration.
func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}
