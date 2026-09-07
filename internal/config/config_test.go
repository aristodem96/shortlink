package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setMinimalEnv sets only the variables that have no safe default.
func setMinimalEnv(t *testing.T) {
	t.Helper()
	t.Setenv("IP_HASH_SALT", "test-salt")
}

func TestLoad_AppliesDefaults(t *testing.T) {
	setMinimalEnv(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, ":8080", cfg.HTTP.Addr)
	assert.Equal(t, 7, cfg.CodeLength)
	assert.Equal(t, time.Hour, cfg.Cache.TTL)
	assert.Equal(t, 30*time.Second, cfg.Cache.NegativeTTL)
	assert.Equal(t, "json", cfg.Log.Format)
}

func TestLoad_TrimsTrailingSlashFromBaseURL(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("BASE_URL", "https://sho.rt/")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "https://sho.rt", cfg.BaseURL)
}

func TestLoad_RequiresIPHashSalt(t *testing.T) {
	// Deliberately no IP_HASH_SALT: hashing IPs without a salt is reversible.
	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "IP_HASH_SALT")
}

func TestLoad_ReportsAllProblemsAtOnce(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("CODE_LENGTH", "1")
	t.Setenv("RATE_LIMIT_RPS", "0")
	t.Setenv("LOG_LEVEL", "verbose")

	_, err := Load()
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "CODE_LENGTH")
	assert.Contains(t, msg, "RATE_LIMIT_RPS")
	assert.Contains(t, msg, "LOG_LEVEL")
}

// TestLoadDatabase_DoesNotRequireServerSettings keeps the operator CLI usable:
// issuing an API key must not demand an IP hash salt, a base URL or rate
// limits that it will never use.
func TestLoadDatabase_DoesNotRequireServerSettings(t *testing.T) {
	t.Setenv("POSTGRES_DSN", "postgres://user:pass@db:5432/shortlink?sslmode=disable")

	db, err := LoadDatabase()
	require.NoError(t, err)
	assert.Equal(t, "postgres://user:pass@db:5432/shortlink?sslmode=disable", db.DSN)
	assert.Equal(t, int32(10), db.MaxConns)

	// The full server configuration, in contrast, still refuses to start.
	_, err = Load()
	assert.Error(t, err)
}

func TestLoadDatabase_RejectsAMalformedPoolSize(t *testing.T) {
	t.Setenv("POSTGRES_MAX_CONNS", "99999999999999999999")

	_, err := LoadDatabase()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "POSTGRES_MAX_CONNS")
}

func TestLoad_RejectsMalformedValues(t *testing.T) {
	setMinimalEnv(t)
	t.Setenv("CACHE_TTL", "one hour")
	t.Setenv("POSTGRES_MAX_CONNS", "many")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CACHE_TTL")
	assert.Contains(t, err.Error(), "POSTGRES_MAX_CONNS")
}
