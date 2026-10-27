package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"testing/fstest"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/edit/yamledit"
	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/inventory"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// InstanceResource is one resource of a blueprint instance, as it is now.
type InstanceResource struct {
	ID       string `json:"id"`
	Resource string `json:"resource" doc:"The resource ID"`
	Exists   bool   `json:"exists" doc:"False when someone removed it by hand"`
	Status   string `json:"status,omitempty"`
}

// InstanceView is a blueprint instance, from its record in Git.
type InstanceView struct {
	Repository       string             `json:"repository"`
	Blueprint        string             `json:"blueprint"`
	BlueprintVersion string             `json:"blueprintVersion,omitempty"`
	Name             string             `json:"name"`
	Owner            string             `json:"owner"`
	Path             string             `json:"path"`
	Inputs           map[string]any     `json:"inputs"`
	Resources        []InstanceResource `json:"resources"`
}

// CommitText is a commit as git shows it: header, message and patch.
type CommitText struct {
	SHA  string `json:"sha"`
	Text string `json:"text"`
}

// RunView is a run reported by a runner.
type RunView struct {
	ID         string          `json:"id"`
	Repository string          `json:"repository"`
	Stack      string          `json:"stack"`
	Commit     string          `json:"commit"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	URL        string          `json:"url,omitempty"`
	Summary    json.RawMessage `json:"summary"`
	StartedAt  time.Time       `json:"startedAt"`
}

type logInput struct {
	Repository string `path:"repository"`
	Path       string `query:"path"`
	Limit      int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}

type commitInput struct {
	Repository string `path:"repository"`
	SHA        string `path:"sha" pattern:"^[0-9a-f]{7,64}$"`
}

type instanceInput struct {
	Repository string `path:"repository"`
	Blueprint  string `path:"blueprint"`
	Name       string `path:"name"`
}

type runInput struct {
	Authorization string `header:"Authorization" required:"true" doc:"Bearer and the repository's run token"`
	Body          struct {
		Repository string          `json:"repository"`
		Stack      string          `json:"stack"`
		Commit     string          `json:"commit" pattern:"^[0-9a-f]{7,64}$"`
		Kind       string          `json:"kind" enum:"plan,apply,sync,drift"`
		Status     string          `json:"status" enum:"running,succeeded,failed"`
		URL        string          `json:"url,omitempty" format:"uri"`
		Summary    json.RawMessage `json:"summary,omitempty"`
	}
}

type publishInput struct {
	Repository string `path:"repository"`
	Name       string `path:"name"`
	Body       struct {
		Blueprint map[string]any `json:"blueprint" doc:"The blueprint definition (quai.dev/v1beta1)"`
	}
}

type eventsInput struct {
	Limit int `query:"limit" minimum:"1" maximum:"500" default:"100"`
}

func registerHistory(a huma.API, reg *repositories.Registry, s *store.Store) {
	huma.Register(a, huma.Operation{
		OperationID: "repository-log", Method: http.MethodGet, Path: Prefix + "/repositories/{repository}/log",
		Summary: "List a repository's latest commits", Tags: []string{"Repositories"},
	}, func(ctx context.Context, in *logInput) (*out[[]CommitView], error) {
		repo, err := reg.Get(in.Repository)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		if _, err := authorize(ctx, authz.ReadInventory, authz.Object{Repository: repo.Name}); err != nil {
			return nil, err
		}
		if _, err := repo.Snapshot(ctx, false); err != nil {
			return nil, err
		}
		entries, err := repo.Mirror.Log(ctx, repo.Branch, in.Path, in.Limit)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		views := []CommitView{}
		for _, e := range entries {
			views = append(views, CommitView{SHA: e.SHA, Author: e.Author, Email: e.Email, Date: e.Date, Subject: e.Subject, Body: e.Body})
		}
		return &out[[]CommitView]{Body: views}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "repository-commit", Method: http.MethodGet, Path: Prefix + "/repositories/{repository}/commits/{sha}",
		Summary: "Show a commit", Tags: []string{"Repositories"},
	}, func(ctx context.Context, in *commitInput) (*out[CommitText], error) {
		repo, err := reg.Get(in.Repository)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		if _, err := authorize(ctx, authz.ReadInventory, authz.Object{Repository: repo.Name}); err != nil {
			return nil, err
		}
		text, err := repo.Mirror.Show(ctx, in.SHA)
		if err != nil {
			return nil, huma.Error404NotFound("no commit " + in.SHA)
		}
		return &out[CommitText]{Body: CommitText{SHA: in.SHA, Text: text}}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "list-instances", Method: http.MethodGet, Path: Prefix + "/instances",
		Summary: "List blueprint instances", Tags: []string{"Inventory"},
	}, func(ctx context.Context, _ *struct{}) (*out[[]InstanceView], error) {
		views := []InstanceView{}
		for _, repo := range reg.List() {
			snap, err := repo.Snapshot(ctx, false)
			if err != nil {
				return nil, err
			}
			for _, inst := range instances(snap) {
				if _, err := authorize(ctx, authz.ReadInventory, authz.Object{Repository: repo.Name, Owner: inst.Owner}); err == nil {
					views = append(views, inst)
				}
			}
		}
		if len(views) == 0 {
			if id, _ := auth.FromContext(ctx); id.Principal.Name == "" {
				return nil, huma.Error401Unauthorized("not signed in")
			}
		}
		return &out[[]InstanceView]{Body: views}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "get-instance", Method: http.MethodGet, Path: Prefix + "/instances/{repository}/{blueprint}/{name}",
		Summary: "Describe a blueprint instance and its resources", Tags: []string{"Inventory"},
	}, func(ctx context.Context, in *instanceInput) (*out[InstanceView], error) {
		snap, err := snapshot(ctx, reg, in.Repository)
		if err != nil {
			return nil, err
		}
		for _, inst := range instances(snap) {
			if inst.Blueprint != in.Blueprint || inst.Name != in.Name {
				continue
			}
			if _, err := authorize(ctx, authz.ReadInventory, authz.Object{Repository: in.Repository, Owner: inst.Owner}); err != nil {
				return nil, err
			}
			for i, r := range inst.Resources {
				repoName, typeName, key, err := locate.ParseID(r.Resource)
				if err != nil || repoName != snap.Repository {
					continue
				}
				if res, err := inventory.Get(snap, typeName, key); err == nil {
					inst.Resources[i].Exists, inst.Resources[i].Status = true, res.Status
				}
			}
			return &out[InstanceView]{Body: inst}, nil
		}
		return nil, huma.Error404NotFound("no instance " + in.Blueprint + "/" + in.Name)
	})

	huma.Register(a, huma.Operation{
		OperationID: "list-events", Method: http.MethodGet, Path: Prefix + "/events",
		Summary: "List what happened lately", Tags: []string{"Requests"},
	}, func(ctx context.Context, in *eventsInput) (*out[[]EventView], error) {
		if _, err := authorize(ctx, authz.ReadInventory, authz.Object{}); err != nil {
			return nil, err
		}
		evs, err := s.ListRecentEvents(ctx, int32(in.Limit)) // #nosec G115 -- bounded by the schema
		if err != nil {
			return nil, err
		}
		views := []EventView{}
		for _, e := range evs {
			views = append(views, EventView{Kind: e.Kind, Actor: e.Actor, Data: e.Data, At: e.CreatedAt.Time})
		}
		return &out[[]EventView]{Body: views}, nil
	})

	// authz: public — runners authenticate with the repository's run token, checked here.
	// The path is auth.RunsPath.
	huma.Register(a, huma.Operation{
		OperationID: "report-run", Method: http.MethodPost, Path: Prefix + "/runs",
		Summary: "Report a plan, apply, sync or drift run (runners)", Tags: []string{"Runs"}, DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *runInput) (*out[RunView], error) {
		repo, err := reg.Get(in.Body.Repository)
		if err != nil {
			return nil, huma.Error401Unauthorized("invalid run token")
		}
		token, _ := strings.CutPrefix(in.Authorization, "Bearer ")
		sum := sha256.Sum256([]byte(token))
		if repo.RunTokenSHA256 == "" || subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(repo.RunTokenSHA256)) != 1 {
			return nil, huma.Error401Unauthorized("invalid run token")
		}
		summary := in.Body.Summary
		if len(summary) == 0 {
			summary = json.RawMessage(`{}`)
		}
		r, err := s.ReportRun(ctx, db.ReportRunParams{ID: uuid.New(), Repository: repo.Name, Stack: in.Body.Stack, Commit: in.Body.Commit,
			Kind: in.Body.Kind, Status: in.Body.Status, Url: in.Body.URL, Summary: summary})
		if err != nil {
			return nil, err
		}
		return &out[RunView]{Body: runView(r)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "publish-blueprint", Method: http.MethodPut, Path: Prefix + "/repositories/{repository}/blueprints/{name}",
		Summary: "Publish a blueprint from the designer", Tags: []string{"Catalog"},
	}, func(ctx context.Context, in *publishInput) (*out[BlueprintView], error) {
		repo, err := reg.Get(in.Repository)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		me, err := authorize(ctx, authz.PublishBlueprint, authz.Object{Repository: repo.Name, CatalogItem: in.Name})
		if err != nil {
			return nil, err
		}
		return publish(ctx, repo, s, me, in.Name, in.Body.Blueprint)
	})
}

// blueprintKeys orders a published blueprint's keys as hand-written ones are.
var blueprintKeys = []string{"apiVersion", "kind", "metadata", "spec", "name", "title", "description", "icon", "category", "version",
	"owners", "tags", "inputs", "ui", "resources", "approval", "actions", "id", "type", "repository", "params", "values", "when",
	"dependsOn", "rules", "approvers", "min", "apply", "set"}

// publish validates a blueprint against the repository's types and commits it.
func publish(ctx context.Context, repo *repositories.Repo, s *store.Store, me auth.Identity, name string, doc map[string]any) (*out[BlueprintView], error) {
	if meta, _ := doc["metadata"].(map[string]any); meta == nil || meta["name"] != name {
		return nil, huma.Error422UnprocessableEntity("metadata.name must be " + name)
	}
	src, err := yamledit.Create(doc, blueprintKeys)
	if err != nil {
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	file := path.Join(definitions.BlueprintsDir, name+".yaml")
	var b *definitions.Blueprint
	err = jobs.WithRepositoryLock(ctx, s.Pool, repo.Name, time.Second, func(ctx context.Context) error {
		snap, err := repo.Snapshot(ctx, true)
		if err != nil {
			return err
		}
		overlay := overlayFS{base: snap.FS, files: fstest.MapFS{file: {Data: src}}}
		bp, problems := definitions.LoadBlueprint(overlay, file, snap.Definitions.Types)
		if len(problems) > 0 {
			details := make([]error, len(problems))
			for i, p := range problems {
				details[i] = &huma.ErrorDetail{Location: "body.blueprint." + p.Field, Message: p.Message}
			}
			return huma.Error422UnprocessableEntity("the blueprint does not load", details...)
		}
		b = bp
		fp, err := locate.Fingerprint(snap.FS, locate.Region{Strategy: locate.File, Path: file})
		if err != nil {
			return err
		}
		op := plan.OpUpdate
		if fp == locate.Absent {
			op = plan.OpCreate
		}
		email := me.Email
		if email == "" {
			email = me.Principal.Name + "@users.quai"
		}
		_, err = repo.Mirror.CommitAndPush(ctx, gitx.Commit{
			Branch: repo.Branch, Author: gitx.Signature{Name: me.Principal.Name, Email: email},
			Message: gitx.Message("quai: publish blueprint "+name, "", gitx.Trailer{Key: "Published-by", Value: me.Principal.Name + " <" + email + ">"}),
			Changes: []plan.Change{{Path: file, Op: op, After: src}}, Fingerprints: map[string]string{file: fp},
		})
		if errors.Is(err, gitx.ErrNoChange) {
			return nil
		}
		return err
	})
	var se huma.StatusError
	if errors.As(err, &se) {
		return nil, se
	}
	if err != nil {
		return nil, fmt.Errorf("publish %s: %w", name, err)
	}
	if _, err := repo.Snapshot(ctx, true); err != nil {
		return nil, err
	}
	return &out[BlueprintView]{Body: blueprintView(repo.Name, b)}, nil
}

// overlayFS is a repository's files with some replaced or added.
type overlayFS struct {
	base  fs.FS
	files fstest.MapFS
}

func (o overlayFS) Open(name string) (fs.File, error) {
	if _, ok := o.files[name]; ok {
		return o.files.Open(name)
	}
	return o.base.Open(name)
}

// instances reads the instance records of a snapshot, sorted.
func instances(snap *repositories.Snapshot) []InstanceView {
	files, _ := fs.Glob(snap.FS, plan.InstancesDir+"/*/*.yaml")
	views := []InstanceView{}
	for _, f := range files {
		src, err := fs.ReadFile(snap.FS, f)
		if err != nil {
			continue
		}
		decoded, err := yamledit.Decode(src)
		doc, _ := decoded.(map[string]any)
		if err != nil || doc == nil || doc["kind"] != "Instance" {
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		spec, _ := doc["spec"].(map[string]any)
		v := InstanceView{Repository: snap.Repository, Path: f, Resources: []InstanceResource{}, Inputs: map[string]any{}}
		v.Name, _ = meta["name"].(string)
		v.Blueprint, _ = meta["blueprint"].(string)
		v.BlueprintVersion, _ = meta["blueprintVersion"].(string)
		v.Owner, _ = meta["owner"].(string)
		if inputs, ok := spec["inputs"].(map[string]any); ok {
			v.Inputs = inputs
		}
		resources, _ := spec["resources"].(map[string]any)
		for id, rid := range resources {
			if s, ok := rid.(string); ok {
				v.Resources = append(v.Resources, InstanceResource{ID: id, Resource: s})
			}
		}
		sort.Slice(v.Resources, func(i, j int) bool { return v.Resources[i].ID < v.Resources[j].ID })
		if v.Name != "" && strings.HasPrefix(f, plan.InstancesDir+"/"+v.Blueprint+"/") {
			views = append(views, v)
		}
	}
	return views
}

func runView(r db.Run) RunView {
	return RunView{ID: r.ID.String(), Repository: r.Repository, Stack: r.Stack, Commit: r.Commit, Kind: r.Kind, Status: r.Status,
		URL: r.Url, Summary: r.Summary, StartedAt: r.StartedAt.Time}
}
