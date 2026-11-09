package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/store/db"
	"github.com/damhau/quai/backend/internal/telemetry"
)

// digestInterval is how often the digest job looks for digests that are due: a digest goes at
// most this long after its hour.
const digestInterval = 5 * time.Minute

// DigestArgs gathers the digests that are due.
type DigestArgs struct{}

// Kind names the job.
func (DigestArgs) Kind() string { return "notify_digests" }

// InsertOpts puts it on the notifications' queue. It runs again at the next interval anyway.
func (DigestArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: jobs.QueueNotifications, MaxAttempts: 3}
}

// SendDigestArgs sends a digest.
type SendDigestArgs struct {
	DigestID int64 `json:"digestId"`
}

// Kind names the job.
func (SendDigestArgs) Kind() string { return "send_digest" }

// InsertOpts puts it on the notifications' queue.
func (SendDigestArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: jobs.QueueNotifications, MaxAttempts: MaxAttempts}
}

// Periodic is the job that gathers the digests that are due, for jobs.Options.
func Periodic() *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(digestInterval), func() (river.JobArgs, *river.InsertOpts) {
		return DigestArgs{}, nil
	}, &river.PeriodicJobOpts{RunOnStart: true})
}

type digestWorker struct {
	river.WorkerDefaults[DigestArgs]
	s *Service
}

func (w *digestWorker) Work(ctx context.Context, _ *river.Job[DigestArgs]) error {
	return w.s.gather(ctx)
}

type sendDigestWorker struct {
	river.WorkerDefaults[SendDigestArgs]
	s *Service
}

func (w *sendDigestWorker) Work(ctx context.Context, job *river.Job[SendDigestArgs]) error {
	return w.s.sendDigest(ctx, job.Args.DigestID, job.Attempt >= job.MaxAttempts)
}

func (w *sendDigestWorker) NextRetry(job *river.Job[SendDigestArgs]) time.Time {
	return w.s.now().Add(w.s.opts.Backoff(job.Attempt))
}

// Timeout leaves a send its own deadline (sendTimeout) and the database's work around it: a
// digest composes a message per notification.
func (w *sendDigestWorker) Timeout(*river.Job[SendDigestArgs]) time.Duration {
	return sendTimeout + 2*time.Minute
}

// gather makes the digests that are due: for each recipient and channel with notifications that
// wait for one, when their latest digest hour has passed (lastDue), a digest of the notifications
// added before that hour, with the job that sends it. The key (recipient, channel, period) makes
// one digest per local date however often the job runs; a notification joins one digest only.
// No digest is made with nothing in it.
func (s *Service) gather(ctx context.Context) error {
	waiting, err := s.opts.Store.WaitingForDigest(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	var errs []error
	for _, w := range waiting {
		name, ok := strings.CutPrefix(w.Recipient, "user:")
		if !ok {
			continue // only people choose digests
		}
		p, err := s.preferences(ctx, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		hour, loc := s.opts.Settings.Digest(p)
		due, period := lastDue(now, hour, loc)
		err = s.opts.Store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
			d, err := q.AddDigest(ctx, db.AddDigestParams{Recipient: w.Recipient, Channel: w.Channel, Period: pgtype.Date{Time: period, Valid: true},
				Due: ts(due), CreatedAt: ts(now)})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // made already: what was added since waits for the next
			}
			if err != nil {
				return err
			}
			n, err := q.GatherDigest(ctx, db.GatherDigestParams{DigestID: pgtype.Int8{Int64: d.ID, Valid: true}, Recipient: w.Recipient,
				Channel: w.Channel, Before: ts(due)})
			if err != nil {
				return err
			}
			if n == 0 {
				return errNothing // added after the hour: they wait for the next
			}
			return jobs.InsertTx(ctx, s.jobs, tx, SendDigestArgs{DigestID: d.ID}, nil)
		})
		if err != nil && !errors.Is(err, errNothing) {
			errs = append(errs, fmt.Errorf("digest of %s on %s: %w", w.Recipient, w.Channel, err))
		}
	}
	return errors.Join(errs...)
}

// errNothing rolls back a digest with nothing in it.
var errNothing = errors.New("nothing to gather")

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// DigestItem is a notification in a digest: what its own message would say, in short.
type DigestItem struct {
	Label    string // Approved, Approval needed, …
	Headline string
	Request  string // #12 Create vm big01 (infra-live)
	Link     string
}

// sendDigest sends a digest, once: a sent or skipped one is left alone. Each notification in it
// is composed now, under the rules of an immediate one: a notification whose recipient may no
// longer read its request, or that an approval no longer waits for, is skipped and left out. A
// digest with nothing left is skipped. A failed attempt is recorded and returned, for River to
// try again; after the last one, the digest and its notifications have failed.
func (s *Service) sendDigest(ctx context.Context, id int64, last bool) error {
	d, err := s.opts.Store.GetDigest(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if d.State != Pending {
		return nil
	}
	ns, err := s.opts.Store.ListDigestNotifications(ctx, pgtype.Int8{Int64: d.ID, Valid: true})
	if err != nil {
		return err
	}
	skip := func(n db.Notification, why string) error {
		telemetry.Notified(ctx, n.Channel, Skipped)
		return s.opts.Store.NotificationSkipped(ctx, db.NotificationSkippedParams{ID: n.ID, LastError: why})
	}
	c := s.channels[d.Channel]
	var (
		to    Recipient
		items []DigestItem
	)
	for _, n := range ns {
		if c == nil {
			if err := skip(n, "the channel is no longer configured"); err != nil {
				return err
			}
			continue
		}
		r, m, why, err := s.compose(ctx, n, c)
		if err != nil {
			return err
		}
		if why != "" {
			if err := skip(n, why); err != nil {
				return err
			}
			continue
		}
		to = r
		item := DigestItem{Label: subjects[n.Kind], Headline: m.Headline, Link: m.Link}
		if rc := m.Request; rc != nil {
			item.Request = fmt.Sprintf("#%d %s (%s)", rc.Number, rc.Title, rc.Repository)
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return s.opts.Store.DigestSkipped(ctx, db.DigestSkippedParams{ID: d.ID, LastError: "nothing left to tell: each of its notifications says why"})
	}
	updates := "updates"
	if len(items) == 1 {
		updates = "update"
	}
	date := d.Period.Time.Format("Monday 2 January 2006")
	m := Message{Kind: DigestKind, Subject: fmt.Sprintf("[Quai] Your digest of %s: %d %s", date, len(items), updates),
		Headline: "What happened to your requests, and to those that wait for your approval, since your last digest.",
		Digest:   items, Link: s.link("/requests"), Preferences: s.link("/profile")}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	_, err = c.Send(sctx, to, m)
	cancel()
	if err != nil {
		state, result, level := Pending, "retried", slog.LevelWarn
		if last {
			state, result, level = Failure, Failure, slog.LevelError
		}
		msg := clip(err.Error(), 1000)
		telemetry.Notified(ctx, d.Channel, result)
		s.opts.Log.Log(ctx, level, "a digest was not sent", slog.Int64("digest", d.ID), slog.String("channel", d.Channel),
			slog.String("recipient", d.Recipient), slog.Int("attempt", int(d.Attempts)+1), slog.Bool("last", last), slog.String("error", msg))
		uerr := s.opts.Store.Tx(ctx, func(_ pgx.Tx, q *db.Queries) error {
			if err := q.DigestAttempted(ctx, db.DigestAttemptedParams{ID: d.ID, LastError: msg, State: state}); err != nil || !last {
				return err
			}
			return q.DigestNotificationsFailed(ctx, db.DigestNotificationsFailedParams{DigestID: pgtype.Int8{Int64: d.ID, Valid: true},
				Attempts: d.Attempts + 1, LastError: msg})
		})
		if uerr != nil {
			return errors.Join(err, uerr)
		}
		return fmt.Errorf("digest %d: %w", d.ID, err)
	}
	for range items {
		telemetry.Notified(ctx, d.Channel, Sent)
	}
	now := ts(s.now())
	return s.opts.Store.Tx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		if err := q.DigestSent(ctx, db.DigestSentParams{ID: d.ID, SentAt: now}); err != nil {
			return err
		}
		return q.DigestNotificationsSent(ctx, db.DigestNotificationsSentParams{DigestID: pgtype.Int8{Int64: d.ID, Valid: true}, SentAt: now})
	})
}
