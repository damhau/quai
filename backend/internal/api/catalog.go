package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/edit/yamledit"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/inventory"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// ProblemView is a problem with a definition.
type ProblemView struct {
	Path    string `json:"path"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// RepositoryView describes a connected repository at its head.
type RepositoryView struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Connector   string          `json:"connector"`
	ManagedBy   string          `json:"managedBy" enum:"config,api" doc:"config: declared in Quai's configuration and read-only through the API; api: managed through the API"`
	Branch      string          `json:"branch"`
	Delivery    string          `json:"delivery"`
	Commit      string          `json:"commit"`
	Types       int             `json:"types"`
	Blueprints  int             `json:"blueprints"`
	Problems    []ProblemView   `json:"problems"`
	Trust       TrustView       `json:"trust"`
	Enforcement EnforcementView `json:"enforcement"`
}

// EnforcementView is whether the platform enforces Quai's approvals on the target branch, as last
// verified.
type EnforcementView struct {
	Required  bool       `json:"required" doc:"Pull-request delivery: the target branch must require the check, or Quai opens no pull request"`
	Checked   bool       `json:"checked" doc:"False until the first verification: at registration, hourly, and when a branch policy changes"`
	Problems  []string   `json:"problems" doc:"What is missing; none when enforced"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
}

// TrustView is whether a repository's definitions only change through review, as last checked.
type TrustView struct {
	Checked   bool       `json:"checked" doc:"False until the first check, every poll interval"`
	Problems  []string   `json:"problems" doc:"What lets definitions change without review: the definitions branch unprotected, or no required reviewers on the definitions folder"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
}

// CatalogItem is something people can request: a type or a blueprint.
type CatalogItem struct {
	Kind        string   `json:"kind" enum:"type,blueprint"`
	Repository  string   `json:"repository"`
	Name        string   `json:"name"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Icon        string   `json:"icon,omitempty"`
	Category    string   `json:"category,omitempty"`
	Version     string   `json:"version,omitempty"`
	Tags        []string `json:"tags"`
	Trust       []string `json:"trust,omitempty" doc:"Why the item's definitions may change without review, shown as a warning: its repository's trust problems"`
}

// ParamView is a locator param, with the values found in the repository for discovered ones.
type ParamView struct {
	Name        string   `json:"name"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type"`
	Options     []string `json:"options,omitempty"`
	Pattern     string   `json:"pattern,omitempty"`
}

// UIView holds form hints.
type UIView struct {
	Order  []string               `json:"order"`
	Fields map[string]UIFieldView `json:"fields"`
}

// UIFieldView holds the hints of one field.
type UIFieldView struct {
	Immutable bool   `json:"immutable,omitempty"`
	VisibleIf string `json:"visibleIf,omitempty"`
}

// ActionView is a day-2 action and its form.
type ActionView struct {
	Name        string              `json:"name"`
	Title       string              `json:"title,omitempty"`
	Icon        string              `json:"icon,omitempty"`
	Description string              `json:"description,omitempty"`
	Kind        string              `json:"kind"`
	Inputs      []definitions.Input `json:"inputs"`
}

// TypeView describes a resource type for its form and its pages.
type TypeView struct {
	CatalogItem
	Path          string       `json:"path"`
	Locate        string       `json:"locate"`
	Params        []ParamView  `json:"params"`
	FormSchema    any          `json:"schema" doc:"The input schema as written"`
	PropertyOrder []string     `json:"propertyOrder"`
	UI            UIView       `json:"ui"`
	Actions       []ActionView `json:"actions"`
	Stack         string       `json:"stack,omitempty"`
	Deletion      string       `json:"deletion"`
	OwnerField    string       `json:"ownerField,omitempty"`
	ManagedFields []string     `json:"managedFields" doc:"Fields Quai sets itself (from the locator params, the labels and the owner), left out of forms"`
}

// BlueprintResourceView is one resource of a blueprint.
type BlueprintResourceView struct {
	ID         string   `json:"id"`
	Repository string   `json:"repository"`
	Type       string   `json:"type"`
	When       string   `json:"when,omitempty"`
	DependsOn  []string `json:"dependsOn"`
}

// BlueprintView describes a blueprint for its form and its page.
type BlueprintView struct {
	CatalogItem
	Path          string                  `json:"path"`
	FormSchema    any                     `json:"schema"`
	PropertyOrder []string                `json:"propertyOrder"`
	UI            UIView                  `json:"ui"`
	Resources     []BlueprintResourceView `json:"resources"`
	Actions       []ActionView            `json:"actions"`
	Definition    map[string]any          `json:"definition,omitempty" doc:"The blueprint's file as written, decoded: what the designer edits and publishes"`
}

// ResourceView is a resource in the inventory.
type ResourceView struct {
	ID         string            `json:"id"`
	Repository string            `json:"repository"`
	Type       string            `json:"type"`
	Key        string            `json:"key"`
	Path       string            `json:"path"`
	Params     map[string]string `json:"params"`
	Owner      string            `json:"owner,omitempty"`
	Stack      string            `json:"stack,omitempty"`
	Status     string            `json:"status" enum:"conforming,non-conforming,unparseable"`
	Problems   map[string]string `json:"problems,omitempty"`
	Error      string            `json:"error,omitempty"`
	LastApply  *RunView          `json:"lastApply,omitempty" doc:"The latest apply reported for the resource's stack"`
	State      string            `json:"state,omitempty" enum:"in_repo,pending_change,planning,applying,applied,failed,drifted" doc:"What the runs of its stack say since its last change (docs/design/06)"`
	StateRun   *RunView          `json:"stateRun,omitempty" doc:"The run the state comes from"`
}

// CommitView is a commit in a history.
type CommitView struct {
	SHA     string    `json:"sha"`
	Author  string    `json:"author"`
	Email   string    `json:"email"`
	Date    time.Time `json:"date"`
	Subject string    `json:"subject"`
	Body    string    `json:"body,omitempty"`
	Request int64     `json:"request,omitempty" doc:"The number of the Quai request the commit delivered (its Quai-Request trailer); none for a person's commit"`
}

// quaiRequest is the trailer of the commits Quai makes (docs/design/05-delivery-and-approvals.md).
var quaiRequest = regexp.MustCompile(`(?m)^Quai-Request: #([0-9]+)\b`)

// requestOf returns the request a commit message names in its Quai-Request trailer.
func requestOf(body string) int64 {
	if m := quaiRequest.FindStringSubmatch(body); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return n
	}
	return 0
}

// ResourceDetail is a resource with its content, history and actions.
type ResourceDetail struct {
	ResourceView
	Commit     string           `json:"commit"`
	Document   map[string]any   `json:"document,omitempty"`
	Raw        string           `json:"raw"`
	Actions    []ActionView     `json:"actions"`
	History    []CommitView     `json:"history"`
	CanOperate bool             `json:"canOperate" doc:"Whether the caller may change the resource through requests"`
	Requests   []RequestSummary `json:"requests" doc:"The latest requests on the resource that the caller may read, newest first"`
}

// RequestSummary is a request on a resource's page.
type RequestSummary struct {
	ID        string    `json:"id"`
	Number    int64     `json:"number"`
	Kind      string    `json:"kind"`
	Action    string    `json:"action,omitempty"`
	State     string    `json:"state"`
	Requester string    `json:"requester"`
	CreatedAt time.Time `json:"createdAt"`
}

type repositoryInput struct {
	Repository string `path:"repository"`
}

type itemInput struct {
	Repository string `path:"repository"`
	Name       string `path:"name"`
}

type inventoryInput struct {
	Repository string `query:"repository"`
	Type       string `query:"type"`
}

type resourceInput struct {
	ID string `query:"id" required:"true" doc:"<repository>/<type>/<key>"`
}

type out[T any] struct {
	Body T
}

func registerCatalog(a huma.API, reg *repositories.Registry, s *store.Store) {
	runs := lastApplies{s: s}
	huma.Register(a, huma.Operation{
		OperationID: "list-repositories", Method: http.MethodGet, Path: Prefix + "/repositories",
		Summary: "List the connected repositories", Tags: []string{"Repositories"},
	}, func(ctx context.Context, _ *struct{}) (*out[[]RepositoryView], error) {
		views := []RepositoryView{}
		for _, repo := range reg.List() {
			if _, err := authorize(ctx, authz.ReadInventory, authz.Object{Repository: repo.Name}); err != nil {
				continue
			}
			snap, err := repo.Snapshot(ctx, false)
			if err != nil {
				return nil, err
			}
			v := repositoryView(repo, snap)
			v.Trust, v.Enforcement = trustOf(ctx, s, repo.Name), enforcementOf(ctx, s, repo)
			views = append(views, v)
		}
		if len(views) == 0 {
			if _, err := authorize(ctx, authz.ReadInventory, authz.Object{}); err != nil {
				return nil, err
			}
		}
		return &out[[]RepositoryView]{Body: views}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "sync-repository", Method: http.MethodPost, Path: Prefix + "/repositories/{repository}/sync",
		Summary: "Fetch a repository now", Tags: []string{"Repositories"},
	}, func(ctx context.Context, in *repositoryInput) (*out[RepositoryView], error) {
		repo, err := reg.Get(in.Repository)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		if _, err := authorize(ctx, authz.ReadInventory, authz.Object{Repository: repo.Name}); err != nil {
			return nil, err
		}
		snap, err := repo.Snapshot(ctx, true)
		if err != nil {
			return nil, err
		}
		v := repositoryView(repo, snap)
		v.Trust, v.Enforcement = trustOf(ctx, s, repo.Name), enforcementOf(ctx, s, repo)
		return &out[RepositoryView]{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "list-catalog", Method: http.MethodGet, Path: Prefix + "/catalog",
		Summary: "List what you can request", Tags: []string{"Catalog"},
	}, func(ctx context.Context, _ *struct{}) (*out[[]CatalogItem], error) {
		items := []CatalogItem{}
		denied := 0
		for _, repo := range reg.List() {
			snap, err := repo.Snapshot(ctx, false)
			if err != nil {
				return nil, err
			}
			warn := trustOf(ctx, s, repo.Name).Problems
			for _, name := range slices.Sorted(maps.Keys(snap.Definitions.Types)) {
				t := snap.Definitions.Types[name]
				if t.Spec.Catalog == "hidden" {
					continue
				}
				if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: repo.Name, CatalogItem: name}); err != nil {
					denied++
					continue
				}
				it := item("type", repo.Name, t.Metadata)
				it.Trust = warn
				items = append(items, it)
			}
			for _, name := range slices.Sorted(maps.Keys(snap.Definitions.Blueprints)) {
				b := snap.Definitions.Blueprints[name]
				if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: repo.Name, CatalogItem: name}); err != nil {
					denied++
					continue
				}
				it := item("blueprint", repo.Name, b.Metadata)
				it.Trust = warn
				items = append(items, it)
			}
		}
		if len(items) == 0 && denied > 0 {
			_, err := authorize(ctx, authz.ReadCatalog, authz.Object{})
			return nil, err
		}
		return &out[[]CatalogItem]{Body: items}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "get-type", Method: http.MethodGet, Path: Prefix + "/repositories/{repository}/types/{name}",
		Summary: "Describe a resource type", Tags: []string{"Catalog"},
	}, func(ctx context.Context, in *itemInput) (*out[TypeView], error) {
		snap, err := snapshot(ctx, reg, in.Repository)
		if err != nil {
			return nil, err
		}
		if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: in.Repository, CatalogItem: in.Name}); err != nil {
			return nil, err
		}
		t := snap.Definitions.Types[in.Name]
		if t == nil {
			return nil, huma.Error404NotFound("no type " + in.Name + " in " + in.Repository)
		}
		v := typeView(snap, t)
		v.Trust = trustOf(ctx, s, in.Repository).Problems
		return &out[TypeView]{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "get-blueprint", Method: http.MethodGet, Path: Prefix + "/repositories/{repository}/blueprints/{name}",
		Summary: "Describe a blueprint", Tags: []string{"Catalog"},
	}, func(ctx context.Context, in *itemInput) (*out[BlueprintView], error) {
		snap, err := snapshot(ctx, reg, in.Repository)
		if err != nil {
			return nil, err
		}
		if _, err := authorize(ctx, authz.ReadCatalog, authz.Object{Repository: in.Repository, CatalogItem: in.Name}); err != nil {
			return nil, err
		}
		b := snap.Definitions.Blueprints[in.Name]
		if b == nil {
			return nil, huma.Error404NotFound("no blueprint " + in.Name + " in " + in.Repository)
		}
		v := blueprintDetail(snap, b)
		v.Trust = trustOf(ctx, s, in.Repository).Problems
		return &out[BlueprintView]{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "list-inventory", Method: http.MethodGet, Path: Prefix + "/inventory",
		Summary: "List resources", Tags: []string{"Inventory"},
	}, func(ctx context.Context, in *inventoryInput) (*out[[]ResourceView], error) {
		// Each resource is checked: someone may see only what they own.
		views := []ResourceView{}
		applies := map[string]*RunView{} // by stack: resources share stacks
		st := newStatuses(s)
		for _, repo := range reg.List() {
			if in.Repository != "" && repo.Name != in.Repository {
				continue
			}
			snap, err := repo.Snapshot(ctx, false)
			if err != nil {
				return nil, err
			}
			rs, err := inventory.Scan(snap)
			if err != nil {
				return nil, err
			}
			for _, r := range rs {
				if in.Type != "" && r.Type != in.Type {
					continue
				}
				if _, err := authorize(ctx, authz.ReadInventory, resourceObject(r)); err == nil {
					v := resourceView(r)
					key := r.Repository + "/" + r.Stack
					if _, ok := applies[key]; !ok {
						applies[key] = runs.get(ctx, r.Repository, r.Stack)
					}
					v.LastApply = applies[key]
					v.State, v.StateRun = st.of(ctx, repo, snap, r)
					views = append(views, v)
				}
			}
		}
		if len(views) == 0 {
			if id, _ := auth.FromContext(ctx); id.Principal.Name == "" {
				return nil, huma.Error401Unauthorized("not signed in")
			}
		}
		return &out[[]ResourceView]{Body: views}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "get-resource", Method: http.MethodGet, Path: Prefix + "/resource",
		Summary: "Describe a resource", Tags: []string{"Inventory"},
	}, func(ctx context.Context, in *resourceInput) (*out[ResourceDetail], error) {
		repoName, typeName, key, err := locate.ParseID(in.ID)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		snap, err := snapshot(ctx, reg, repoName)
		if err != nil {
			return nil, err
		}
		r, err := inventory.Get(snap, typeName, key)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, huma.Error404NotFound("no resource " + in.ID)
		}
		if err != nil {
			return nil, err
		}
		if _, err := authorize(ctx, authz.ReadInventory, resourceObject(r)); err != nil {
			return nil, err
		}
		repo, _ := reg.Get(repoName)
		d := resourceDetail(ctx, repo, snap, r)
		d.LastApply = runs.get(ctx, r.Repository, r.Stack)
		d.State, d.StateRun = newStatuses(s).of(ctx, repo, snap, r)
		d.CanOperate = allowed(ctx, authz.OperateResource, resourceObject(r))
		if d.Requests, err = requestSummaries(ctx, s, r.Repository, r.ID); err != nil {
			return nil, err
		}
		return &out[ResourceDetail]{Body: d}, nil
	})
}

// lastApplies looks up the latest apply reported for a stack.
type lastApplies struct{ s *store.Store }

func (l lastApplies) get(ctx context.Context, repo, stack string) *RunView {
	if l.s == nil || stack == "" {
		return nil
	}
	r, err := l.s.LatestRun(ctx, db.LatestRunParams{Repository: repo, Stack: stack, Kind: "apply"})
	if err != nil {
		return nil
	}
	v := runView(r)
	return &v
}

func snapshot(ctx context.Context, reg *repositories.Registry, name string) (*repositories.Snapshot, error) {
	repo, err := reg.Get(name)
	if err != nil {
		return nil, huma.Error404NotFound(err.Error())
	}
	return repo.Snapshot(ctx, false)
}

// enforcementOf returns the repository's last enforcement verification; unchecked before the
// first, or without a database.
func enforcementOf(ctx context.Context, s *store.Store, repo *repositories.Repo) EnforcementView {
	v := EnforcementView{Required: repo.Document.Spec.Delivery.Mode == "pull-request", Problems: []string{}}
	if s == nil {
		return v
	}
	row, err := s.GetRepositorySync(ctx, repo.Name)
	if err != nil || !row.EnforcementCheckedAt.Valid {
		return v
	}
	_ = json.Unmarshal(row.EnforcementProblems, &v.Problems)
	if v.Problems == nil {
		v.Problems = []string{}
	}
	v.Checked, v.CheckedAt = true, &row.EnforcementCheckedAt.Time
	return v
}

// trustOf returns the repository's last trust check; unchecked before the first poll, or without a
// database.
func trustOf(ctx context.Context, s *store.Store, repository string) TrustView {
	v := TrustView{Problems: []string{}}
	if s == nil {
		return v
	}
	row, err := s.GetRepositorySync(ctx, repository)
	if err != nil || !row.TrustCheckedAt.Valid {
		return v
	}
	_ = json.Unmarshal(row.TrustProblems, &v.Problems)
	if v.Problems == nil {
		v.Problems = []string{}
	}
	v.Checked, v.CheckedAt = true, &row.TrustCheckedAt.Time
	return v
}

func repositoryView(repo *repositories.Repo, snap *repositories.Snapshot) RepositoryView {
	v := RepositoryView{Name: repo.Name, Title: repo.Title, Connector: repo.Document.Spec.Connector, ManagedBy: repo.ManagedBy,
		Branch: repo.Branch, Delivery: repo.Delivery, Commit: snap.Commit,
		Types: len(snap.Definitions.Types), Blueprints: len(snap.Definitions.Blueprints), Problems: []ProblemView{}, Trust: TrustView{Problems: []string{}}}
	for _, p := range snap.Problems {
		v.Problems = append(v.Problems, ProblemView{Path: p.Path, Field: p.Field, Message: p.Message})
	}
	return v
}

func item(kind, repo string, m definitions.Metadata) CatalogItem {
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	return CatalogItem{Kind: kind, Repository: repo, Name: m.Name, Title: m.Title, Description: m.Description,
		Icon: m.Icon, Category: m.Category, Version: m.Version, Tags: tags}
}

func uiView(ui definitions.UI) UIView {
	v := UIView{Order: ui.Order, Fields: map[string]UIFieldView{}}
	if v.Order == nil {
		v.Order = []string{}
	}
	for k, f := range ui.Fields {
		v.Fields[k] = UIFieldView{Immutable: f.Immutable, VisibleIf: f.VisibleIf}
	}
	return v
}

func actionViews(actions []definitions.Action) []ActionView {
	out := []ActionView{}
	for _, a := range actions {
		kind := a.Kind
		if kind == "" {
			kind = definitions.ActionDeclarative
		}
		inputs := a.Inputs
		if inputs == nil {
			inputs = []definitions.Input{}
		}
		out = append(out, ActionView{Name: a.Name, Title: a.Title, Icon: a.Icon, Description: a.Description, Kind: kind, Inputs: inputs})
	}
	return out
}

func typeView(snap *repositories.Snapshot, t *definitions.ResourceType) TypeView {
	v := TypeView{
		CatalogItem: item("type", snap.Repository, t.Metadata), Path: t.Path, Locate: t.Spec.Locate.Path,
		FormSchema: t.InputSchema.Document(), PropertyOrder: t.InputSchema.PropertyOrder(), UI: uiView(t.Spec.UI),
		Actions: actionViews(t.Spec.Actions), Stack: t.Spec.Stack, Deletion: t.Spec.Deletion, Params: []ParamView{},
	}
	if v.Deletion == "" {
		v.Deletion = "approval"
	}
	if o := t.Spec.Ownership; o != nil {
		v.OwnerField = o.Field
	}
	v.ManagedFields = slices.Concat(slices.Collect(maps.Values(t.Spec.ParamFields)), slices.Collect(maps.Keys(t.Spec.Labels)))
	if v.OwnerField != "" {
		v.ManagedFields = append(v.ManagedFields, v.OwnerField)
	}
	slices.Sort(v.ManagedFields)
	v.ManagedFields = slices.Compact(v.ManagedFields)
	for _, p := range t.Spec.Params {
		pv := ParamView{Name: p.Name, Title: p.Title, Description: p.Description, Type: p.Type, Options: p.Options, Pattern: p.Pattern}
		if pv.Type == "" {
			pv.Type = "string"
		}
		if p.Discover {
			pv.Options = inventory.Discover(snap, t, p.Name)
		}
		v.Params = append(v.Params, pv)
	}
	return v
}

// blueprintDetail is a blueprint's view with its file as written.
func blueprintDetail(snap *repositories.Snapshot, b *definitions.Blueprint) BlueprintView {
	v := blueprintView(snap.Repository, b)
	if src, err := fs.ReadFile(snap.FS, b.Path); err == nil {
		if doc, err := yamledit.Decode(src); err == nil {
			v.Definition, _ = doc.(map[string]any)
		}
	}
	return v
}

func blueprintView(repo string, b *definitions.Blueprint) BlueprintView {
	v := BlueprintView{CatalogItem: item("blueprint", repo, b.Metadata), Path: b.Path, FormSchema: b.InputSchema.Document(),
		PropertyOrder: b.InputSchema.PropertyOrder(), UI: uiView(b.Spec.UI), Resources: []BlueprintResourceView{}, Actions: []ActionView{}}
	for _, r := range b.Spec.Resources {
		rv := BlueprintResourceView{ID: r.ID, Repository: r.Type.Repository, Type: r.Type.Name, When: r.When, DependsOn: r.DependsOn}
		if rv.Repository == "" {
			rv.Repository = repo
		}
		if rv.DependsOn == nil {
			rv.DependsOn = []string{}
		}
		v.Resources = append(v.Resources, rv)
	}
	for _, a := range b.Spec.Actions {
		inputs := a.Inputs
		if inputs == nil {
			inputs = []definitions.Input{}
		}
		v.Actions = append(v.Actions, ActionView{Name: a.Name, Title: a.Title, Icon: a.Icon, Description: a.Description, Kind: definitions.ActionDeclarative, Inputs: inputs})
	}
	return v
}

func resourceObject(r inventory.Resource) authz.Object {
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

// requestSummaries returns the latest requests on a target that the caller may read.
func requestSummaries(ctx context.Context, s *store.Store, repository, target string) ([]RequestSummary, error) {
	out := []RequestSummary{}
	if s == nil {
		return out, nil
	}
	rs, err := s.ListRequests(ctx, db.ListRequestsParams{States: []string{}, Target: target, MaxRows: 20})
	if err != nil {
		return nil, err
	}
	for _, q := range rs {
		if allowed(ctx, authz.ReadRequest, authz.Object{Repository: repository, Requester: q.Requester}) {
			out = append(out, RequestSummary{ID: q.ID.String(), Number: q.Number, Kind: q.Kind, Action: q.Action,
				State: q.State, Requester: q.Requester, CreatedAt: q.CreatedAt.Time})
		}
	}
	return out, nil
}

func resourceView(r inventory.Resource) ResourceView {
	params := r.Params
	if params == nil {
		params = map[string]string{}
	}
	return ResourceView{ID: r.ID, Repository: r.Repository, Type: r.Type, Key: r.Key, Path: r.Path, Params: params,
		Owner: r.Owner, Stack: r.Stack, Status: r.Status, Problems: r.Problems, Error: r.Error}
}

func resourceDetail(ctx context.Context, repo *repositories.Repo, snap *repositories.Snapshot, r inventory.Resource) ResourceDetail {
	d := ResourceDetail{ResourceView: resourceView(r), Commit: snap.Commit, Document: r.Document, Actions: []ActionView{}, History: []CommitView{}}
	if raw, err := fs.ReadFile(snap.FS, r.Path); err == nil {
		d.Raw = string(raw)
	}
	t := snap.Definitions.Types[r.Type]
	vars := expr.Vars{Inputs: r.Document, Params: anyParams(r.Params)}
	for _, a := range t.Spec.Actions {
		if a.Available != nil {
			if ok, err := a.Available.Test(vars); err != nil || !ok {
				continue
			}
		}
		d.Actions = append(d.Actions, actionViews([]definitions.Action{a})...)
	}
	if entries, err := repo.Mirror.Log(ctx, repo.Branch, r.Path, 30); err == nil {
		for _, e := range entries {
			d.History = append(d.History, CommitView{SHA: e.SHA, Author: e.Author, Email: e.Email, Date: e.Date, Subject: e.Subject, Body: e.Body, Request: requestOf(e.Body)})
		}
	}
	return d
}

func anyParams(p map[string]string) map[string]any {
	out := make(map[string]any, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}
