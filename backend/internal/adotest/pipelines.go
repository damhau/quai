package adotest

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// AddFiles commits files (path → content), which must not exist yet, on top of branch.
func (s *Sandbox) AddFiles(ctx context.Context, repoID, branch, message string, files map[string]string) error {
	var refs struct {
		Value []struct {
			Name     string `json:"name"`
			ObjectID string `json:"objectId"`
		} `json:"value"`
	}
	if err := s.Do(ctx, http.MethodGet, Path(s.Project, "_apis", "git", "repositories", repoID, "refs"), map[string][]string{"filter": {"heads/" + branch}}, nil, &refs); err != nil {
		return err
	}
	old := ""
	for _, r := range refs.Value {
		if r.Name == "refs/heads/"+branch {
			old = r.ObjectID
		}
	}
	if old == "" {
		return fmt.Errorf("no branch %s", branch)
	}
	var changes []map[string]any
	for path, content := range files {
		changes = append(changes, map[string]any{"changeType": "add", "item": map[string]any{"path": "/" + strings.TrimPrefix(path, "/")},
			"newContent": map[string]any{"content": content, "contentType": "rawtext"}})
	}
	push := map[string]any{
		"refUpdates": []map[string]any{{"name": "refs/heads/" + branch, "oldObjectId": old}},
		"commits":    []map[string]any{{"comment": message, "changes": changes}},
	}
	return s.Do(ctx, http.MethodPost, Path(s.Project, "_apis", "git", "repositories", repoID, "pushes"), nil, push, nil)
}

// Pipeline creates a YAML pipeline from file in a repository of the sandbox, named after the
// repository, and deletes it when the test ends. It returns the pipeline's ID.
func (s *Sandbox) Pipeline(t *testing.T, repoName, repoID, file string) int {
	t.Helper()
	ctx := context.Background()
	var p struct {
		ID int `json:"id"`
	}
	body := map[string]any{"name": repoName + "-" + strings.TrimSuffix(strings.TrimPrefix(file, "/"), ".yml"), "folder": `\`,
		"configuration": map[string]any{"type": "yaml", "path": "/" + strings.TrimPrefix(file, "/"),
			"repository": map[string]any{"id": repoID, "name": repoName, "type": "azureReposGit"}}}
	if err := s.Do(ctx, http.MethodPost, Path(s.Project, "_apis", "pipelines"), nil, body, &p); err != nil {
		t.Fatalf("create the pipeline %s of %s: %v", file, repoName, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		// The Pipelines API cannot delete; a pipeline is a build definition.
		if err := s.Do(ctx, http.MethodDelete, Path(s.Project, "_apis", "build", "definitions", strconv.Itoa(p.ID)), nil, nil, nil); err != nil {
			t.Errorf("delete the pipeline %d: %v", p.ID, err)
		}
	})
	return p.ID
}

// PipelineRun is a run of a pipeline.
type PipelineRun struct {
	ID     int    `json:"id"`
	State  string `json:"state"`  // unknown, inProgress, canceling, completed
	Result string `json:"result"` // once completed: succeeded, failed, canceled
	URL    string `json:"url"`
}

// Run queues a run of a pipeline on branch.
func (s *Sandbox) Run(ctx context.Context, pipeline int, branch string) (PipelineRun, error) {
	var run PipelineRun
	body := map[string]any{"resources": map[string]any{"repositories": map[string]any{"self": map[string]any{"refName": "refs/heads/" + branch}}}}
	err := s.Do(ctx, http.MethodPost, Path(s.Project, "_apis", "pipelines", strconv.Itoa(pipeline), "runs"), nil, body, &run)
	return run, err
}

// Await waits until a run completes and returns it. The sandbox has one hosted parallel job: a
// run may wait behind another first.
func (s *Sandbox) Await(t *testing.T, pipeline, run int, timeout time.Duration) PipelineRun {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var r PipelineRun
		err := s.Do(context.Background(), http.MethodGet, Path(s.Project, "_apis", "pipelines", strconv.Itoa(pipeline), "runs", strconv.Itoa(run)), nil, nil, &r)
		if err == nil && r.State == "completed" {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("pipeline %d, run %d: not completed after %s (%+v, %v)", pipeline, run, timeout, r, err)
		}
		time.Sleep(5 * time.Second)
	}
}
