package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/store/db"
)

// helperEnv makes the test binary a worker process for TestWorkerKilledMidJob.
const helperEnv = "QUAI_JOBS_TEST_WORKER_DB"

func TestMain(m *testing.M) {
	if url := os.Getenv(helperEnv); url != "" {
		os.Exit(helperWorker(url))
	}
	os.Exit(pgtest.Main(m))
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func pool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	url := pgtest.URL(t)
	p, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p, url
}

// slowArgs is a job whose first attempt never ends by itself; later attempts finish at once.
type slowArgs struct{}

func (slowArgs) Kind() string { return "test_slow" }

type slowWorker struct {
	river.WorkerDefaults[slowArgs]
	q *db.Queries
}

func (w *slowWorker) Work(ctx context.Context, job *river.Job[slowArgs]) error {
	if job.Attempt == 1 {
		if _, err := w.q.AddEvent(ctx, db.AddEventParams{Kind: "slow_started", Data: json.RawMessage(`{}`)}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	_, err := w.q.AddEvent(ctx, db.AddEventParams{Kind: "slow_finished", Data: json.RawMessage(`{}`)})
	return err
}

func start(t *testing.T, p *pgxpool.Pool, opts Options) *Client {
	t.Helper()
	opts.Logger = quiet
	c, err := NewWorker(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.StopAndCancel(ctx)
	})
	return c
}

// wait returns the next event for a job of kind, or fails after timeout.
func wait(t *testing.T, events <-chan *river.Event, kind string, timeout time.Duration) *river.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case e := <-events:
			if e.Job.Kind == kind {
				return e
			}
		case <-deadline:
			t.Fatalf("no event for a %s job within %s", kind, timeout)
		}
	}
}

func TestCleanupSessions(t *testing.T) {
	p, _ := pool(t)
	ctx := context.Background()
	if _, err := p.Exec(ctx, `INSERT INTO sessions (token, data, expiry) VALUES
		('old', '\x00', now() - interval '1 minute'), ('live', '\x00', now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	c, err := NewWorker(p, Options{Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := c.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Stop(ctx) }()
	// The periodic job runs at start.
	wait(t, events, CleanupSessionsArgs{}.Kind(), 30*time.Second)
	var tokens []string
	rows, _ := p.Query(ctx, "SELECT token FROM sessions")
	tokens, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(tokens) != 1 || tokens[0] != "live" {
		t.Errorf("sessions = %v, %v", tokens, err)
	}
}

type recordArgs struct{ Name string }

func (recordArgs) Kind() string { return "test_record" }

type recordWorker struct {
	river.WorkerDefaults[recordArgs]
	q *db.Queries
}

func (w *recordWorker) Work(ctx context.Context, job *river.Job[recordArgs]) error {
	_, err := w.q.AddEvent(ctx, db.AddEventParams{Kind: "recorded", Target: job.Args.Name, Data: json.RawMessage(`{}`)})
	return err
}

func TestInsertTx(t *testing.T) {
	p, _ := pool(t)
	ctx := context.Background()
	ins, err := NewInserter(p, quiet)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(name string, commit bool) {
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := InsertTx(ctx, ins, tx, recordArgs{Name: name}, nil); err != nil {
			t.Fatal(err)
		}
		if commit {
			err = tx.Commit(ctx)
		} else {
			err = tx.Rollback(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("rolled-back", false)
	insert("committed", true)
	var n int
	if err := p.QueryRow(ctx, "SELECT count(*) FROM river_job WHERE kind = 'test_record'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("jobs = %d, %v", n, err)
	}
	c := start(t, p, Options{Workers: func(w *river.Workers) { river.AddWorker(w, &recordWorker{q: db.New(p)}) }})
	events, cancel := c.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	if e := wait(t, events, "test_record", 30*time.Second); e.Job.EncodedArgs == nil {
		t.Error("no arguments")
	}
	evs, err := db.New(p).ListTargetEvents(ctx, db.ListTargetEventsParams{Target: "committed", MaxRows: 5})
	if err != nil || len(evs) != 1 {
		t.Errorf("the committed job did not run: %v, %v", evs, err)
	}
	if err := InsertTx(ctx, ins, nil, recordArgs{}, &river.InsertOpts{Queue: "!"}); err == nil {
		t.Error("an invalid insert succeeded")
	}
}

func TestRepositoryLock(t *testing.T) {
	p, _ := pool(t)
	ctx := context.Background()
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithRepositoryLock(ctx, p, "infra-live", time.Second, func(context.Context) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	err := WithRepositoryLock(ctx, p, "infra-live", 5*time.Second, func(context.Context) error {
		t.Error("ran while another job held the lock")
		return nil
	})
	var snooze *rivertype.JobSnoozeError
	if !errors.Is(err, ErrLocked) || !errors.As(err, &snooze) || snooze.Duration != 5*time.Second {
		t.Errorf("second holder: %v", err)
	}
	ran := false
	if err := WithRepositoryLock(ctx, p, "other-repo", time.Second, func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Errorf("another repository: %v, ran %v", err, ran)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := WithRepositoryLock(ctx, p, "infra-live", time.Second, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("after release: %v", err)
	}
	// The failed run released the lock too.
	if err := WithRepositoryLock(ctx, p, "infra-live", time.Second, func(context.Context) error { return nil }); err != nil {
		t.Errorf("after a failed run: %v", err)
	}
}

// helperWorker runs a worker until it is killed. Its first attempt of a slow job never ends.
func helperWorker(url string) int {
	ctx := context.Background()
	p, err := pgxpool.New(ctx, url)
	if err != nil {
		return 2
	}
	c, err := NewWorker(p, Options{Logger: quiet, JobTimeout: -1, Workers: func(w *river.Workers) {
		river.AddWorker(w, &slowWorker{q: db.New(p)})
	}})
	if err != nil {
		return 2
	}
	if err := c.Start(ctx); err != nil {
		return 2
	}
	select {}
}

// A worker process killed in the middle of a job leaves it running; another worker rescues it
// and runs it again.
func TestWorkerKilledMidJob(t *testing.T) {
	p, url := pool(t)
	ctx := context.Background()
	ins, err := NewInserter(p, quiet)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ins.Insert(ctx, slowArgs{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^$") // #nosec G204 -- the test binary itself
	cmd.Env = append(os.Environ(), helperEnv+"="+url)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		var n int
		if err := p.QueryRow(ctx, "SELECT count(*) FROM events WHERE kind = 'slow_started'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("the worker process did not start the job")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	var state string
	if err := p.QueryRow(ctx, "SELECT state FROM river_job WHERE id = $1", res.Job.ID).Scan(&state); err != nil || state != "running" {
		t.Fatalf("after the kill the job is %q, %v; want running", state, err)
	}

	time.Sleep(1500 * time.Millisecond) // older than RescueStuckJobsAfter below
	c, err := NewWorker(p, Options{Logger: quiet, JobTimeout: 500 * time.Millisecond, RescueStuckJobsAfter: time.Second, Workers: func(w *river.Workers) {
		river.AddWorker(w, &slowWorker{q: db.New(p)})
	}})
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := c.Subscribe(river.EventKindJobCompleted)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.StopAndCancel(ctx) }()
	e := wait(t, events, slowArgs{}.Kind(), 90*time.Second)
	if e.Job.ID != res.Job.ID || e.Job.Attempt != 2 {
		t.Errorf("completed job %d at attempt %d, want job %d at attempt 2", e.Job.ID, e.Job.Attempt, res.Job.ID)
	}
}
