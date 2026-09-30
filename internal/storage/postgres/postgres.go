// Package postgres implements the durable storage ports on top of PostgreSQL
// using pgx.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/aristodem96/shortlink/migrations"
)

// uniqueViolation is the SQLSTATE Postgres raises for a broken UNIQUE
// constraint. Matching on the code rather than the message keeps the mapping
// stable across server versions and locales.
const uniqueViolation = "23505"

// NewPool opens and verifies a connection pool.
func NewPool(ctx context.Context, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 1
	// Recycling connections bounds the damage from a leaked session state and
	// lets a rolling database upgrade drain old backends.
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies the embedded migrations. It is safe to run on every start:
// goose takes an advisory lock, so concurrently starting replicas do not race.
func Migrate(ctx context.Context, dsn string, log *slog.Logger) error {
	// goose speaks database/sql, so the pgx driver is wrapped in its stdlib
	// adapter for the duration of the migration and closed straight after.
	connCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("postgres: parse dsn for migrations: %w", err)
	}

	db := stdlib.OpenDB(*connCfg)
	defer func() {
		if err := db.Close(); err != nil {
			log.Warn("could not close migration connection", slog.Any("error", err))
		}
	}()

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("postgres: set goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("postgres: apply migrations: %w", err)
	}

	version, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return fmt.Errorf("postgres: read schema version: %w", err)
	}
	log.Info("database schema is up to date", slog.Int64("version", version))
	return nil
}

// isUniqueViolation reports whether err is a broken UNIQUE constraint.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}
