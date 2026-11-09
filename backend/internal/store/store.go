// Package store is Quai's PostgreSQL storage: a connection pool (and one for advisory locks),
// the embedded migrations and the typed queries sqlc generates from internal/store/queries into
// internal/store/db (docs/engineering/backend.md). Queries are written by hand in SQL; there is
// no ORM.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
	events EventHook
}

// EventHook is told of each event RecordEvent adds to a timeline, in the transaction that adds
// it: notifications enqueue their jobs from it, so that an event is notified if and only if it
// is committed (docs/design/05-delivery-and-approvals.md#notifications). An error rolls the
// transaction back.
type EventHook func(ctx context.Context, tx pgx.Tx, e db.Event) error

// UseEvents makes hook the one told of recorded events; nil: none. Set it before the store is
// used.
func (s *Store) UseEvents(hook EventHook) { s.events = hook }

// RecordEvent adds an event to a timeline in tx, and tells the event hook.
func (s *Store) RecordEvent(ctx context.Context, tx pgx.Tx, p db.AddEventParams) (db.Event, error) {
	e, err := s.Queries.WithTx(tx).AddEvent(ctx, p)
	if err != nil || s.events == nil {
		return e, err
	}
	return e, s.events(ctx, tx, e)
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
var AuditTables = []string{"audit_events", "audit_personal", "audit_checkpoints"}

// auditInsertColumns are the columns of an audit table the application role may write, when not
// all of them: audit_events.recorded is the database's clock, which pruning judges an event's age
// by (open question 36), so the role cannot set it. A column added to the table later is the
// database's until it is listed here.
var auditInsertColumns = map[string][]string{
	"audit_events": {"seq", "at", "actor", "action", "object", "details", "prev", "hash"},
}

// readOnlyTables are the migrations' bookkeeping and the audit log's retention, which only quai
// migrate writes.
var readOnlyTables = []string{"goose_db_version", "river_migration", "audit_retention"}

// auditFunctions are the schema owner's functions the application role may execute: pruning the
// audit log as the retention allows (open question 19).
var auditFunctions = []string{"audit_prune()"}

// Grant gives the application role what the server and the worker need, from the schema owner's
// connection pool, after the migrations (open question 19): every table to read and write, but
// the audit log's to read and append only (audit_events without its recorded column), the
// migrations' bookkeeping and the audit log's retention to read only, and the pruning function to
// execute. It revokes the rest from the role, and the audit log from PUBLIC.
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
		tn := pgx.Identifier{t}.Sanitize()
		// Revoking a table's privileges revokes its columns' too.
		stmts = append(stmts, "REVOKE ALL ON "+tn+" FROM PUBLIC", "REVOKE ALL ON "+tn+" FROM "+r)
		if cols, ok := auditInsertColumns[t]; ok {
			ids := make([]string, len(cols))
			for i, c := range cols {
				ids[i] = pgx.Identifier{c}.Sanitize()
			}
			stmts = append(stmts, "GRANT SELECT ON "+tn+" TO "+r, "GRANT INSERT ("+strings.Join(ids, ", ")+") ON "+tn+" TO "+r)
		} else {
			stmts = append(stmts, "GRANT SELECT, INSERT ON "+tn+" TO "+r)
		}
	}
	for _, f := range auditFunctions {
		stmts = append(stmts, "REVOKE ALL ON FUNCTION "+f+" FROM PUBLIC", "GRANT EXECUTE ON FUNCTION "+f+" TO "+r)
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

// SetAuditRetention sets how long audit events are kept, from the schema owner's connection pool
// (quai migrate), unless it is set already: the application role may only read it, and pruning
// deletes nothing younger.
func SetAuditRetention(ctx context.Context, pool *pgxpool.Pool, d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("audit retention: %s is not a duration", d)
	}
	want := pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
	if got, err := db.New(pool).AuditRetention(ctx); err == nil && got == want {
		return nil
	}
	if _, err := pool.Exec(ctx, "UPDATE audit_retention SET retention = $1", want); err != nil {
		return fmt.Errorf("audit retention: %w", err)
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
