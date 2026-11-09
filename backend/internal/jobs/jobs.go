// Package jobs runs Quai's background work with River, a job queue in PostgreSQL
// (docs/adr/0007-postgresql-for-state-and-jobs.md). A state change and the jobs it triggers are
// inserted in one transaction (InsertTx), so a crash can never leave a request submitted without
// its job. Every job can run twice: a worker killed mid-job is rescued and its job runs again,
// so workers check the current state before acting.
//
// Jobs that change a repository take its advisory lock (WithRepositoryLock): one at a time per
// repository, the others snoozed rather than failed. Locks are held on a pool of their own
// (store.Locks), so that holders never take the connections their work and River need.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Client is a River client on Quai's database.
type Client = river.Client[pgx.Tx]

// Options configures a worker client.
type Options struct {
	Logger     *slog.Logger
	MaxWorkers int // jobs run at once; 10 when zero
	// Workers adds the workers of other packages to the built-in ones, and Periodic their
	// periodic jobs.
	Workers  func(*river.Workers)
	Periodic []*river.PeriodicJob
	// RescueStuckJobsAfter is how long a job may stay running before it is considered abandoned
	// by a worker that died, and run again. One hour when zero.
	RescueStuckJobsAfter time.Duration
	// JobTimeout bounds one run of a job. One minute when zero; -1 for none.
	JobTimeout time.Duration
}

// QueueNotifications is the queue of notifications: an SMTP server that hangs takes its few workers,
// never those of the default queue, so a notification never holds a request back.
const QueueNotifications = "notifications"

const notificationWorkers = 4

// NewWorker returns a client that works jobs and runs the periodic ones. Start it, and Stop it
// to let jobs in flight finish.
func NewWorker(pool *pgxpool.Pool, opts Options) (*Client, error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, &cleanupSessions{q: db.New(pool)})
	if opts.Workers != nil {
		opts.Workers(workers)
	}
	maxWorkers := opts.MaxWorkers
	if maxWorkers == 0 {
		maxWorkers = 10
	}
	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger: opts.Logger,
		Queues: map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: maxWorkers},
			QueueNotifications: {MaxWorkers: notificationWorkers}},
		Workers:              workers,
		RescueStuckJobsAfter: opts.RescueStuckJobsAfter,
		JobTimeout:           opts.JobTimeout,
		PeriodicJobs: append([]*river.PeriodicJob{
			river.NewPeriodicJob(river.PeriodicInterval(time.Hour), func() (river.JobArgs, *river.InsertOpts) {
				return CleanupSessionsArgs{}, nil
			}, &river.PeriodicJobOpts{RunOnStart: true}),
		}, opts.Periodic...),
	})
}

// NewInserter returns a client that only inserts jobs, for the server process.
func NewInserter(pool *pgxpool.Pool, logger *slog.Logger) (*Client, error) {
	return river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: logger})
}

// InsertTx inserts a job in tx, so that it exists if and only if tx commits.
func InsertTx(ctx context.Context, c *Client, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) error {
	if _, err := c.InsertTx(ctx, tx, args, opts); err != nil {
		return fmt.Errorf("insert %s job: %w", args.Kind(), err)
	}
	return nil
}

// ErrLocked is wrapped in the snooze error WithRepositoryLock returns when the lock is taken.
var ErrLocked = errors.New("another job holds the repository's lock")

// WithRepositoryLock runs fn while holding the repository's advisory lock in PostgreSQL. When
// another job holds it, it returns River's snooze error: the job runs again after snooze,
// without counting as a failed attempt. The lock is held on a connection of the locks' pool
// (store.Locks) for the duration of fn and released after it, or by PostgreSQL if the worker
// dies.
func WithRepositoryLock(ctx context.Context, locks *store.Locks, repository string, snooze time.Duration, fn func(context.Context) error) error {
	ran, err := tryLocked(ctx, locks, lockKey(repository), fn)
	if err == nil && !ran {
		return errors.Join(river.JobSnooze(snooze), ErrLocked)
	}
	return err
}

// WithLock runs fn holding the advisory lock named name, waiting for it: work that must not
// interleave, such as reading a request's state and publishing its status. A waiter holds no
// connection: it tries again after a pause that doubles up to lockRetryMax, until ctx is done.
func WithLock(ctx context.Context, locks *store.Locks, name string, fn func(context.Context) error) error {
	key := nameKey(name)
	pause := lockRetryMin
	for {
		ran, err := tryLocked(ctx, locks, key, fn)
		if ran || err != nil {
			return err
		}
		t := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		pause = min(2*pause, lockRetryMax)
	}
}

// TryLock runs fn holding the advisory lock named name if no one holds it, and reports whether it
// did: periodic work that another worker is doing already, such as exporting the audit log to a
// destination, is skipped rather than waited for.
func TryLock(ctx context.Context, locks *store.Locks, name string, fn func(context.Context) error) (bool, error) {
	return tryLocked(ctx, locks, nameKey(name), fn)
}

// nameKey maps a lock's name to the 64-bit key of its advisory lock.
func nameKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64()) // #nosec G115 -- any 64 bits make a key
}

// The pauses of a waiter for a lock (WithLock).
const (
	lockRetryMin = 20 * time.Millisecond
	lockRetryMax = time.Second
)

// tryLocked runs fn holding the advisory lock key if no one holds it, and reports whether it
// did. Locks do not nest: a holder taking another lock would hold two of the locks' connections,
// and holders waiting for a second one could take them all.
func tryLocked(ctx context.Context, locks *store.Locks, key int64, fn func(context.Context) error) (bool, error) {
	conn, err := locks.Pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&locked); err != nil {
		return false, err
	}
	if !locked {
		return false, nil
	}
	defer func() {
		// The connection goes back to the pool: release the lock even if ctx is done. A session
		// takes its own advisory locks again, so a connection the lock may still be held on is
		// closed instead, which releases it.
		bg := context.WithoutCancel(ctx)
		if _, err := conn.Exec(bg, "SELECT pg_advisory_unlock($1)", key); err != nil {
			_ = conn.Conn().Close(bg)
		}
	}()
	return true, fn(ctx)
}

// lockKey maps a repository to the 64-bit key of its advisory lock.
func lockKey(repository string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("quai/repository/" + repository))
	return int64(h.Sum64()) // #nosec G115 -- any 64 bits make a key
}

// CleanupSessionsArgs deletes expired sessions. It runs at start and every hour.
type CleanupSessionsArgs struct{}

// Kind names the job.
func (CleanupSessionsArgs) Kind() string { return "cleanup_sessions" }

type cleanupSessions struct {
	river.WorkerDefaults[CleanupSessionsArgs]
	q *db.Queries
}

func (w *cleanupSessions) Work(ctx context.Context, _ *river.Job[CleanupSessionsArgs]) error {
	_, err := w.q.DeleteExpiredSessions(ctx)
	return err
}
