package azuredevops

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/gitx"
)

// planPipeline writes a plan file and publishes it as the artifact "plan".
const planPipeline = `trigger: none
pool:
  vmImage: ubuntu-latest
steps:
  - script: |
      echo '{"format_version": "1.2"}' > $(Build.ArtifactStagingDirectory)/plan.json
  - publish: $(Build.ArtifactStagingDirectory)/plan.json
    artifact: plan
`

// TestLivePipelines: a run of a YAML pipeline is read back through the Builds API with its
// commit, its page and its artifact; the pipeline as a build validation policy runs on a pull
// request, whose policy evaluations name that run. Skipped without the sandbox's PAT.
func TestLivePipelines(t *testing.T) {
	s := newSandbox(t)
	ctx := context.Background()
	name, id := s.Repository(t)
	if err := s.AddFiles(ctx, id, "main", "A plan pipeline", map[string]string{"azure-pipelines.yml": planPipeline}); err != nil {
		t.Fatal(err)
	}
	pipelineID := s.Pipeline(t, name, id, "azure-pipelines.yml")
	c, err := New(connectors.Config{URL: s.URL(name), HTTP: s.HTTP, Credentials: s.PAT})
	if err != nil {
		t.Fatal(err)
	}
	ado := c.(*Connector)
	repo, err := ado.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Pipelines()

	run, err := p.Trigger(ctx, name+"-azure-pipelines", "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	runID, _ := strconv.Atoi(run.ID)
	s.Await(t, pipelineID, runID, 10*time.Minute)
	got, err := p.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "completed" || got.Result != "succeeded" || got.Branch != "refs/heads/main" || len(got.Commit) != 40 || got.PullRequest != "" ||
		got.URL != RunURL(s.Collection, s.Project, runID) || got.Pipeline != name+"-azure-pipelines" || got.Finished.IsZero() {
		t.Errorf("the run = %+v", got)
	}
	// The pipeline's latest runs on main, as a poll reads them; none on another branch.
	if latest, err := p.Runs(ctx, name+"-azure-pipelines", "main", 5); err != nil || len(latest) != 1 || latest[0].ID != run.ID || latest[0].State != "completed" {
		t.Errorf("the runs on main = %+v, %v", latest, err)
	}
	if other, err := p.Runs(ctx, name+"-azure-pipelines", "release", 5); err != nil || len(other) != 0 {
		t.Errorf("the runs on release = %+v, %v", other, err)
	}
	planFile, err := p.Artifact(ctx, run.ID, "plan", "plan.json")
	if err != nil || !strings.Contains(string(planFile), `"format_version": "1.2"`) {
		t.Errorf("the artifact = %q, %v", planFile, err)
	}
	if _, err := p.Artifact(ctx, run.ID, "plan", "nope.json"); err == nil {
		t.Error("a file the artifact does not hold")
	}

	// The same pipeline validates pull requests to main.
	var policy struct {
		ID int `json:"id"`
	}
	settings := map[string]any{"buildDefinitionId": pipelineID, "queueOnSourceUpdateOnly": false, "manualQueueOnly": false, "displayName": "plan",
		"validDuration": 0, "scope": []map[string]any{{"repositoryId": repo.ID, "refName": "refs/heads/main", "matchKind": "exact"}}}
	if err := ado.client.Do(ctx, http.MethodPost, path(repo.ProjectID, "_apis", "policy", "configurations"), APIVersion, nil,
		map[string]any{"isEnabled": true, "isBlocking": true, "type": map[string]string{"id": buildPolicy}, "settings": settings}, &policy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ado.client.Do(context.Background(), http.MethodDelete, path(repo.ProjectID, "_apis", "policy", "configurations", strconv.Itoa(policy.ID)), APIVersion, nil, nil, nil)
	})
	m, err := gitx.OpenMirror(ctx, s.URL(name), filepath.Join(t.TempDir(), "m.git"),
		gitx.Options{Committer: gitx.Signature{Name: "Quai", Email: "quai@example.com"}, Auth: c.GitAuth, Proxy: s.Network.Proxy})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CommitAndPush(ctx, gitx.Commit{Branch: "quai/r-1-web01", Base: "main", Message: "Resize web01\n",
		Author:  gitx.Signature{Name: "Bob", Email: "bob@example.com"},
		Changes: []plan.Change{{Op: plan.OpCreate, Path: "web01.yaml", After: []byte("cpu: 8\n")}}}); err != nil {
		t.Fatal(err)
	}
	pr, err := c.PullRequests().Open(ctx, connectors.NewPullRequest{Source: "quai/r-1-web01", Target: "main", Title: "Resize web01"})
	if err != nil {
		t.Fatal(err)
	}
	var validation connectors.Run
	deadline := time.Now().Add(3 * time.Minute)
	for validation.ID == "" {
		runs, err := p.PullRequestRuns(ctx, pr.ID)
		if err == nil && len(runs) > 0 {
			validation = runs[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no build validation run on pull request %s: %v", pr.ID, err)
		}
		time.Sleep(5 * time.Second)
	}
	if validation.PullRequest != pr.ID || validation.Branch != "refs/pull/"+pr.ID+"/merge" {
		t.Errorf("the build validation run = %+v", validation)
	}
	vID, _ := strconv.Atoi(validation.ID)
	s.Await(t, pipelineID, vID, 10*time.Minute)
	if done, err := p.Get(ctx, validation.ID); err != nil || done.Result != "succeeded" || done.PullRequest != pr.ID {
		t.Errorf("the build validation run once done = %+v, %v", done, err)
	}
}
