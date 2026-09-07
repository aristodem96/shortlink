package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors of the domain. Transport layers map these onto protocol
// specific responses (see internal/transport/http/response.go), which keeps the
// domain free of any knowledge about HTTP status codes.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrGone          = errors.New("link expired")
	ErrUnauthorized  = errors.New("unauthorized")
	ErrForbidden     = errors.New("forbidden")
	ErrRateLimited   = errors.New("rate limit exceeded")

	// ErrCacheMiss means a cache holds no entry for a key at all. It is
	// distinct from ErrNotFound, which a cache returns for a negatively cached
	// key: "this was looked up before and it does not exist".
	ErrCacheMiss = errors.New("cache miss")
)

// ValidationError describes a single invalid input field. It wraps no sentinel
// on purpose: callers detect it with errors.As, which also gives them the field
// name to report back to the client.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Reason)
}

func invalid(field, reason string) error {
	return &ValidationError{Field: field, Reason: reason}
}
