package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Break-glass (docs/design/05-delivery-and-approvals.md#break-glass): a member of the break-glass
// role merges a request without its approvals, and two people sign off its post-incident review.

// BreakGlassView is the break-glass of a request and its post-incident review.
type BreakGlassView struct {
	By       string `json:"by" doc:"Who broke glass"`
	Reason   string `json:"reason"`
	Incident string `json:"incident" doc:"The incident's reference"`
	// The incident in ServiceNow, when itsm.servicenow is configured.
	IncidentCheck   string    `json:"incidentCheck,omitempty" enum:"verified,unverified,refuted" doc:"What ServiceNow says of the incident: verified; unverified, ServiceNow could not say yet (Quai asks again); refuted, the problem says why"`
	IncidentURL     string    `json:"incidentUrl,omitempty" doc:"The incident's form in ServiceNow, once verified"`
	IncidentLink    string    `json:"incidentLink,omitempty" enum:"linked,kept,none,refused" doc:"The incident's link to the change request: linked; kept, it names another change request; none, the request has no change request; refused by ServiceNow"`
	IncidentProblem string    `json:"incidentProblem,omitempty" doc:"What someone must look at: an incident ServiceNow refutes, or a link it refused"`
	At              time.Time `json:"at"`
	From            string    `json:"from" doc:"The request's state then: pending_approval, or approved and held by its change windows"`
	// ChangeSet and Head are the content glass was broken for.
	ChangeSet string                  `json:"changeSet" doc:"The change set's hash glass was broken for"`
	Head      string                  `json:"head,omitempty" doc:"The pull request's head commit glass was broken for"`
	StepUp    bool                    `json:"stepUp" doc:"The breaker's sign-in satisfied the step-up"`
	Open      bool                    `json:"open" doc:"Its post-incident review waits for sign-offs"`
	ClosedAt  *time.Time              `json:"closedAt,omitempty" doc:"When the review's last sign-off closed it"`
	Needed    int                     `json:"needed" doc:"How many people sign off the review to close it"`
	SignOffs  []BreakGlassSignOffView `json:"signOffs"`
	// CanSignOff and CannotSignOff are the caller's.
	CanSignOff    bool   `json:"canSignOff" doc:"You may sign off its review now"`
	CannotSignOff string `json:"cannotSignOff,omitempty" doc:"Why you cannot sign off its review, when your role or ownership would let you"`
}

// BreakGlassSignOffView is a sign-off of a break-glass review.
type BreakGlassSignOffView struct {
	Signer   string     `json:"signer"`
	Capacity string     `json:"capacity" enum:"platform-admin,auditor,owner" doc:"In which capacity they signed it off"`
	Comment  string     `json:"comment"`
	At       time.Time  `json:"at"`
	StepUp   bool       `json:"stepUp"`
	AuthTime *time.Time `json:"authTime,omitempty"`
}

// BreakGlassItemView is a request merged by break-glass, in the list of reviews.
type BreakGlassItemView struct {
	Request    string         `json:"request" format:"uuid"`
	Number     int64          `json:"number"`
	Target     string         `json:"target"`
	Repository string         `json:"repository"`
	Requester  string         `json:"requester"`
	State      string         `json:"state"`
	BreakGlass BreakGlassView `json:"breakGlass"`
}

// BreakGlassBody is a break-glass: why, the incident, and the content the page showed.
type BreakGlassBody struct {
	Reason    string `json:"reason" maxLength:"500" doc:"Why the change cannot wait for its approvals; kept in Quai and the audit log, never sent to the platform"`
	Incident  string `json:"incident" maxLength:"64" doc:"The incident's reference, such as INC0012345; with itsm.servicenow configured, a ServiceNow incident's number, checked against its incidents"`
	ChangeSet string `json:"changeSet" minLength:"1" maxLength:"128" doc:"The change set's hash the page showed (changeSet): refused if the request changed since"`
	Head      string `json:"head,omitempty" maxLength:"128" doc:"With a pull request, its head commit the page showed (delivery.head)"`
}

type breakGlassInput struct {
	ID   string `path:"id" format:"uuid"`
	Body BreakGlassBody
}

// BreakGlassSignOffBody is a sign-off of a break-glass review.
type BreakGlassSignOffBody struct {
	Comment string `json:"comment" maxLength:"2000" doc:"What the review found"`
}

type breakGlassSignOffInput struct {
	ID   string `path:"id" format:"uuid"`
	Body BreakGlassSignOffBody
}

type listBreakGlassInput struct {
	Open bool `query:"open" doc:"Only the open reviews"`
}

func registerBreakGlass(a huma.API, svc *requests.Service, s *store.Store) {
	huma.Register(a, huma.Operation{
		OperationID: "break-glass", Method: http.MethodPost, Path: Prefix + "/requests/{id}/break-glass",
		Summary: "Merge a request without its approvals (break-glass)",
		Description: "For the break-glass role, in a browser session, after a step-up when auth.stepUp is configured; never on one's own request. " +
			"The request must be pending approval, or approved and held by its change windows, and still hold the change set (and head) given. " +
			"It merges through its delivery like any approved request, past its change windows; the platform's own policies still apply. " +
			"The alert recipients and the owning team are alerted, and a post-incident review opens.",
		Tags: []string{"Requests"},
	}, func(ctx context.Context, in *breakGlassInput) (*out[RequestView], error) {
		r, me, err := loadRequest(ctx, svc, in.ID, authz.UseBreakGlass)
		if err != nil {
			return nil, err
		}
		next, err := svc.BreakGlass(ctx, me, r.ID, requests.BreakGlassInput{Reason: in.Body.Reason, Incident: in.Body.Incident,
			ChangeSet: in.Body.ChangeSet, Head: in.Body.Head})
		if err != nil {
			return nil, Problem(err, "body")
		}
		return &out[RequestView]{Body: requestView(ctx, svc, s, next, me, true)}, nil
	})
	huma.Register(a, huma.Operation{
		OperationID: "sign-off-break-glass", Method: http.MethodPost, Path: Prefix + "/requests/{id}/break-glass/sign-off",
		Summary: "Sign off the post-incident review of a break-glass",
		Description: "For platform admins, auditors and the request's owners, in a browser session, after a step-up when auth.stepUp is configured; " +
			"never the person who broke glass, the requester or their delegates. Each person signs off once and a sign-off is never withdrawn; " +
			"the second closes the review.",
		Tags: []string{"Requests"},
	}, func(ctx context.Context, in *breakGlassSignOffInput) (*out[RequestView], error) {
		rid, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, huma.Error404NotFound("no such request")
		}
		r, err := svc.Get(ctx, rid)
		if err != nil {
			return nil, Problem(err, "path")
		}
		me, err := authorize(ctx, authz.SignOffBreakGlass, svc.BreakGlassObject(ctx, r))
		if err != nil {
			return nil, err
		}
		next, err := svc.SignOffBreakGlass(ctx, me, r.ID, in.Body.Comment)
		if err != nil {
			return nil, Problem(err, "body")
		}
		return &out[RequestView]{Body: requestView(ctx, svc, s, next, me, true)}, nil
	})
	huma.Register(a, huma.Operation{
		OperationID: "list-break-glass", Method: http.MethodGet, Path: Prefix + "/break-glass",
		Summary: "List the requests merged by break-glass, the open reviews first (auditors and platform admins)", Tags: []string{"Requests"},
	}, func(ctx context.Context, in *listBreakGlassInput) (*out[[]BreakGlassItemView], error) {
		me, err := authorize(ctx, authz.ReadBreakGlass, authz.Object{})
		if err != nil {
			return nil, err
		}
		list, err := svc.ListBreakGlass(ctx, in.Open, 200)
		if err != nil {
			return nil, err
		}
		items := []BreakGlassItemView{}
		for _, bg := range list {
			r, err := svc.Get(ctx, bg.RequestID)
			if err != nil {
				continue
			}
			signs, err := s.ListBreakGlassSignOffs(ctx, bg.RequestID)
			if err != nil {
				return nil, err
			}
			items = append(items, BreakGlassItemView{Request: r.ID.String(), Number: r.Number, Target: r.Target, Repository: svc.Repository(ctx, r),
				Requester: r.Requester, State: r.State, BreakGlass: breakGlassView(ctx, svc, r, bg, signs, me)})
		}
		return &out[[]BreakGlassItemView]{Body: items}, nil
	})
}

// breakGlassView is a request's break-glass and its review, as the caller sees them.
func breakGlassView(ctx context.Context, svc *requests.Service, r db.Request, bg db.BreakGlass, signs []db.BreakGlassSignOff, me auth.Identity) BreakGlassView {
	v := BreakGlassView{By: bg.Breaker, Reason: bg.Reason, Incident: bg.Incident, At: bg.At.Time, From: bg.FromState, ChangeSet: bg.ChangeSetHash,
		Head: bg.Head, StepUp: bg.StepUp, Open: !bg.ClosedAt.Valid, Needed: requests.SignOffsNeeded, SignOffs: []BreakGlassSignOffView{},
		IncidentCheck: bg.IncidentCheck, IncidentURL: svc.IncidentURL(bg), IncidentLink: bg.IncidentLink, IncidentProblem: bg.IncidentProblem}
	if bg.ClosedAt.Valid {
		v.ClosedAt = &bg.ClosedAt.Time
	}
	for _, x := range signs {
		sv := BreakGlassSignOffView{Signer: x.Signer, Capacity: x.Capacity, Comment: x.Comment, At: x.At.Time, StepUp: x.StepUp}
		if x.AuthTime.Valid {
			sv.AuthTime = &x.AuthTime.Time
		}
		v.SignOffs = append(v.SignOffs, sv)
	}
	if !v.Open || authz.BreakGlassReviewer(me.Principal, svc.BreakGlassObject(ctx, r)) == "" || !allowed(ctx, authz.SignOffBreakGlass, svc.BreakGlassObject(ctx, r)) {
		return v
	}
	if me.TokenID != "" || me.Principal.Scopes != nil {
		v.CannotSignOff = "a break-glass review is signed off in Quai, signed in, never with an API token"
		return v
	}
	why, err := svc.WhyNotSignOff(ctx, bg, r, signs, me.Principal)
	switch {
	case err != nil:
	case why != "":
		v.CannotSignOff = why
	default:
		v.CanSignOff = true
	}
	return v
}

// breakGlassOf fills a request view's break-glass, and whether the caller may break glass for it.
func breakGlassOf(ctx context.Context, svc *requests.Service, r db.Request, me auth.Identity, v *RequestView) {
	v.BreakGlassStepUp = svc.BreakGlassStepUp()
	if bg, signs, err := svc.BreakGlassOf(ctx, r.ID); err == nil {
		bv := breakGlassView(ctx, svc, r, bg, signs, me)
		v.BreakGlass = &bv
	}
	if !requests.Breakable(r) || !allowed(ctx, authz.UseBreakGlass, requestObject(ctx, svc, r)) {
		return
	}
	if v.BreakGlass != nil {
		return // broken before, for content it no longer has: it takes its approvals
	}
	if me.TokenID != "" || me.Principal.Scopes != nil {
		v.CannotBreakGlass = "break-glass is done in Quai, signed in, never with an API token"
		return
	}
	why, err := svc.WhyNotBreak(ctx, r, me.Principal)
	switch {
	case err != nil:
	case why != "":
		v.CannotBreakGlass = why
	default:
		v.CanBreakGlass = true
	}
}
