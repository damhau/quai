package adotest

import (
	"context"
	"testing"
	"time"
)

// TestPipeline runs a one-step YAML pipeline on the sandbox's hosted agents. Skipped without the
// sandbox's PAT.
func TestPipeline(t *testing.T) {
	s := New(t)
	ctx := context.Background()
	name, id := s.Repository(t)
	if err := s.AddFiles(ctx, id, "main", "A pipeline", map[string]string{"azure-pipelines.yml": "trigger: none\npool:\n  vmImage: ubuntu-latest\nsteps:\n  - script: echo quai\n"}); err != nil {
		t.Fatal(err)
	}
	pipeline := s.Pipeline(t, name, id, "azure-pipelines.yml")
	run, err := s.Run(ctx, pipeline, "main")
	if err != nil {
		t.Fatal(err)
	}
	if done := s.Await(t, pipeline, run.ID, 10*time.Minute); done.Result != "succeeded" {
		t.Errorf("the run = %+v", done)
	}
}
