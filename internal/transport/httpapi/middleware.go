package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"shortlink/internal/domain"
	"shortlink/internal/observability"
)

type ctxKey int

const apiKeyCtxKey ctxKey = iota

// RateLimiter decides whether a caller may proceed. The interface lives here,
// on the consumer side, so the transport does not depend on the Redis package.
type RateLimiter interface {
	Allow(ctx context.Context, key string) (allowed bool, remaining int, retryAfter time.Duration)
}

// Authenticator resolves an API key secret to its owner.
type Authenticator interface {
	Authenticate(ctx context.Context, secret string) (*domain.APIKey, error)
}

// requestID assigns every request an identifier, echoes it in the response and
// puts it on the context so log lines can be joined into one trace.
//
// An inbound X-Request-Id is honoured, which lets a gateway or a caller
// correlate across services -- but only if it is short and printable, so a
// client cannot inject newlines into the logs through it.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if !isSafeRequestID(id) {
			id = newRequestID()
		}

		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(observability.WithRequestID(r.Context(), id)))
	})
}

func isSafeRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		isAllowed := c == '-' || c == '_' ||
			(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !isAllowed {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Uniqueness is a convenience here, not a guarantee worth failing over.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf[:])
}

// requestLogger emits one structured line per request.
func requestLogger(log *slog.Logger, trustProxy bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			// Log at a level that matches the outcome, so an alert on
			// level=error is not drowned by 404s from bots.
			level := slog.LevelInfo
			if ww.Status() >= http.StatusInternalServerError {
				level = slog.LevelError
			}

			log.Log(r.Context(), level, "request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route", routePattern(r)),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("ip", clientIP(r, trustProxy)),
			)
		})
	}
}

// metrics records request counts and latency, labelled by the *route template*
// rather than the path. Labelling by path would mint one time series per short
// code and take the metrics backend down with it.
func metrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		route := routePattern(r)
		observability.HTTPRequests.WithLabelValues(route, r.Method, strconv.Itoa(ww.Status())).Inc()
		observability.HTTPDuration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	})
}

func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if pattern := rctx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return "unmatched"
}

// recoverer turns a panic in a handler into a 500 instead of a dropped
// connection, and logs the stack.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			//nolint:contextcheck // The deferred closure uses r.Context(); the
			// linter cannot see through the defer.
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is the documented way for a handler to
				// abort a connection on purpose; re-panic so the server sees it.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}

				log.ErrorContext(r.Context(), "handler panicked",
					slog.Any("panic", rec),
					slog.String("path", r.URL.Path),
					slog.String("stack", stackTrace()),
				)
				writeErrorDetail(r.Context(), w, log, http.StatusInternalServerError, errorDetail{
					Code: "internal_error", Message: "something went wrong on our side",
				})
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// authenticate requires a valid API key and puts it on the request context.
func authenticate(auth Authenticator, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			secret, ok := bearerToken(r)
			if !ok {
				// Advertising the scheme is what makes a 401 actionable.
				w.Header().Set("WWW-Authenticate", `Bearer realm="shortlink"`)
				writeError(ctx, w, log, domain.ErrUnauthorized)
				return
			}

			key, err := auth.Authenticate(ctx, secret)
			if err != nil {
				writeError(ctx, w, log, err)
				return
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, apiKeyCtxKey, key)))
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "

	header := r.Header.Get("Authorization")
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

// apiKeyFromContext returns the authenticated key. It is only called from
// handlers behind the authenticate middleware, so a missing key is a
// programming error rather than a client one.
func apiKeyFromContext(ctx context.Context) (*domain.APIKey, bool) {
	key, ok := ctx.Value(apiKeyCtxKey).(*domain.APIKey)
	return key, ok
}

// rateLimit applies the token bucket, keyed by API key when the caller is
// authenticated and by client IP otherwise. Keying by API key means one noisy
// tenant cannot spend the quota of everyone behind the same NAT.
func rateLimit(limiter RateLimiter, log *slog.Logger, trustProxy bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			bucket := "ip:" + clientIP(r, trustProxy)
			if key, ok := apiKeyFromContext(ctx); ok {
				bucket = "key:" + strconv.FormatInt(key.ID, 10)
			}

			allowed, remaining, retryAfter := limiter.Allow(ctx, bucket)
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))

			if !allowed {
				observability.RateLimited.Inc()
				seconds := int(retryAfter.Seconds())
				if seconds < 1 {
					seconds = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				writeError(ctx, w, log, domain.ErrRateLimited)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// clientIP returns the caller's address.
//
// X-Forwarded-For is honoured only when the deployment is known to sit behind a
// trusted proxy, because the header is client-controlled: trusting it
// unconditionally would let anyone forge an address and evade the per-IP rate
// limit by rotating a header value.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		// The left-most entry is the original client; everything after it was
		// appended by intermediaries.
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
		if real := strings.TrimSpace(r.Header.Get("X-Real-Ip")); real != "" {
			return real
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// stackTrace returns the panicking goroutine's stack, capped so a runaway
// recursion cannot write a megabyte into the logs.
func stackTrace() string {
	const maxStack = 8 << 10

	stack := debug.Stack()
	if len(stack) > maxStack {
		stack = stack[:maxStack]
	}
	return string(stack)
}
