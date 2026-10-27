// Package jobs runs Quai's background work with River, a job queue in PostgreSQL
// (docs/adr/0007-postgresql-for-state-and-jobs.md). A state change and the jobs it triggers are
// inserted in one transaction (InsertTx), so a crash can never leave a request submitted without
// its job. Every job can run twice: a worker killed mid-job is rescued and its job runs again,
// so workers check the current state before acting.
//
// Jobs that change a repository take its advisory lock (WithRepositoryLock): one at a time per
// repository, the others snoozed rather than failed.
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

	"github.com/damhau/quai/backend/internal/store/db"
)

// Client is a River client on Quai's database.
type Client = river.Client[pgx.Tx]

// Options configures a worker client.
type Options struct {
	Logger     *slog.Logger
	MaxWorkers int // jobs run at once; 10 when zero
	// Workers adds the workers of other packages to the built-in ones.
	Workers func(*river.Workers)
	// RescueStuckJobsAfter is how long a job may stay running before it is considered abandoned
	// by a worker that died, and run again. One hour when zero.
	RescueStuckJobsAfter time.Duration
	// JobTimeout bounds one run of a job. One minute when zero; -1 for none.
	JobTimeout time.Duration
}

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
		Logger:               opts.Logger,
		Queues:               map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: maxWorkers}},
		Workers:              workers,
		RescueStuckJobsAfter: opts.RescueStuckJobsAfter,
		JobTimeout:           opts.JobTimeout,
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(river.PeriodicInterval(time.Hour), func() (river.JobArgs, *river.InsertOpts) {
				return CleanupSessionsArgs{}, nil
			}, &river.PeriodicJobOpts{RunOnStart: true}),
		},
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
// without counting as a failed attempt. The lock is held on one connection for the duration of
// fn and released after it, or by PostgreSQL if the worker dies.
func WithRepositoryLock(ctx context.Context, pool *pgxpool.Pool, repository string, snooze time.Duration, fn func(context.Context) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	key := lockKey(repository)
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return errors.Join(river.JobSnooze(snooze), ErrLocked)
	}
	defer func() {
		// The connection goes back to the pool: release the lock even if ctx is done.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", key)
	}()
	return fn(ctx)
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
