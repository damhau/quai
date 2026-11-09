package requests

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/servicenow"
	"github.com/damhau/quai/backend/internal/store/db"
	"github.com/damhau/quai/backend/internal/telemetry"
)

// Closing a change request (docs/design/05-delivery-and-approvals.md#itsm, open questions 115 to
// 118). Once the content a request's change request is for is applied, or its apply failed, Quai
// attaches the request's evidence to it (the bundle without its proof, and its PDF: open question
// 102), then takes it Implement, Review and Closed, with the close code successful or
// unsuccessful and close notes. Only the change request whose sys_id Quai stored, current, for the
// content that landed: a superseded one is never closed. One never approved (broken by
// break-glass, or merged outside Quai) gets the evidence and a work note, and is left to its change
// manager. Each step is recorded on the change request, or read from ServiceNow, before it is
// taken, so that a retry attaches nothing twice and moves nothing twice: a file tried before is
// looked up by its name, and the state is read first. A move ServiceNow refuses is a problem on
// the request page, and is not tried again; an unreachable ServiceNow is tried again with the
// backoff of failed().

// Evidence makes the evidence Quai attaches to a request's change request: the bundle's JSON
// without its proof, and its PDF summary (open question 102). The api package makes it.
type Evidence func(ctx context.Context, r db.Request) (bundle, pdf []byte, err error)

// UseEvidence gives the evidence attached to change requests when they close.
func (s *Service) UseEvidence(e Evidence) { s.evidence = e }

// closeOutcome is the close code of a request's change request after a transition: successful
// when its content is applied, unsuccessful when its apply failed; "" for any other (a delivery
// that failed leaves its change request as it is, open question 114).
func closeOutcome(to, kind string) string {
	switch {
	case to == Applied:
		return servicenow.CloseSuccessful
	case to == Failed && kind == "apply_failed":
		return servicenow.CloseUnsuccessful
	}
	return ""
}

// closing reports whether Quai is to close a change request, and is not done.
func closing(cr db.ChangeRequest) bool { return cr.CloseRequestedAt.Valid && !cr.ClosedAt.Valid }

// closeChangeRequest records, in tx, that the request's change request is to be closed with the
// outcome, and the job that closes it: only its current one, for the content that landed (the
// request's change set now); nothing without ServiceNow, when it has none, or when it was asked
// already.
func (s *Service) closeChangeRequest(ctx context.Context, tx pgx.Tx, q *db.Queries, r db.Request, outcome string) error {
	if s.snCfg == nil {
		return nil
	}
	cr, err := q.CurrentChangeRequest(ctx, r.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	sets, err := q.ListChangeSets(ctx, r.ID)
	if err != nil {
		return err
	}
	if len(sets) == 0 || sets[0].Hash != cr.ChangeSetHash {
		return nil // for other content than landed: it is no longer the request's
	}
	token := make([]byte, 8)
	_, _ = rand.Read(token)
	n, err := q.SetChangeRequestClosing(ctx, db.SetChangeRequestClosingParams{ID: cr.ID, CloseOutcome: outcome, At: instant(s.now()),
		EvidenceToken: hex.EncodeToString(token)})
	if err != nil || n == 0 || s.jobs == nil {
		return err // without jobs, the sweep closes it
	}
	return jobs.InsertTx(ctx, s.jobs, tx, ChangeRequestCloseArgs{ChangeRequestID: cr.ID}, nil)
}

// ChangeRequestCloseArgs closes a change request in ServiceNow, its content applied or its apply
// failed.
type ChangeRequestCloseArgs struct {
	ChangeRequestID uuid.UUID `json:"changeRequest"`
}

// Kind names the job.
func (ChangeRequestCloseArgs) Kind() string { return "servicenow_close" }

// InsertOpts closes a change request once; a failure is recorded on it, and the sweep tries it
// again when due.
func (ChangeRequestCloseArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 10, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

type closeWorker struct {
	river.WorkerDefaults[ChangeRequestCloseArgs]
	s *Service
}

func (w *closeWorker) Work(ctx context.Context, job *river.Job[ChangeRequestCloseArgs]) error {
	err := w.s.syncLocked(ctx, job.Args.ChangeRequestID)
	var f *snFailure
	if errors.As(err, &f) && !f.retry {
		return nil // recorded: the sweep tries again when due
	}
	return err
}

// How Quai is done with a change request it was to close.
const (
	closedByQuai  = "closed"  // Quai closed it
	closedAlready = "already" // closed or canceled in ServiceNow before Quai did
	closedLeft    = "left"    // never approved, or not Quai's: the evidence and a note, its state left to its change manager
	closeRefused  = "refused" // ServiceNow refused a move: the problem says which
)

// evidenceFiles are the files of a change request's evidence, their names Quai's own: the
// request's number and the change request's random token (open question 116), never a person's
// text.
func evidenceFiles(r db.Request, cr db.ChangeRequest) []struct{ name, contentType string } {
	base := fmt.Sprintf("quai-evidence-%d-%s", r.Number, cr.EvidenceToken)
	return []struct{ name, contentType string }{{base + ".json", "application/json"}, {base + ".pdf", "application/pdf"}}
}

// close attaches the request's evidence to its change request, then closes it, from the state
// ServiceNow reports: Scheduled, Implement or Review, approved, are taken to Closed; one closed or
// canceled is left as it is; one never approved gets a work note and is left to its change manager.
// Only a change request whose correlation ID is still Quai's gets anything.
func (s *Service) close(ctx context.Context, r db.Request, cr db.ChangeRequest) error {
	sysID := cr.SysID.String
	rec, err := s.sn.Get(ctx, sysID)
	if err != nil {
		return s.failed(ctx, cr, "close", err)
	}
	if rec.SysID != sysID {
		return s.failed(ctx, cr, "close", errors.New("ServiceNow answered another change request"))
	}
	if rec.CorrelationID != cr.CorrelationID {
		problem := "ServiceNow reports another correlation ID on " + cr.Number + " than Quai set: Quai neither attached the evidence nor closed it"
		s.log.Warn(problem, "request", r.ID)
		return s.closed(ctx, r, cr, rec, closedLeft, problem)
	}
	var problems []string
	if problem, err := s.attachEvidence(ctx, r, cr); err != nil {
		return err
	} else if problem != "" {
		problems = append(problems, problem)
	}
	attached, err := s.store.ListChangeRequestAttachments(ctx, cr.ID)
	if err != nil {
		return err
	}
	var names []string
	for _, a := range attached {
		if a.SysID.Valid {
			names = append(names, a.Name)
		}
	}
	result := closedByQuai
	switch {
	case rec.State == "3" && rec.CloseCode == cr.CloseOutcome:
		// Closed as Quai closes it: by Quai, an answer lost, or by someone else the same way.
	case rec.State == "3" || rec.State == "4":
		result = closedAlready
	case rec.Approval != "approved" || rec.State != "-2" && rec.State != "-1" && rec.State != "0":
		result = closedLeft
		if err := s.sn.WorkNote(ctx, sysID, s.leftNote(r, cr, names)); servicenow.Unreachable(err) {
			return s.failed(ctx, cr, "close", err)
		} else if err != nil {
			problems = append(problems, oneLine("ServiceNow refused Quai's work note on "+cr.Number+": "+err.Error(), 300))
		}
	default:
		var problem string
		if rec, problem, err = s.toClosed(ctx, r, cr, rec, names); err != nil {
			return err
		}
		if problem != "" {
			result = closeRefused
			problems = append(problems, problem)
		}
	}
	problem := strings.Join(problems, " ")
	if problem != "" {
		s.log.Warn("ServiceNow refused part of closing the change request", "request", r.ID, "changeRequest", cr.Number, "problem", problem)
	}
	telemetry.ServiceNowCalled(ctx, "close", "ok")
	return s.closed(ctx, r, cr, rec, result, oneLine(problem, 600))
}

// toClosed takes the change request from the state ServiceNow reports to Closed: Scheduled to
// Implement, Implement to Review, Review to Closed with its close code and notes, each answer
// reporting the state asked for. A refusal, or an answer in another state, is the problem
// returned, and nothing more is asked.
func (s *Service) toClosed(ctx context.Context, r db.Request, cr db.ChangeRequest, rec servicenow.Record, files []string) (servicenow.Record, string, error) {
	sysID := cr.SysID.String
	for range 3 {
		var (
			next servicenow.Record
			err  error
			to   string
			want string
		)
		switch rec.State {
		case "-2":
			to, want = "implement", "-1"
			next, err = s.sn.Move(ctx, sysID, to)
		case "-1":
			to, want = "review", "0"
			next, err = s.sn.Move(ctx, sysID, to)
		case "0":
			to, want = "closed", "3"
			next, err = s.sn.Close(ctx, sysID, cr.CloseOutcome, s.closeNotes(r, cr, files))
		default:
			return rec, "", nil
		}
		switch {
		case servicenow.Unreachable(err):
			return rec, "", s.failed(ctx, cr, "close", err)
		case err != nil:
			return rec, oneLine("ServiceNow did not take "+cr.Number+" to "+to+": "+err.Error(), 300), nil
		case next.SysID != sysID || next.State != want:
			return rec, fmt.Sprintf("ServiceNow answered %s in %s after Quai asked for %s", cr.Number, oneLine(next.Label(), 40), to), nil
		}
		rec = next
	}
	return rec, "", nil
}

// closeNotes are the close notes of a change request Quai closes: what happened to the request's
// content, when, and where its evidence is; identifiers only.
func (s *Service) closeNotes(r db.Request, cr db.ChangeRequest, files []string) string {
	at := cr.CloseRequestedAt.Time.UTC().Format(time.DateTime)
	what := fmt.Sprintf("Quai: request #%d was applied at %s UTC (change set %s).", r.Number, at, short(cr.ChangeSetHash))
	if cr.CloseOutcome == servicenow.CloseUnsuccessful {
		what = fmt.Sprintf("Quai: the apply of request #%d failed at %s UTC (change set %s).", r.Number, at, short(cr.ChangeSetHash))
	}
	return what + s.evidenceSaid(r, files)
}

// leftNote is the work note on a change request Quai does not close, never approved: it was
// merged without its approval (break-glass, or outside Quai).
func (s *Service) leftNote(r db.Request, cr db.ChangeRequest, files []string) string {
	at := cr.CloseRequestedAt.Time.UTC().Format(time.DateTime)
	how := "was merged outside Quai"
	if broken(r) {
		how = "was merged by break-glass"
	}
	what := "applied"
	if cr.CloseOutcome == servicenow.CloseUnsuccessful {
		what = "applied, and its apply failed,"
	}
	return fmt.Sprintf("Quai: request #%d %s without this change request's approval, and was %s at %s UTC (change set %s). "+
		"Quai leaves this change request to its change manager.%s", r.Number, how, what, at, short(cr.ChangeSetHash), s.evidenceSaid(r, files))
}

// evidenceSaid says which files of the evidence are attached, and the request's link.
func (s *Service) evidenceSaid(r db.Request, files []string) string {
	said := " The evidence is in Quai."
	if len(files) > 0 {
		said = " The evidence is attached: " + strings.Join(files, ", ") + " (verify the JSON bundle with quai audit verify --bundle)."
	}
	if l := s.link(r); l != "" {
		said += "\nRequest: " + l
	}
	return said
}

// attachEvidence attaches the request's evidence to its change request, a file at a time: one
// recorded attached is skipped; one tried before, its answer lost, is looked up by its name and
// recorded when found; any other is made (once a try), recorded with its size and hash, and
// uploaded. It returns what someone must see (a file over servicenow.MaxAttachment, a refusal),
// after which nothing more is attached; an unreachable ServiceNow is failed().
func (s *Service) attachEvidence(ctx context.Context, r db.Request, cr db.ChangeRequest) (string, error) {
	rows, err := s.store.ListChangeRequestAttachments(ctx, cr.ID)
	if err != nil {
		return "", err
	}
	tried := map[string]db.ChangeRequestAttachment{}
	for _, a := range rows {
		tried[a.Name] = a
	}
	sysID := cr.SysID.String
	var made map[string][]byte
	for _, f := range evidenceFiles(r, cr) {
		row, before := tried[f.name]
		if before && row.SysID.Valid {
			continue
		}
		var found *servicenow.Attachment
		if before {
			found, err = s.sn.FindAttachment(ctx, sysID, f.name)
			switch {
			case servicenow.Unreachable(err):
				return "", s.failed(ctx, cr, "close", err)
			case err != nil:
				return oneLine("ServiceNow refused Quai's lookup of "+f.name+" on "+cr.Number+": "+err.Error(), 300), nil
			case found != nil && found.Size >= 0 && found.Size != int64(row.Bytes):
				return fmt.Sprintf("%s on %s is not the file Quai attached (%d bytes, not %d): Quai attached no more", f.name, cr.Number, found.Size, row.Bytes), nil
			}
		}
		if found == nil {
			if made == nil {
				if made, err = s.makeEvidence(ctx, r, cr); err != nil {
					return "", err
				}
			}
			data := made[f.contentType]
			if len(data) > servicenow.MaxAttachment {
				return fmt.Sprintf("the evidence's %s is over %d MiB: Quai attached no more, download it from the request's page", f.name, servicenow.MaxAttachment>>20), nil
			}
			sum := sha256.Sum256(data)
			if err := s.store.AddChangeRequestAttachment(ctx, db.AddChangeRequestAttachmentParams{ChangeRequestID: cr.ID, Name: f.name,
				Bytes: int32(len(data)), Sha256: hex.EncodeToString(sum[:])}); err != nil { // #nosec G115 -- at most MaxAttachment
				return "", err
			}
			a, err := s.sn.Attach(ctx, sysID, f.name, f.contentType, data)
			switch {
			case servicenow.Unreachable(err):
				return "", s.failed(ctx, cr, "close", err)
			case err != nil:
				return oneLine("ServiceNow refused "+f.name+" on "+cr.Number+": "+err.Error(), 300), nil
			}
			found = &a
		}
		if err := s.store.SetChangeRequestAttached(ctx, db.SetChangeRequestAttachedParams{ChangeRequestID: cr.ID, Name: f.name,
			SysID: text(found.SysID), At: instant(s.now())}); err != nil {
			return "", err
		}
	}
	return "", nil
}

// makeEvidence makes the request's evidence, by content type. What fails is logged, and recorded
// on the change request as Quai's own failure, tried again with the backoff.
func (s *Service) makeEvidence(ctx context.Context, r db.Request, cr db.ChangeRequest) (map[string][]byte, error) {
	if s.evidence == nil {
		return nil, s.failed(ctx, cr, "close", errors.New("this process of Quai makes no evidence to attach"))
	}
	bundle, pdf, err := s.evidence(ctx, r)
	if err != nil {
		s.log.Error("the evidence of a change request to close", "request", r.ID, "error", err)
		return nil, s.failed(ctx, cr, "close", errors.New("the request's evidence could not be made"))
	}
	return map[string][]byte{"application/json": bundle, "application/pdf": pdf}, nil
}

// closed records that Quai is done with the change request it was to close: how (result), what
// ServiceNow reported last, and what someone must do (problem); the request's timeline and the
// audit log say so, with the files attached.
func (s *Service) closed(ctx context.Context, r db.Request, cr db.ChangeRequest, rec servicenow.Record, result, problem string) error {
	attached, err := s.store.ListChangeRequestAttachments(ctx, cr.ID)
	if err != nil {
		return err
	}
	names, files := []string{}, []map[string]any{}
	for _, a := range attached {
		if a.SysID.Valid {
			names = append(names, a.Name)
			files = append(files, map[string]any{"name": a.Name, "bytes": a.Bytes, "sha256": a.Sha256, "sysId": a.SysID.String})
		}
	}
	return s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := q.LockChangeRequest(ctx, cr.ID); err != nil {
			return err
		}
		if err := q.SetChangeRequestClosed(ctx, db.SetChangeRequestClosedParams{ID: cr.ID, At: instant(s.now()), CloseResult: result, Problem: problem,
			State: rec.State, StateLabel: rec.Label(), Approval: rec.Approval}); err != nil {
			return err
		}
		data := map[string]any{"changeRequest": cr.Number, "outcome": cr.CloseOutcome, "result": result, "files": names, "url": s.sn.Link(cr.SysID.String)}
		if problem != "" {
			data["problem"] = problem
		}
		if err := s.event(ctx, tx, r, "change_request_closed", "", data); err != nil {
			return err
		}
		details := changeRequestDetails(cr)
		details["outcome"], details["result"], details["state"], details["files"] = cr.CloseOutcome, result, rec.State, files
		if problem != "" {
			details["problem"] = problem
		}
		return recordRequest(ctx, tx, r, "change_request_closed", details)
	})
}
