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
