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

	"github.com/damhau/quai/backend/internal/audit"
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

// AuditTables are the tables of the audit log: the application role may only read and append.
var AuditTables = []string{"audit_events", "audit_personal"}

// readOnlyTables are the migrations' bookkeeping, which only quai migrate writes.
var readOnlyTables = []string{"goose_db_version", "river_migration"}

// Grant gives the application role what the server and the worker need, from the schema owner's
// connection pool, after the migrations (open question 19): every table to read and write, but
// the audit log's to read and append only, and the migrations' bookkeeping to read only. It
// revokes the rest from the role, and the audit log from PUBLIC.
func Grant(ctx context.Context, pool *pgxpool.Pool, role string) error {
	r := pgx.Identifier{role}.Sanitize()
	var schema string
	if err := pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		return fmt.Errorf("grants: %w", err)
	}
	sc := pgx.Identifier{schema}.Sanitize()
	stmts := []string{
		"GRANT USAGE ON SCHEMA " + sc + " TO " + r,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA " + sc + " TO " + r,
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA " + sc + " TO " + r,
	}
	for _, t := range readOnlyTables {
		stmts = append(stmts, "REVOKE ALL ON "+pgx.Identifier{t}.Sanitize()+" FROM "+r,
			"GRANT SELECT ON "+pgx.Identifier{t}.Sanitize()+" TO "+r)
	}
	for _, t := range AuditTables {
		stmts = append(stmts, "REVOKE ALL ON "+pgx.Identifier{t}.Sanitize()+" FROM PUBLIC",
			"REVOKE ALL ON "+pgx.Identifier{t}.Sanitize()+" FROM "+r,
			"GRANT SELECT, INSERT ON "+pgx.Identifier{t}.Sanitize()+" TO "+r)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("grants: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, st := range stmts {
		if _, err := tx.Exec(ctx, st); err != nil {
			return fmt.Errorf("grants: %s: %w", st, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("grants: %w", err)
	}
	return nil
}

// Tx runs fn in a transaction, committed when fn returns nil and rolled back otherwise. A state
// change, the jobs it triggers and the audit events that record it (audit.Add with tx) are
// written in one transaction; the audit events last, under the audit log's lock.
func (s *Store) Tx(ctx context.Context, fn func(tx pgx.Tx, q *db.Queries) error) error {
	raw, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	tx := audit.Wrap(raw)
	err = fn(tx, s.Queries.WithTx(raw))
	if err == nil {
		err = tx.Record(ctx)
	}
	if err != nil {
		if rb := raw.Rollback(ctx); rb != nil && !errors.Is(rb, pgx.ErrTxClosed) {
			return errors.Join(err, rb)
		}
		return err
	}
	return raw.Commit(ctx)
}
