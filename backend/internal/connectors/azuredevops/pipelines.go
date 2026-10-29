package azuredevops

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/damhau/quai/backend/internal/connectors"
)

// buildPolicy is the type of the "Build validation" branch policy.
const buildPolicy = "0609b952-1397-4640-95ec-e00a01b2c241"

// maxArtifact bounds an artifact Quai downloads, zipped.
const maxArtifact = 64 << 20

type pipelines Connector

type build struct {
	ID         int    `json:"id"`
	Status     string `json:"status"` // notStarted, inProgress, completed, cancelling, postponed
	Result     string `json:"result"` // succeeded, partiallySucceeded, failed, canceled
	SourceVer  string `json:"sourceVersion"`
	Branch     string `json:"sourceBranch"`
	StartTime  string `json:"startTime"`
	FinishTime string `json:"finishTime"`
	Definition struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"definition"`
	Repository struct {
		ID string `json:"id"`
	} `json:"repository"`
	TriggerInfo map[string]string `json:"triggerInfo"`
	Parameters  map[string]string `json:"templateParameters"`
}

// Get reads a run through the Builds API: the run of a YAML pipeline is a build. A run of
// another repository's pipeline is ErrNotFound.
func (p *pipelines) Get(ctx context.Context, id string) (connectors.Run, error) {
	c := (*Connector)(p)
	repo, err := c.Resolve(ctx)
	if err != nil {
		return connectors.Run{}, err
	}
	if _, err := strconv.Atoi(id); err != nil {
		return connectors.Run{}, fmt.Errorf("azure devops: run %q: %w", id, connectors.ErrNotFound)
	}
	var b build
	if err := c.client.Do(ctx, http.MethodGet, path(repo.ProjectID, "_apis", "build", "builds", id), APIVersion, nil, nil, &b); err != nil {
		return connectors.Run{}, err
	}
	if !strings.EqualFold(b.Repository.ID, repo.ID) {
		return connectors.Run{}, fmt.Errorf("azure devops: run %s is of another repository: %w", id, connectors.ErrNotFound)
	}
	return c.run(repo, b), nil
}

func (c *Connector) run(repo Repository, b build) connectors.Run {
	r := connectors.Run{Pipeline: b.Definition.Name, ID: strconv.Itoa(b.ID), Commit: b.SourceVer, Branch: b.Branch, Result: b.Result,
		PullRequest: b.TriggerInfo["pr.number"], URL: RunURL(c.loc.Collection, repo.ProjectName, b.ID), Parameters: b.Parameters}
	switch b.Status {
	case "completed":
		r.State = "completed"
	case "inProgress", "cancelling":
		r.State = "running"
	default:
		r.State = "queued"
	}
	if r.PullRequest == "" {
		// refs/pull/<id>/merge: the merge a build validation runs on.
		if rest, ok := strings.CutPrefix(b.Branch, "refs/pull/"); ok {
			r.PullRequest, _, _ = strings.Cut(rest, "/")
		}
	}
	r.Started, _ = time.Parse(time.RFC3339, b.StartTime)
	r.Finished, _ = time.Parse(time.RFC3339, b.FinishTime)
	return r
}

// RunURL is a run's results page, as `quai report` names it from the pipeline's variables
// (System.CollectionUri, System.TeamProject, Build.BuildId): both describe the same run.
func RunURL(collection, project string, id int) string {
	return strings.TrimSuffix(collection, "/") + "/" + url.PathEscape(project) + "/_build/results?buildId=" + strconv.Itoa(id)
}

// find returns the ID of the pipeline named pipeline in the repository's project.
func (p *pipelines) find(ctx context.Context, repo Repository, pipeline string) (int, error) {
	c := (*Connector)(p)
	var list struct {
		Value []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"value"`
	}
	if err := c.client.Do(ctx, http.MethodGet, path(repo.ProjectID, "_apis", "pipelines"), APIVersion, url.Values{"$top": {"1000"}}, nil, &list); err != nil {
		return 0, err
	}
	for _, v := range list.Value {
		if v.Name == pipeline {
			return v.ID, nil
		}
	}
	return 0, fmt.Errorf("azure devops: no pipeline %q in the project: %w", pipeline, connectors.ErrNotFound)
}

// Check verifies that the pipeline exists and that its runs can be read.
func (p *pipelines) Check(ctx context.Context, pipeline string) error {
	c := (*Connector)(p)
	repo, err := c.Resolve(ctx)
	if err != nil {
		return err
	}
	id, err := p.find(ctx, repo, pipeline)
	if err != nil {
		return err
	}
	return c.client.Do(ctx, http.MethodGet, path(repo.ProjectID, "_apis", "pipelines", strconv.Itoa(id), "runs"), APIVersion, nil, nil, nil)
}

// Trigger queues a run of the pipeline named pipeline on branch, with template parameters (which
// its YAML must declare).
func (p *pipelines) Trigger(ctx context.Context, pipeline, branch string, parameters map[string]string) (connectors.Run, error) {
	c := (*Connector)(p)
	repo, err := c.Resolve(ctx)
	if err != nil {
		return connectors.Run{}, err
	}
	id, err := p.find(ctx, repo, pipeline)
	if err != nil {
		return connectors.Run{}, err
	}
	var run struct {
		ID int `json:"id"`
	}
	body := map[string]any{"resources": map[string]any{"repositories": map[string]any{"self": map[string]any{"refName": "refs/heads/" + strings.TrimPrefix(branch, "refs/heads/")}}}}
	if len(parameters) > 0 {
		body["templateParameters"] = parameters
	}
	if err := c.client.Do(ctx, http.MethodPost, path(repo.ProjectID, "_apis", "pipelines", strconv.Itoa(id), "runs"), APIVersion, nil, body, &run); err != nil {
		return connectors.Run{}, err
	}
	return p.Get(ctx, strconv.Itoa(run.ID))
}

// PullRequestRuns returns the runs of the pull request's "Build validation" policies, from its
// policy evaluations: those started.
func (p *pipelines) PullRequestRuns(ctx context.Context, pullRequest string) ([]connectors.Run, error) {
	c := (*Connector)(p)
	repo, err := c.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	var list struct {
		Value []struct {
			Configuration struct {
				IsEnabled bool `json:"isEnabled"`
				Type      struct {
					ID string `json:"id"`
				} `json:"type"`
			} `json:"configuration"`
			Context struct {
				BuildID int `json:"buildId"`
			} `json:"context"`
		} `json:"value"`
	}
	artifact := "vstfs:///CodeReview/CodeReviewId/" + repo.ProjectID + "/" + pullRequest
	if err := c.client.Do(ctx, http.MethodGet, path(repo.ProjectID, "_apis", "policy", "evaluations"), previewVersion,
		url.Values{"artifactId": {artifact}}, nil, &list); err != nil {
		return nil, err
	}
	var runs []connectors.Run
	for _, v := range list.Value {
		if !v.Configuration.IsEnabled || !strings.EqualFold(v.Configuration.Type.ID, buildPolicy) || v.Context.BuildID == 0 {
			continue
		}
		r, err := p.Get(ctx, strconv.Itoa(v.Context.BuildID))
		if errors.Is(err, connectors.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if r.PullRequest == "" {
			r.PullRequest = pullRequest
		}
		runs = append(runs, r)
	}
	return runs, nil
}

// Artifact returns a file of one of a run's artifacts: its signed content (a zip), from the
// Pipelines API, downloaded without the PAT.
func (p *pipelines) Artifact(ctx context.Context, run, artifact, file string) ([]byte, error) {
	c := (*Connector)(p)
	repo, err := c.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	var b build
	if err := c.client.Do(ctx, http.MethodGet, path(repo.ProjectID, "_apis", "build", "builds", run), APIVersion, nil, nil, &b); err != nil {
		return nil, err
	}
	if !strings.EqualFold(b.Repository.ID, repo.ID) {
		return nil, fmt.Errorf("azure devops: run %s is of another repository: %w", run, connectors.ErrNotFound)
	}
	var a struct {
		SignedContent struct {
			URL string `json:"url"`
		} `json:"signedContent"`
	}
	if err := c.client.Do(ctx, http.MethodGet, path(repo.ProjectID, "_apis", "pipelines", strconv.Itoa(b.Definition.ID), "runs", run, "artifacts"), APIVersion,
		url.Values{"artifactName": {artifact}, "$expand": {"signedContent"}}, nil, &a); err != nil {
		return nil, err
	}
	if a.SignedContent.URL == "" {
		return nil, fmt.Errorf("azure devops: artifact %s of run %s has no content: %w", artifact, run, connectors.ErrNotFound)
	}
	zipped, err := c.client.Download(ctx, a.SignedContent.URL, maxArtifact)
	if err != nil {
		return nil, err
	}
	z, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		return nil, fmt.Errorf("azure devops: artifact %s of run %s: %w", artifact, run, err)
	}
	want := strings.TrimPrefix(file, "/")
	for _, f := range z.File {
		// Entries are <artifact>/<path>.
		if name := strings.TrimPrefix(f.Name, artifact+"/"); name == want {
			r, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer func() { _ = r.Close() }()
			out, err := io.ReadAll(io.LimitReader(r, maxArtifact+1))
			if err != nil {
				return nil, err
			}
			if len(out) > maxArtifact {
				return nil, fmt.Errorf("azure devops: %s in artifact %s is larger than %d bytes", file, artifact, maxArtifact)
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("azure devops: no %s in artifact %s of run %s: %w", file, artifact, run, connectors.ErrNotFound)
}
