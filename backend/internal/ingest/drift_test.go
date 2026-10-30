package ingest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/generic"
	"github.com/damhau/quai/backend/internal/connectors/pipelinestest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store/db"
)

// exampleRemote is a bare repository holding the example GitOps repository on main.
func exampleRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bare, work := filepath.Join(dir, "infra.git"), filepath.Join(dir, "work")
	gitRun := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...) // #nosec G204 -- test helper
		cmd.Dir = dir
		cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + os.TempDir(), "PATH=" + os.Getenv("PATH"),
			"GIT_AUTHOR_NAME=Alice", "GIT_AUTHOR_EMAIL=alice@example.com", "GIT_COMMITTER_NAME=Alice", "GIT_COMMITTER_EMAIL=alice@example.com"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitRun("", "init", "--quiet", "--bare", "--initial-branch=main", bare)
	gitRun("", "clone", "--quiet", bare, work)
	if err := os.CopyFS(work, os.DirFS("../../../examples/gitops-repo")); err != nil {
		t.Fatal(err)
	}
	gitRun(work, "add", "--all")
	gitRun(work, "commit", "--quiet", "-m", "the existing infrastructure")
	gitRun(work, "push", "--quiet", "origin", "HEAD:main")
	return bare
}

// TestDriftSchedule: the schedule queues the drift check of each stack the drift-check pipeline
// covers once per drift interval, each run queued with its stack as a template parameter
// (docs/design/06-runs-status-and-drift.md#drift).
func TestDriftSchedule(t *testing.T) {
	ctx := context.Background()
	fake := &pipelinestest.Pipelines{}
	e := start(t, "infra", `connector: generic
  url: `+exampleRemote(t)+`
  runner:
    driftInterval: 12h
    pipelines: [{ name: infra-drift, kind: drift-check, stacks: "envs/{env}" }]`, repositories.Options{
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"generic": pipelinestest.Factory(generic.New, fake)}},
	})
	t0 := time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC)
	tick := func(at time.Time) int {
		t.Helper()
		e.svc.now = func() time.Time { return at }
		if err := e.svc.QueueDueDrift(ctx, "infra"); err != nil {
			t.Fatal(err)
		}
		return len(fake.Triggered())
	}
	if n := tick(t0); n != 2 {
		t.Fatalf("the first schedule queued %d", n)
	}
	if got := fake.Triggered(); got[0].Parameters["stack"] != "envs/dev" || got[1].Parameters["stack"] != "envs/prod" || got[0].Pipeline != "infra-drift" {
		t.Errorf("queued = %+v", got)
	}
	runs, _ := e.store.ListRuns(ctx, db.ListRunsParams{Repository: "infra", Kind: "drift-check", MaxRows: 10})
	if len(runs) != 2 || runs[0].Status != "running" || runs[0].Url == "" {
		t.Errorf("the queued runs = %+v", runs)
	}
	if n := tick(t0.Add(time.Hour)); n != 2 {
		t.Errorf("an hour later, %d queued", n)
	}
	if n := tick(t0.Add(11 * time.Hour)); n != 2 {
		t.Errorf("within the interval, %d queued", n)
	}
	if n := tick(t0.Add(12 * time.Hour)); n != 4 {
		t.Errorf("one interval later, %d queued", n)
	}

	// A repository without a drift-check pipeline queues nothing.
	other := start(t, "other", `connector: generic
  url: `+exampleRemote(t), repositories.Options{})
	if err := other.svc.QueueDueDrift(ctx, "other"); err != nil {
		t.Error(err)
	}
}

// TestMergedPullRequestRuns: a pull request's build validation runs are read again when it
// merges, so that a plan that finished after the last poll is recorded; an abandoned one's are
// not.
func TestMergedPullRequestRuns(t *testing.T) {
	ctx := context.Background()
	fake := &pipelinestest.Pipelines{}
	e := start(t, "infra", `connector: generic
  url: `+exampleRemote(t)+`
  runner:
    pipelines: [{ name: infra-plan, kind: plan, stacks: "envs/{env}" }]`, repositories.Options{
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"generic": pipelinestest.Factory(generic.New, fake)}},
	})
	for _, pr := range []string{"7", "8"} {
		run := fake.Validate("infra-plan", pr, "abcdef1")
		fake.Finish(run.ID, "succeeded", "abcdef1")
	}
	if err := e.svc.handle(ctx, e.repo(), connectors.PullRequest{ID: "7", State: connectors.PRMerged}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.handle(ctx, e.repo(), connectors.PullRequest{ID: "8", State: connectors.PRAbandoned}); err != nil {
		t.Fatal(err)
	}
	plans, _ := e.store.ListRuns(ctx, db.ListRunsParams{Repository: "infra", Kind: "plan", MaxRows: 10})
	if len(plans) != 2 || plans[0].PullRequest != "7" || plans[1].PullRequest != "7" || plans[0].Status != "succeeded" {
		t.Errorf("the plans = %+v", plans)
	}
}

// TestPolledPipelineRuns: each poll reads the latest runs of the apply and drift-check pipelines
// on the default branch, for when their service hooks cannot reach Quai. A run already recorded
// changes nothing; plan pipelines and other branches are not read.
func TestPolledPipelineRuns(t *testing.T) {
	ctx := context.Background()
	fake := &pipelinestest.Pipelines{}
	e := start(t, "infra", `connector: generic
  url: `+exampleRemote(t)+`
  runner:
    pipelines:
      - { name: infra-plan, kind: plan, stacks: "envs/{env}" }
      - { name: infra-apply, kind: apply, stacks: "envs/{env}" }`, repositories.Options{
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"generic": pipelinestest.Factory(generic.New, fake)}},
	})
	apply, _ := fake.Trigger(ctx, "infra-apply", "main", nil)
	_, _ = fake.Trigger(ctx, "infra-apply", "release", nil)
	_, _ = fake.Trigger(ctx, "infra-plan", "main", nil)
	poll := func() []db.Run {
		t.Helper()
		if err := e.svc.Poll(ctx, "infra"); err != nil {
			t.Fatal(err)
		}
		runs, _ := e.store.ListRuns(ctx, db.ListRunsParams{Repository: "infra", MaxRows: 20})
		return runs
	}
	if runs := poll(); len(runs) != 2 || runs[0].Kind != "apply" || runs[0].Status != "running" || runs[1].Status != "running" {
		t.Fatalf("after the first poll, the runs = %+v", runs)
	}
	fake.Finish(apply.ID, "failed", "abcdef1")
	if runs := poll(); len(runs) != 2 || runs[0].Status != "failed" || runs[0].Commit != "abcdef1" || runs[1].Status != "failed" {
		t.Errorf("once it failed, the runs = %+v", runs)
	}
	reported := len(e.handler.reported())
	poll()
	if again := len(e.handler.reported()); again != reported {
		t.Errorf("the same runs again were handed on: %d, then %d", reported, again)
	}
}
