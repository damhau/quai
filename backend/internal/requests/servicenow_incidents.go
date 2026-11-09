package requests

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/engine/schema"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/servicenow"
	"github.com/damhau/quai/backend/internal/store/db"
	"github.com/damhau/quai/backend/internal/telemetry"
)

// Break-glass incidents in ServiceNow (docs/design/05-delivery-and-approvals.md#break-glass, open
// questions 119 to 122). With itsm.servicenow configured, a break-glass's incident must be a
// ServiceNow incident's number, and is checked against ServiceNow's incidents before the
// break-glass's transaction, never inside it: one ServiceNow answers it has not, two of, or closed
// is refused (a field error on incident). When ServiceNow cannot say (unreachable, refused, an
// answer neither yes nor no), break-glass goes ahead, the incident unverified, because an emergency
// must not wait on ServiceNow: a job asks again, and a later "no" is a problem on the break-glass
// review and an audit event, nothing undone. A verified incident is then linked, from a job, to the
// request's change request for the content broken for (the incident's rfc field, open question
// 119), unless it names another already.

const (
	// incidentWait bounds the check of an incident while the breaker waits.
	incidentWait = 10 * time.Second
	// incidentNotOpened is how long linking waits for the change request to be opened.
	incidentNotOpened = time.Minute
)

// What ServiceNow said of a break-glass's incident.
const (
	incidentVerified   = "verified"
	incidentUnverified = "unverified"
	incidentRefuted    = "refuted"
)

// How a break-glass's incident is linked to its change request.
const (
	incidentLinked  = "linked"  // its rfc names the change request
	incidentKept    = "kept"    // its rfc names another change request, left as it is
	incidentNone    = "none"    // the request has no change request for the content broken for
	incidentRefused = "refused" // ServiceNow refused: the problem says what
)

// incidentCheck is what ServiceNow said of a break-glass's incident while the breaker waited: "",
// without ServiceNow; verified, with its sys_id; unverified, failure saying why.
type incidentCheck struct {
	result, sysID, failure string
}

// checkIncident checks a break-glass's incident against ServiceNow's incidents, within
// incidentWait: a field error when ServiceNow answers it has none, two, or that it is closed or
// canceled; unverified when ServiceNow cannot say. The number is servicenow.ValidNumber already.
func (s *Service) checkIncident(ctx context.Context, number string) (incidentCheck, error) {
	if s.sn == nil {
		return incidentCheck{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, incidentWait)
	defer cancel()
	inc, err := s.sn.FindIncident(ctx, number)
	if refuted := incidentRefusal(number, inc, err); refuted != "" {
		telemetry.ServiceNowCalled(ctx, "incident", "ok")
		return incidentCheck{}, &schema.ValidationError{Fields: map[string]string{"incident": refuted}}
	}
	if err == nil && inc.Active == "true" {
		telemetry.ServiceNowCalled(ctx, "incident", "ok")
		return incidentCheck{result: incidentVerified, sysID: inc.SysID}, nil
	}
	if err == nil {
		err = servicenow.ErrUnclear
	}
	result := "refused"
	if servicenow.Unreachable(err) || errors.Is(err, context.DeadlineExceeded) {
		result = "unreachable"
	}
	telemetry.ServiceNowCalled(ctx, "incident", result)
	s.log.Warn("ServiceNow could not say whether the break-glass's incident exists: glass is broken, the incident not verified",
		"incident", number, "error", err)
	return incidentCheck{result: incidentUnverified, failure: oneLine(err.Error(), 300)}, nil
}

// incidentRefusal says why ServiceNow's answer refutes an incident: none, two, or one closed or
// canceled (open question 120); "" when it does not.
func incidentRefusal(number string, inc servicenow.Incident, err error) string {
	switch {
	case errors.Is(err, servicenow.ErrNoIncident):
		return "ServiceNow has no incident " + number
	case errors.Is(err, servicenow.ErrIncidentAmbiguous):
		return "ServiceNow holds more than one incident " + number + ": name the incident another way, or ask ServiceNow's administrators"
	case err == nil && (inc.Active == "false" || inc.State == "7" || inc.State == "8"):
		return "incident " + number + " is closed or canceled in ServiceNow: break glass for an open incident"
	}
	return ""
}

// BreakGlassIncidentArgs checks a break-glass's incident against ServiceNow again, when it could
// not be, and links it to the request's change request.
type BreakGlassIncidentArgs struct {
	RequestID uuid.UUID `json:"request"`
}

// Kind names the job.
func (BreakGlassIncidentArgs) Kind() string { return "servicenow_incident" }

// InsertOpts runs it once a break-glass; a failure is recorded on the break-glass, and the sweep
// tries it again when due.
func (BreakGlassIncidentArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 10, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

type incidentWorker struct {
	river.WorkerDefaults[BreakGlassIncidentArgs]
	s *Service
}

func (w *incidentWorker) Work(ctx context.Context, job *river.Job[BreakGlassIncidentArgs]) error {
	_, err := w.s.incidentLocked(ctx, job.Args.RequestID)
	return err
}

// incidentLocked takes the next steps of a break-glass's incident under its advisory lock: one at a
// time, whichever of the sweep or the job; nothing when another holds it. unreachable says
// ServiceNow did not answer.
func (s *Service) incidentLocked(ctx context.Context, id uuid.UUID) (unreachable bool, err error) {
	_, err = jobs.TryLock(ctx, s.store.Locks, "servicenow-incident:"+id.String(), func(ctx context.Context) error {
		var e error
		unreachable, e = s.incident(ctx, id)
		return e
	})
	return unreachable, err
}

// incident checks a break-glass's incident again while it is unverified, then links a verified one
// to the change request. Each step is recorded once done; one that did not reach ServiceNow is
// recorded with when to try it again (incidentFailed).
func (s *Service) incident(ctx context.Context, id uuid.UUID) (bool, error) {
	if s.sn == nil {
		return false, nil
	}
	for range 2 {
		bg, err := s.store.GetBreakGlass(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		switch {
		case bg.IncidentProblem != "":
			return false, nil
		case bg.IncidentCheck == incidentUnverified:
			if unreachable, err := s.recheckIncident(ctx, bg); err != nil || unreachable {
				return unreachable, err
			}
		case bg.IncidentCheck == incidentVerified && bg.IncidentLink == "":
			return s.linkIncident(ctx, bg)
		default:
			return false, nil
		}
	}
	return false, nil
}

// recheckIncident asks ServiceNow again for an incident it could not check while glass was broken:
// verified, or refuted (a problem on the break-glass review); a refusal to say is a problem too,
// asked no more. Each is an audit event.
func (s *Service) recheckIncident(ctx context.Context, bg db.BreakGlass) (bool, error) {
	inc, err := s.sn.FindIncident(ctx, bg.Incident)
	if servicenow.Unreachable(err) {
		return true, s.incidentFailed(ctx, bg, "incident", err)
	}
	check, problem := incidentVerified, ""
	if refuted := incidentRefusal(bg.Incident, inc, err); refuted != "" {
		check, problem = incidentRefuted, refuted+": glass was broken for it, unverified"
	} else if err != nil || inc.Active != "true" {
		if err == nil {
			err = servicenow.ErrUnclear
		}
		check, problem = incidentUnverified, oneLine("ServiceNow does not say whether incident "+bg.Incident+" exists: "+err.Error(), 300)
	}
	telemetry.ServiceNowCalled(ctx, "incident", "ok")
	if problem != "" {
		s.log.Warn("the break-glass's incident is not verified in ServiceNow", "request", bg.RequestID, "incident", bg.Incident, "problem", problem)
	}
	r, err := s.get(ctx, bg.RequestID)
	if err != nil {
		return false, err
	}
	return false, s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := q.SetBreakGlassIncidentChecked(ctx, db.SetBreakGlassIncidentCheckedParams{RequestID: bg.RequestID, IncidentCheck: check,
			IncidentSysID: inc.SysID, At: instant(s.now()), IncidentProblem: problem}); err != nil {
			return err
		}
		data := map[string]any{"incident": bg.Incident, "check": check}
		details := map[string]any{"incident": bg.Incident, "check": check}
		if check == incidentVerified {
			details["sysId"] = inc.SysID
			data["url"] = s.sn.IncidentLink(inc.SysID)
		}
		if problem != "" {
			data["problem"], details["problem"] = problem, problem
		}
		if err := s.event(ctx, tx, r, "break_glass_incident", "", data); err != nil {
			return err
		}
		return audit.Add(ctx, tx, audit.Event{Actor: serviceNowActor, Action: "request.break_glass_incident",
			Object: audit.Object{Type: "request", ID: r.ID.String(), Resource: r.Target}, Details: details})
	})
}

// linkIncident links a verified incident to the request's change request for the content glass was
// broken for (its rfc field), once that change request is opened: an incident whose rfc names it
// already is linked (an answer lost), one that names another is kept as it is.
func (s *Service) linkIncident(ctx context.Context, bg db.BreakGlass) (bool, error) {
	cr, err := s.store.CurrentChangeRequest(ctx, bg.RequestID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && cr.ChangeSetHash != bg.ChangeSetHash {
		return false, s.store.SetBreakGlassIncidentLinked(ctx, db.SetBreakGlassIncidentLinkedParams{RequestID: bg.RequestID,
			IncidentLink: incidentNone, At: instant(s.now())})
	}
	if err != nil {
		return false, err
	}
	if !cr.SysID.Valid {
		var f *snFailure
		if err := s.syncLocked(ctx, cr.ID); err != nil && !errors.As(err, &f) {
			return false, err
		}
		if cr, err = s.store.GetChangeRequest(ctx, cr.ID); err != nil {
			return false, err
		}
		if !cr.SysID.Valid {
			return false, s.incidentWait(ctx, bg, "its change request is not opened in ServiceNow yet", incidentNotOpened)
		}
	}
	inc, err := s.sn.GetIncident(ctx, bg.IncidentSysID)
	if err == nil && !inc.HasChange {
		inc, err = s.sn.LinkIncident(ctx, inc.SysID, cr.SysID.String)
	}
	link, problem := incidentLinked, ""
	switch {
	case servicenow.Unreachable(err):
		return true, s.incidentFailed(ctx, bg, "link", err)
	case err != nil:
		link, problem = incidentRefused, oneLine("ServiceNow did not link incident "+bg.Incident+" to "+cr.Number+": "+err.Error(), 300)
		s.log.Warn(problem, "request", bg.RequestID)
	case inc.Change != cr.SysID.String:
		link = incidentKept
	}
	telemetry.ServiceNowCalled(ctx, "link", "ok")
	r, err := s.get(ctx, bg.RequestID)
	if err != nil {
		return false, err
	}
	return false, s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := q.SetBreakGlassIncidentLinked(ctx, db.SetBreakGlassIncidentLinkedParams{RequestID: bg.RequestID, IncidentLink: link,
			At: instant(s.now()), IncidentProblem: problem}); err != nil {
			return err
		}
		data := map[string]any{"incident": bg.Incident, "changeRequest": cr.Number, "link": link}
		details := map[string]any{"incident": bg.Incident, "sysId": bg.IncidentSysID, "changeRequest": cr.Number, "changeSysId": cr.SysID.String, "link": link}
		if problem != "" {
			data["problem"], details["problem"] = problem, problem
		}
		if err := s.event(ctx, tx, r, "incident_linked", "", data); err != nil {
			return err
		}
		return recordRequest(ctx, tx, r, "incident_linked", details)
	})
}

// incidentFailed records a step of a break-glass's incident that did not reach ServiceNow, and when
// to try it again: a backoff doubling from a minute, within snMaxWait.
func (s *Service) incidentFailed(ctx context.Context, bg db.BreakGlass, op string, err error) error {
	telemetry.ServiceNowCalled(ctx, op, "unreachable")
	wait := min(time.Minute<<min(bg.IncidentFailures, 12), snMaxWait)
	s.log.Warn("ServiceNow failed", "operation", op, "request", bg.RequestID, "incident", bg.Incident, "failures", bg.IncidentFailures+1,
		"retryAt", s.now().Add(wait), "error", err)
	return s.incidentWait(ctx, bg, err.Error(), wait)
}

// incidentWait records why a step of a break-glass's incident waits, and until when.
func (s *Service) incidentWait(ctx context.Context, bg db.BreakGlass, why string, wait time.Duration) error {
	return s.store.SetBreakGlassIncidentFailed(ctx, db.SetBreakGlassIncidentFailedParams{RequestID: bg.RequestID,
		IncidentFailure: oneLine(why, 300), NextAt: instant(s.now().Add(wait))})
}

// sweepIncidents takes the next steps of the break-glass incidents due, at most snBatch, one at a
// time, until ServiceNow does not answer.
func (s *Service) sweepIncidents(ctx context.Context) error {
	due, err := s.store.DueBreakGlassIncidents(ctx, db.DueBreakGlassIncidentsParams{Now: instant(s.now()), MaxRows: snBatch})
	if err != nil {
		return err
	}
	var errs []error
	for _, bg := range due {
		if ctx.Err() != nil {
			break
		}
		unreachable, err := s.incidentLocked(ctx, bg.RequestID)
		if err != nil {
			errs = append(errs, err)
		}
		if unreachable {
			break
		}
	}
	return errors.Join(errs...)
}

// IncidentURL is a break-glass's incident's form in ServiceNow, from the configured instance and the
// sys_id Quai checked; "" when it was not verified.
func (s *Service) IncidentURL(bg db.BreakGlass) string {
	if s.sn == nil || bg.IncidentSysID == "" {
		return ""
	}
	return s.sn.IncidentLink(bg.IncidentSysID)
}
