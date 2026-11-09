package requests

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/engine/window"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/servicenow"
	"github.com/damhau/quai/backend/internal/store/db"
	"github.com/damhau/quai/backend/internal/telemetry"
)

// ServiceNow change requests (docs/design/05-delivery-and-approvals.md#itsm). A request that an
// external: servicenow rule applies to gets a change request in ServiceNow for its content: a
// standard change from the template its catalog item maps to (itsm.servicenow.standardChanges),
// or a normal change. The rule is satisfied when ServiceNow reports that change request approved,
// and the request is rejected when it is rejected, canceled or closed; Quai's other rules still
// apply. Quai records the change request when the request is submitted, or when a plan makes the
// rule apply, and opens it in ServiceNow from a job; the sweep opens those it could not and reads
// those it waits on, every itsm.servicenow.pollInterval, and the request's page asks for a
// reading when it opens. What ServiceNow reports is applied under the request's lock, to the
// change request still current for the request's content: a reading of another changes nothing.
// When the content changes, the change request is superseded and a new one is made: an approval
// is for the content it was given for (open question 107).

// ServiceNowSystem is the external system of a rule a change request in ServiceNow decides.
const ServiceNowSystem = "servicenow"

const (
	// snBatch is how many change requests a sweep opens or reads at most, snParallel how many at
	// once.
	snBatch    = 50
	snParallel = 4
	// snMaxWait is the longest between two tries of a change request ServiceNow does not answer
	// for.
	snMaxWait = time.Hour
	// snSoon is how recent a reading the request page takes without asking ServiceNow again.
	snSoon = 30 * time.Second
	// snLongOutage is how long ServiceNow fails before it is logged as an error.
	snLongOutage = time.Hour
)

// The work notes Quai adds to a change request.
const (
	notePlan       = "plan"        // a plan of the request reported
	noteSuperseded = "superseded"  // the request's content changed
	noteBreakGlass = "break_glass" // glass was broken for the request
)

// UseServiceNow makes Quai open change requests in ServiceNow for the requests an
// external: servicenow rule applies to, and read their approval (itsm.servicenow). Without it,
// such a rule is never satisfied: the request waits, and expires or is broken.
func (s *Service) UseServiceNow(cfg *config.ServiceNow, client *servicenow.Client) {
	s.snCfg, s.sn = cfg, client
}

// waitsOnServiceNow reports whether a rule decided to apply needs ServiceNow's approval.
func waitsOnServiceNow(rules []policy.Rule) bool {
	return slices.ContainsFunc(rules, func(r policy.Rule) bool {
		return r.External == ServiceNowSystem && !r.NotApplicable() && !r.Undecided()
	})
}

// waitsOnChange reports whether a request waits on what its change request says: pending approval,
// or approved and not merging yet, without break-glass.
func waitsOnChange(r db.Request) bool {
	return (r.State == PendingApproval || r.State == Approved) && !broken(r)
}

// ended reports whether a request ended before it could merge: its change request is opened no
// more.
func ended(r db.Request) bool {
	return r.State == Rejected || r.State == Cancelled || r.State == Expired || r.State == Stale
}

// correlationID is set on the generation-th change request of a request, for a creation tried
// again to find the one made: with a random part, so that no one can create in ServiceNow, before
// Quai, a change request Quai would take for its own.
func correlationID(r db.Request, generation int32) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("quai:%s:%d:%s", r.ID, generation, hex.EncodeToString(b))
}

// text is a sys_id as the database keeps it.
func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// openChangeRequest records, in tx, the change request a request needs for its content (hash),
// and the job that opens it in ServiceNow: nothing when no rule decided to apply waits on
// ServiceNow, ServiceNow is not configured, or the request has one for this content already.
func (s *Service) openChangeRequest(ctx context.Context, tx pgx.Tx, q *db.Queries, r db.Request, rules []policy.Rule, repository, hash string) error {
	if !waitsOnServiceNow(rules) {
		return nil
	}
	if s.snCfg == nil {
		s.log.Warn("a rule of the request waits on ServiceNow, and itsm.servicenow is not configured: the request waits until it expires or glass is broken",
			"request", r.ID)
		return nil
	}
	switch cur, err := q.CurrentChangeRequest(ctx, r.ID); {
	case err == nil && cur.ChangeSetHash == hash:
		return nil
	case err == nil:
		if err := s.supersede(ctx, tx, q, r); err != nil {
			return err
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	n, err := q.CountChangeRequests(ctx, r.ID)
	if err != nil {
		return err
	}
	generation := int32(n) + 1 // #nosec G115 -- a request has few change requests
	kind, template := "normal", s.snCfg.Template(repository, r.TypeName, r.Blueprint, r.Kind, r.Action)
	if template != "" {
		kind = "standard"
	}
	cr, err := q.CreateChangeRequest(ctx, db.CreateChangeRequestParams{ID: uuid.New(), RequestID: r.ID, Generation: generation,
		CorrelationID: correlationID(r, generation), ChangeSetHash: hash, Kind: kind, Template: template, CreatedAt: instant(s.now())})
	if err != nil || s.jobs == nil {
		return err // without jobs, the sweep opens it
	}
	return jobs.InsertTx(ctx, s.jobs, tx, ChangeRequestArgs{ChangeRequestID: cr.ID}, nil)
}

// supersede marks the request's current change request superseded, in tx: its content changed,
// and its approval does not count for the new content (open question 107). A work note says so in
// ServiceNow; one ServiceNow created without its answer reaching Quai is found by its correlation
// ID first.
func (s *Service) supersede(ctx context.Context, tx pgx.Tx, q *db.Queries, r db.Request) error {
	old, err := q.SupersedeChangeRequest(ctx, db.SupersedeChangeRequestParams{RequestID: r.ID, At: instant(s.now())})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	data := map[string]any{"changeRequest": old.Number, "changeSet": old.ChangeSetHash}
	if err := s.event(ctx, tx, r, "change_request_superseded", "", data); err != nil {
		return err
	}
	if err := recordRequest(ctx, tx, r, "change_request_superseded", changeRequestDetails(old)); err != nil {
		return err
	}
	switch {
	case s.jobs == nil:
		return nil
	case old.SysID.Valid:
		return jobs.InsertTx(ctx, s.jobs, tx, ChangeRequestNoteArgs{ChangeRequestID: old.ID, Note: noteSuperseded}, nil)
	}
	return jobs.InsertTx(ctx, s.jobs, tx, ChangeRequestArgs{ChangeRequestID: old.ID}, nil)
}

// changeRequestDetails is what the audit log records of a change request.
func changeRequestDetails(cr db.ChangeRequest) map[string]any {
	d := map[string]any{"changeRequest": cr.Number, "sysId": cr.SysID.String, "kind": cr.Kind, "correlationId": cr.CorrelationID,
		"changeSet": cr.ChangeSetHash}
	if cr.Template != "" {
		d["template"] = cr.Template
	}
	return d
}

// serviceNowActor is ServiceNow as the audit log's actor of what its change requests decide.
var serviceNowActor = audit.Actor{Type: audit.System, ID: ServiceNowSystem}

// externalOf is what a request's change request says to the policy: approved, or rejected (a
// rejection, a cancellation, a closure), when ServiceNow reported so for the content hash; nothing,
// pending, otherwise.
func externalOf(cr db.ChangeRequest, hash string) map[string]string {
	if cr.SupersededAt.Valid || !cr.SysID.Valid || cr.ChangeSetHash != hash {
		return nil
	}
	switch cr.Outcome {
	case servicenow.Approved:
		return map[string]string{ServiceNowSystem: "approved"}
	case servicenow.Rejected, servicenow.Cancelled, servicenow.Closed:
		return map[string]string{ServiceNowSystem: "rejected"}
	}
	return nil
}

// contextFor is contextOf with what the request's change request says of its content (hash).
func (s *Service) contextFor(ctx context.Context, q *db.Queries, r db.Request, hash string) (policy.Context, error) {
	pc := contextOf(r)
	cr, err := q.CurrentChangeRequest(ctx, r.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pc, nil
	}
	if err != nil {
		return pc, err
	}
	pc.External = externalOf(cr, hash)
	return pc, nil
}

// ChangeRequestArgs opens a change request in ServiceNow, or reads it, now: after its request's
// submission or a change of its content, or when the request's page opens.
type ChangeRequestArgs struct {
	ChangeRequestID uuid.UUID `json:"changeRequest"`
}

// Kind names the job.
func (ChangeRequestArgs) Kind() string { return "servicenow_change" }

// InsertOpts keeps one at a time for a change request; a failure is recorded on it, and the sweep
// tries it again when due.
func (ChangeRequestArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 10, UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: snSoon}}
}

type changeRequestWorker struct {
	river.WorkerDefaults[ChangeRequestArgs]
	s *Service
}

func (w *changeRequestWorker) Work(ctx context.Context, job *river.Job[ChangeRequestArgs]) error {
	err := w.s.syncLocked(ctx, job.Args.ChangeRequestID)
	var f *snFailure
	if errors.As(err, &f) && !f.retry {
		return nil // recorded: the sweep tries again when due
	}
	return err
}

// ChangeRequestNoteArgs adds a work note to a change request.
type ChangeRequestNoteArgs struct {
	ChangeRequestID uuid.UUID `json:"changeRequest"`
	Note            string    `json:"note"`
	Run             uuid.UUID `json:"run,omitzero"` // the plan, for a plan's note
}

// Kind names the job.
func (ChangeRequestNoteArgs) Kind() string { return "servicenow_note" }

// InsertOpts adds a note once.
func (ChangeRequestNoteArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 25, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

type noteWorker struct {
	river.WorkerDefaults[ChangeRequestNoteArgs]
	s *Service
}

func (w *noteWorker) Work(ctx context.Context, job *river.Job[ChangeRequestNoteArgs]) error {
	return w.s.note(ctx, job.Args)
}

// ServiceNowSweepArgs opens and reads the change requests due.
type ServiceNowSweepArgs struct{}

// Kind names the job.
func (ServiceNowSweepArgs) Kind() string { return "servicenow_sweep" }

// InsertOpts keeps one sweep at a time in the queue.
func (ServiceNowSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByPeriod: SweepEvery}}
}

// ServiceNowPeriodic is the change requests' sweep's periodic job, for jobs.Options.
func ServiceNowPeriodic() *river.PeriodicJob {
	return river.NewPeriodicJob(river.PeriodicInterval(SweepEvery), func() (river.JobArgs, *river.InsertOpts) {
		return ServiceNowSweepArgs{}, nil
	}, &river.PeriodicJobOpts{RunOnStart: true})
}

type serviceNowSweepWorker struct {
	river.WorkerDefaults[ServiceNowSweepArgs]
	s *Service
}

func (w *serviceNowSweepWorker) Work(ctx context.Context, _ *river.Job[ServiceNowSweepArgs]) error {
	return w.s.SweepServiceNow(ctx)
}

// SweepServiceNow opens and reads the change requests due, at most snBatch, snParallel at once,
// the most overdue first. When ServiceNow is unreachable, the sweep stops: the others wait for the
// next.
func (s *Service) SweepServiceNow(ctx context.Context) error {
	if s.sn == nil {
		return nil
	}
	due, err := s.store.DueChangeRequests(ctx, db.DueChangeRequestsParams{Now: instant(s.now()), MaxRows: snBatch})
	if err != nil {
		return err
	}
	var (
		stop atomic.Bool
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	slots := make(chan struct{}, snParallel)
	for _, cr := range due {
		slots <- struct{}{}
		if stop.Load() || ctx.Err() != nil {
			<-slots
			break
		}
		wg.Go(func() {
			defer func() { <-slots }()
			err := s.syncLocked(ctx, cr.ID)
			if servicenow.Unreachable(err) {
				stop.Store(true)
			}
			var f *snFailure
			if err != nil && !errors.As(err, &f) {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// PollServiceNowSoon reads the request's change request again, from a job, its page being open:
// not when it was read in the last snSoon, nor while ServiceNow fails (its backoff holds).
func (s *Service) PollServiceNowSoon(ctx context.Context, r db.Request) {
	if s.snCfg == nil || s.jobs == nil || !waitsOnChange(r) {
		return
	}
	cr, err := s.store.CurrentChangeRequest(ctx, r.ID)
	if err != nil {
		return
	}
	now := s.now()
	if cr.Failures > 0 && cr.NextPollAt.Valid && cr.NextPollAt.Time.After(now) || cr.PolledAt.Valid && now.Sub(cr.PolledAt.Time) < snSoon {
		return
	}
	if _, err := s.jobs.Insert(ctx, ChangeRequestArgs{ChangeRequestID: cr.ID}, nil); err != nil {
		s.log.Warn("the change request's reading", "request", r.ID, "error", err)
	}
}

// snFailure is a call about a change request that failed, recorded on it; retry: River tries it
// again (a change request the sweep does not take, superseded).
type snFailure struct {
	err   error
	retry bool
}

func (f *snFailure) Error() string { return f.err.Error() }
func (f *snFailure) Unwrap() error { return f.err }

// syncLocked opens or reads a change request under its advisory lock: one at a time, whichever of
// the sweep, its job or the page asked; nothing when another holds it.
func (s *Service) syncLocked(ctx context.Context, id uuid.UUID) error {
	_, err := jobs.TryLock(ctx, s.store.Locks, "servicenow:"+id.String(), func(ctx context.Context) error {
		return s.sync(ctx, id)
	})
	return err
}

// sync takes a change request's next steps: opened in ServiceNow, prepared (its long fields, taken
// for approval), then read. Each step is recorded once done, so that a crash or a failure takes it
// again, and only it.
func (s *Service) sync(ctx context.Context, id uuid.UUID) error {
	if s.sn == nil {
		return nil
	}
	for range 3 {
		cr, err := s.store.GetChangeRequest(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		r, err := s.get(ctx, cr.RequestID)
		if err != nil {
			return err
		}
		switch {
		case cr.SupersededAt.Valid && cr.SysID.Valid:
			return nil // its note says it is superseded
		case cr.SupersededAt.Valid:
			return s.findSuperseded(ctx, cr)
		case !cr.SysID.Valid && ended(r):
			return nil
		case !cr.SysID.Valid:
			err = s.create(ctx, r, cr)
		case !cr.PreparedAt.Valid:
			err = s.prepare(ctx, r, cr)
		case !waitsOnChange(r):
			return nil
		default:
			return s.poll(ctx, cr)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// failed records a call about a change request that failed, and when to try it again: an
// exponential backoff from a minute (opening) or the poll interval (reading), within snMaxWait.
func (s *Service) failed(ctx context.Context, cr db.ChangeRequest, op string, err error) error {
	result := "refused"
	if servicenow.Unreachable(err) {
		result = "unreachable"
	}
	telemetry.ServiceNowCalled(ctx, op, result)
	now := s.now()
	base := time.Minute
	if cr.SysID.Valid && cr.PreparedAt.Valid {
		base = s.snCfg.PollInterval
	}
	wait := min(base<<min(cr.Failures, 12), snMaxWait)
	level, msg := "warn", "ServiceNow failed"
	if cr.FailingSince.Valid && now.Sub(cr.FailingSince.Time) > snLongOutage {
		level, msg = "error", "ServiceNow has been failing for over an hour: the requests that wait on it wait"
	}
	attrs := []any{"operation", op, "request", cr.RequestID, "changeRequest", cr.Number, "failures", cr.Failures + 1,
		"retryAt", now.Add(wait), "error", err}
	if level == "error" {
		s.log.Error(msg, attrs...)
	} else {
		s.log.Warn(msg, attrs...)
	}
	if dbErr := s.store.SetChangeRequestFailed(ctx, db.SetChangeRequestFailedParams{ID: cr.ID, Failure: oneLine(err.Error(), 300),
		At: instant(now), NextPollAt: instant(now.Add(wait))}); dbErr != nil {
		return dbErr
	}
	return &snFailure{err: err, retry: cr.SupersededAt.Valid}
}

// oneLine is s on one line, cut to max bytes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "") + "…"
	}
	return s
}

// create creates the change request in ServiceNow, or finds the one an earlier try created whose
// answer was lost (by its correlation ID), and records its sys_id: the only change request Quai
// reads from then on.
func (s *Service) create(ctx context.Context, r db.Request, cr db.ChangeRequest) error {
	rec, err := s.sn.Find(ctx, cr.CorrelationID)
	if err != nil {
		return s.failed(ctx, cr, "open", err)
	}
	problem := ""
	if rec == nil {
		ch, err := s.changeOf(ctx, r, cr)
		if err != nil {
			return err
		}
		created, err := s.sn.Create(ctx, ch)
		if err != nil {
			return s.failed(ctx, cr, "open", err)
		}
		rec = &created
		if created.CorrelationID != cr.CorrelationID || slices.Contains(created.Ignored, "correlation_id") {
			problem = "ServiceNow did not keep Quai's correlation ID on " + created.Number + ": Quai does not take its approval"
		}
	}
	telemetry.ServiceNowCalled(ctx, "open", "ok")
	return s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		locked, err := q.LockChangeRequest(ctx, cr.ID)
		if err != nil {
			return err
		}
		n, err := q.SetChangeRequestOpened(ctx, db.SetChangeRequestOpenedParams{ID: cr.ID, SysID: text(rec.SysID), Number: rec.Number,
			State: rec.State, StateLabel: rec.Label(), Approval: rec.Approval, At: instant(s.now())})
		if err != nil || n == 0 {
			return err
		}
		if problem != "" {
			s.log.Warn(problem, "request", r.ID)
			if err := q.SetChangeRequestPrepared(ctx, db.SetChangeRequestPreparedParams{ID: cr.ID, At: instant(s.now()), Problem: problem}); err != nil {
				return err
			}
		}
		locked.SysID, locked.Number = text(rec.SysID), rec.Number
		data := map[string]any{"changeRequest": rec.Number, "kind": cr.Kind, "url": s.sn.Link(rec.SysID)}
		if err := s.event(ctx, tx, r, "change_request_opened", "", data); err != nil {
			return err
		}
		if err := recordRequest(ctx, tx, r, "change_request_opened", changeRequestDetails(locked)); err != nil {
			return err
		}
		if locked.SupersededAt.Valid && s.jobs != nil {
			return jobs.InsertTx(ctx, s.jobs, tx, ChangeRequestNoteArgs{ChangeRequestID: cr.ID, Note: noteSuperseded}, nil)
		}
		return nil
	})
}

// findSuperseded finds a superseded change request that ServiceNow may have created without its
// answer reaching Quai, records it, and notes that it is superseded.
func (s *Service) findSuperseded(ctx context.Context, cr db.ChangeRequest) error {
	rec, err := s.sn.Find(ctx, cr.CorrelationID)
	if err != nil {
		return s.failed(ctx, cr, "open", err)
	}
	if rec == nil {
		return nil
	}
	r, err := s.get(ctx, cr.RequestID)
	if err != nil {
		return err
	}
	return s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		n, err := q.SetChangeRequestOpened(ctx, db.SetChangeRequestOpenedParams{ID: cr.ID, SysID: text(rec.SysID), Number: rec.Number,
			State: rec.State, StateLabel: rec.Label(), Approval: rec.Approval, At: instant(s.now())})
		if err != nil || n == 0 {
			return err
		}
		cr.SysID, cr.Number = text(rec.SysID), rec.Number
		if err := recordRequest(ctx, tx, r, "change_request_opened", changeRequestDetails(cr)); err != nil {
			return err
		}
		if s.jobs == nil {
			return nil
		}
		return jobs.InsertTx(ctx, s.jobs, tx, ChangeRequestNoteArgs{ChangeRequestID: cr.ID, Note: noteSuperseded}, nil)
	})
}

// prepare writes the change request's long fields, then takes it for approval: a normal change
// from New to Assess, a standard one from New to Scheduled, as ServiceNow's state model allows. A
// move ServiceNow refuses is recorded as the change request's problem, for people to see: it waits
// for someone to move it in ServiceNow.
func (s *Service) prepare(ctx context.Context, r db.Request, cr db.ChangeRequest) error {
	fields, err := s.fieldsOf(ctx, r, cr)
	if err != nil {
		return err
	}
	if err := s.sn.Update(ctx, cr.SysID.String, fields); err != nil {
		return s.failed(ctx, cr, "open", err)
	}
	rec, err := s.sn.Get(ctx, cr.SysID.String)
	if err != nil {
		return s.failed(ctx, cr, "open", err)
	}
	problem := ""
	if rec.State == "-5" {
		to := "assess"
		if cr.Kind == "standard" {
			to = "scheduled"
		}
		if _, err := s.sn.Move(ctx, cr.SysID.String, to); servicenow.Unreachable(err) {
			return s.failed(ctx, cr, "open", err)
		} else if err != nil {
			problem = oneLine("ServiceNow did not take "+cr.Number+" to "+to+": "+err.Error(), 300)
			s.log.Warn(problem, "request", r.ID)
		}
	}
	telemetry.ServiceNowCalled(ctx, "open", "ok")
	return s.store.SetChangeRequestPrepared(ctx, db.SetChangeRequestPreparedParams{ID: cr.ID, At: instant(s.now()), Problem: problem})
}

// poll reads the change request from ServiceNow, by its sys_id, and applies what it says.
func (s *Service) poll(ctx context.Context, cr db.ChangeRequest) error {
	rec, err := s.sn.Get(ctx, cr.SysID.String)
	if err != nil {
		return s.failed(ctx, cr, "poll", err)
	}
	telemetry.ServiceNowCalled(ctx, "poll", "ok")
	return s.applyReading(ctx, cr.ID, rec)
}

// applyReading applies what ServiceNow reports of a change request, under its request's lock:
// recorded on the change request still current for the request's content, and, when it decides
// the request, through the same transitions as a review. A reading of a change request
// superseded meanwhile, or of another sys_id, changes nothing. Only an explicit approval of a
// scheduled or implementing change approves (servicenow.Record.Outcome); a value ServiceNow does
// not document, or a correlation ID that is not Quai's, keeps it pending, and is logged.
func (s *Service) applyReading(ctx context.Context, id uuid.UUID, rec servicenow.Record) error {
	outcome, known := rec.Outcome()
	return s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		cr, err := q.GetChangeRequest(ctx, id)
		if err != nil {
			return err
		}
		r, err := q.LockRequest(ctx, cr.RequestID)
		if err != nil {
			return err
		}
		if cr, err = q.LockChangeRequest(ctx, id); err != nil {
			return err
		}
		sets, err := q.ListChangeSets(ctx, r.ID)
		if err != nil {
			return err
		}
		if cr.SupersededAt.Valid || cr.SysID.String != rec.SysID || len(sets) == 0 || sets[0].Hash != cr.ChangeSetHash {
			return nil
		}
		switch {
		case rec.CorrelationID != cr.CorrelationID:
			s.log.Warn("ServiceNow reports another correlation ID on the change request than Quai set: it stays pending",
				"request", r.ID, "changeRequest", cr.Number)
			outcome = servicenow.Pending
		case !known:
			s.log.Warn("ServiceNow reports a state or an approval of the change request that Quai does not know: it stays pending",
				"request", r.ID, "changeRequest", cr.Number, "state", oneLine(rec.State, 40), "approval", oneLine(rec.Approval, 40))
		}
		now := s.now()
		if err := q.SetChangeRequestPolled(ctx, db.SetChangeRequestPolledParams{ID: id, State: rec.State, StateLabel: rec.Label(),
			Approval: rec.Approval, Outcome: outcome, At: instant(now), NextPollAt: instant(now.Add(s.snCfg.PollInterval))}); err != nil {
			return err
		}
		if !waitsOnChange(r) {
			return nil
		}
		cr.Outcome, cr.State, cr.Approval = outcome, rec.State, rec.Approval
		return s.decideByChange(ctx, tx, q, r, cr, sets[0].Hash)
	})
}

// decideByChange decides a request with what its change request says, under the request's lock:
// approved when it satisfies the last rule the request waited for, rejected when ServiceNow
// rejected, canceled or closed it, back to pending approval when an approval it had is gone.
func (s *Service) decideByChange(ctx context.Context, tx pgx.Tx, q *db.Queries, r db.Request, cr db.ChangeRequest, hash string) error {
	var rules []policy.Rule
	_ = json.Unmarshal(r.Rules, &rules)
	all, err := q.ListReviews(ctx, r.ID)
	if err != nil {
		return err
	}
	reviews := counted(r, all, hash)
	pc := contextOf(r)
	pc.External = externalOf(cr, hash)
	outcome := policy.Evaluate(rules, toReviews(reviews, s.dir), pc)
	data := map[string]any{"changeRequest": cr.Number, "system": ServiceNowSystem}
	object := audit.Object{Type: "request", ID: r.ID.String(), Resource: r.Target}
	said := changeRequestDetails(cr)
	said["state"], said["approval"], said["outcome"] = cr.State, cr.Approval, cr.Outcome
	switch {
	case r.State == PendingApproval && outcome.State == policy.Approved:
		if _, err := q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: PendingApproval, ToState: Approved}); err != nil {
			return conflict(err, r)
		}
		if err := s.event(ctx, tx, r, "approved", "", data); err != nil {
			return err
		}
		details := approvedDetails(hash, r, reviews)
		details["changeRequest"] = said
		if err := audit.Add(ctx, tx, audit.Event{Actor: serviceNowActor, Action: "request.approved", Object: object, Details: details}); err != nil {
			return err
		}
		if s.jobs == nil {
			return nil
		}
		return jobs.InsertTx(ctx, s.jobs, tx, DeliverArgs{RequestID: r.ID}, nil)
	case outcome.State == policy.Denied && cr.Outcome != servicenow.Pending && cr.Outcome != servicenow.Approved:
		if _, err := q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: r.State, ToState: Rejected}); err != nil {
			return conflict(err, r)
		}
		actor := "ServiceNow"
		if cr.Number != "" {
			actor += " (" + cr.Number + ")"
		}
		data["outcome"] = cr.Outcome
		if err := s.event(ctx, tx, r, "rejected", actor, data); err != nil {
			return err
		}
		if err := audit.Add(ctx, tx, audit.Event{Actor: serviceNowActor, Action: "request.rejected", Object: object,
			Details: map[string]any{"changeSet": hash, "changeRequest": said}}); err != nil {
			return err
		}
		return s.statusJob(ctx, tx, q, r)
	case r.State == Approved && outcome.State != policy.Approved:
		next, err := q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: Approved, ToState: PendingApproval})
		if err != nil {
			return conflict(err, r)
		}
		data["reason"] = "ServiceNow no longer reports " + cr.Number + " approved"
		if err := s.event(ctx, tx, r, "approval_needed", "", data); err != nil {
			return err
		}
		if err := audit.Add(ctx, tx, audit.Event{Actor: serviceNowActor, Action: "request.approval_withdrawn", Object: object,
			Details: map[string]any{"changeSet": hash, "changeRequest": said}}); err != nil {
			return err
		}
		return s.statusJob(ctx, tx, q, next)
	}
	return nil
}

// note adds a work note to a change request; one not opened yet is opened first.
func (s *Service) note(ctx context.Context, a ChangeRequestNoteArgs) error {
	if s.sn == nil {
		return nil
	}
	cr, err := s.store.GetChangeRequest(ctx, a.ChangeRequestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !cr.SysID.Valid {
		if err := s.syncLocked(ctx, cr.ID); err != nil {
			return err
		}
		if cr, err = s.store.GetChangeRequest(ctx, a.ChangeRequestID); err != nil {
			return err
		}
		if !cr.SysID.Valid {
			return fmt.Errorf("change request %s is not opened in ServiceNow yet", cr.CorrelationID)
		}
	}
	r, err := s.get(ctx, cr.RequestID)
	if err != nil {
		return err
	}
	text, err := s.noteText(ctx, r, cr, a)
	if err != nil || text == "" {
		return err
	}
	if err := s.sn.WorkNote(ctx, cr.SysID.String, text); err != nil {
		result := "refused"
		if servicenow.Unreachable(err) {
			result = "unreachable"
		}
		telemetry.ServiceNowCalled(ctx, "note", result)
		return err
	}
	telemetry.ServiceNowCalled(ctx, "note", "ok")
	return nil
}

// noteText is a work note's text: identifiers, counts and the request's link, never a plan's
// values, a break-glass's reason or another request's data.
func (s *Service) noteText(ctx context.Context, r db.Request, cr db.ChangeRequest, a ChangeRequestNoteArgs) (string, error) {
	link := ""
	if l := s.link(r); l != "" {
		link = "\nRequest: " + l
	}
	switch a.Note {
	case notePlan:
		run, err := s.store.GetRun(ctx, a.Run)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return "Quai: " + planSummary(run) + link, nil
	case noteSuperseded:
		sets, err := s.store.ListChangeSets(ctx, r.ID)
		if err != nil {
			return "", err
		}
		now := ""
		if len(sets) > 0 {
			now = " (now change set " + short(sets[0].Hash) + ")"
		}
		return fmt.Sprintf("Quai: this change request no longer counts. The content of request #%d changed%s, and an approval is for the "+
			"content it was given for: Quai opens a new change request for the new content.%s", r.Number, now, link), nil
	case noteBreakGlass:
		bg, err := s.store.GetBreakGlass(ctx, r.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		// Identifiers only (open question 97): never the reason.
		return fmt.Sprintf("Quai: glass was broken for request #%d at %s UTC by %s, for incident %s: it merges without its approvals "+
			"and outside its change windows, and Quai no longer waits on this change request.%s",
			r.Number, bg.At.Time.UTC().Format(time.DateTime), bg.Breaker, bg.Incident, link), nil
	}
	return "", fmt.Errorf("unknown work note %q", a.Note)
}

// planSummary says what a plan reported: its stack, its outcome, what it adds, changes and
// destroys, its cost and policies; never a value.
func planSummary(run db.Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "the plan of %s %s", run.Stack, run.Status)
	if run.FinishedAt.Valid {
		fmt.Fprintf(&b, " at %s UTC", run.FinishedAt.Time.UTC().Format(time.DateTime))
	}
	var sum struct{ Add, Change, Destroy int }
	if json.Unmarshal(run.Summary, &sum) == nil && run.Status == "succeeded" {
		fmt.Fprintf(&b, ": %d to add, %d to change, %d to destroy", sum.Add, sum.Change, sum.Destroy)
	}
	b.WriteString(".")
	var cost struct {
		Currency     string  `json:"currency"`
		MonthlyDelta float64 `json:"monthlyDelta"`
	}
	if json.Unmarshal(run.Cost, &cost) == nil && cost.Currency != "" {
		fmt.Fprintf(&b, " Cost: %+.2f %s a month.", cost.MonthlyDelta, cost.Currency)
	}
	var policies []struct{ Name, Result string }
	if json.Unmarshal(run.Policies, &policies) == nil && len(policies) > 0 {
		var said []string
		for _, p := range policies {
			said = append(said, p.Name+" "+p.Result)
		}
		b.WriteString(" Policies: " + strings.Join(said, ", ") + ".")
	}
	return b.String()
}

// short is a hash's first 12 characters.
func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// requestTitle is a request's title, as its page shows it.
func requestTitle(r db.Request) string {
	switch r.Kind {
	case "create":
		return "Create " + r.Target
	case "update":
		return "Update " + r.Target
	case "delete":
		return "Delete " + r.Target
	case "action":
		a := r.Action
		if a == "" {
			a = "Action"
		}
		return a + " on " + r.Target
	case "migrate":
		return "Migrate " + r.Target
	}
	return r.Target
}

// changeOf is the change request created for a request's content: its short description, the
// correlation, the planned window, and the configured fields (itsm.servicenow.fields, as written:
// they are the operator's). The long fields follow (fieldsOf).
func (s *Service) changeOf(ctx context.Context, r db.Request, cr db.ChangeRequest) (servicenow.Change, error) {
	create := map[string]string{}
	for k, v := range s.snCfg.Fields {
		create[k] = v
	}
	create["short_description"] = servicenow.Text(fmt.Sprintf("Quai #%d: %s", r.Number, requestTitle(r)), servicenow.MaxShort)
	create["correlation_id"] = cr.CorrelationID
	create["correlation_display"] = servicenow.Text(fmt.Sprintf("Quai #%d", r.Number), servicenow.MaxCorrelation)
	start, end, err := s.plannedWindow(ctx, r)
	if err != nil {
		return servicenow.Change{}, err
	}
	if start != "" {
		create["start_date"] = start
	}
	if end != "" {
		create["end_date"] = end
	}
	return servicenow.Change{Template: cr.Template, Create: create}, nil
}

// plannedWindow is when the request's change windows let it merge next, in ServiceNow's format
// (UTC): from now to their close when open, their next opening to its close when closed; nothing
// when no window applies, or they cannot be decided.
func (s *Service) plannedWindow(ctx context.Context, r db.Request) (string, string, error) {
	ws, err := s.WindowsOf(ctx, r)
	if err != nil || ws.Err != nil || len(ws.Applied) == 0 {
		return "", "", nil // without windows, ServiceNow's own planning
	}
	var from, to time.Time
	switch {
	case ws.Open:
		from, to = ws.At, ws.Closes
	case !ws.Opens.IsZero():
		from, to = ws.Opens, window.At(ws.Applied, ws.Opens).Closes
	}
	return snTime(from), snTime(to), nil
}

// snTime is a time as ServiceNow's API takes a date and time: in UTC; "" for none.
func snTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.DateTime)
}

// maxFiles is how many files a change request lists.
const maxFiles = 50

// fieldsOf are the long fields of a request's change request, each neutral for ServiceNow and cut
// to its limit: the description (who asks for what, the link), the justification (the reason), the
// implementation plan (the files and the lines they change, never their content), and the risk
// and impact analysis (the rules that apply). A standard change keeps its template's description
// and implementation plan: what they would say is a work note instead.
func (s *Service) fieldsOf(ctx context.Context, r db.Request, cr db.ChangeRequest) (map[string]string, error) {
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	if err != nil || len(sets) == 0 {
		return nil, fmt.Errorf("request %s has no change set: %w", r.ID, err)
	}
	cs := sets[0]
	changes, err := s.store.ListChanges(ctx, cs.ID)
	if err != nil {
		return nil, err
	}
	var desc strings.Builder
	fmt.Fprintf(&desc, "Requested in Quai by %s: %s, in %s.\n", r.Requester, requestTitle(r), cs.Repository)
	switch {
	case r.TypeName != "":
		fmt.Fprintf(&desc, "Type: %s. ", r.TypeName)
	case r.Blueprint != "":
		fmt.Fprintf(&desc, "Blueprint: %s. ", r.Blueprint)
	}
	fmt.Fprintf(&desc, "Request #%d (%s).", r.Number, r.ID)
	if l := s.link(r); l != "" {
		desc.WriteString("\nRequest: " + l)
	}
	var impl strings.Builder
	fmt.Fprintf(&impl, "Quai merges the request's change into %s through its Git review, as approved in Quai; the deployment pipeline applies it.\n", cs.Repository)
	fmt.Fprintf(&impl, "Change set %s, %d files:\n", cs.Hash, len(changes))
	for i, c := range changes {
		if i == maxFiles {
			fmt.Fprintf(&impl, "and %d more files\n", len(changes)-maxFiles)
			break
		}
		added, removed := 0, 0
		for _, l := range (plan.Change{Path: c.Path, Op: c.Op, Before: c.Before, After: c.After}).Diff() {
			switch l.Op {
			case '+':
				added++
			case '-':
				removed++
			}
		}
		fmt.Fprintf(&impl, "- %s %s (+%d -%d)\n", c.Op, c.Path, added, removed)
	}
	var rules []policy.Rule
	_ = json.Unmarshal(r.Rules, &rules)
	var risk strings.Builder
	risk.WriteString("The approval rules of the request in Quai:\n")
	for _, rule := range rules {
		switch {
		case rule.NotApplicable():
			continue
		case rule.External != "":
			fmt.Fprintf(&risk, "- %s: %s's approval", strings.Join(rule.Names, ", "), rule.External)
		default:
			fmt.Fprintf(&risk, "- %s: %d of %s", strings.Join(rule.Names, ", "), rule.Min, strings.Join(rule.Approvers, ", "))
		}
		if rule.Undecided() {
			risk.WriteString(", if its plan makes it apply")
		}
		risk.WriteString("\n")
	}
	risk.WriteString("The plan's result is added as a work note when it reports.")
	fields := map[string]string{
		"justification":        servicenow.Text(r.Reason, servicenow.MaxText),
		"risk_impact_analysis": servicenow.Text(risk.String(), servicenow.MaxText),
	}
	if cr.Kind == "standard" {
		fields["work_notes"] = servicenow.Text("Quai: "+desc.String()+"\n\n"+impl.String(), servicenow.MaxText)
	} else {
		fields["description"] = servicenow.Text(desc.String(), servicenow.MaxText)
		fields["implementation_plan"] = servicenow.Text(impl.String(), servicenow.MaxText)
	}
	return fields, nil
}

// notePlanReported adds the plan's result to the request's change request, from a job.
func (s *Service) notePlanReported(ctx context.Context, r db.Request, run db.Run) {
	if s.snCfg == nil || s.jobs == nil {
		return
	}
	cr, err := s.store.CurrentChangeRequest(ctx, r.ID)
	if err != nil {
		return
	}
	if _, err := s.jobs.Insert(ctx, ChangeRequestNoteArgs{ChangeRequestID: cr.ID, Note: notePlan, Run: run.ID}, nil); err != nil {
		s.log.Warn("the plan's work note", "request", r.ID, "error", err)
	}
}

// noteBroken records break-glass on the request's change request, in tx: a work note with its
// time, who broke glass and the incident. Open question 97; P4-20 links the incident to it.
func (s *Service) noteBroken(ctx context.Context, tx pgx.Tx, q *db.Queries, r db.Request) error {
	if s.snCfg == nil || s.jobs == nil {
		return nil
	}
	cr, err := q.CurrentChangeRequest(ctx, r.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return jobs.InsertTx(ctx, s.jobs, tx, ChangeRequestNoteArgs{ChangeRequestID: cr.ID, Note: noteBreakGlass}, nil)
}

// ChangeRequest is a request's change request in ServiceNow, for its page.
type ChangeRequest struct {
	db.ChangeRequest
	// URL is its form in ServiceNow, from the configured instance and its sys_id.
	URL string
	// Superseded is how many change requests of earlier content the request had.
	Superseded int
}

// ChangeRequestOf returns a request's current change request; ok false when it has none. Without
// ServiceNow's configuration, a request a rule waits on ServiceNow for has none, and
// NotConfigured says so.
func (s *Service) ChangeRequestOf(ctx context.Context, r db.Request) (ChangeRequest, bool, error) {
	list, err := s.store.ListChangeRequests(ctx, r.ID)
	if err != nil || len(list) == 0 || list[0].SupersededAt.Valid {
		return ChangeRequest{}, false, err
	}
	c := ChangeRequest{ChangeRequest: list[0], Superseded: len(list) - 1}
	if s.sn != nil && list[0].SysID.Valid {
		c.URL = s.sn.Link(list[0].SysID.String)
	}
	return c, true, nil
}

// ServiceNowMissing reports whether a rule of the request waits on ServiceNow and Quai is not
// configured to ask it: the rule is never satisfied.
func (s *Service) ServiceNowMissing(r db.Request) bool {
	var rules []policy.Rule
	_ = json.Unmarshal(r.Rules, &rules)
	return s.snCfg == nil && slices.ContainsFunc(rules, func(r policy.Rule) bool { return r.External == ServiceNowSystem && !r.NotApplicable() })
}
