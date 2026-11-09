package requests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/engine/window"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Change windows and freezes (docs/design/05-delivery-and-approvals.md#change-windows-and-freezes):
// an approved request merges, or is committed, only while every change window that applies to it
// is open. The decision is taken at merge time (gate), under the request's row lock, with the
// windows of Quai's configuration and of the repository document as they are stored then, never
// as they were when it was approved. Outside its windows, an approved request waits, held:
// quai/approval pending with the reason, no merge asked of the platform, no commit. A periodic job
// (the windows' sweep) releases the requests whose windows opened, for their delivery to go on,
// and holds those whose windows closed before they merged.

// Subject is what a change window's when sees of one of a request's resources: its type and its
// params. The request's kind, action, blueprint and repository come from the request; its reason
// never does, so that what a requester writes cannot decide when the change merges.
type Subject struct {
	Type   string            `json:"type,omitempty"`
	Params map[string]string `json:"params,omitempty"`
}

// UseChangeWindows sets the change windows of Quai's configuration (changeWindows).
func (s *Service) UseChangeWindows(ws []*window.Window) { s.windows = ws }

// Windows is what the change windows say of a request at a time.
type Windows struct {
	window.State
	// At is the time they were decided at.
	At time.Time
	// Err: a window's when failed, or the repository's windows cannot be read. The request waits,
	// whatever the time, until that is fixed: an error never lets it merge.
	Err error
	// Applied are the windows that apply to the request, Quai's configuration's first.
	Applied []*window.Window
}

// MayMerge says whether the request may merge now.
func (w Windows) MayMerge() bool { return w.Err == nil && w.Open }

// MaxReason is the longest quai/approval description Quai publishes, in characters: GitHub's
// limit for a commit status.
const MaxReason = 140

// Reason is quai/approval's description while an approved request waits for its windows:
// "Approved; waits for the change window, opens Mon 08:00 Europe/Zurich".
func (w Windows) Reason() string {
	switch {
	case w.Err != nil:
		return clip("Approved; held: "+w.Err.Error(), MaxReason)
	case w.Opens.IsZero():
		return "Approved; waits for the change window: none opens within a year"
	}
	return clip("Approved; waits for the change window, opens "+window.When(w.Opens, w.At, w.Zone()), MaxReason)
}

// clip shortens s to max characters, with an ellipsis.
func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// names are the windows' names.
func names(ws []*window.Window) []string {
	out := []string{}
	for _, w := range ws {
		out = append(out, w.Name)
	}
	return out
}

// exempt says whether a request merges whatever its change windows say. None does yet: P4-17's
// break-glass requests will (docs/design/05-delivery-and-approvals.md#break-glass), and this is
// the one place that lifts the windows for them.
func exempt(db.Request) bool { return false }

// WindowsOf decides the change windows of r now, by the service's clock, as gate would: the
// request page shows when it may merge.
func (s *Service) WindowsOf(ctx context.Context, r db.Request) (Windows, error) {
	repo, err := s.reg.Get(s.Repository(ctx, r))
	if err != nil {
		return Windows{}, err
	}
	return s.windowsOf(ctx, s.store.Queries, r, repo, s.now())
}

// windowsOf decides the change windows of r, a request on repo, at now: Quai's configuration's
// and the repository document's as stored now (read through q), each applying when its when holds
// for one of the request's resources. The error is the database's; what keeps the windows from
// being decided is Windows.Err.
func (s *Service) windowsOf(ctx context.Context, q *db.Queries, r db.Request, repo *repositories.Repo, now time.Time) (Windows, error) {
	out := Windows{At: now}
	if exempt(r) {
		out.Open = true
		return out, nil
	}
	own, err := s.repositoryWindows(ctx, q, repo)
	if err != nil {
		var unreadable *unreadableWindows
		if !errors.As(err, &unreadable) {
			return out, err
		}
		out.Err = err
		return out, nil
	}
	subjects, err := subjectsOf(r, repo.Name)
	if err != nil {
		out.Err = err
		return out, nil
	}
	for _, w := range append(slices.Clone(s.windows), own...) {
		applies, err := w.Applies(subjects)
		if err != nil {
			out.Err = err
			return out, nil
		}
		if applies {
			out.Applied = append(out.Applied, w)
		}
	}
	out.State = window.At(out.Applied, now)
	return out, nil
}

// unreadableWindows: the repository document as stored has change windows Quai cannot compile.
type unreadableWindows struct{ err error }

func (e *unreadableWindows) Error() string {
	return "the repository's change windows cannot be read: " + e.err.Error()
}

// repositoryWindows returns the repository document's change windows as stored now: the
// registry's, compiled, while it has the stored generation; compiled from the stored document
// otherwise, since the registry reloads only every few seconds and a freeze added meanwhile holds.
func (s *Service) repositoryWindows(ctx context.Context, q *db.Queries, repo *repositories.Repo) ([]*window.Window, error) {
	row, err := q.GetRepository(ctx, repo.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return repo.ChangeWindows, nil // not stored: configured without a database
	}
	if err != nil {
		return nil, err
	}
	if row.Generation == repo.Generation {
		return repo.ChangeWindows, nil
	}
	var d repositories.Document
	if err := json.Unmarshal(row.Document, &d); err != nil {
		return nil, &unreadableWindows{err}
	}
	ws, err := d.Windows()
	if err != nil {
		return nil, &unreadableWindows{err}
	}
	return ws, nil
}

// subjectsOf returns what the windows' conditions see of r's resources; none for a request
// planned before they were kept, to which every window with a condition applies.
func subjectsOf(r db.Request, repository string) ([]expr.Vars, error) {
	if len(r.WindowSubjects) == 0 {
		return nil, nil
	}
	var subjects []Subject
	if err := json.Unmarshal(r.WindowSubjects, &subjects); err != nil {
		return nil, fmt.Errorf("the request's resources cannot be read: %w", err)
	}
	out := make([]expr.Vars, 0, len(subjects))
	for _, sub := range subjects {
		out = append(out, expr.Vars{Params: anyMap(sub.Params), Request: expr.Request{Kind: r.Kind, Action: r.Action,
			Type: sub.Type, Blueprint: r.Blueprint, Repository: repository}})
	}
	return out, nil
}

// gate decides, at merge time, whether r may merge now: under its row lock, on its state read
// again, with the change windows as stored now. It is the only way to merging, so that nothing
// merges outside its windows.
//
// In its windows, an approved request moves to merging when merge is true (Quai merges; false
// when a person does), and a held one is released: ok. Outside them, or when they cannot be
// decided, the request waits, approved (a merging one goes back), held, its quai/approval pending
// with the reason: not ok. A request no longer approved or merging is left alone: not ok.
func (s *Service) gate(ctx context.Context, r db.Request, repo *repositories.Repo, merge bool, data map[string]any) (db.Request, bool, error) {
	ok := false
	err := s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		cur, err := q.LockRequest(ctx, r.ID)
		if err != nil {
			return err
		}
		r = cur
		if r.State != Approved && r.State != Merging {
			return nil
		}
		w, err := s.windowsOf(ctx, q, r, repo, s.now())
		if err != nil {
			return err
		}
		if !w.MayMerge() {
			return s.hold(ctx, tx, q, &r, w)
		}
		if r.WindowHeldAt.Valid {
			if err := s.release(ctx, tx, q, &r, w); err != nil {
				return err
			}
		}
		if merge && r.State == Approved {
			next, err := q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: Approved, ToState: Merging})
			if err != nil {
				return conflict(err, r)
			}
			r = next
			if err := s.event(ctx, tx, r, "merging", "", data); err != nil {
				return err
			}
		}
		ok = true
		return nil
	})
	return r, ok, err
}

// hold has r, locked, wait for its windows: a merging request goes back to approved; the first
// time, the hold is recorded (window_waiting); quai/approval follows, then again whenever the
// reason it gives changes (a freeze added while it waits).
func (s *Service) hold(ctx context.Context, tx pgx.Tx, q *db.Queries, r *db.Request, w Windows) error {
	from, first, reason := r.State, !r.WindowHeldAt.Valid, w.Reason()
	if from == Merging {
		next, err := q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: Merging, ToState: Approved})
		if err != nil {
			return conflict(err, *r)
		}
		*r = next
	} else if !first && r.WindowReason == reason {
		return nil // held already, with that reason
	}
	at := r.WindowHeldAt
	if first {
		at = instant(w.At)
	}
	if err := q.SetRequestWindowHeld(ctx, db.SetRequestWindowHeldParams{ID: r.ID, At: at, Reason: reason}); err != nil {
		return err
	}
	r.WindowHeldAt, r.WindowReason = at, reason
	if !first && from != Merging {
		return s.statusJob(ctx, tx, q, *r)
	}
	data, details := map[string]any{"reason": reason}, map[string]any{"from": from, "windows": names(w.Holding)}
	if w.Err != nil {
		data["error"], details["error"] = w.Err.Error(), w.Err.Error()
	} else if !w.Opens.IsZero() {
		data["opens"], details["opens"] = audit.Time(w.Opens), audit.Time(w.Opens)
	}
	if len(w.Holding) > 0 {
		data["windows"] = names(w.Holding)
	}
	if err := s.event(ctx, tx, *r, "window_waiting", "", data); err != nil {
		return err
	}
	if err := recordRequest(ctx, tx, *r, "window_waiting", details); err != nil {
		return err
	}
	return s.statusJob(ctx, tx, q, *r)
}

// release ends r's hold, locked, its windows open (window_opened).
func (s *Service) release(ctx context.Context, tx pgx.Tx, q *db.Queries, r *db.Request, w Windows) error {
	if err := q.SetRequestWindowHeld(ctx, db.SetRequestWindowHeldParams{ID: r.ID}); err != nil {
		return err
	}
	r.WindowHeldAt, r.WindowReason = pgtype.Timestamptz{}, ""
	data := map[string]any{"windows": names(w.Applied)}
	if !w.Closes.IsZero() {
		data["closes"] = audit.Time(w.Closes)
	}
	if err := s.event(ctx, tx, *r, "window_opened", "", data); err != nil {
		return err
	}
	return recordRequest(ctx, tx, *r, "window_opened", data)
}

// WindowSweepArgs holds and releases approved requests as their change windows close and open.
type WindowSweepArgs struct{}

// Kind names the job.
func (WindowSweepArgs) Kind() string { return "window_sweep" }

// InsertOpts keeps one sweep at a time in the queue.
func (WindowSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByPeriod: SweepEvery}}
}

// WindowsPeriodic is the windows' sweep's periodic job, for jobs.Options: requests merge within a
// minute of their windows opening.
func WindowsPeriodic() *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(SweepEvery), func() (river.JobArgs, *river.InsertOpts) {
		return WindowSweepArgs{}, nil
	}, &river.PeriodicJobOpts{RunOnStart: true})
}

type windowSweepWorker struct {
	river.WorkerDefaults[WindowSweepArgs]
	s *Service
}

func (w *windowSweepWorker) Work(ctx context.Context, _ *river.Job[WindowSweepArgs]) error {
	return w.s.SweepWindows(ctx)
}

// SweepWindows decides the change windows of each approved request at the service's clock: a
// held request whose windows opened is released and delivered; an approved one whose windows
// closed is held (a person merging it, merge: manual, sees quai/approval pending again), and a
// held one's quai/approval follows its reason (a freeze added while it waits); a merging one
// whose windows closed is delivered again, for the delivery to take it back under its merge
// lock. Each is decided again under the request's row lock; one that fails is tried at the next
// sweep, the others go on.
func (s *Service) SweepWindows(ctx context.Context) error {
	list, err := s.store.ListApprovedRequests(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	var errs []error
	for _, r := range list {
		if err := s.sweepWindows(ctx, r, now); err != nil {
			errs = append(errs, fmt.Errorf("request %s: %w", r.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) sweepWindows(ctx context.Context, r db.Request, now time.Time) error {
	repo, err := s.reg.Get(s.Repository(ctx, r))
	if err != nil {
		return nil // disconnected: nothing is delivered there
	}
	w, err := s.windowsOf(ctx, s.store.Queries, r, repo, now)
	if err != nil {
		return err
	}
	switch {
	case r.State == Approved && r.WindowHeldAt.Valid && w.MayMerge():
		return s.reopen(ctx, r.ID, repo)
	case r.State == Approved && !w.MayMerge() && (!r.WindowHeldAt.Valid || r.WindowReason != w.Reason()):
		return s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
			r, err := q.LockRequest(ctx, r.ID)
			if err != nil || r.State != Approved {
				return err
			}
			w, err := s.windowsOf(ctx, q, r, repo, s.now())
			if err != nil || w.MayMerge() {
				return err
			}
			return s.hold(ctx, tx, q, &r, w)
		})
	case r.State == Merging && !w.MayMerge() && s.jobs != nil:
		// The delivery decides again under the merge lock, where a merge may be under way, and
		// takes the request back: only pull requests stay merging between deliveries.
		if _, err := s.store.GetDelivery(ctx, r.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		_, err := s.jobs.Insert(ctx, DeliverArgs{RequestID: r.ID}, &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: 10 * time.Second}})
		return err
	}
	return nil
}

// reopen releases a held request whose windows are open, under its row lock, and queues its
// delivery in the same transaction.
func (s *Service) reopen(ctx context.Context, id uuid.UUID, repo *repositories.Repo) error {
	return s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		r, err := q.LockRequest(ctx, id)
		if err != nil || r.State != Approved || !r.WindowHeldAt.Valid {
			return err
		}
		w, err := s.windowsOf(ctx, q, r, repo, s.now())
		if err != nil || !w.MayMerge() {
			return err
		}
		if err := s.release(ctx, tx, q, &r, w); err != nil {
			return err
		}
		if s.jobs == nil {
			return nil
		}
		return jobs.InsertTx(ctx, s.jobs, tx, DeliverArgs{RequestID: r.ID}, nil)
	})
}
