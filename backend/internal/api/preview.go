package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/repositories"
)

// PreviewView is the definitions of a branch or a pull request, as the catalog would show them
// once merged. Nothing can be requested from a preview.
type PreviewView struct {
	Repository  string        `json:"repository"`
	Branch      string        `json:"branch,omitempty"`
	PullRequest string        `json:"pullRequest,omitempty"`
	Commit      string        `json:"commit" doc:"The commit previewed: the branch's or the pull request's head"`
	Items       []CatalogItem `json:"items"`
	Problems    []ProblemView `json:"problems" doc:"What is wrong with the definitions at that commit"`
}

type previewInput struct {
	Repository  string `path:"repository"`
	Branch      string `query:"branch" doc:"The branch to preview"`
	PullRequest string `query:"pullRequest" doc:"The pull request to preview (its head), by its ID on the platform"`
}

type previewItemInput struct {
	Repository  string `path:"repository"`
	Name        string `path:"name"`
	Branch      string `query:"branch" doc:"The branch to preview"`
	PullRequest string `query:"pullRequest" doc:"The pull request to preview (its head), by its ID on the platform"`
}

func (in previewItemInput) ref() previewInput {
	return previewInput{Repository: in.Repository, Branch: in.Branch, PullRequest: in.PullRequest}
}

func registerPreview(a huma.API, reg *repositories.Registry) {
	huma.Register(a, huma.Operation{
		OperationID: "preview-definitions", Method: http.MethodGet, Path: Prefix + "/repositories/{repository}/preview",
		Summary: "Preview the definitions of a branch or a pull request", Tags: []string{"Catalog"},
		Description: "Catalog authors try types and blueprints before merging them. Nothing can be requested from a preview.",
	}, func(ctx context.Context, in *previewInput) (*out[PreviewView], error) {
		snap, err := preview(ctx, reg, *in)
		if err != nil {
			return nil, err
		}
		v := PreviewView{Repository: snap.Repository, Branch: in.Branch, PullRequest: in.PullRequest, Commit: snap.Commit,
			Items: []CatalogItem{}, Problems: []ProblemView{}}
		for _, name := range slices.Sorted(maps.Keys(snap.Definitions.Types)) {
			v.Items = append(v.Items, item("type", snap.Repository, snap.Definitions.Types[name].Metadata))
		}
		for _, name := range slices.Sorted(maps.Keys(snap.Definitions.Blueprints)) {
			v.Items = append(v.Items, item("blueprint", snap.Repository, snap.Definitions.Blueprints[name].Metadata))
		}
		for _, p := range snap.Problems {
			v.Problems = append(v.Problems, ProblemView{Path: p.Path, Field: p.Field, Message: p.Message})
		}
		return &out[PreviewView]{Body: v}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "preview-type", Method: http.MethodGet, Path: Prefix + "/repositories/{repository}/preview/types/{name}",
		Summary: "Preview a resource type from a branch or a pull request", Tags: []string{"Catalog"},
	}, func(ctx context.Context, in *previewItemInput) (*out[TypeView], error) {
		snap, err := preview(ctx, reg, in.ref())
		if err != nil {
			return nil, err
		}
		t := snap.Definitions.Types[in.Name]
		if t == nil {
			return nil, huma.Error404NotFound("no type " + in.Name + " at " + snap.Commit)
		}
		return &out[TypeView]{Body: typeView(snap, t)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "preview-blueprint", Method: http.MethodGet, Path: Prefix + "/repositories/{repository}/preview/blueprints/{name}",
		Summary: "Preview a blueprint from a branch or a pull request", Tags: []string{"Catalog"},
	}, func(ctx context.Context, in *previewItemInput) (*out[BlueprintView], error) {
		snap, err := preview(ctx, reg, in.ref())
		if err != nil {
			return nil, err
		}
		b := snap.Definitions.Blueprints[in.Name]
		if b == nil {
			return nil, huma.Error404NotFound("no blueprint " + in.Name + " at " + snap.Commit)
		}
		return &out[BlueprintView]{Body: blueprintDetail(snap, b)}, nil
	})
}

// preview loads the definitions at the ref, for someone who may preview them in the repository.
func preview(ctx context.Context, reg *repositories.Registry, in previewInput) (*repositories.Snapshot, error) {
	repo, err := reg.Get(in.Repository)
	if err != nil {
		return nil, huma.Error404NotFound(err.Error())
	}
	if _, err := authorize(ctx, authz.PreviewDefinitions, authz.Object{Repository: repo.Name}); err != nil {
		return nil, err
	}
	snap, err := repo.Preview(ctx, repositories.Ref{Branch: in.Branch, PullRequest: in.PullRequest})
	switch {
	case errors.Is(err, repositories.ErrNoRef):
		return nil, huma.Error400BadRequest(err.Error())
	case errors.Is(err, gitx.ErrNotFound), errors.Is(err, connectors.ErrNotFound):
		return nil, huma.Error404NotFound("nothing to preview: " + err.Error())
	case errors.Is(err, connectors.ErrUnsupported):
		return nil, huma.Error422UnprocessableEntity(err.Error())
	}
	return snap, err
}
