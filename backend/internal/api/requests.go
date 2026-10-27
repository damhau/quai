package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// DiffLine is a line of a diff: ' ' kept, '-' removed, '+' added.
type DiffLine struct {
	Op   string `json:"op" enum:" ,-,+"`
	Text string `json:"text"`
}

// ChangeView is a file a request changes.
type ChangeView struct {
	Path string     `json:"path"`
	Op   string     `json:"op" enum:"create,update,delete"`
	Diff []DiffLine `json:"diff"`
}

// RuleView is an approval requirement, with its progress on a request.
type RuleView struct {
	Names     []string `json:"names"`
	Approvers []string `json:"approvers"`
	Min       int      `json:"min"`
	External  string   `json:"external,omitempty"`
	Approvals []string `json:"approvals"`
	Satisfied bool     `json:"satisfied"`
}

// PlanView is what a request would change.
type PlanView struct {
	Kind       string       `json:"kind"`
	Action     string       `json:"action,omitempty"`
	Target     string       `json:"target"`
	Repository string       `json:"repository"`
	Type       string       `json:"type,omitempty"`
	Blueprint  string       `json:"blueprint,omitempty"`
	Stack      string       `json:"stack,omitempty"`
	Commit     string       `json:"commit"`
	Hash       string       `json:"hash"`
	Changes    []ChangeView `json:"changes"`
	Rules      []RuleView   `json:"rules"`
}

// ReviewView is a review of a request.
type ReviewView struct {
	Reviewer string    `json:"reviewer"`
	Decision string    `json:"decision"`
	Comment  string    `json:"comment,omitempty"`
	At       time.Time `json:"at"`
}

// EventView is something that happened to a request.
type EventView struct {
	Kind  string          `json:"kind"`
	Actor string          `json:"actor,omitempty"`
	Data  json.RawMessage `json:"data"`
	At    time.Time       `json:"at"`
}

// RequestView is a request.
type RequestView struct {
	ID         string         `json:"id"`
	Number     int64          `json:"number"`
	Kind       string         `json:"kind"`
	Action     string         `json:"action,omitempty"`
	Target     string         `json:"target"`
	Repository string         `json:"repository"`
	Type       string         `json:"type,omitempty"`
	Blueprint  string         `json:"blueprint,omitempty"`
	Requester  string         `json:"requester"`
	Reason     string         `json:"reason"`
	State      string         `json:"state"`
	Stack      string         `json:"stack,omitempty"`
	Inputs     map[string]any `json:"inputs"`
	CreatedAt  time.Time      `json:"createdAt"`
	UpdatedAt  time.Time      `json:"updatedAt"`
	CanReview  bool           `json:"canReview"`
	CanCancel  bool           `json:"canCancel"`
	CanRetry   bool           `json:"canRetry"`
	Rules      []RuleView     `json:"rules"`
	Reviews    []ReviewView   `json:"reviews,omitempty"`
	Changes    []ChangeView   `json:"changes,omitempty"`
	Events     []EventView    `json:"events,omitempty"`
}

type typeRequestInput struct {
	Body struct {
		requests.TypeInput
		Reason string `json:"reason,omitempty" maxLength:"2000"`
	}
}

type blueprintRequestInput struct {
	Body struct {
		requests.BlueprintInput
		Reason string `json:"reason,omitempty" maxLength:"2000"`
	}
}

type listRequestsInput struct {
	View string `query:"view" enum:"mine,inbox,all" default:"mine"`
}

type requestIDInput struct {
	ID string `path:"id" format:"uuid"`
}

type reviewInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Decision string `json:"decision" enum:"approve,deny"`
		Comment  string `json:"comment,omitempty" maxLength:"2000"`
	}
}

func registerRequests(a huma.API, svc *requests.Service, s *store.Store, log *slog.Logger) {
	planned := func(ctx context.Context, p *requests.Plan, err error) (*requests.Plan, error) {
		if p != nil && p.Authorize != "" {
			if _, err := authorize(ctx, p.Authorize, p.Object); err != nil {
				return nil, err
			}
		}
		if err != nil {
			return nil, Problem(err, "body")
		}
		return p, nil
	}
	who := func(ctx context.Context) auth.Identity { id, _ := auth.FromContext(ctx); return id }

	huma.Register(a, huma.Operation{
		OperationID: "plan-type", Method: http.MethodPost, Path: Prefix + "/plans/type",
		Summary: "Plan a change to a resource", Tags: []string{"Requests"},
	}, func(ctx context.Context, in *struct{ Body requests.TypeInput }) (*out[PlanView], error) {
		if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: in.Body.Repository, CatalogItem: in.Body.Type}); err != nil {
			return nil, err
		}
		p, err := svc.PlanType(ctx, who(ctx), in.Body)
		p, err = planned(ctx, p, err)
		if err != nil {
			return nil, err
		}
		return &out[PlanView]{Body: planView(p)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "plan-blueprint", Method: http.MethodPost, Path: Prefix + "/plans/blueprint",
		Summary: "Plan a new blueprint instance", Tags: []string{"Requests"},
	}, func(ctx context.Context, in *struct{ Body requests.BlueprintInput }) (*out[PlanView], error) {
		if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: in.Body.Repository, CatalogItem: in.Body.Blueprint}); err != nil {
			return nil, err
		}
		p, err := svc.PlanBlueprint(ctx, who(ctx), in.Body)
		p, err = planned(ctx, p, err)
		if err != nil {
			return nil, err
		}
		return &out[PlanView]{Body: planView(p)}, nil
	})

	submit := func(ctx context.Context, p *requests.Plan, reason string) (*out[RequestView], error) {
		r, err := svc.Submit(ctx, who(ctx), p, reason)
		if err != nil {
			return nil, Problem(err, "body")
		}
		return &out[RequestView]{Body: requestView(ctx, svc, s, r, who(ctx), true)}, nil
	}
	huma.Register(a, huma.Operation{
		OperationID: "request-type", Method: http.MethodPost, Path: Prefix + "/requests/type",
		Summary: "Request a change to a resource", Tags: []string{"Requests"}, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *typeRequestInput) (*out[RequestView], error) {
		if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: in.Body.Repository, CatalogItem: in.Body.Type}); err != nil {
			return nil, err
		}
		p, err := svc.PlanType(ctx, who(ctx), in.Body.TypeInput)
		p, err = planned(ctx, p, err)
		if err != nil {
			return nil, err
		}
		return submit(ctx, p, in.Body.Reason)
	})
	huma.Register(a, huma.Operation{
		OperationID: "request-blueprint", Method: http.MethodPost, Path: Prefix + "/requests/blueprint",
		Summary: "Request a new blueprint instance", Tags: []string{"Requests"}, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *blueprintRequestInput) (*out[RequestView], error) {
		if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: in.Body.Repository, CatalogItem: in.Body.Blueprint}); err != nil {
			return nil, err
		}
		p, err := svc.PlanBlueprint(ctx, who(ctx), in.Body.BlueprintInput)
		p, err = planned(ctx, p, err)
		if err != nil {
			return nil, err
		}
		return submit(ctx, p, in.Body.Reason)
	})

	huma.Register(a, huma.Operation{
		OperationID: "list-requests", Method: http.MethodGet, Path: Prefix + "/requests",
		Summary: "List requests: yours, those waiting for your review, or all you can see", Tags: []string{"Requests"},
	}, func(ctx context.Context, in *listRequestsInput) (*out[[]RequestView], error) {
		me, err := authorize(ctx, authz.ReadRequest, authz.Object{Requester: who(ctx).Principal.Name})
		if err != nil {
			return nil, err
		}
		params := db.ListRequestsParams{States: []string{}, MaxRows: 200}
		switch in.View {
		case "mine":
			params.Requester = me.Principal.Name
		case "inbox":
			params.States = []string{requests.PendingApproval}
		}
		rs, err := s.ListRequests(ctx, params)
		if err != nil {
			return nil, err
		}
		views := []RequestView{}
		for _, r := range rs {
			if in.View == "inbox" && !requests.CanReview(r, me.Principal) {
				continue
			}
			if _, err := authorize(ctx, authz.ReadRequest, requestObject(ctx, svc, r)); err != nil && !requests.CanReview(r, me.Principal) {
				continue
			}
			views = append(views, requestView(ctx, svc, s, r, me, false))
		}
		return &out[[]RequestView]{Body: views}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "get-request", Method: http.MethodGet, Path: Prefix + "/requests/{id}",
		Summary: "Describe a request", Tags: []string{"Requests"},
	}, func(ctx context.Context, in *requestIDInput) (*out[RequestView], error) {
		r, me, err := loadRequest(ctx, svc, in.ID, authz.ReadRequest)
		if err != nil {
			return nil, err
		}
		return &out[RequestView]{Body: requestView(ctx, svc, s, r, me, true)}, nil
	})
	huma.Register(a, huma.Operation{
		OperationID: "review-request", Method: http.MethodPost, Path: Prefix + "/requests/{id}/review",
		Summary: "Approve or deny a request", Tags: []string{"Requests"},
	}, func(ctx context.Context, in *reviewInput) (*out[RequestView], error) {
		r, me, err := loadRequest(ctx, svc, in.ID, authz.ReviewRequest)
		if err != nil {
			return nil, err
		}
		next, err := svc.Review(ctx, me, r.ID, in.Body.Decision, in.Body.Comment)
		if err != nil {
			return nil, Problem(err, "body")
		}
		return &out[RequestView]{Body: requestView(ctx, svc, s, next, me, true)}, nil
	})
	for _, op := range []struct {
		id, path, summary string
		action            authz.Action
		run               func(context.Context, auth.Identity, uuid.UUID) (db.Request, error)
	}{
		{"cancel-request", "/cancel", "Cancel a request", authz.CancelRequest, svc.Cancel},
		{"retry-request", "/retry", "Deliver a failed request again", authz.RetryRequest, svc.Retry},
	} {
		huma.Register(a, huma.Operation{
			OperationID: op.id, Method: http.MethodPost, Path: Prefix + "/requests/{id}" + op.path,
			Summary: op.summary, Tags: []string{"Requests"},
		}, func(ctx context.Context, in *requestIDInput) (*out[RequestView], error) {
			r, me, err := loadRequest(ctx, svc, in.ID, op.action)
			if err != nil {
				return nil, err
			}
			next, err := op.run(ctx, me, r.ID)
			if err != nil {
				return nil, Problem(err, "path")
			}
			log.InfoContext(ctx, op.id, slog.String("request", r.ID.String()), slog.String("by", me.Principal.Name))
			return &out[RequestView]{Body: requestView(ctx, svc, s, next, me, true)}, nil
		})
	}
}

// loadRequest returns a request if the caller may do action on it. Approvers may read what they
// review even without a role that shows them the request.
func loadRequest(ctx context.Context, svc *requests.Service, id string, action authz.Action) (db.Request, auth.Identity, error) {
	rid, err := uuid.Parse(id)
	if err != nil {
		return db.Request{}, auth.Identity{}, huma.Error404NotFound("no such request")
	}
	r, err := svc.Get(ctx, rid)
	if err != nil {
		return r, auth.Identity{}, Problem(err, "path")
	}
	me, err := authorize(ctx, action, requestObject(ctx, svc, r))
	if caller, _ := auth.FromContext(ctx); err != nil && action == authz.ReadRequest && requests.CanReview(r, caller.Principal) {
		return r, caller, nil
	}
	return r, me, err
}

func requestObject(ctx context.Context, svc *requests.Service, r db.Request) authz.Object {
	return authz.Object{Repository: svc.Repository(ctx, r), Requester: r.Requester}
}

func diffView(c plan.Change) []DiffLine {
	lines := []DiffLine{}
	for _, l := range c.Diff() {
		lines = append(lines, DiffLine{Op: string(l.Op), Text: l.Text})
	}
	return lines
}

func ruleViews(rules []policy.Rule, statuses []policy.Status) []RuleView {
	out := []RuleView{}
	for i, r := range rules {
		v := RuleView{Names: r.Names, Approvers: r.Approvers, Min: r.Min, External: r.External, Approvals: []string{}}
		if i < len(statuses) {
			v.Approvals, v.Satisfied = statuses[i].Approvals, statuses[i].Satisfied
			if v.Approvals == nil {
				v.Approvals = []string{}
			}
		}
		out = append(out, v)
	}
	return out
}

func planView(p *requests.Plan) PlanView {
	v := PlanView{Kind: p.Kind, Action: p.Action, Target: p.Target, Repository: p.Repository, Type: p.Type, Blueprint: p.Blueprint,
		Stack: p.Stack, Commit: p.Commit, Hash: p.ChangeSet.Hash, Changes: []ChangeView{}, Rules: ruleViews(p.Rules, nil)}
	for _, c := range p.ChangeSet.Changes {
		v.Changes = append(v.Changes, ChangeView{Path: c.Path, Op: c.Op, Diff: diffView(c)})
	}
	return v
}

func requestView(ctx context.Context, svc *requests.Service, s *store.Store, r db.Request, me auth.Identity, detail bool) RequestView {
	v := RequestView{ID: r.ID.String(), Number: r.Number, Kind: r.Kind, Action: r.Action, Target: r.Target, Repository: svc.Repository(ctx, r),
		Type: r.TypeName, Blueprint: r.Blueprint, Requester: r.Requester, Reason: r.Reason, State: r.State, Stack: r.Stack,
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time, CanReview: requests.CanReview(r, me.Principal)}
	v.CanCancel = requests.Cancellable(r.State) && allowed(ctx, authz.CancelRequest, requestObject(ctx, svc, r))
	v.CanRetry = r.State == requests.Failed && allowed(ctx, authz.RetryRequest, requestObject(ctx, svc, r))
	_ = json.Unmarshal(r.Inputs, &v.Inputs)
	var rules []policy.Rule
	_ = json.Unmarshal(r.Rules, &rules)
	reviews, outcome, err := svc.Reviews(ctx, r)
	if err != nil {
		reviews = nil
	}
	v.Rules = ruleViews(rules, outcome.Rules)
	if !detail {
		return v
	}
	for _, rv := range reviews {
		v.Reviews = append(v.Reviews, ReviewView{Reviewer: rv.Reviewer, Decision: rv.Decision, Comment: rv.Comment, At: rv.CreatedAt.Time})
	}
	if sets, err := s.ListChangeSets(ctx, r.ID); err == nil {
		for _, cs := range sets {
			changes, err := s.ListChanges(ctx, cs.ID)
			if err != nil {
				continue
			}
			for _, c := range changes {
				pc := plan.Change{Path: c.Path, Op: c.Op, Before: c.Before, After: c.After}
				v.Changes = append(v.Changes, ChangeView{Path: c.Path, Op: c.Op, Diff: diffView(pc)})
			}
		}
	}
	if evs, err := s.ListRequestEvents(ctx, pgtype.UUID{Bytes: r.ID, Valid: true}); err == nil {
		for _, e := range evs {
			v.Events = append(v.Events, EventView{Kind: e.Kind, Actor: e.Actor, Data: e.Data, At: e.CreatedAt.Time})
		}
	}
	return v
}
