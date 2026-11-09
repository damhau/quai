package requests

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Reminders, escalation and expiry (docs/design/05-delivery-and-approvals.md#reminders-escalation-expiry):
// a periodic job, the sweep, finds the requests that wait for approval and takes each step due,
// by the service's clock, from instants kept with the request (its submission, its expiry). Each
// step is decided under the request's row lock, on its state read again, and marked in the
// transaction that records its event: a sweep run twice, or at the same moment as a review, takes
// a step once, and never on a request decided meanwhile.

// Steps of a request that waits for approval.
const (
	stepExpire   = "expire"   // it waited longer than its expiry: it expires
	stepWarn     = "warn"     // it expires soon: its requester hears of it
	stepRemind   = "remind"   // approvals.reminders.after: its approvers are reminded
	stepEscalate = "escalate" // approvals.escalation.after: the escalation group hears of it
)

// SweepEvery is how often the sweep runs: steps are taken within a minute of being due.
const SweepEvery = time.Minute

// UseApprovals sets the reminders and the escalation (the approvals configuration).
func (s *Service) UseApprovals(a config.Approvals) { s.waiting = a }

// expires keeps the shortest expiry of the plan's and these approval blocks.
func (p *Plan) expires(approvals ...definitions.Approval) {
	p.Expiry, _ = definitions.Expiry(append([]definitions.Approval{{Expiry: p.Expiry}}, approvals...)...)
}

// SweepArgs takes the steps due for the requests that wait for approval.
type SweepArgs struct{}

// Kind names the job.
func (SweepArgs) Kind() string { return "approval_sweep" }

// InsertOpts keeps one sweep at a time in the queue.
func (SweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByPeriod: SweepEvery}}
}

// Periodic is the sweep's periodic job, for jobs.Options.
func Periodic() *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(SweepEvery), func() (river.JobArgs, *river.InsertOpts) {
		return SweepArgs{}, nil
	}, &river.PeriodicJobOpts{RunOnStart: true})
}

type sweepWorker struct {
	river.WorkerDefaults[SweepArgs]
	s *Service
}

func (w *sweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error { return w.s.Sweep(ctx) }

// Sweep takes the steps due now for each request that waits for approval: its expiry, or the
// warning of it, the reminder and the escalation; and reminds the open break-glass reviews due
// (sweepReviews). A step that fails is tried again at the next sweep; the others go on.
func (s *Service) Sweep(ctx context.Context) error {
	list, err := s.store.ListPendingApproval(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	var errs []error
	for _, r := range list {
		for _, step := range s.due(r, now) {
			if err := s.step(ctx, r.ID, step); err != nil {
				errs = append(errs, fmt.Errorf("request %s, %s: %w", r.ID, step, err))
			}
		}
	}
	// And the reminders of the open break-glass reviews.
	if err := s.sweepReviews(ctx, now); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// due returns the steps due for a request at now: its expiry alone once it is due; otherwise the
// warning of its expiry, the reminder and the escalation not taken yet whose time came. Each
// counts from the submission (open question 79).
func (s *Service) due(r db.Request, now time.Time) []string {
	if r.State != PendingApproval {
		return nil
	}
	if r.ExpiresAt.Valid && !now.Before(r.ExpiresAt.Time) {
		return []string{stepExpire}
	}
	var out []string
	if r.ExpiresAt.Valid && !r.ExpiryWarnedAt.Valid && !now.Before(warnAt(r)) {
		out = append(out, stepWarn)
	}
	submitted := r.SubmittedAt.Time
	if after := s.waiting.Reminders.After; after > 0 && !r.RemindedAt.Valid && !now.Before(submitted.Add(after)) {
		out = append(out, stepRemind)
	}
	if after := s.waiting.Escalation.After; after > 0 && !r.EscalatedAt.Valid && !now.Before(submitted.Add(after)) {
		out = append(out, stepEscalate)
	}
	return out
}

// ExpiringSoon is how long before its expiry a request's requester is warned: a day, or half
// the request's expiry when that is shorter.
const ExpiringSoon = 24 * time.Hour

// warnAt is when a request's requester is warned of its expiry.
func warnAt(r db.Request) time.Time {
	before := min(ExpiringSoon, r.ExpiresAt.Time.Sub(r.SubmittedAt.Time)/2)
	return r.ExpiresAt.Time.Add(-before)
}

// step takes a step of a request, under its row lock, when it is still due: not taken by another
// sweep, the request still waiting for approval.
func (s *Service) step(ctx context.Context, id uuid.UUID, step string) error {
	return s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		r, err := q.LockRequest(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		now := s.now()
		if !slices.Contains(s.due(r, now), step) {
			return nil // decided or taken meanwhile
		}
		switch step {
		case stepExpire:
			return s.expire(ctx, tx, q, r, now)
		case stepWarn:
			if err := q.SetRequestExpiryWarned(ctx, db.SetRequestExpiryWarnedParams{ID: r.ID, At: instant(now)}); err != nil {
				return err
			}
			return s.event(ctx, tx, r, "expiring_soon", "", map[string]any{"expiry": r.Expiry, "expiresAt": audit.Time(r.ExpiresAt.Time)})
		case stepRemind:
			if err := q.SetRequestReminded(ctx, db.SetRequestRemindedParams{ID: r.ID, At: instant(now)}); err != nil {
				return err
			}
			awaiting, err := s.Awaiting(ctx, r)
			if err != nil {
				return err
			}
			after := s.waiting.Reminders.After.String()
			if err := s.event(ctx, tx, r, "reminded", "", map[string]any{"after": after}); err != nil {
				return err
			}
			return recordRequest(ctx, tx, r, "reminded", map[string]any{"after": after, "approvers": remindedPeople(awaiting)})
		case stepEscalate:
			if err := q.SetRequestEscalated(ctx, db.SetRequestEscalatedParams{ID: r.ID, At: instant(now)}); err != nil {
				return err
			}
			after := s.waiting.Escalation.After.String()
			if err := s.event(ctx, tx, r, "escalated", "", map[string]any{"after": after}); err != nil {
				return err
			}
			to := []any{}
			for _, entry := range s.waiting.Escalation.To {
				if name, ok := strings.CutPrefix(entry, "user:"); ok {
					to = append(to, map[string]any{"user": audit.Personal(name)})
				} else {
					to = append(to, entry)
				}
			}
			return recordRequest(ctx, tx, r, "escalated", map[string]any{"after": after, "to": to})
		}
		return fmt.Errorf("unknown step %q", step)
	})
}

// remindedPeople are the people a reminder is for, as personal data: each person the request
// waits for, and the delegator a delegate is there for.
func remindedPeople(awaiting []Awaiting) []map[string]any {
	out := []map[string]any{}
	seen := map[string]bool{}
	for _, a := range awaiting {
		if a.Rule.Undecided() {
			continue
		}
		for _, u := range a.People {
			if seen[u.Name] {
				continue
			}
			seen[u.Name] = true
			p := map[string]any{"approver": audit.Personal(u.Name)}
			if f := a.For[u.Name]; f != "" {
				p["onBehalfOf"] = audit.Personal(f)
			}
			out = append(out, p)
		}
	}
	return out
}

// Voided returns the reviews a request's expiry voided, oldest first: shown with the request, as
// its history, never counted (open question 80).
func (s *Service) Voided(ctx context.Context, r db.Request) ([]db.Review, error) {
	all, err := s.store.ListReviews(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(v db.Review) bool { return !v.VoidedAt.Valid }), nil
}

// Escalation returns the people an escalation of a request notifies (approvals.escalation.to):
// the people of the directory each entry names, not deactivated, never the request's requester.
// It notifies them; it makes no one an approver (open question 25).
func (s *Service) Escalation(ctx context.Context, r db.Request) ([]db.User, error) {
	if len(s.waiting.Escalation.To) == 0 {
		return nil, nil
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var out []db.User
	for _, u := range users {
		if u.DeactivatedAt.Valid || u.Name == r.Requester {
			continue
		}
		for _, entry := range s.waiting.Escalation.To {
			kind, name, _ := strings.Cut(entry, ":")
			if kind == "user" && u.Name == name || kind == "group" && slices.Contains(u.Groups, name) {
				out = append(out, u)
				break
			}
		}
	}
	return out, nil
}

// expire expires a request that waited for approval longer than its expiry, in the transaction
// that holds its row lock: its reviews are void and never count again, and quai/approval and its
// pull request follow (statusJob, closeExpired). Expired is final: its requester asks again
// with a new request (open question 80).
func (s *Service) expire(ctx context.Context, tx pgx.Tx, q *db.Queries, r db.Request, now time.Time) error {
	next, err := q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: PendingApproval, ToState: Expired})
	if err != nil {
		return conflict(err, r)
	}
	voided, err := q.VoidReviews(ctx, db.VoidReviewsParams{RequestID: r.ID, At: instant(now)})
	if err != nil {
		return err
	}
	data := map[string]any{"expiry": r.Expiry, "expiresAt": audit.Time(r.ExpiresAt.Time)}
	if err := s.event(ctx, tx, next, "expired", "", data); err != nil {
		return err
	}
	if err := recordRequest(ctx, tx, next, "expired", map[string]any{"expiry": r.Expiry, "expiresAt": audit.Time(r.ExpiresAt.Time),
		"submittedAt": audit.Time(r.SubmittedAt.Time), "voidedReviews": voided}); err != nil {
		return err
	}
	return s.statusJob(ctx, tx, q, next)
}

// expiredMarker is the line of the comment Quai leaves on an expired request's pull request, by
// which a retry finds the comment already there.
func expiredMarker(id uuid.UUID) string { return "<!-- quai:expired=" + id.String() + " -->" }

// closeExpired ends the pull request of an expired request: only the request's own, the one Quai
// opened and recorded (d), never one found by its branch. While it is open, quai/approval fails
// ("Expired in Quai"), a comment says why, once, and Quai closes it and deletes its branch. One
// merged meanwhile is a merge without approval (merged: an alert); one closed by someone else is
// left as it is (open question 81). Called under the request's status lock (publishLatest).
func (s *Service) closeExpired(ctx context.Context, repo *repositories.Repo, r db.Request, d db.Delivery, st connectors.Status) error {
	prs := repo.Connector.PullRequests()
	pr, err := prs.Get(ctx, d.PullRequest)
	if err != nil {
		return fmt.Errorf("read pull request %s: %w", d.PullRequest, err)
	}
	closing, err := s.hasEvent(ctx, r, "pull_request_closed")
	if err != nil {
		return err
	}
	switch pr.State {
	case connectors.PRMerged:
		return s.merged(ctx, r, d, pr)
	case connectors.PRAbandoned:
		if closing {
			return repo.Mirror.DeleteBranch(ctx, d.Branch) // closed by Quai, by an earlier run
		}
		return s.once(ctx, r, "pull_request_left", map[string]any{"pullRequest": d.PullRequest, "reason": "closed outside Quai"})
	}
	st.State, st.Description = connectors.StatusFailure, "Expired in Quai"
	if err := repo.Connector.Statuses().Set(ctx, connectors.StatusTarget{PullRequest: d.PullRequest, Commit: d.Head}, st); err != nil {
		return fmt.Errorf("publish %s on pull request %s: %w", st.Name, d.PullRequest, err)
	}
	if err := s.commentExpired(ctx, prs, r, d); err != nil {
		return err
	}
	if !closing {
		// Recorded before Quai closes it, so that a run stopped in between knows the pull request
		// closed is Quai's doing, and closes it again if it is still open.
		if err := s.store.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
			if err := s.event(ctx, tx, r, "pull_request_closed", "", map[string]any{"pullRequest": d.PullRequest, "reason": Expired}); err != nil {
				return err
			}
			return recordRequest(ctx, tx, r, "pull_request_closed", map[string]any{"pullRequest": d.PullRequest, "reason": Expired})
		}); err != nil {
			return err
		}
	}
	if err := prs.Abandon(ctx, d.PullRequest); err != nil {
		return fmt.Errorf("close pull request %s: %w", d.PullRequest, err)
	}
	return repo.Mirror.DeleteBranch(ctx, d.Branch)
}

// commentExpired says on an expired request's pull request why it closes, once: a comment that
// carries the marker is not added again, whether this run or an earlier one (which may have
// stopped before recording it) posted it. It says what the pull request's readers may see:
// the request's number, its expiry and its link.
func (s *Service) commentExpired(ctx context.Context, prs connectors.PullRequests, r db.Request, d db.Delivery) error {
	if done, err := s.hasEvent(ctx, r, "expiry_commented"); err != nil || done {
		return err
	}
	marker := expiredMarker(r.ID)
	comments, err := prs.Comments(ctx, d.PullRequest)
	if err != nil {
		return fmt.Errorf("read the comments of pull request %s: %w", d.PullRequest, err)
	}
	if !slices.ContainsFunc(comments, func(c string) bool { return strings.Contains(c, marker) }) {
		var b strings.Builder
		fmt.Fprintf(&b, "Expired in Quai: request #%d waited for approval longer than its expiry", r.Number)
		if r.Expiry != "" {
			fmt.Fprintf(&b, " (%s)", r.Expiry)
		}
		b.WriteString(". Its approvals are void and this pull request is closed; request the change again in Quai.\n")
		if l := s.link(r); l != "" {
			b.WriteString("\n" + l + "\n")
		}
		b.WriteString("\n" + marker + "\n")
		if err := prs.Comment(ctx, d.PullRequest, b.String()); err != nil {
			return fmt.Errorf("comment on pull request %s: %w", d.PullRequest, err)
		}
	}
	return s.store.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		return s.event(ctx, tx, r, "expiry_commented", "", map[string]any{"pullRequest": d.PullRequest})
	})
}

// hasEvent reports whether a request has an event of a kind.
func (s *Service) hasEvent(ctx context.Context, r db.Request, kind string) (bool, error) {
	events, err := s.store.ListRequestEvents(ctx, pgtype.UUID{Bytes: r.ID, Valid: true})
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(events, func(e db.Event) bool { return e.Kind == kind }), nil
}

// once records a request's event unless it has one of that kind.
func (s *Service) once(ctx context.Context, r db.Request, kind string, data map[string]any) error {
	if done, err := s.hasEvent(ctx, r, kind); err != nil || done {
		return err
	}
	return s.store.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		return s.event(ctx, tx, r, kind, "", data)
	})
}
