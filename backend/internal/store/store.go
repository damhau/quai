// Package store is Quai's PostgreSQL storage: a connection pool (and one for advisory locks),
// the embedded migrations and the typed queries sqlc generates from internal/store/queries into
// internal/store/db (docs/engineering/backend.md). Queries are written by hand in SQL; there is
// no ORM.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/damhau/quai/backend/internal/store/db"
	"github.com/damhau/quai/backend/migrations"
)

// Store is a connection pool with its queries, and the pool advisory locks are held on.
type Store struct {
	Pool  *pgxpool.Pool
	Locks *Locks
	*db.Queries
}

// Locks is the pool PostgreSQL's advisory locks are held on (jobs.WithLock,
// jobs.WithRepositoryLock), apart from Store.Pool: a lock keeps its connection until it is
// released, while the work under it queries Store.Pool. Held on Store.Pool's connections, a few
// locks at once would leave none to that work or to the job queue, each holder waiting for a
// connection another holds until its job times out.
type Locks struct{ Pool *pgxpool.Pool }

// Open connects to the database at url and checks that it answers. The locks' pool has the
// same configuration, pool_max_conns included, and connects when a lock is first taken.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: %w", err)
	}
	locks, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: %w", err)
	}
	return &Store{Pool: pool, Locks: &Locks{Pool: locks}, Queries: db.New(pool)}, nil
}

// Close closes the pools.
func (s *Store) Close() {
	s.Pool.Close()
	s.Locks.Pool.Close()
}

// Migrate applies the migrations that are not applied yet, Quai's and then those of the River
// job queue, and returns how many it applied.
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
	m, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return 0, fmt.Errorf("job queue migrations: %w", err)
	}
	river, err := m.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return 0, fmt.Errorf("job queue migrations: %w", err)
	}
	return len(results) + len(river.Versions), nil
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
