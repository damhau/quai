// Package requests runs the request lifecycle (docs/architecture/domain-model.md#request): plan
// a change, submit it with the approval rules it matches, record reviews, and deliver approved
// requests with a job that commits to the repository (direct delivery in Phase 0). Plans are
// always computed again on the server; what a client shows is never trusted.
package requests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/edit/yamledit"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/engine/schema"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/inventory"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Request states (docs/architecture/domain-model.md#request).
const (
	PendingApproval = "pending_approval"
	Approved        = "approved"
	Rejected        = "rejected"
	Cancelled       = "cancelled"
	Stale           = "stale"
	Merging         = "merging"
	Applied         = "applied"
	Failed          = "failed"
)

var (
	// ErrNotFound is returned for a request, type or resource that does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned when a request cannot do this in its state, or another request is
	// open for the same target.
	ErrConflict = errors.New("conflict")
	// ErrNotApprover is returned when someone reviews a request no rule lets them review.
	ErrNotApprover = errors.New("you are not an approver of this request")
)

// Service runs requests.
type Service struct {
	store *store.Store
	reg   *repositories.Registry
	jobs  *jobs.Client
	log   *slog.Logger
}

// New returns the request service. jobsClient inserts the delivery jobs.
func New(s *store.Store, reg *repositories.Registry, jobsClient *jobs.Client, log *slog.Logger) *Service {
	return &Service{store: s, reg: reg, jobs: jobsClient, log: log}
}

// UseJobs sets the client that inserts jobs, for a worker whose client is made after the
// service its workers use.
func (s *Service) UseJobs(c *jobs.Client) { s.jobs = c }

// TypeInput is a change to one resource of a type.
type TypeInput struct {
	Repository   string            `json:"repository"`
	Type         string            `json:"type"`
	Kind         string            `json:"kind" enum:"create,update,delete,action"`
	Key          string            `json:"key,omitempty" doc:"The resource's key, for update, delete and action"`
	Params       map[string]string `json:"params,omitempty" doc:"The locator params, for create"`
	Values       map[string]any    `json:"values,omitempty" doc:"The form values, for create and update"`
	Action       string            `json:"action,omitempty"`
	ActionInputs map[string]any    `json:"actionInputs,omitempty"`
}

// BlueprintInput creates an instance of a blueprint.
type BlueprintInput struct {
	Repository string         `json:"repository"`
	Blueprint  string         `json:"blueprint"`
	Name       string         `json:"name"`
	Inputs     map[string]any `json:"inputs,omitempty"`
}

// InstanceActionInput runs a day-2 action of a blueprint on one of its instances.
type InstanceActionInput struct {
	Repository   string         `json:"repository"`
	Blueprint    string         `json:"blueprint"`
	Instance     string         `json:"instance"`
	Action       string         `json:"action"`
	ActionInputs map[string]any `json:"actionInputs,omitempty"`
}

// Plan is what a request would change and who would have to approve it.
type Plan struct {
	Kind, Action, Target, Repository, Type, Blueprint, Stack string
	Commit                                                   string
	ChangeSet                                                *plan.ChangeSet
	Rules                                                    []policy.Rule
	Owners                                                   []string
	Inputs                                                   map[string]any
	// Authorize is the access question the caller must answer yes before showing or submitting.
	Authorize authz.Action
	Object    authz.Object
}

// PlanType plans a change to a resource of a type. Once it knows what the change is on, it
// returns the plan with its access question even when planning fails, so that the caller can
// refuse an unauthorized person before telling them why the change would not work.
func (s *Service) PlanType(ctx context.Context, who auth.Identity, in TypeInput) (*Plan, error) {
	repo, snap, err := s.snapshot(ctx, in.Repository)
	if err != nil {
		return nil, err
	}
	t := snap.Definitions.Types[in.Type]
	if t == nil {
		return nil, fmt.Errorf("%w: type %s in %s", ErrNotFound, in.Type, in.Repository)
	}
	p := &Plan{Kind: in.Kind, Action: in.Action, Repository: repo.Name, Type: t.Metadata.Name, Commit: snap.Commit}
	req := generate.Request{Kind: in.Kind, Owner: who.Principal.Name}
	vars := expr.Vars{User: expr.User{Name: who.Principal.Name, Groups: who.Principal.Groups}, Request: expr.Request{Kind: in.Kind, Action: in.Action}}
	var current map[string]any
	approval := t.Spec.Approval

	if in.Kind == generate.Create {
		p.Authorize, p.Object = authz.CreateRequest, authz.Object{Repository: repo.Name, CatalogItem: t.Metadata.Name}
		params, err := checkParams(snap, t, in.Params)
		if err != nil {
			return p, err
		}
		req.Params, req.Values = params, in.Values
		p.Owners = []string{who.Principal.Name}
	} else {
		r, err := inventory.Get(snap, t.Metadata.Name, in.Key)
		if err != nil {
			return nil, fmt.Errorf("%w: resource %s", ErrNotFound, locate.ID(repo.Name, t.Metadata.Name, in.Key))
		}
		p.Authorize, p.Object = authz.OperateResource, objectOf(r)
		if r.Status != inventory.Conforming {
			return p, fmt.Errorf("%w: %s is %s; fix it in Git before Quai edits it", ErrConflict, r.Path, r.Status)
		}
		current = r.Document
		req.Params, req.Current = r.Params, r.Document
		if r.Owner != "" {
			p.Owners = []string{r.Owner}
		}
		switch in.Kind {
		case generate.Update:
			req.Values = in.Values
		case generate.Action:
			a := findAction(t, in.Action)
			if a == nil {
				return p, fmt.Errorf("%w: type %s has no action %s", ErrNotFound, t.Metadata.Name, in.Action)
			}
			if a.Kind == definitions.ActionImperative {
				return p, fmt.Errorf("%w: imperative actions come with executors", ErrConflict)
			}
			vars.Inputs, vars.Params = r.Document, anyMap(r.Params)
			if a.Available != nil {
				if ok, err := a.Available.Test(vars); err != nil || !ok {
					return p, fmt.Errorf("%w: action %s is not available for this resource", ErrConflict, a.Name)
				}
			}
			inputs, err := generate.ValidateInputs(a.Inputs, in.ActionInputs, vars)
			if err != nil {
				return p, schema.Under("actionInputs", err)
			}
			req.ActionName, req.ActionInputs = a.Name, inputs
			vars.Action = inputs
			if a.Approval != nil {
				approval = *a.Approval
			}
		case generate.Delete:
			switch t.Spec.Deletion {
			case "forbidden":
				return p, fmt.Errorf("%w: type %s forbids deletion through Quai", ErrConflict, t.Metadata.Name)
			}
		default:
			return p, fmt.Errorf("%w: unknown kind %q", ErrConflict, in.Kind)
		}
	}

	cs, err := plan.Type(snap.FS, t, req)
	if err != nil {
		return p, schema.Under("values", err)
	}
	p.ChangeSet = cs
	loc, _ := locate.New(t.Spec.Locate.Strategy, t.Spec.Locate.Path)
	region, _ := loc.Region(req.Params)
	p.Target = locate.ID(repo.Name, t.Metadata.Name, region.Key)
	p.Stack = stack(t, req.Params)

	// The rules see the document as it will be, and as it was.
	next := current
	if c := cs.Changes[0]; c.After != nil {
		if doc, err := yamledit.Decode(c.After); err == nil {
			next, _ = doc.(map[string]any)
		}
	}
	p.Inputs = next
	vars.Inputs, vars.Previous, vars.Params = next, current, anyMap(req.Params)
	sets := []policy.RuleSet{{Source: policy.SourceFloor, Rules: repo.FloorRules, Vars: vars}, {Source: policy.SourceType, Rules: approval.Rules, Vars: vars}}
	if p.Rules, err = policy.Match(sets...); err != nil {
		return p, err
	}
	if in.Kind == generate.Delete && t.Spec.Deletion != "allowed" && len(p.Rules) == 0 {
		// Deleting always needs an approval (docs/design/02, deletion): from the owners, or from
		// whoever the repository's floor rules name, unless the type allows deletion outright.
		approvers := []string{"owners"}
		for _, f := range repo.FloorRules {
			approvers = append(approvers, f.Approvers...)
		}
		slices.Sort(approvers)
		p.Rules = []policy.Rule{{Names: []string{"type/deletion"}, Approvers: slices.Compact(approvers), Min: 1, Distinct: true}}
	}
	return p, nil
}

// PlanBlueprint plans a new instance of a blueprint.
func (s *Service) PlanBlueprint(ctx context.Context, who auth.Identity, in BlueprintInput) (*Plan, error) {
	repo, snap, err := s.snapshot(ctx, in.Repository)
	if err != nil {
		return nil, err
	}
	b := snap.Definitions.Blueprints[in.Blueprint]
	if b == nil {
		return nil, fmt.Errorf("%w: blueprint %s in %s", ErrNotFound, in.Blueprint, in.Repository)
	}
	if !locate.SafeSegment(in.Name) {
		return nil, &schema.ValidationError{Fields: map[string]string{"name": "use 1 to 63 letters, digits, '.', '_' or '-'"}}
	}
	cs, err := plan.Blueprint(snap.FS, snap.Definitions, b, plan.BlueprintRequest{Name: in.Name, Inputs: in.Inputs, Owner: who.Principal.Name, Repository: repo.Name})
	if err != nil {
		return nil, err
	}
	inputs := map[string]any{}
	if d, ok := b.InputSchema.Defaults().(map[string]any); ok {
		inputs = d
	}
	maps.Copy(inputs, in.Inputs)
	user := expr.User{Name: who.Principal.Name, Groups: who.Principal.Groups}
	request := expr.Request{Kind: generate.Create}
	var sets []policy.RuleSet
	for _, r := range cs.Resources {
		t := snap.Definitions.Types[r.Type]
		vars := expr.Vars{Inputs: r.Values, Params: anyMap(r.Params), User: user, Request: request}
		sets = append(sets, policy.RuleSet{Source: policy.SourceFloor, Rules: repo.FloorRules, Vars: vars},
			policy.RuleSet{Source: policy.SourceType, Rules: t.Spec.Approval.Rules, Vars: vars})
	}
	sets = append(sets, policy.RuleSet{Source: policy.SourceBlueprint, Rules: b.Spec.Approval.Rules, Vars: expr.Vars{Inputs: inputs, User: user, Request: request}})
	rules, err := policy.Match(sets...)
	if err != nil {
		return nil, err
	}
	return &Plan{
		Kind: generate.Create, Target: b.Metadata.Name + "/" + in.Name, Repository: repo.Name, Blueprint: b.Metadata.Name,
		Commit: snap.Commit, ChangeSet: cs, Rules: rules, Owners: []string{who.Principal.Name}, Inputs: inputs,
		Authorize: authz.CreateRequest, Object: authz.Object{Repository: repo.Name, CatalogItem: b.Metadata.Name},
	}, nil
}

// Submit records a planned request. Without rules to satisfy it is approved at once and its
// delivery job is queued in the same transaction.
// PlanInstanceAction plans a day-2 action on a blueprint instance: the changes it makes to the
// instance's resources, and the approvals they need. Like PlanType, it returns the plan with its
// access question even when planning fails.
func (s *Service) PlanInstanceAction(ctx context.Context, who auth.Identity, in InstanceActionInput) (*Plan, error) {
	repo, snap, err := s.snapshot(ctx, in.Repository)
	if err != nil {
		return nil, err
	}
	b := snap.Definitions.Blueprints[in.Blueprint]
	if b == nil {
		return nil, fmt.Errorf("%w: blueprint %s in %s", ErrNotFound, in.Blueprint, in.Repository)
	}
	inst, err := plan.ReadInstance(snap.FS, plan.InstancePath(b.Metadata.Name, in.Instance))
	if err != nil {
		return nil, fmt.Errorf("%w: instance %s/%s", ErrNotFound, b.Metadata.Name, in.Instance)
	}
	p := &Plan{Kind: generate.Action, Action: in.Action, Target: b.Metadata.Name + "/" + inst.Name, Repository: repo.Name, Blueprint: b.Metadata.Name,
		Commit: snap.Commit, Authorize: authz.OperateResource, Object: ownedObject(repo.Name, b.Metadata.Name, inst.Owner)}
	if inst.Owner != "" {
		p.Owners = []string{inst.Owner}
	}
	for _, rid := range inst.Resources {
		_, typeName, key, err := locate.ParseID(rid)
		if err != nil {
			continue
		}
		if r, err := inventory.Get(snap, typeName, key); err == nil && r.Status != inventory.Conforming {
			return p, fmt.Errorf("%w: %s is %s; fix it in Git before Quai edits it", ErrConflict, r.Path, r.Status)
		}
	}
	user := expr.User{Name: who.Principal.Name, Groups: who.Principal.Groups}
	cs, inputs, err := plan.BlueprintAction(snap.FS, snap.Definitions, b, plan.BlueprintActionRequest{
		Repository: repo.Name, Instance: inst, Action: in.Action, Inputs: in.ActionInputs, User: user,
	})
	if err != nil {
		var re *plan.ResourceError
		if !errors.As(err, &re) {
			err = schema.Under("actionInputs", err)
		}
		return p, err
	}
	p.ChangeSet, p.Inputs = cs, inputs

	// Each resource's rules see its document as it will be and as it was; the blueprint's see the
	// instance's inputs and the action's.
	request := expr.Request{Kind: generate.Action, Action: in.Action}
	var sets []policy.RuleSet
	for _, r := range cs.Resources {
		t := snap.Definitions.Types[r.Type]
		var previous map[string]any
		if res, err := inventory.Get(snap, r.Type, r.Key); err == nil {
			previous = res.Document
		}
		vars := expr.Vars{Inputs: r.Values, Previous: previous, Params: anyMap(r.Params), User: user, Request: request}
		sets = append(sets, policy.RuleSet{Source: policy.SourceFloor, Rules: repo.FloorRules, Vars: vars},
			policy.RuleSet{Source: policy.SourceType, Rules: t.Spec.Approval.Rules, Vars: vars})
	}
	approval := b.Spec.Approval
	if i := slices.IndexFunc(b.Spec.Actions, func(a definitions.BlueprintAction) bool { return a.Name == in.Action }); i >= 0 && b.Spec.Actions[i].Approval != nil {
		approval = *b.Spec.Actions[i].Approval
	}
	sets = append(sets, policy.RuleSet{Source: policy.SourceBlueprint, Rules: approval.Rules, Vars: expr.Vars{Inputs: inst.Inputs, Action: inputs, User: user, Request: request}})
	if p.Rules, err = policy.Match(sets...); err != nil {
		return p, err
	}
	return p, nil
}

func (s *Service) Submit(ctx context.Context, who auth.Identity, p *Plan, reason string) (db.Request, error) {
	rules, _ := json.Marshal(p.Rules)
	inputs, _ := json.Marshal(p.Inputs)
	if p.Inputs == nil {
		inputs = []byte(`{}`)
	}
	owners := p.Owners
	if owners == nil {
		owners = []string{}
	}
	outcome := policy.Evaluate(p.Rules, nil, policy.Context{Requester: who.Principal.Name, Owners: owners})
	state := PendingApproval
	if outcome.State == policy.Approved {
		state = Approved
	}
	var created db.Request
	err := s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		r, err := q.CreateRequest(ctx, db.CreateRequestParams{
			ID: uuid.New(), Kind: p.Kind, Action: p.Action, Target: p.Target, TypeName: p.Type, Blueprint: p.Blueprint,
			Requester: who.Principal.Name, RequesterEmail: who.Email, Reason: reason, Inputs: inputs, State: state,
			DefinitionsCommit: p.Commit, BaseCommit: p.Commit, Stack: p.Stack, Rules: rules, Owners: owners,
		})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return fmt.Errorf("%w: %s already has an open request; wait for it or cancel it first", ErrConflict, p.Target)
		}
		if err != nil {
			return err
		}
		fps, _ := json.Marshal(p.ChangeSet.Fingerprints)
		cs, err := q.CreateChangeSet(ctx, db.CreateChangeSetParams{ID: uuid.New(), RequestID: r.ID, Repository: p.Repository, Hash: p.ChangeSet.Hash, Fingerprints: fps})
		if err != nil {
			return err
		}
		for _, c := range p.ChangeSet.Changes {
			if err := q.CreateChange(ctx, db.CreateChangeParams{ChangeSetID: cs.ID, Path: c.Path, Op: c.Op, Before: c.Before, After: c.After}); err != nil {
				return err
			}
		}
		if err := event(ctx, q, r, "submitted", who.Principal.Name, nil); err != nil {
			return err
		}
		if state == Approved {
			if err := event(ctx, q, r, "approved", "", map[string]any{"reason": "no approval rule applies"}); err != nil {
				return err
			}
			if err := jobs.InsertTx(ctx, s.jobs, tx, DeliverArgs{RequestID: r.ID}, nil); err != nil {
				return err
			}
		}
		created = r
		return nil
	})
	return created, err
}

// Reviews returns a request's reviews and the decision they make.
func (s *Service) Reviews(ctx context.Context, r db.Request) ([]db.Review, policy.Outcome, error) {
	reviews, err := s.store.ListReviews(ctx, r.ID)
	if err != nil {
		return nil, policy.Outcome{}, err
	}
	var rules []policy.Rule
	_ = json.Unmarshal(r.Rules, &rules)
	return reviews, policy.Evaluate(rules, toReviews(reviews), policy.Context{Requester: r.Requester, Owners: r.Owners}), nil
}

// CanReview reports whether someone may approve or deny a request.
func CanReview(r db.Request, p authzPrincipal) bool {
	if r.State != PendingApproval {
		return false
	}
	var rules []policy.Rule
	_ = json.Unmarshal(r.Rules, &rules)
	ctx := policy.Context{Requester: r.Requester, Owners: r.Owners}
	return slices.ContainsFunc(rules, func(rule policy.Rule) bool { return policy.CanReview(rule, p.Name, p.Groups, ctx) })
}

type authzPrincipal = authz.Principal

// Review records an approval or a denial and moves the request on when the rules decide. A
// request whose regions changed in Git since it was planned becomes stale instead.
func (s *Service) Review(ctx context.Context, who auth.Identity, id uuid.UUID, decision, comment string) (db.Request, error) {
	r, err := s.get(ctx, id)
	if err != nil {
		return r, err
	}
	if r.State != PendingApproval {
		return r, fmt.Errorf("%w: the request is %s", ErrConflict, r.State)
	}
	if !CanReview(r, who.Principal) {
		return r, ErrNotApprover
	}
	if decision != policy.Approve && decision != policy.Deny {
		return r, &schema.ValidationError{Fields: map[string]string{"decision": "must be approve or deny"}}
	}
	if decision == policy.Deny && strings.TrimSpace(comment) == "" {
		return r, &schema.ValidationError{Fields: map[string]string{"comment": "is required to deny: say what to change"}}
	}
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	if err != nil {
		return r, err
	}
	if stale, err := s.stale(ctx, sets); err != nil {
		return r, err
	} else if stale != "" {
		return s.transition(ctx, r, PendingApproval, Stale, "stale", map[string]any{"region": stale})
	}
	var next db.Request
	err = s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if _, err := q.CreateReview(ctx, db.CreateReviewParams{
			ID: uuid.New(), RequestID: r.ID, Reviewer: who.Principal.Name, Groups: nonNil(who.Principal.Groups),
			Decision: decision, Comment: comment, ChangeSetHash: sets[0].Hash,
		}); err != nil {
			return err
		}
		if err := event(ctx, q, r, decision, who.Principal.Name, map[string]any{"comment": comment}); err != nil {
			return err
		}
		reviews, err := q.ListReviews(ctx, r.ID)
		if err != nil {
			return err
		}
		var rules []policy.Rule
		_ = json.Unmarshal(r.Rules, &rules)
		next = r
		switch policy.Evaluate(rules, toReviews(reviews), policy.Context{Requester: r.Requester, Owners: r.Owners}).State {
		case policy.Approved:
			if next, err = q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: PendingApproval, ToState: Approved}); err != nil {
				return err
			}
			if err := event(ctx, q, r, "approved", "", nil); err != nil {
				return err
			}
			return jobs.InsertTx(ctx, s.jobs, tx, DeliverArgs{RequestID: r.ID}, nil)
		case policy.Denied:
			if next, err = q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: PendingApproval, ToState: Rejected}); err != nil {
				return err
			}
			return event(ctx, q, r, "rejected", who.Principal.Name, nil)
		}
		return nil
	})
	return next, err
}

// Refresh makes this process see what an applied request committed: a worker in another process
// may have delivered it since this process last fetched the repository, and the requester's next
// look at the resource or the log should show it.
func (s *Service) Refresh(ctx context.Context, r db.Request) {
	if r.State != Applied {
		return
	}
	repo, err := s.reg.Get(s.Repository(ctx, r))
	if err != nil {
		return
	}
	if err := repo.Refresh(ctx, r.UpdatedAt.Time); err != nil {
		s.log.WarnContext(ctx, "refreshing the repository after a delivery", slog.String("repository", repo.Name), slog.Any("error", err))
	}
}

// Reviewed reports whether someone has reviewed a request already. An error counts as not.
func (s *Service) Reviewed(ctx context.Context, r db.Request, name string) bool {
	reviews, err := s.store.ListReviews(ctx, r.ID)
	return err == nil && slices.ContainsFunc(reviews, func(v db.Review) bool { return v.Reviewer == name })
}

// Cancellable reports whether a request in state can be cancelled: it is not delivered yet.
func Cancellable(state string) bool {
	switch state {
	case PendingApproval, Approved, Stale, Failed:
		return true
	}
	return false
}

// Cancel withdraws a request that is not delivered yet.
func (s *Service) Cancel(ctx context.Context, who auth.Identity, id uuid.UUID) (db.Request, error) {
	r, err := s.get(ctx, id)
	if err != nil {
		return r, err
	}
	if !Cancellable(r.State) {
		return r, fmt.Errorf("%w: a %s request cannot be cancelled", ErrConflict, r.State)
	}
	return s.transition(ctx, r, r.State, Cancelled, "cancelled", map[string]any{"by": who.Principal.Name})
}

// Retry delivers a failed request again.
func (s *Service) Retry(ctx context.Context, who auth.Identity, id uuid.UUID) (db.Request, error) {
	r, err := s.get(ctx, id)
	if err != nil {
		return r, err
	}
	if r.State != Failed {
		return r, fmt.Errorf("%w: only a failed request can be retried; a stale one must be planned again", ErrConflict)
	}
	var next db.Request
	err = s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var err error
		if next, err = q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: Failed, ToState: Approved}); err != nil {
			return conflict(err, r)
		}
		if err := event(ctx, q, r, "retried", who.Principal.Name, nil); err != nil {
			return err
		}
		return jobs.InsertTx(ctx, s.jobs, tx, DeliverArgs{RequestID: r.ID}, nil)
	})
	return next, err
}

// Get returns a request.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (db.Request, error) { return s.get(ctx, id) }

// Repository returns the repository a request writes to.
func (s *Service) Repository(ctx context.Context, r db.Request) string {
	if sets, err := s.store.ListChangeSets(ctx, r.ID); err == nil && len(sets) > 0 {
		return sets[0].Repository
	}
	return ""
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (db.Request, error) {
	r, err := s.store.GetRequest(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("%w: request %s", ErrNotFound, id)
	}
	return r, err
}

func (s *Service) transition(ctx context.Context, r db.Request, from, to, kind string, data map[string]any) (db.Request, error) {
	var next db.Request
	err := s.store.Tx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		var err error
		if next, err = q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: from, ToState: to}); err != nil {
			return conflict(err, r)
		}
		return event(ctx, q, r, kind, "", data)
	})
	return next, err
}

func conflict(err error, r db.Request) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: the request changed state meanwhile (it was %s)", ErrConflict, r.State)
	}
	return err
}

// stale returns the first region of the change sets that changed on its branch, or "".
func (s *Service) stale(ctx context.Context, sets []db.ChangeSet) (string, error) {
	for _, cs := range sets {
		_, snap, err := s.snapshot(ctx, cs.Repository)
		if err != nil {
			return "", err
		}
		var fps map[string]string
		_ = json.Unmarshal(cs.Fingerprints, &fps)
		for _, p := range slices.Sorted(maps.Keys(fps)) {
			now, err := locate.Fingerprint(snap.FS, locate.Region{Strategy: locate.File, Path: p})
			if err != nil {
				return "", err
			}
			if now != fps[p] {
				return p, nil
			}
		}
	}
	return "", nil
}

func (s *Service) snapshot(ctx context.Context, name string) (*repositories.Repo, *repositories.Snapshot, error) {
	repo, err := s.reg.Get(name)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	snap, err := repo.Snapshot(ctx, true)
	if err != nil {
		return nil, nil, err
	}
	return repo, snap, nil
}

// DeliverArgs delivers an approved request.
type DeliverArgs struct {
	RequestID uuid.UUID `json:"requestId"`
}

// Kind names the job.
func (DeliverArgs) Kind() string { return "deliver_request" }

// Worker delivers approved requests by direct commit.
type Worker struct {
	river.WorkerDefaults[DeliverArgs]
	s *Service
}

// NewWorker returns the delivery worker.
func NewWorker(s *Service) *Worker { return &Worker{s: s} }

// Work commits an approved request under its repository's lock. It is safe to run twice: a
// request that is no longer approved or merging is left alone, and changes already on the branch
// make no second commit.
func (w *Worker) Work(ctx context.Context, job *river.Job[DeliverArgs]) error {
	s := w.s
	r, err := s.get(ctx, job.Args.RequestID)
	if err != nil {
		return err
	}
	if r.State != Approved && r.State != Merging {
		return nil
	}
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	if err != nil || len(sets) == 0 {
		return fmt.Errorf("request %s has no change set: %w", r.ID, err)
	}
	cs := sets[0]
	repo, err := s.reg.Get(cs.Repository)
	if err != nil {
		return err
	}
	return jobs.WithRepositoryLock(ctx, s.store.Pool, repo.Name, 5*time.Second, func(ctx context.Context) error {
		if r.State == Approved {
			if r, err = s.transition(ctx, r, Approved, Merging, "merging", nil); err != nil {
				return err
			}
		}
		changes, err := s.store.ListChanges(ctx, cs.ID)
		if err != nil {
			return err
		}
		var fps map[string]string
		_ = json.Unmarshal(cs.Fingerprints, &fps)
		commit := gitx.Commit{Branch: repo.Branch, Message: message(r, cs), Author: author(r), Fingerprints: fps}
		for _, c := range changes {
			commit.Changes = append(commit.Changes, plan.Change{Path: c.Path, Op: c.Op, Before: c.Before, After: c.After})
		}
		sha, err := repo.Mirror.CommitAndPush(ctx, commit)
		switch {
		case errors.Is(err, gitx.ErrStale):
			_, err = s.transition(ctx, r, Merging, Stale, "stale", map[string]any{"error": err.Error()})
			return err
		case errors.Is(err, gitx.ErrNoChange):
			_, err = s.transition(ctx, r, Merging, Applied, "applied", map[string]any{"commit": sha, "note": "the changes were already on the branch"})
			return err
		case err != nil:
			if job.Attempt >= job.MaxAttempts {
				_, _ = s.transition(ctx, r, Merging, Failed, "failed", map[string]any{"error": err.Error()})
			}
			return err
		}
		if _, err := repo.Snapshot(ctx, true); err != nil {
			s.log.WarnContext(ctx, "refresh after commit", slog.Any("error", err))
		}
		// Without a runner, a merged change is final (docs/design/01: resources show In repo).
		_, err = s.transition(ctx, r, Merging, Applied, "committed", map[string]any{"commit": sha})
		return err
	})
}

// message is the commit message of docs/design/05.
func message(r db.Request, cs db.ChangeSet) string {
	verb := r.Kind
	if r.Kind == generate.Action {
		verb = r.Action
	}
	trailers := []gitx.Trailer{
		{Key: "Quai-Request", Value: fmt.Sprintf("#%d %s", r.Number, r.ID)},
		{Key: "Quai-Change-Set", Value: cs.Hash},
	}
	if r.Stack != "" {
		trailers = append(trailers, gitx.Trailer{Key: "Quai-Stack", Value: r.Stack})
	}
	a := author(r)
	trailers = append(trailers, gitx.Trailer{Key: "Requested-by", Value: a.Name + " <" + a.Email + ">"})
	return gitx.Message("quai: "+verb+" "+r.Target, r.Reason, trailers...)
}

func author(r db.Request) gitx.Signature {
	email := r.RequesterEmail
	if email == "" {
		email = r.Requester + "@users.quai"
	}
	return gitx.Signature{Name: r.Requester, Email: email}
}

func event(ctx context.Context, q *db.Queries, r db.Request, kind, actor string, data map[string]any) error {
	raw := []byte(`{}`)
	if data != nil {
		raw, _ = json.Marshal(data)
	}
	_, err := q.AddEvent(ctx, db.AddEventParams{RequestID: pgtype.UUID{Bytes: r.ID, Valid: true}, Target: r.Target, Kind: kind, Actor: actor, Data: raw})
	return err
}

func toReviews(rs []db.Review) []policy.Review {
	out := make([]policy.Review, len(rs))
	for i, r := range rs {
		out[i] = policy.Review{Reviewer: r.Reviewer, Groups: r.Groups, Decision: r.Decision}
	}
	return out
}

// checkParams checks create params against the type's params: pattern, options, and for
// discovered params, the values the repository has.
func checkParams(snap *repositories.Snapshot, t *definitions.ResourceType, params map[string]string) (map[string]string, error) {
	errs := map[string]string{}
	out := map[string]string{}
	for _, p := range t.Spec.Params {
		v := strings.TrimSpace(params[p.Name])
		switch {
		case v == "":
			errs["params."+p.Name] = "is required"
		case !locate.SafeSegment(v):
			errs["params."+p.Name] = "use 1 to 63 letters, digits, '.', '_' or '-', starting with a letter or a digit"
		case p.Pattern != "" && !matches(p.Pattern, v):
			errs["params."+p.Name] = "must match " + p.Pattern
		case len(p.Options) > 0 && !slices.Contains(p.Options, v):
			errs["params."+p.Name] = "must be one of " + strings.Join(p.Options, ", ")
		case p.Discover && !slices.Contains(inventory.Discover(snap, t, p.Name), v):
			errs["params."+p.Name] = "must be one of " + strings.Join(inventory.Discover(snap, t, p.Name), ", ")
		}
		out[p.Name] = v
	}
	if len(errs) > 0 {
		return nil, &schema.ValidationError{Fields: errs}
	}
	return out, nil
}

func matches(pattern, v string) bool {
	re, err := regexp.Compile("^(?:" + pattern + ")$")
	return err == nil && re.MatchString(v)
}

func findAction(t *definitions.ResourceType, name string) *definitions.Action {
	for i := range t.Spec.Actions {
		if t.Spec.Actions[i].Name == name {
			return &t.Spec.Actions[i]
		}
	}
	return nil
}

func objectOf(r inventory.Resource) authz.Object {
	return ownedObject(r.Repository, r.Type, r.Owner)
}

// ownedObject is something in a repository, made from a catalog item and owned by a person or
// a team ("team:<name>").
func ownedObject(repository, item, owner string) authz.Object {
	o := authz.Object{Repository: repository, CatalogItem: item}
	if team, ok := strings.CutPrefix(owner, "team:"); ok {
		o.Team = team
	} else {
		o.Owner = owner
	}
	return o
}

func stack(t *definitions.ResourceType, params map[string]string) string {
	if t.Spec.Stack == "" {
		return ""
	}
	p, err := locate.Compile(t.Spec.Stack)
	if err != nil {
		return t.Spec.Stack
	}
	s, _ := p.Format(params)
	return s
}

func anyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
