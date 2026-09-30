package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aristodem96/shortlink/internal/domain"
)

// APIKeyRepository stores API keys in PostgreSQL. It satisfies
// service.APIKeyRepository.
type APIKeyRepository struct {
	pool *pgxpool.Pool
}

// NewAPIKeyRepository wires an APIKeyRepository.
func NewAPIKeyRepository(pool *pgxpool.Pool) *APIKeyRepository {
	return &APIKeyRepository{pool: pool}
}

// GetByHash resolves a key by the digest of its secret. The plaintext secret
// never reaches this layer.
func (r *APIKeyRepository) GetByHash(ctx context.Context, hash []byte) (*domain.APIKey, error) {
	const query = `
		SELECT ` + apiKeyColumns + `
		FROM api_keys
		WHERE key_hash = $1`

	var key domain.APIKey
	err := r.pool.QueryRow(ctx, query, hash).Scan(apiKeyDest(&key)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get api key: %w", err)
	}
	return &key, nil
}

// Create stores a new API key.
func (r *APIKeyRepository) Create(ctx context.Context, name string, hash []byte, expiresAt *time.Time) (*domain.APIKey, error) {
	const query = `
		INSERT INTO api_keys (name, key_hash, expires_at)
		VALUES ($1, $2, $3)
		RETURNING ` + apiKeyColumns

	var key domain.APIKey
	err := r.pool.QueryRow(ctx, query, name, hash, expiresAt).Scan(apiKeyDest(&key)...)
	if isUniqueViolation(err) {
		return nil, domain.ErrAlreadyExists
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: create api key: %w", err)
	}
	return &key, nil
}

// apiKeyColumns and apiKeyDest are kept side by side so the column list and the
// scan targets cannot drift apart: every query that returns a key uses both.
//
//nolint:gosec // G101: a column list, not a credential; the word "key" is what trips the check.
const apiKeyColumns = `id, name, created_at, revoked_at, expires_at, telegram_user_id`

func apiKeyDest(k *domain.APIKey) []any {
	return []any{&k.ID, &k.Name, &k.CreatedAt, &k.RevokedAt, &k.ExpiresAt, &k.TelegramUserID}
}
