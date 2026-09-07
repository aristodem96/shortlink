package postgres

import (
	"context"
	"errors"
	"fmt"

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
		SELECT id, name, created_at, revoked_at
		FROM api_keys
		WHERE key_hash = $1`

	var key domain.APIKey
	err := r.pool.QueryRow(ctx, query, hash).
		Scan(&key.ID, &key.Name, &key.CreatedAt, &key.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get api key: %w", err)
	}
	return &key, nil
}

// Create stores a new API key.
func (r *APIKeyRepository) Create(ctx context.Context, name string, hash []byte) (*domain.APIKey, error) {
	const query = `
		INSERT INTO api_keys (name, key_hash)
		VALUES ($1, $2)
		RETURNING id, name, created_at, revoked_at`

	var key domain.APIKey
	err := r.pool.QueryRow(ctx, query, name, hash).
		Scan(&key.ID, &key.Name, &key.CreatedAt, &key.RevokedAt)
	if isUniqueViolation(err) {
		return nil, domain.ErrAlreadyExists
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: create api key: %w", err)
	}
	return &key, nil
}
