// Package postgres implements payment.Store and tenant lookup on PostgreSQL.
package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/tancon23/unified-payments-assessment/internal/payment"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate applies all pending migrations. An advisory lock lets the api and
// the worker both call it at startup without racing.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	// Retry the lock every second for up to five minutes.
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 300))
	if err != nil {
		return err
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, dir, goose.WithSessionLocker(locker))
	if err != nil {
		return err
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("applying migrations: %w", err)
	}
	return nil
}

type Store struct {
	pool *pgxpool.Pool
}

var _ payment.Store = (*Store)(nil)

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Tx(ctx context.Context, fn func(payment.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(&txStore{tx: tx})
	})
}

// TenantForAPIKey resolves an API key to its tenant. Keys are stored as
// SHA-256 hashes; they are long random strings, so no salt is needed.
func (s *Store) TenantForAPIKey(ctx context.Context, apiKey string) (string, bool, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM tenants WHERE api_key_hash = $1`, hashAPIKey(apiKey)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// UpsertTenant creates a tenant or replaces its name and API key.
func (s *Store) UpsertTenant(ctx context.Context, id, name, apiKey string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tenants (id, name, api_key_hash) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, api_key_hash = EXCLUDED.api_key_hash`,
		id, name, hashAPIKey(apiKey))
	return err
}

func hashAPIKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}
