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

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/chatlink"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/edit"
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
	"github.com/damhau/quai/backend/internal/telemetry"
)

// Request states (docs/architecture/domain-model.md#request).
const (
	PendingApproval = "pending_approval"
	Approved        = "approved"
	Rejected        = "rejected"
	Cancelled       = "cancelled"
	Stale           = "stale"
	Merging         = "merging"
	Applying        = "applying" // merged, waiting for the apply that includes it (with a runner)
	Applied         = "applied"
	Failed          = "failed"
	Expired         = "expired" // waited for approval longer than its expiry: final, its reviews void
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
	store     *store.Store
	reg       *repositories.Registry
	jobs      *jobs.Client
	log       *slog.Logger
	publicURL string          // for the links in pull requests; "" leaves them out
	dir       *auth.Directory // the teams of the groups of reviews from before auth.teams
	stepUp    *auth.StepUp
	reach     func(ctx context.Context, u db.User, kind string) (bool, error) // whether notifications reach a person; nil without a channel
	links     *chatlink.Signer                                                // opens the links of chat cards; nil: none are
	waiting   config.Approvals                                                // reminders and escalation
	now       func() time.Time                                                // delegations are judged by it (UseClock)
}

// New returns the request service. jobsClient inserts the delivery jobs.
func New(s *store.Store, reg *repositories.Registry, jobsClient *jobs.Client, log *slog.Logger) *Service {
	return &Service{store: s, reg: reg, jobs: jobsClient, log: log, dir: &auth.Directory{}, stepUp: auth.NewStepUp(config.Auth{}, s), now: time.Now}
}

// UseStepUp gives the step-up settings of the configuration (auth.stepUp); without them, step-up
// requests need an authentication of the last config.DefaultStepUpMaxAge.
func (s *Service) UseStepUp(su *auth.StepUp) { s.stepUp = su }

// UseDirectory gives the directory of the configuration (auth.teams), for the reviews that kept
// no teams.
func (s *Service) UseDirectory(d *auth.Directory) { s.dir = d }

// UseJobs sets the client that inserts jobs, for a worker whose client is made after the
// service its workers use.
func (s *Service) UseJobs(c *jobs.Client) { s.jobs = c }

// TypeInput is a change to one resource of a type.
type TypeInput struct {
	Repository   string            `json:"repository"`
	Type         string            `json:"type"`
	Kind         string            `json:"kind" enum:"create,update,delete,action,migrate"`
	Key          string            `json:"key,omitempty" doc:"The resource's key, for update, delete and action; a migrate request is on every non-conforming resource of the type"`
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
	Stacks                                                   []string // every stack the change set is in
	Commit                                                   string
	ChangeSet                                                *plan.ChangeSet
	Rules                                                    []policy.Rule
	Owners                                                   []string
	// Codeowners are the owners the Git platform names for the change set's paths, when a rule's
	// approvers include codeowners (docs/design/05-delivery-and-approvals.md#approver-kinds).
	Codeowners repositories.Owners
	// Manager is the requester's manager, a person of the directory, when a rule's approvers include
	// manager; ManagerReason says why there is none (docs/design/05-delivery-and-approvals.md
	// #approver-kinds). Submitting keeps them with the request.
	Manager, ManagerReason string
	Inputs                 map[string]any
	// Unmigrated are the non-conforming resources a migrate request leaves as they are, and why.
	Unmigrated []plan.Unmigrated
	// Separation is the repository's separation of duties as the plan applies it: its options,
	// and who changed the definitions in the period before (docs/design/05-delivery-and-approvals.md
	// #separation-of-duties). Submitting keeps it with the request.
	Separation Separation
	// StepUp says that the approvals must be made with a step-up: the stepUp condition of an
	// approval block that applies held (docs/design/05-delivery-and-approvals.md#step-up-authentication).
	StepUp bool
	// Expiry is the shortest approval expiry among the approval blocks of the definitions that
	// apply (the type's and its action's, each resource type's and the blueprint's, its action's),
	// as written; "" for none. Submitting keeps it with the request, and when the request expires
	// (open question 78).
	Expiry string
	// Authorize is the access question the caller must answer yes before showing or submitting:
	// on Object, and on each of Also (a migrate request operates every resource it covers).
	Authorize authz.Action
	Object    authz.Object
	Also      []authz.Object
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

	if in.Kind == generate.Migrate {
		return s.planMigrate(ctx, who, repo, snap, t, p)
	}
	if t.Spec.Locate.Strategy == locate.Field && (in.Kind == generate.Create || in.Kind == generate.Delete) {
		return nil, fmt.Errorf("%w: type %s writes fields of something it does not own: it has no %s", ErrConflict, t.Metadata.Name, in.Kind)
	}
	if in.Kind == generate.Create {
		// The environment's list applies as the env param will be written: checkParams trims it.
		p.Authorize, p.Object = authz.CreateRequest, authz.Object{Repository: repo.Name, CatalogItem: t.Metadata.Name,
			Entitlement: EntitlementOf(t.Spec.Entitlements, in.Params[definitions.EnvParam])}
		params, err := checkParams(snap, t, in.Params)
		if err != nil {
			return p, err
		}
		p.Object.Entitlement = EntitlementOf(t.Spec.Entitlements, params[definitions.EnvParam])
		req.Params, req.Values = params, in.Values
		p.Owners = []string{who.Principal.Name}
		if o := t.Spec.Ownership; o != nil && o.Codeowners {
			// The new resource's owners are those the platform names for its path.
			loc, _ := t.Spec.Locate.Locator()
			region, err := loc.Region(params)
			if err != nil {
				return p, err
			}
			owners, err := s.codeownersOf(ctx, repo, snap, region.DocumentFile())
			if err != nil {
				return p, err
			}
			p.Owners = owners.Entries
		}
	} else {
		r, err := inventory.Get(snap, t.Metadata.Name, in.Key)
		if err != nil {
			return nil, fmt.Errorf("%w: resource %s", ErrNotFound, locate.ID(repo.Name, t.Metadata.Name, in.Key))
		}
		p.Authorize, p.Object = authz.OperateResource, objectOf(r)
		if r.Status != inventory.Conforming {
			if Migratable(t, r) {
				return p, fmt.Errorf("%w: %s is %s; fix it in Git, or with a migrate request on type %s, before Quai edits it", ErrConflict, r.Path, r.Status, t.Metadata.Name)
			}
			return p, fmt.Errorf("%w: %s is %s; fix it in Git before Quai edits it", ErrConflict, r.Path, r.Status)
		}
		current = r.Document
		req.Params, req.Current = r.Params, r.Document
		if r.Owner != "" {
			p.Owners = []string{r.Owner}
		}
		if r.Owners != nil {
			p.Owners = r.Owners
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
		// A value the file's editor will not write (one defined in code) is a field's problem.
		if at, reason, ok := edit.Refused(err); ok && len(at) > 0 {
			err = &schema.ValidationError{Fields: map[string]string{edit.Field(at): reason}}
		}
		return p, schema.Under("values", err)
	}
	p.ChangeSet = cs
	loc, _ := t.Spec.Locate.Locator()
	region, _ := loc.Region(req.Params)
	p.Target = locate.ID(repo.Name, t.Metadata.Name, region.Key)
	p.Stack = stack(t, req.Params)
	if p.Stack != "" {
		p.Stacks = []string{p.Stack}
	}

	// The rules see the document as it will be, and as it was.
	next := current
	for _, c := range cs.Changes {
		if c.Path != region.DocumentFile() || c.After == nil {
			continue
		}
		if doc, _, err := generate.Decode(snap.FS, t, c.Path, region.Params, c.After); err == nil {
			// An entry's document is its value, a field region's its fields, a directory's its
			// record; a deleted entry keeps the document it had.
			if value, _, err := locate.Document(doc, nil, region); err == nil {
				next = value
			}
		}
	}
	p.Inputs = next
	vars.Inputs, vars.Previous, vars.Params = next, current, anyMap(req.Params)
	if t.Spec.Generate.Kind == definitions.GenerateMapping && (in.Kind == generate.Create || in.Kind == generate.Update) {
		// The form is not the file: rules read the form's values, the values before the change
		// read back from the file, and the file as doc.
		vars.Inputs, vars.Doc = generate.Inputs(t, req.Values), next
		if in.Kind == generate.Update {
			if previous, err := generate.Read(t, current, req.Params); err == nil {
				vars.Previous = previous
			}
		}
	}
	// An action's approval replaces the type's rules, not its step-up (open question 50).
	p.StepUp = needsStepUp(t.Spec.Approval, vars) || needsStepUp(approval, vars)
	p.expires(t.Spec.Approval, approval)
	sets := []policy.RuleSet{{Source: policy.SourceFloor, Rules: repo.FloorRules, Vars: vars}, {Source: policy.SourceType, Rules: approval.Rules, Vars: vars}}
	if p.Rules, err = policy.Match(sets...); err != nil {
		return p, err
	}
	if in.Kind == generate.Delete && t.Spec.Deletion != "allowed" && len(p.Rules) == 0 {
		// Deleting always needs an approval (docs/design/02, deletion): from the owners, or from
		// whoever the repository's floor rules name, unless the type allows deletion outright.
		p.Rules = []policy.Rule{{Names: []string{"type/deletion"}, Approvers: fallbackApprovers(repo), Min: 1, Distinct: true}}
	}
	if err := s.withSeparation(ctx, repo, p, t.Files); err != nil {
		return p, err
	}
	if err := s.withCodeowners(ctx, repo, snap, p); err != nil {
		return p, err
	}
	if err := s.withManager(ctx, who, p); err != nil {
		return p, err
	}
	return p, s.deliverable(ctx, repo)
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
	inputs := map[string]any{}
	if d, ok := b.InputSchema.Defaults().(map[string]any); ok {
		inputs = d
	}
	maps.Copy(inputs, in.Inputs)
	// Like PlanType, the plan carries its access question even when planning fails: the
	// blueprint's entitlements for the env input as the form gives it, or its default.
	p := &Plan{
		Kind: generate.Create, Target: b.Metadata.Name + "/" + in.Name, Repository: repo.Name, Blueprint: b.Metadata.Name,
		Commit: snap.Commit, Owners: []string{who.Principal.Name}, Inputs: inputs,
		Authorize: authz.CreateRequest, Object: authz.Object{Repository: repo.Name, CatalogItem: b.Metadata.Name,
			Entitlement: EntitlementOf(b.Spec.Entitlements, EnvValue(inputs[definitions.EnvParam]))},
	}
	if !locate.SafeSegment(in.Name) {
		return p, &schema.ValidationError{Fields: map[string]string{"name": "use 1 to 63 letters, digits, '.', '_' or '-'"}}
	}
	cs, err := plan.Blueprint(snap.FS, snap.Definitions, b, plan.BlueprintRequest{Name: in.Name, Inputs: in.Inputs, Owner: who.Principal.Name, Repository: repo.Name})
	if err != nil {
		return p, err
	}
	// Creating a resource through a blueprint needs what requesting its type needs, for the
	// resource's environment (open question 47): a blueprint does not open a type to more people.
	for _, r := range cs.Resources {
		if t := snap.Definitions.Types[r.Type]; t != nil && t.Spec.Entitlements != nil {
			e := EntitlementOf(t.Spec.Entitlements, r.Params[definitions.EnvParam])
			e.Item = "type " + t.Metadata.Name
			p.Also = append(p.Also, authz.Object{Repository: repo.Name, CatalogItem: b.Metadata.Name, Entitlement: e})
		}
	}
	p.ChangeSet, p.Stacks = cs, stacksOf(snap, cs.Resources)
	user := expr.User{Name: who.Principal.Name, Groups: who.Principal.Groups}
	request := expr.Request{Kind: generate.Create}
	var sets []policy.RuleSet
	for _, r := range cs.Resources {
		t := snap.Definitions.Types[r.Type]
		vars := expr.Vars{Inputs: r.Values, Params: anyMap(r.Params), User: user, Request: request}
		sets = append(sets, policy.RuleSet{Source: policy.SourceFloor, Rules: repo.FloorRules, Vars: vars},
			policy.RuleSet{Source: policy.SourceType, Rules: t.Spec.Approval.Rules, Vars: vars})
		p.StepUp = p.StepUp || needsStepUp(t.Spec.Approval, vars)
		p.expires(t.Spec.Approval)
	}
	bvars := expr.Vars{Inputs: inputs, User: user, Request: request}
	sets = append(sets, policy.RuleSet{Source: policy.SourceBlueprint, Rules: b.Spec.Approval.Rules, Vars: bvars})
	p.StepUp = p.StepUp || needsStepUp(b.Spec.Approval, bvars)
	p.expires(b.Spec.Approval)
	if p.Rules, err = policy.Match(sets...); err != nil {
		return p, err
	}
	if err := s.withSeparation(ctx, repo, p, b.Files); err != nil {
		return p, err
	}
	if err := s.withCodeowners(ctx, repo, snap, p); err != nil {
		return p, err
	}
	if err := s.withManager(ctx, who, p); err != nil {
		return p, err
	}
	return p, s.deliverable(ctx, repo)
}

// EntitlementOf is the access question of a request for a catalog item with the entitlements e,
// for the environment env (the value of the env param or input; "" for none): the item's lists,
// and the environment's narrower list when e narrows it.
func EntitlementOf(e *definitions.Entitlements, env string) authz.Entitlement {
	if e == nil {
		return authz.Entitlement{}
	}
	out := authz.Entitlement{Visibility: e.Visibility, Requesters: e.Requesters}
	if key, list, ok := e.Environment(env); ok {
		out.Environment, out.EnvironmentRequesters = key, list
	}
	return out
}

// EnvValue is the environment a blueprint's env input names: a string, or a number or a boolean
// as written; "" for none.
func EnvValue(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case float64, int, int64, bool:
		return fmt.Sprint(v)
	}
	return ""
}

// Migratable reports whether a resource does not conform to its type's schema and the type's
// migration makes it conform: a migrate request on the type fixes it.
func Migratable(t *definitions.ResourceType, r inventory.Resource) bool {
	if t == nil || t.Spec.MigrateTemplate == nil || r.Status != inventory.NonConforming || r.Error != "" {
		return false
	}
	_, err := generate.Identity(t, generate.Request{Kind: generate.Migrate, Params: r.Params, Current: r.Document, Code: r.Code})
	return err == nil
}

// planMigrate plans a migrate request (docs/design/02, schema evolution): the type's migration
// over every non-conforming resource of the type, in one change set, reviewed like any other. It
// changes resources whoever owns them, so it needs operator access to the type's resources, and
// their owners approve it.
func (s *Service) planMigrate(ctx context.Context, who auth.Identity, repo *repositories.Repo, snap *repositories.Snapshot, t *definitions.ResourceType, p *Plan) (*Plan, error) {
	p.Target = repo.Name + "/" + t.Metadata.Name
	if t.Spec.MigrateTemplate == nil {
		return p, fmt.Errorf("%w: type %s has no migration (spec.migrate)", ErrConflict, t.Metadata.Name)
	}
	rs, err := inventory.ScanType(snap, t)
	if err != nil {
		return p, err
	}
	todo := map[string]map[string]string{}
	byKey := map[string]inventory.Resource{}
	for _, r := range rs {
		// Schema problems only: a file Quai cannot read safely is fixed in Git.
		if r.Status == inventory.NonConforming && r.Error == "" {
			todo[r.Key], byKey[r.Key] = r.Params, r
		}
	}
	if len(todo) == 0 {
		return p, fmt.Errorf("type %s: every resource Quai can read conforms to its schema: %w", t.Metadata.Name, plan.ErrNoChange)
	}
	// The request operates every resource it covers (docs/design/08: the operator role and
	// ownership of each, or a platform admin).
	for i, key := range slices.Sorted(maps.Keys(byKey)) {
		if i == 0 {
			p.Authorize, p.Object = authz.OperateResource, objectOf(byKey[key])
		} else {
			p.Also = append(p.Also, objectOf(byKey[key]))
		}
	}
	cs, left, err := plan.Migrate(snap.FS, t, todo)
	p.Unmigrated = left
	if err != nil {
		return p, err
	}
	p.ChangeSet = cs
	for _, u := range left {
		delete(byKey, u.Key)
	}
	user := expr.User{Name: who.Principal.Name, Groups: who.Principal.Groups}
	var sets []policy.RuleSet
	var owners []string
	for _, key := range slices.Sorted(maps.Keys(byKey)) {
		r := byKey[key]
		if r.Owner != "" {
			owners = append(owners, r.Owner)
		}
		owners = append(owners, r.Owners...)
		if st := stack(t, r.Params); st != "" && !slices.Contains(p.Stacks, st) {
			p.Stacks = append(p.Stacks, st)
		}
		// The rules see each resource as the migration leaves it, and as it was.
		next, _ := generate.Migrated(t, r.Document, r.Params)
		vars := expr.Vars{Inputs: next, Previous: r.Document, Params: anyMap(r.Params), User: user, Request: expr.Request{Kind: generate.Migrate}}
		sets = append(sets, policy.RuleSet{Source: policy.SourceFloor, Rules: repo.FloorRules, Vars: vars},
			policy.RuleSet{Source: policy.SourceType, Rules: t.Spec.Approval.Rules, Vars: vars})
		p.StepUp = p.StepUp || needsStepUp(t.Spec.Approval, vars)
		p.expires(t.Spec.Approval)
	}
	slices.Sort(owners)
	p.Owners = slices.Compact(owners)
	if len(p.Stacks) == 1 {
		p.Stack = p.Stacks[0]
	}
	if p.Rules, err = policy.Match(sets...); err != nil {
		return p, err
	}
	if err := s.withSeparation(ctx, repo, p, t.Files); err != nil {
		return p, err
	}
	if err := s.withCodeowners(ctx, repo, snap, p); err != nil {
		return p, err
	}
	if err := s.withManager(ctx, who, p); err != nil {
		return p, err
	}
	return p, s.deliverable(ctx, repo)
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
	p.ChangeSet, p.Inputs, p.Stacks = cs, inputs, stacksOf(snap, cs.Resources)

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
		p.StepUp = p.StepUp || needsStepUp(t.Spec.Approval, vars)
		p.expires(t.Spec.Approval)
	}
	approval := b.Spec.Approval
	if i := slices.IndexFunc(b.Spec.Actions, func(a definitions.BlueprintAction) bool { return a.Name == in.Action }); i >= 0 && b.Spec.Actions[i].Approval != nil {
		approval = *b.Spec.Actions[i].Approval
	}
	bvars := expr.Vars{Inputs: inst.Inputs, Action: inputs, User: user, Request: request}
	sets = append(sets, policy.RuleSet{Source: policy.SourceBlueprint, Rules: approval.Rules, Vars: bvars})
	p.StepUp = p.StepUp || needsStepUp(b.Spec.Approval, bvars) || needsStepUp(approval, bvars)
	p.expires(b.Spec.Approval, approval)
	if p.Rules, err = policy.Match(sets...); err != nil {
		return p, err
	}
	if err := s.withSeparation(ctx, repo, p, b.Files); err != nil {
		return p, err
	}
	if err := s.withCodeowners(ctx, repo, snap, p); err != nil {
		return p, err
	}
	if err := s.withManager(ctx, who, p); err != nil {
		return p, err
	}
	return p, s.deliverable(ctx, repo)
}

func (s *Service) Submit(ctx context.Context, who auth.Identity, p *Plan, reason string) (db.Request, error) {
	ctx = acting(ctx, who)
	rules, _ := json.Marshal(p.Rules)
	inputs, _ := json.Marshal(p.Inputs)
	if p.Inputs == nil {
		inputs = []byte(`{}`)
	}
	owners := p.Owners
	if owners == nil {
		owners = []string{}
	}
	prMode, prefix := false, ""
	if repo, err := s.reg.Get(p.Repository); err == nil && repo.Document.Spec.Delivery.Mode == PullRequestMode {
		prMode, prefix = true, repo.Document.Spec.Delivery.BranchPrefix
		if err := s.deliverable(ctx, repo); err != nil {
			return db.Request{}, err
		}
	}
	if !prMode {
		// Delivered by a direct commit, a request has no plan before it is approved: a rule that
		// reads the plan applies.
		p.Rules = policy.WithoutPlan(p.Rules, "no plan comes before a direct commit")
		rules, _ = json.Marshal(p.Rules)
	}
	outcome := policy.Evaluate(p.Rules, nil, policy.Context{Requester: who.Principal.Name, Owners: owners, Codeowners: p.Codeowners.Entries, Manager: p.Manager,
		DefinitionAuthors: p.Separation.Excluded(), DefinitionAuthorEmails: p.Separation.Emails})
	separation, err := json.Marshal(p.Separation)
	if err != nil {
		return db.Request{}, err
	}
	state := PendingApproval
	if outcome.State == policy.Approved {
		state = Approved
	}
	// Reminders, escalation and expiry count from the submission, by the service's clock.
	submitted := s.now()
	expiresAt := pgtype.Timestamptz{}
	if _, d := definitions.Expiry(definitions.Approval{Expiry: p.Expiry}); d > 0 {
		expiresAt = pgtype.Timestamptz{Time: submitted.Add(d), Valid: true}
	}
	var created db.Request
	err = s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		r, err := q.CreateRequest(ctx, db.CreateRequestParams{
			ID: uuid.New(), Kind: p.Kind, Action: p.Action, Target: p.Target, TypeName: p.Type, Blueprint: p.Blueprint,
			Requester: who.Principal.Name, RequesterEmail: who.Email, Reason: reason, Inputs: inputs, State: state,
			DefinitionsCommit: p.Commit, BaseCommit: p.Commit, Stack: p.Stack, Rules: rules, Owners: owners, Stacks: nonNil(p.Stacks),
			Codeowners: nonNil(p.Codeowners.Entries), CodeownersUnmatched: nonNil(p.Codeowners.Unmatched), StepUp: p.StepUp,
			Manager: p.Manager, ManagerReason: p.ManagerReason, Separation: separation,
			SubmittedAt: pgtype.Timestamptz{Time: submitted, Valid: true}, Expiry: p.Expiry, ExpiresAt: expiresAt,
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
			var entries []byte
			if c.Entries != nil {
				if entries, err = json.Marshal(c.Entries); err != nil {
					return err
				}
			}
			if err := q.CreateChange(ctx, db.CreateChangeParams{ChangeSetID: cs.ID, Path: c.Path, Op: c.Op, Before: c.Before, After: c.After, Entries: entries}); err != nil {
				return err
			}
		}
		if err := s.event(ctx, tx, r, "submitted", who.Principal.Name, nil); err != nil {
			return err
		}
		details := map[string]any{"kind": p.Kind, "action": p.Action, "type": p.Type, "blueprint": p.Blueprint,
			"repository": p.Repository, "changeSet": p.ChangeSet.Hash, "definitionsCommit": p.Commit, "reason": audit.Personal(reason)}
		if p.Expiry != "" && state == PendingApproval {
			details["expiry"], details["expiresAt"] = p.Expiry, audit.Time(expiresAt.Time)
		}
		if sod := separationDetails(p.Separation); sod != nil {
			details["separationOfDuties"] = sod
		}
		if err := recordRequest(ctx, tx, r, "submitted", details); err != nil {
			return err
		}
		if state == Approved {
			if err := s.event(ctx, tx, r, "approved", "", map[string]any{"reason": "no approval rule applies"}); err != nil {
				return err
			}
			if err := audit.Add(ctx, tx, audit.Event{Actor: audit.Quai, Action: "request.approved",
				Object:  audit.Object{Type: "request", ID: r.ID.String(), Resource: r.Target},
				Details: map[string]any{"changeSet": p.ChangeSet.Hash, "reason": "no approval rule applies"}}); err != nil {
				return err
			}
		}
		switch {
		case prMode:
			// The pull request opens now, so that CI can plan while approvals are pending; the
			// opening job delivers an approved request next.
			if _, err := q.PutDelivery(ctx, db.PutDeliveryParams{RequestID: r.ID, Repository: p.Repository, Branch: branch(prefix, r)}); err != nil {
				return err
			}
			if err := jobs.InsertTx(ctx, s.jobs, tx, OpenArgs{RequestID: r.ID}, nil); err != nil {
				return err
			}
		case state == Approved:
			if err := jobs.InsertTx(ctx, s.jobs, tx, DeliverArgs{RequestID: r.ID}, nil); err != nil {
				return err
			}
		}
		created = r
		return nil
	})
	return created, err
}

// withCodeowners fills the plan's codeowners from the owners the platform names for the change
// set's paths on the branch, when a rule's approvers include codeowners. When they cannot be read,
// the plan fails: who may approve would be unknown.
func (s *Service) withCodeowners(ctx context.Context, repo *repositories.Repo, snap *repositories.Snapshot, p *Plan) error {
	if p.ChangeSet == nil || !slices.ContainsFunc(p.Rules, func(r policy.Rule) bool { return slices.Contains(r.Approvers, "codeowners") }) {
		return nil
	}
	paths := make([]string, 0, len(p.ChangeSet.Changes))
	for _, c := range p.ChangeSet.Changes {
		paths = append(paths, c.Path)
	}
	owners, err := s.codeownersOf(ctx, repo, snap, paths...)
	p.Codeowners = owners
	return err
}

// codeownersOf returns the owners the platform names for paths on the branch at the snapshot.
func (s *Service) codeownersOf(ctx context.Context, repo *repositories.Repo, snap *repositories.Snapshot, paths ...string) (repositories.Owners, error) {
	table, err := repo.Codeowners(ctx, snap.Commit)
	if err != nil {
		return table.Of(), err
	}
	return table.Of(paths...), nil
}

// Reviews returns a request's reviews and the decision they make.
func (s *Service) Reviews(ctx context.Context, r db.Request) ([]db.Review, policy.Outcome, error) {
	reviews, err := s.reviews(ctx, r)
	if err != nil {
		return nil, policy.Outcome{}, err
	}
	var rules []policy.Rule
	_ = json.Unmarshal(r.Rules, &rules)
	return reviews, policy.Evaluate(rules, toReviews(reviews, s.dir), contextOf(r)), nil
}

// contextOf is what the policy needs to know of a request to decide it.
func contextOf(r db.Request) policy.Context {
	sep := SeparationOf(r)
	return policy.Context{Requester: r.Requester, Owners: r.Owners, Codeowners: r.Codeowners, Manager: r.Manager,
		DefinitionAuthors: sep.Excluded(), DefinitionAuthorEmails: sep.Emails}
}

// CanReview reports whether someone may approve or deny a request.
func CanReview(r db.Request, p authzPrincipal) bool {
	return r.State == PendingApproval && eligible(r, p, contextOf(r))
}

// eligible reports whether a rule of a request takes someone's review in ctx.
func eligible(r db.Request, p authzPrincipal, ctx policy.Context) bool {
	var rules []policy.Rule
	_ = json.Unmarshal(r.Rules, &rules)
	who := policy.Person{Name: p.Name, Email: p.Email, Groups: p.Groups, Teams: p.Teams}
	for i := range rules {
		if policy.Eligible(rules, i, who, ctx) {
			return true
		}
	}
	return false
}

type authzPrincipal = authz.Principal

// Review records an approval or a denial and moves the request on when the rules decide. A
// request whose regions changed in Git since it was planned becomes stale instead.
func (s *Service) Review(ctx context.Context, who auth.Identity, id uuid.UUID, decision, comment string) (db.Request, error) {
	return s.ReviewLink(ctx, who, id, ReviewInput{Decision: decision, Comment: comment})
}

// ReviewInput is a review: a decision, the comment that goes with it, the link of the chat card it
// confirms ("" for none), and on whose behalf it is made ("" in one's own right).
type ReviewInput struct {
	Decision, Comment, Link, OnBehalfOf string
}

// ReviewLink is Review confirming the decision of a link from a chat card (in.Link; "" for none),
// on a delegator's behalf when in.OnBehalfOf names one: the link's checks (OpenLink) come first,
// on top of every check of a review, and the change set reviewed must be the one the link is
// bound to. A review on someone's behalf needs a delegation from them active now, and their rights
// to reach the request now, under the reviewer's own checks (Capacity); its step-up is the
// reviewer's, the human acting.
func (s *Service) ReviewLink(ctx context.Context, who auth.Identity, id uuid.UUID, in ReviewInput) (db.Request, error) {
	ctx = acting(ctx, who)
	decision, comment := in.Decision, in.Comment
	r, err := s.get(ctx, id)
	if err != nil {
		return r, err
	}
	var opened *chatlink.Link
	if in.Link != "" {
		l, err := s.OpenLink(ctx, who, r, in.Link, decision, in.OnBehalfOf)
		if err != nil {
			return r, err
		}
		opened = &l
	}
	if r.State != PendingApproval {
		return r, fmt.Errorf("%w: the request is %s", ErrConflict, r.State)
	}
	var behalf *Delegator
	if in.OnBehalfOf == "" {
		if !CanReview(r, who.Principal) {
			return r, ErrNotApprover
		}
	} else {
		d, err := s.reviewingFor(ctx, r, who, in.OnBehalfOf)
		if err != nil {
			return r, err
		}
		behalf = &d
	}
	if decision != policy.Approve && decision != policy.Deny {
		return r, &schema.ValidationError{Fields: map[string]string{"decision": "must be approve or deny"}}
	}
	if decision == policy.Deny && strings.TrimSpace(comment) == "" {
		return r, &schema.ValidationError{Fields: map[string]string{"comment": "is required to deny: say what to change"}}
	}
	// A step-up request's reviews, approvals and denials alike (open question 50), need the
	// reviewer's authentication to satisfy the step-up now; the review keeps whether it did.
	steppedUp := s.stepUp.Check(who) == ""
	if r.StepUp {
		if err := s.stepUp.Require(ctx, who, "request.review", audit.Object{Type: "request", ID: r.ID.String(), Resource: r.Target}); err != nil {
			return r, err
		}
		steppedUp = true
	}
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	if err != nil {
		return r, err
	}
	if opened != nil && (len(sets) == 0 || sets[0].Hash != opened.ChangeSet) {
		// Changed since the link was checked.
		return r, s.linkRefused(ctx, who, r, opened, &chatlink.Refusal{Code: chatlink.Changed,
			Reason: "the request changed since the card was sent: review its new content here"})
	}
	if stale, err := s.stale(ctx, sets); err != nil {
		return r, err
	} else if stale != "" {
		return s.transition(ctx, r, PendingApproval, Stale, "stale", map[string]any{"region": stale})
	}
	if changed, err := s.definitionsChanged(ctx, r, sets); err != nil {
		return r, err
	} else if changed != "" {
		return s.transition(ctx, r, PendingApproval, Stale, "stale", map[string]any{"definitions": changed})
	}
	if why := s.separationTightened(r, sets[0].Repository); why != "" {
		return s.transition(ctx, r, PendingApproval, Stale, "stale", map[string]any{"separation": why})
	}
	var next db.Request
	err = s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		// Decided under the request's row lock, on its state read again: a review and the
		// request's expiry, a cancellation or another review deciding it at the same moment are
		// taken one after the other, and the later one sees what the earlier one did.
		locked, err := q.LockRequest(ctx, r.ID)
		if err != nil {
			return err
		}
		if locked.State != PendingApproval {
			return fmt.Errorf("%w: the request is %s", ErrConflict, locked.State)
		}
		r = locked
		params := db.CreateReviewParams{
			ID: uuid.New(), RequestID: r.ID, Reviewer: who.Principal.Name, Groups: nonNil(who.Principal.Groups),
			Teams: nonNil(who.Principal.Teams), Decision: decision, Comment: comment, ChangeSetHash: sets[0].Hash,
			ReviewerEmail:    pgtype.Text{String: who.Principal.Email, Valid: true},
			ReviewerAuthTime: pgtype.Timestamptz{Time: who.AuthTime, Valid: !who.AuthTime.IsZero()},
			ReviewerAcr:      who.ACR, ReviewerAcrs: nonNil(who.ACRS), StepUp: steppedUp,
		}
		data := map[string]any{"comment": comment}
		details := map[string]any{"decision": decision, "comment": audit.Personal(comment), "changeSet": sets[0].Hash, "stepUp": steppedUp}
		if behalf != nil {
			// The delegation holds until the review is recorded: a revocation waits for it.
			d, err := q.ShareDelegation(ctx, behalf.Delegation.ID)
			if err != nil {
				return err
			}
			if !delegationOf(d).Active(s.now()) {
				return fmt.Errorf("%w on %s's behalf: the delegation was revoked or ended", ErrNotApprover, behalf.User.Name)
			}
			u := behalf.User
			params.OnBehalfOf = pgtype.Text{String: u.Name, Valid: true}
			params.DelegationID = pgtype.UUID{Bytes: d.ID, Valid: true}
			params.OnBehalfEmail = pgtype.Text{String: u.Email, Valid: true}
			params.OnBehalfGroups, params.OnBehalfTeams = nonNil(u.Groups), nonNil(s.dir.Teams(u.Groups))
			data["onBehalfOf"], data["delegation"] = u.Name, d.ID.String()
			details["onBehalfOf"], details["delegation"] = audit.Personal(u.Name), d.ID.String()
		}
		if _, err := q.CreateReview(ctx, params); err != nil {
			return err
		}
		if err := s.event(ctx, tx, r, decision, who.Principal.Name, data); err != nil {
			return err
		}
		if who.ACR != "" {
			details["acr"] = who.ACR
		}
		if len(who.ACRS) > 0 {
			details["acrs"] = who.ACRS
		}
		if err := recordRequest(ctx, tx, r, "reviewed", details); err != nil {
			return err
		}
		all, err := q.ListReviews(ctx, r.ID)
		if err != nil {
			return err
		}
		reviews := counted(r, all, sets[0].Hash)
		var rules []policy.Rule
		_ = json.Unmarshal(r.Rules, &rules)
		next = r
		switch policy.Evaluate(rules, toReviews(reviews, s.dir), contextOf(r)).State {
		case policy.Approved:
			if next, err = q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: PendingApproval, ToState: Approved}); err != nil {
				return err
			}
			if err := s.event(ctx, tx, r, "approved", "", nil); err != nil {
				return err
			}
			if err := recordRequest(ctx, tx, r, "approved", approvedDetails(sets[0].Hash, r, reviews)); err != nil {
				return err
			}
			return jobs.InsertTx(ctx, s.jobs, tx, DeliverArgs{RequestID: r.ID}, nil)
		case policy.Denied:
			if next, err = q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: PendingApproval, ToState: Rejected}); err != nil {
				return err
			}
			var rejected map[string]any
			if behalf != nil {
				rejected = map[string]any{"onBehalfOf": behalf.User.Name}
			}
			if err := s.event(ctx, tx, r, "rejected", who.Principal.Name, rejected); err != nil {
				return err
			}
			if err := recordRequest(ctx, tx, r, "rejected", map[string]any{"changeSet": sets[0].Hash}); err != nil {
				return err
			}
			return s.statusJob(ctx, tx, q, r)
		}
		return nil
	})
	return next, err
}

// Refresh makes this process see what a request committed: a worker in another process may have
// delivered it since this process last fetched the repository, and the requester's next look at
// the resource or the log should show it. With a runner, a merged request is applying, then
// applied or failed.
func (s *Service) Refresh(ctx context.Context, r db.Request) {
	switch r.State {
	case Applied, Applying:
	case Failed:
		if !s.landed(ctx, r) {
			return
		}
	default:
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
	reviews, err := s.reviews(ctx, r)
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
	ctx = acting(ctx, who)
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
	ctx = acting(ctx, who)
	r, err := s.get(ctx, id)
	if err != nil {
		return r, err
	}
	if r.State != Failed {
		return r, fmt.Errorf("%w: only a failed request can be retried; a stale one must be planned again", ErrConflict)
	}
	if s.landed(ctx, r) {
		return r, fmt.Errorf("%w: the change is in Git already and its apply failed: the next apply of its stacks reports on it", ErrConflict)
	}
	var next db.Request
	err = s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var err error
		if next, err = q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: Failed, ToState: Approved}); err != nil {
			return conflict(err, r)
		}
		if err := s.event(ctx, tx, r, "retried", who.Principal.Name, nil); err != nil {
			return err
		}
		if err := recordRequest(ctx, tx, r, "retried", nil); err != nil {
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

// transition moves a request from a state to another and records the event; in pull-request
// mode, quai/approval then follows (statusJob).
func (s *Service) transition(ctx context.Context, r db.Request, from, to, kind string, data map[string]any) (db.Request, error) {
	var next db.Request
	err := s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var err error
		if next, err = q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: from, ToState: to}); err != nil {
			return conflict(err, r)
		}
		if err := s.event(ctx, tx, r, kind, "", data); err != nil {
			return err
		}
		for _, action := range transitionAudit(to, kind) {
			if err := recordRequest(ctx, tx, r, action, transitionDetails(from, kind, data)); err != nil {
				return err
			}
		}
		if to == Stale || to == Cancelled || to == Rejected || to == Failed || to == Expired {
			return s.statusJob(ctx, tx, q, r)
		}
		return nil
	})
	if err == nil && (to == Applied || to == Failed) {
		s.delivered(ctx, s.Repository(ctx, r), to)
	}
	return next, err
}

// delivered records a delivery that ended, for the metrics.
func (s *Service) delivered(ctx context.Context, repository, outcome string) {
	mode := ""
	if repo, err := s.reg.Get(repository); err == nil {
		mode = repo.Document.Spec.Delivery.Mode
	}
	telemetry.Delivered(ctx, repository, mode, outcome)
}

// statusJob queues the publishing of quai/approval for a request delivered by pull request.
func (s *Service) statusJob(ctx context.Context, tx pgx.Tx, q *db.Queries, r db.Request) error {
	if s.jobs == nil {
		return nil
	}
	if _, err := q.GetDelivery(ctx, r.ID); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	return jobs.InsertTx(ctx, s.jobs, tx, StatusArgs{RequestID: r.ID}, nil)
}

// deliverable returns repositories.ErrNotEnforced, with what is missing, when Quai must not open
// pull requests on the repository: in pull-request mode, its target branch does not require
// quai/approval as Quai needs it (docs/design/01-repository-connections.md#health).
func (s *Service) deliverable(ctx context.Context, repo *repositories.Repo) error {
	e, err := repositories.EnforcementOf(ctx, s.store, repo)
	if err != nil {
		return fmt.Errorf("%w on %s: it cannot be verified (%v)", repositories.ErrNotEnforced, repo.Name, err)
	}
	return e.Err(repo.Name)
}

// commit is a request's commit, with the fingerprints its regions had when it was planned; with
// approvals, its message carries the Approved-by trailers.
func (s *Service) commit(ctx context.Context, r db.Request, cs db.ChangeSet, repo *repositories.Repo, approvals bool) (gitx.Commit, error) {
	changes, err := s.store.ListChanges(ctx, cs.ID)
	if err != nil {
		return gitx.Commit{}, err
	}
	var fps map[string]string
	_ = json.Unmarshal(cs.Fingerprints, &fps)
	// In direct mode the commit comes after the approvals, and carries them; a pull request's
	// commit comes before (its merge carries them).
	var approved []gitx.Trailer
	if approvals {
		if approved, err = s.approvals(ctx, r); err != nil {
			return gitx.Commit{}, err
		}
	}
	c := gitx.Commit{Branch: repo.Branch, Message: message(r, cs, approved...), Author: author(r), Fingerprints: fps}
	for _, ch := range changes {
		entries, err := plan.DecodeEntries(ch.Entries)
		if err != nil {
			return gitx.Commit{}, fmt.Errorf("the entry changes of %s: %w", ch.Path, err)
		}
		c.Changes = append(c.Changes, plan.Change{Path: ch.Path, Op: ch.Op, Before: ch.Before, After: ch.After, Entries: entries})
	}
	return c, nil
}

func conflict(err error, r db.Request) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: the request changed state meanwhile (it was %s)", ErrConflict, r.State)
	}
	return err
}

// definitionsChanged says how the definition the request was planned with changed since (its
// digest differs, or it is gone), or "". Without the set of the planned commit (planned before
// definitions were kept per commit), it cannot tell and says "".
func (s *Service) definitionsChanged(ctx context.Context, r db.Request, sets []db.ChangeSet) (string, error) {
	if len(sets) == 0 {
		return "", nil
	}
	kind, name := "type", r.TypeName
	if r.Blueprint != "" {
		kind, name = "blueprint", r.Blueprint
	}
	if name == "" {
		return "", nil
	}
	repo := sets[0].Repository
	planned, ok, err := repositories.DefinitionsAt(ctx, s.store, repo, r.DefinitionsCommit)
	if err != nil || !ok {
		return "", err
	}
	_, snap, err := s.snapshot(ctx, repo)
	if err != nil {
		return "", err
	}
	before, now := planned.Types, snap.Set.Types
	if kind == "blueprint" {
		before, now = planned.Blueprints, snap.Set.Blueprints
	}
	v, ok := now[name]
	switch {
	case !ok:
		return fmt.Sprintf("the %s %s was removed", kind, name), nil
	case v.Digest != before[name].Digest:
		return fmt.Sprintf("the %s %s changed in %.12s", kind, name, v.Commit), nil
	}
	return "", nil
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
			now, err := locate.FingerprintRef(snap.FS, p)
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
	if d, err := s.store.GetDelivery(ctx, r.ID); err == nil {
		// A request is delivered with the definitions it was planned with.
		if changed, err := s.definitionsChanged(ctx, r, sets); err != nil {
			return err
		} else if changed != "" {
			_, err = s.transition(ctx, r, r.State, Stale, "stale", map[string]any{"definitions": changed})
			return err
		}
		return s.deliverPR(ctx, job, r, d, repo)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return jobs.WithRepositoryLock(ctx, s.store.Locks, repo.Name, 5*time.Second, func(ctx context.Context) error {
		if r.State == Approved {
			if r, err = s.transition(ctx, r, Approved, Merging, "merging", nil); err != nil {
				return err
			}
		}
		// A request is delivered with the definitions it was planned with: if they changed, it
		// must be planned again.
		if changed, err := s.definitionsChanged(ctx, r, sets); err != nil {
			return err
		} else if changed != "" {
			_, err = s.transition(ctx, r, Merging, Stale, "stale", map[string]any{"definitions": changed})
			return err
		}
		commit, err := s.commit(ctx, r, cs, repo, true)
		if err != nil {
			return err
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
		// The commit that landed the change set, for the runs at it to find the request.
		if err := s.store.SetChangeSetCommit(ctx, db.SetChangeSetCommitParams{RequestID: r.ID, Repository: cs.Repository, Commit: sha}); err != nil {
			return err
		}
		// Without a runner, a committed change is final (docs/design/01: resources show In
		// repo); with one, it waits for the apply that includes the commit.
		if !repo.HasRunner() {
			_, err = s.transition(ctx, r, Merging, Applied, "committed", map[string]any{"commit": sha})
			return err
		}
		if r, err = s.transition(ctx, r, Merging, Applying, "committed", map[string]any{"commit": sha}); err != nil {
			return err
		}
		return s.settle(ctx, r)
	})
}

// message is the commit message of docs/design/05, with the extra trailers (Approved-by).
func message(r db.Request, cs db.ChangeSet, extra ...gitx.Trailer) string {
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
	return gitx.Message("quai: "+verb+" "+r.Target, r.Reason, append(trailers, extra...)...)
}

// approvals are the request's Approved-by trailers, in the order of the approvals.
func (s *Service) approvals(ctx context.Context, r db.Request) ([]gitx.Trailer, error) {
	reviews, err := s.reviews(ctx, r)
	if err != nil {
		return nil, err
	}
	var out []gitx.Trailer
	for _, v := range standing(r, reviews) {
		if v.Decision == policy.Approve {
			// A review on someone's behalf names both: "fiona (on behalf of alice)". gitx drops
			// what could end the line (gitx.TrailerValue).
			out = append(out, gitx.Trailer{Key: "Approved-by", Value: onBehalfLabel(v)})
		}
	}
	return out, nil
}

// standing keeps the reviews that count among a request's (policy.Standing): each person's latest,
// a review on someone's behalf being both people's.
func standing(r db.Request, reviews []db.Review) []db.Review {
	var out []db.Review
	for _, i := range policy.Standing(toReviews(reviews, nil), r.Requester) {
		out = append(out, reviews[i])
	}
	return out
}

func author(r db.Request) gitx.Signature {
	email := r.RequesterEmail
	if email == "" {
		email = r.Requester + "@users.quai"
	}
	return gitx.Signature{Name: r.Requester, Email: email}
}

// event adds an event to a request's timeline in tx, where notifications learn of it
// (store.RecordEvent).
func (s *Service) event(ctx context.Context, tx pgx.Tx, r db.Request, kind, actor string, data map[string]any) error {
	_, err := s.store.RecordEvent(ctx, tx, eventParams(r, kind, actor, data))
	return err
}

func eventParams(r db.Request, kind, actor string, data map[string]any) db.AddEventParams {
	raw := []byte(`{}`)
	if data != nil {
		raw, _ = json.Marshal(data)
	}
	return db.AddEventParams{RequestID: pgtype.UUID{Bytes: r.ID, Valid: true}, Target: r.Target, Kind: kind, Actor: actor, Data: raw}
}

// reviews returns the request's reviews that count (counted).
func (s *Service) reviews(ctx context.Context, r db.Request) ([]db.Review, error) {
	all, err := s.store.ListReviews(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	hash := ""
	if len(sets) > 0 {
		hash = sets[0].Hash
	}
	return counted(r, all, hash), nil
}

// counted keeps the reviews of a request that count: those of the change set hash, its current
// one ("": any), since a review is bound to the content it reviewed
// (docs/design/05-delivery-and-approvals.md#approvals-bound-to-content); and on a step-up request,
// those made with a step-up, as they recorded it when they were made.
func counted(r db.Request, all []db.Review, hash string) []db.Review {
	var out []db.Review
	for _, v := range all {
		// A voided review (the request expired) never counts again.
		if (hash == "" || v.ChangeSetHash == hash) && (!r.StepUp || v.StepUp) && !v.VoidedAt.Valid {
			out = append(out, v)
		}
	}
	return out
}

// needsStepUp reports whether an approval block's stepUp condition holds for vars. A condition
// that cannot be evaluated holds: the stricter answer.
func needsStepUp(a definitions.Approval, vars expr.Vars) bool {
	if a.StepUp == "" {
		return false
	}
	if a.StepUpCondition == nil {
		return true
	}
	ok, err := a.StepUpCondition.Test(vars)
	return ok || err != nil
}

// toReviews are reviews as the policy sees them; dir may be nil when only who reviewed matters.
func toReviews(rs []db.Review, dir *auth.Directory) []policy.Review {
	out := make([]policy.Review, len(rs))
	for i, r := range rs {
		out[i] = policy.Review{Reviewer: r.Reviewer, Email: r.ReviewerEmail, Groups: r.Groups, Teams: r.Teams, Decision: r.Decision}
		if r.OnBehalfOf != "" {
			// The delegator as the directory described them when the review was made.
			out[i].OnBehalfOf = &policy.Person{Name: r.OnBehalfOf, Email: r.OnBehalfEmail, Groups: r.OnBehalfGroups, Teams: r.OnBehalfTeams}
		}
		if r.Teams == nil && dir != nil {
			// A review from before auth.teams (migration 00019) kept only its groups: their teams
			// are what auth.teams makes of them now, each group's name without it. Set later, the
			// mapping must not leave an unmapped group the team it was.
			out[i].Teams = dir.Teams(r.Groups)
		}
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
	o := ownedObject(r.Repository, r.Type, r.Owner)
	o.Owners = r.Owners
	return o
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

// stacksOf returns the stacks the planned resources are in, sorted.
func stacksOf(snap *repositories.Snapshot, rs []plan.Planned) []string {
	var out []string
	for _, r := range rs {
		if t := snap.Definitions.Types[r.Type]; t != nil {
			if st := stack(t, r.Params); st != "" && !slices.Contains(out, st) {
				out = append(out, st)
			}
		}
	}
	slices.Sort(out)
	return out
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
