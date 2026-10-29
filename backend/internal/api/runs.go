package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/engine/tfplan"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/runs"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// RunSummary counts a plan's or an apply's resource changes.
type RunSummary struct {
	Add     int `json:"add" minimum:"0"`
	Change  int `json:"change" minimum:"0"`
	Destroy int `json:"destroy" minimum:"0"`
}

// RunPolicy is one policy check's result in a run.
type RunPolicy struct {
	Name    string `json:"name" minLength:"1"`
	Result  string `json:"result" enum:"passed,warning,failed"`
	Message string `json:"message,omitempty"`
}

// RunCost is a run's cost estimate.
type RunCost struct {
	Currency     string  `json:"currency" pattern:"^[A-Z]{3}$"`
	MonthlyDelta float64 `json:"monthlyDelta"`
}

// RunPlan is the plan a run report carries.
type RunPlan struct {
	Format  string          `json:"format" enum:"terraform-json"`
	Ref     string          `json:"ref,omitempty" doc:"Where the runner keeps the plan, such as artifact://plan.json"`
	Content json.RawMessage `json:"content,omitempty" doc:"The output of terraform show -json. Quai keeps only what it changes, by resource (docs/design/06)"`
}

// PlanValue is an attribute's value before and after a change.
type PlanValue struct {
	Before  any  `json:"before,omitempty"`
	After   any  `json:"after,omitempty"`
	Unknown bool `json:"unknown,omitempty" doc:"The new value is known only after the apply"`
}

// PlanChange is what a plan does to one Terraform resource.
type PlanChange struct {
	Address    string               `json:"address"`
	Action     string               `json:"action" enum:"create,update,delete,replace"`
	Attributes []string             `json:"attributes,omitempty" doc:"The attributes an update or a replace changes, as dotted paths"`
	Values     map[string]PlanValue `json:"values,omitempty" doc:"Their values, when the repository keeps them (spec.runner.planValues); never sensitive ones"`
}

// PlanResource is a Quai resource and what the plan changes of it.
type PlanResource struct {
	Resource string       `json:"resource" doc:"The resource ID"`
	Type     string       `json:"type"`
	Key      string       `json:"key"`
	Changes  []PlanChange `json:"changes"`
}

// PlanAmbiguous is a change that several resources claim.
type PlanAmbiguous struct {
	PlanChange
	Resources []string `json:"resources" doc:"The resource IDs that claim it"`
}

// RunPlanView is what a run's plan changes, by resource.
type RunPlanView struct {
	Resources []PlanResource  `json:"resources"`
	Unmapped  []PlanChange    `json:"unmapped,omitempty" doc:"Changes to Terraform resources no type claims"`
	Ambiguous []PlanAmbiguous `json:"ambiguous,omitempty" doc:"Changes claimed by more than one resource"`
}

// RunView is a run reported by a runner.
type RunView struct {
	ID            string       `json:"id"`
	Repository    string       `json:"repository"`
	Stack         string       `json:"stack"`
	Commit        string       `json:"commit"`
	PullRequest   string       `json:"pullRequest,omitempty"`
	Request       string       `json:"request,omitempty" doc:"The request the run belongs to: its pull request's, or the one whose merge commit it ran at"`
	Kind          string       `json:"kind"`
	Status        string       `json:"status"`
	URL           string       `json:"url,omitempty"`
	Summary       *RunSummary  `json:"summary,omitempty"`
	Policies      []RunPolicy  `json:"policies,omitempty"`
	Cost          *RunCost     `json:"cost,omitempty"`
	Plan          *RunPlanView `json:"plan,omitempty" doc:"What the plan changes, by resource, when the report carried it"`
	StartedAt     time.Time    `json:"startedAt"`
	FinishedAt    *time.Time   `json:"finishedAt,omitempty"`
	RequestNumber int64        `json:"requestNumber,omitempty"`
}

type runInput struct {
	Authorization string `header:"Authorization" required:"true" doc:"Bearer and the repository's run token"`
	Body          struct {
		Repository  string      `json:"repository"`
		Stack       string      `json:"stack"`
		Commit      string      `json:"commit" pattern:"^[0-9a-f]{7,64}$"`
		PullRequest string      `json:"pullRequest,omitempty" doc:"The pull request the run is for, as the platform numbers it"`
		Kind        string      `json:"kind" enum:"plan,apply,sync,drift-check"`
		Status      string      `json:"status" enum:"running,succeeded,failed"`
		StartedAt   *time.Time  `json:"startedAt,omitempty"`
		FinishedAt  *time.Time  `json:"finishedAt,omitempty"`
		URL         string      `json:"url,omitempty" format:"uri"`
		Summary     *RunSummary `json:"summary,omitempty"`
		Policies    []RunPolicy `json:"policies,omitempty"`
		Cost        *RunCost    `json:"cost,omitempty"`
		Plan        *RunPlan    `json:"plan,omitempty"`
	}
}

type driftInput struct {
	Repository string `path:"name"`
	Body       struct {
		Stack string `json:"stack,omitempty" doc:"One stack to check; every stack of the drift-check pipeline by default"`
	}
}

type runsInput struct {
	Repository string `path:"name"`
	Stack      string `query:"stack" doc:"Only this stack's runs"`
	Kind       string `query:"kind" enum:"plan,apply,sync,drift-check," doc:"Only runs of this kind"`
	Limit      int    `query:"limit" minimum:"1" maximum:"500" default:"100"`
}

func registerRuns(a huma.API, reg *repositories.Registry, s *store.Store, svc *requests.Service, log *slog.Logger) {
	// authz: public — runners authenticate with the repository's run token, checked here.
	// The path is auth.RunsPath.
	huma.Register(a, huma.Operation{
		OperationID: "report-run", Method: http.MethodPost, Path: Prefix + "/runs",
		Summary: "Report a plan, apply, sync or drift check (runners)", Tags: []string{"Runs"}, DefaultStatus: http.StatusAccepted,
		MaxBodyBytes: maxRunReport,
		Description: "A report continues the running run of the same repository, stack and kind that has the same url or, when " +
			"either has none, the same commit and pull request; otherwise it is a new run.",
	}, func(ctx context.Context, in *runInput) (*out[RunView], error) {
		repo, err := runTokenRepo(ctx, reg, in.Body.Repository, in.Authorization)
		if err != nil {
			return nil, err
		}
		if s == nil {
			return nil, huma.Error503ServiceUnavailable("no database")
		}
		r, changed, err := report(ctx, s, repo, in)
		if err != nil {
			return nil, err
		}
		// A plan goes on its request's timeline and decides its rules on the plan; an apply
		// settles the requests waiting for it. The same report again changes nothing.
		if svc != nil && changed {
			if err := svc.RunReported(ctx, r); err != nil && log != nil {
				log.ErrorContext(ctx, "moving requests on after a run", slog.String("run", r.ID.String()), slog.Any("error", err))
			}
		}
		return &out[RunView]{Body: runView(r)}, nil
	})

	// authz: public — Flux authenticates with the repository's run token, checked here.
	// The path is auth.FluxRunsPath.
	huma.Register(a, huma.Operation{
		OperationID: "report-flux-event", Method: http.MethodPost, Path: Prefix + "/runs/flux",
		Summary: "Report a Flux reconciliation as a sync run (Flux's notification controller, generic provider)", Tags: []string{"Runs"},
		DefaultStatus: http.StatusAccepted, MaxBodyBytes: 1 << 20,
		Description: "The event of a Flux object (involvedObject, severity, reason, metadata.revision), as the generic provider posts it: " +
			"an error is a failed sync, Progressing a running one, anything else a succeeded one, of the stack named by the query or the object (<namespace>/<name>), " +
			"at the commit of its revision. The events of one object and revision are one run.",
	}, func(ctx context.Context, in *fluxInput) (*out[RunView], error) {
		repo, err := runTokenRepo(ctx, reg, in.Repository, in.Authorization)
		if err != nil {
			return nil, err
		}
		if s == nil {
			return nil, huma.Error503ServiceUnavailable("no database")
		}
		var e fluxEvent
		if err := json.Unmarshal(in.Body, &e); err != nil {
			return nil, huma.Error422UnprocessableEntity("not a Flux event: " + err.Error())
		}
		r, err := e.report(in.Stack)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		run, changed, err := runs.Record(ctx, s, repo, r)
		if err != nil {
			return nil, err
		}
		if svc != nil && changed {
			if err := svc.RunReported(ctx, run); err != nil && log != nil {
				log.ErrorContext(ctx, "moving requests on after a run", slog.String("run", run.ID.String()), slog.Any("error", err))
			}
		}
		return &out[RunView]{Body: runView(run)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "check-drift", Method: http.MethodPost, Path: Prefix + "/repositories/{name}/drift-checks",
		Summary: "Queue drift checks of a repository's stacks now (repository admins)", Tags: []string{"Runs"}, DefaultStatus: http.StatusAccepted,
		Description: "Triggers the drift-check pipeline of spec.runner.pipelines for each stack, or the one given, as the daily schedule does (docs/design/06-runs-status-and-drift.md#drift).",
	}, func(ctx context.Context, in *driftInput) (*out[[]RunView], error) {
		repo, err := reg.Get(in.Repository)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		if _, err := authorize(ctx, authz.ManageRepository, authz.Object{Repository: repo.Name}); err != nil {
			return nil, err
		}
		if s == nil {
			return nil, huma.Error503ServiceUnavailable("no database")
		}
		stacks, err := runs.DriftStacks(ctx, repo)
		if errors.Is(err, runs.ErrNoDriftCheck) {
			return nil, huma.Error409Conflict(repo.Name + " has no drift-check pipeline in spec.runner.pipelines, or its platform cannot trigger it")
		}
		if err != nil {
			return nil, err
		}
		if in.Body.Stack != "" {
			if !slices.Contains(stacks, in.Body.Stack) {
				return nil, huma.Error422UnprocessableEntity("the drift-check pipeline does not cover "+in.Body.Stack, &huma.ErrorDetail{Location: "body.stack", Message: "is not one of " + strings.Join(stacks, ", ")})
			}
			stacks = []string{in.Body.Stack}
		}
		views := []RunView{}
		for _, stack := range stacks {
			run, err := runs.QueueDrift(ctx, s, repo, stack, time.Now())
			if err != nil {
				return nil, huma.Error502BadGateway(err.Error())
			}
			views = append(views, runView(run))
		}
		return &out[[]RunView]{Body: views}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "list-runs", Method: http.MethodGet, Path: Prefix + "/repositories/{name}/runs",
		Summary: "List a repository's runs, newest first", Tags: []string{"Runs"},
	}, func(ctx context.Context, in *runsInput) (*out[[]RunView], error) {
		repo, err := reg.Get(in.Repository)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		if _, err := authorize(ctx, authz.ReadInventory, authz.Object{Repository: repo.Name}); err != nil {
			return nil, err
		}
		views := []RunView{}
		if s == nil {
			return &out[[]RunView]{Body: views}, nil
		}
		runs, err := s.ListRuns(ctx, db.ListRunsParams{Repository: repo.Name, Stack: in.Stack, Kind: in.Kind, MaxRows: int32(in.Limit)}) // #nosec G115 -- bounded by the schema
		if err != nil {
			return nil, err
		}
		numbers := map[uuid.UUID]int64{}
		for _, r := range runs {
			v := runView(r)
			if r.RequestID.Valid {
				id := uuid.UUID(r.RequestID.Bytes)
				if _, ok := numbers[id]; !ok {
					if req, err := s.GetRequest(ctx, id); err == nil {
						numbers[id] = req.Number
					}
				}
				v.RequestNumber = numbers[id]
			}
			views = append(views, v)
		}
		return &out[[]RunView]{Body: views}, nil
	})
}

// runTokenRepo returns the repository a run token, sent as authorization, is the token of.
func runTokenRepo(ctx context.Context, reg *repositories.Registry, name, authorization string) (*repositories.Repo, error) {
	repo, err := reg.Get(name)
	if err != nil {
		return nil, huma.Error401Unauthorized("invalid run token")
	}
	token, _ := strings.CutPrefix(authorization, "Bearer ")
	want, ok, err := repo.RunToken(ctx)
	if err != nil {
		return nil, err
	}
	// The secret holds the token's SHA-256 in hex (docs/design/06-runs-status-and-drift.md),
	// never the token: what is sent is hashed and compared in constant time.
	sent := sha256.Sum256([]byte(token))
	if !ok || subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sent[:])), []byte(strings.ToLower(strings.TrimSpace(want.Reveal())))) != 1 {
		return nil, huma.Error401Unauthorized("invalid run token")
	}
	return repo, nil
}

type fluxInput struct {
	Authorization string          `header:"Authorization" required:"true" doc:"Bearer and the repository's run token"`
	Repository    string          `query:"repository" required:"true" doc:"The repository's name in Quai"`
	Stack         string          `query:"stack" doc:"The stack; the event's object (<namespace>/<name>) otherwise"`
	Body          json.RawMessage `doc:"The Flux event (event.Event): involvedObject, severity, reason, metadata.revision; other fields are ignored"`
}

// fluxEvent is what Flux's notification controller posts (event.Event), the fields Quai reads.
type fluxEvent struct {
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"involvedObject"`
	Severity  string            `json:"severity"`
	Timestamp time.Time         `json:"timestamp"`
	Reason    string            `json:"reason"`
	Metadata  map[string]string `json:"metadata"`
}

// fluxRevision is the commit of a Flux revision: main@sha1:<commit>, sha1:<commit> or <commit>.
var fluxRevision = regexp.MustCompile(`(?:^|[@:])([0-9a-f]{7,64})$`)

// report is the sync run a Flux event says.
func (e fluxEvent) report(stack string) (runs.Report, error) {
	o := e.InvolvedObject
	if o.Name == "" {
		return runs.Report{}, fmt.Errorf("the event names no object (involvedObject)")
	}
	m := fluxRevision.FindStringSubmatch(e.Metadata["revision"])
	if m == nil {
		return runs.Report{}, fmt.Errorf("the event's revision %q names no commit", e.Metadata["revision"])
	}
	if stack == "" {
		stack = o.Namespace + "/" + o.Name
	}
	r := runs.Report{Stack: stack, Commit: m[1], Kind: "sync", Status: "succeeded",
		URL: "flux://" + o.Namespace + "/" + strings.ToLower(o.Kind) + "/" + o.Name + "?revision=" + m[1]}
	at := e.Timestamp
	switch {
	case e.Severity == "error":
		r.Status = "failed"
	case strings.Contains(e.Reason, "Progressing"):
		r.Status = "running"
	}
	if !at.IsZero() {
		if r.Status == "running" {
			r.StartedAt = &at
		} else {
			r.FinishedAt = &at
		}
	}
	return r, nil
}

// maxRunReport bounds a run report, plan included.
const maxRunReport = 64 << 20

// report records a run report through runs.Record.
func report(ctx context.Context, s *store.Store, repo *repositories.Repo, in *runInput) (db.Run, bool, error) {
	b := in.Body
	r := runs.Report{Stack: b.Stack, Commit: b.Commit, PullRequest: b.PullRequest, Kind: b.Kind, Status: b.Status, URL: b.URL,
		StartedAt: b.StartedAt, FinishedAt: b.FinishedAt}
	if b.Summary != nil {
		r.Summary = &runs.Summary{Add: b.Summary.Add, Change: b.Summary.Change, Destroy: b.Summary.Destroy}
	}
	for _, p := range b.Policies {
		r.Policies = append(r.Policies, runs.Policy{Name: p.Name, Result: p.Result, Message: p.Message})
	}
	if b.Cost != nil {
		r.Cost = &runs.Cost{Currency: b.Cost.Currency, MonthlyDelta: b.Cost.MonthlyDelta}
	}
	if b.Plan != nil {
		r.Plan, r.PlanFormat, r.PlanRef = b.Plan.Content, b.Plan.Format, b.Plan.Ref
	}
	run, changed, err := runs.Record(ctx, s, repo, r)
	var pe *runs.PlanError
	if errors.As(err, &pe) {
		return db.Run{}, false, huma.Error422UnprocessableEntity(pe.Error(), &huma.ErrorDetail{Location: "body." + pe.Field, Message: pe.Error()})
	}
	return run, changed, err
}

func runView(r db.Run) RunView {
	v := RunView{ID: r.ID.String(), Repository: r.Repository, Stack: r.Stack, Commit: r.Commit, PullRequest: r.PullRequest, Kind: r.Kind,
		Status: r.Status, URL: r.Url, StartedAt: r.StartedAt.Time}
	if r.RequestID.Valid {
		v.Request = uuid.UUID(r.RequestID.Bytes).String()
	}
	if r.FinishedAt.Valid {
		t := r.FinishedAt.Time
		v.FinishedAt = &t
	}
	var summary RunSummary
	if string(r.Summary) != "{}" && json.Unmarshal(r.Summary, &summary) == nil {
		v.Summary = &summary
	}
	_ = json.Unmarshal(r.Policies, &v.Policies)
	var cost RunCost
	if string(r.Cost) != "{}" && json.Unmarshal(r.Cost, &cost) == nil {
		v.Cost = &cost
	}
	var sp runs.Plan
	if string(r.Plan) != "{}" && json.Unmarshal(r.Plan, &sp) == nil {
		v.Plan = runPlanView(r.Repository, sp.Mapping)
	}
	return v
}

func runPlanView(repository string, m tfplan.Mapping) *RunPlanView {
	v := &RunPlanView{Resources: []PlanResource{}}
	for _, r := range m.Resources {
		v.Resources = append(v.Resources, PlanResource{Resource: locate.ID(repository, r.Type, r.Key), Type: r.Type, Key: r.Key, Changes: planChanges(r.Changes)})
	}
	v.Unmapped = planChanges(m.Unmapped)
	for _, a := range m.Ambiguous {
		pa := PlanAmbiguous{PlanChange: planChanges([]tfplan.Change{a.Change})[0]}
		for _, id := range a.Resources {
			typeName, key, _ := strings.Cut(id, "/")
			pa.Resources = append(pa.Resources, locate.ID(repository, typeName, key))
		}
		v.Ambiguous = append(v.Ambiguous, pa)
	}
	return v
}

func planChanges(changes []tfplan.Change) []PlanChange {
	var out []PlanChange
	for _, c := range changes {
		pc := PlanChange{Address: c.Address, Action: c.Action, Attributes: c.Attributes}
		for attr, val := range c.Values {
			if pc.Values == nil {
				pc.Values = map[string]PlanValue{}
			}
			pc.Values[attr] = PlanValue{Before: val.Before, After: val.After, Unknown: val.Unknown}
		}
		out = append(out, pc)
	}
	return out
}
