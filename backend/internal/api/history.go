package api

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"path"
	"slices"
	"testing/fstest"
	"time"

	"github.com/danielgtaylor/huma/v2"

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
)

// InstanceResource is one resource of a blueprint instance, as it is now.
type InstanceResource struct {
	ID       string `json:"id"`
	Resource string `json:"resource" doc:"The resource ID"`
	Exists   bool   `json:"exists" doc:"False when someone removed it by hand"`
	Status   string `json:"status,omitempty"`
	State    string `json:"state,omitempty" doc:"What the runs of its stack say since its last change (docs/design/06)"`
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
	CanOperate       bool               `json:"canOperate,omitempty" doc:"Whether the caller may run the blueprint's actions on the instance (on one instance only)"`
	Requests         []RequestSummary   `json:"requests,omitempty" doc:"The latest requests on the instance that the caller may read (on one instance only)"`
}

// CommitText is a commit as git shows it: header, message and patch.
type CommitText struct {
	SHA  string `json:"sha"`
	Text string `json:"text"`
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
			repo, _ := reg.Get(in.Repository)
			st := newStatuses(s)
			for i, r := range inst.Resources {
				repoName, typeName, key, err := locate.ParseID(r.Resource)
				if err != nil || repoName != snap.Repository {
					continue
				}
				if res, err := inventory.Get(snap, typeName, key); err == nil {
					inst.Resources[i].Exists, inst.Resources[i].Status = true, res.Status
					if repo != nil {
						inst.Resources[i].State, _ = st.of(ctx, repo, snap, res)
					}
				}
			}
			inst.CanOperate = allowed(ctx, authz.OperateResource, ownedObject(in.Repository, inst.Blueprint, inst.Owner))
			if inst.Requests, err = requestSummaries(ctx, s, in.Repository, inst.Blueprint+"/"+inst.Name); err != nil {
				return nil, err
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
		inst, err := plan.ReadInstance(snap.FS, f)
		if err != nil {
			continue
		}
		v := InstanceView{Repository: snap.Repository, Path: f, Name: inst.Name, Blueprint: inst.Blueprint, BlueprintVersion: inst.BlueprintVersion,
			Owner: inst.Owner, Inputs: inst.Inputs, Resources: []InstanceResource{}}
		for _, id := range slices.Sorted(maps.Keys(inst.Resources)) {
			v.Resources = append(v.Resources, InstanceResource{ID: id, Resource: inst.Resources[id]})
		}
		views = append(views, v)
	}
	return views
}
