// Package store is Quai's PostgreSQL storage: a connection pool, the embedded migrations and
// the typed queries sqlc generates from internal/store/queries into internal/store/db
// (docs/engineering/backend.md). Queries are written by hand in SQL; there is no ORM.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/damhau/quai/backend/internal/store/db"
	"github.com/damhau/quai/backend/migrations"
)

// Store is a connection pool with its queries.
type Store struct {
	Pool *pgxpool.Pool
	*db.Queries
}

// Open connects to the database at url and checks that it answers.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: %w", err)
	}
	return &Store{Pool: pool, Queries: db.New(pool)}, nil
}

// Close closes the pool.
func (s *Store) Close() { s.Pool.Close() }

// Migrate applies the migrations that are not applied yet and returns how many it applied.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	sqldb := stdlib.OpenDBFromPool(pool)
	defer sqldb.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, sqldb, migrations.FS)
	if err != nil {
		return 0, fmt.Errorf("migrations: %w", err)
	}
	results, err := p.Up(ctx)
	if err != nil {
		return 0, fmt.Errorf("migrations: %w", err)
	}
	return len(results), nil
}

// Tx runs fn in a transaction, committed when fn returns nil and rolled back otherwise. A state
// change and the jobs it triggers are written in one transaction.
func (s *Store) Tx(ctx context.Context, fn func(tx pgx.Tx, q *db.Queries) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx, s.Queries.WithTx(tx)); err != nil {
		if rb := tx.Rollback(ctx); rb != nil && !errors.Is(rb, pgx.ErrTxClosed) {
			return errors.Join(err, rb)
		}
		return err
	}
	return tx.Commit(ctx)
}
