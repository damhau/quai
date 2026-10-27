package repositories

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/config"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@localhost", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@localhost")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestRefresh(t *testing.T) {
	root := t.TempDir()
	remote, work := filepath.Join(root, "remote.git"), filepath.Join(root, "work")
	git(t, root, "init", "--quiet", "--bare", "--initial-branch=main", remote)
	git(t, root, "clone", "--quiet", remote, work)
	commit := func(msg string) {
		if err := os.WriteFile(filepath.Join(work, "README.md"), []byte(msg), 0o600); err != nil {
			t.Fatal(err)
		}
		git(t, work, "add", "README.md")
		git(t, work, "commit", "--quiet", "-m", msg)
		git(t, work, "push", "--quiet", "origin", "HEAD:main")
	}
	commit("first")

	cfg := &config.Config{Git: config.Git{CacheDir: filepath.Join(root, "cache")}, Repositories: []config.Repository{
		{Name: "infra", URL: remote, DefaultBranch: "main", Sync: config.SyncSettings{MinInterval: time.Hour}},
	}}
	ctx := context.Background()
	reg, err := Open(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	repo, _ := reg.Get("infra")
	before, err := repo.Snapshot(ctx, false)
	if err != nil {
		t.Fatal(err)
	}

	// Another process commits: reads within the sync interval keep the snapshot they have.
	commit("second")
	if snap, _ := repo.Snapshot(ctx, false); snap.Commit != before.Commit {
		t.Fatal("a read within the sync interval fetched")
	}
	if err := repo.Refresh(ctx, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if snap, _ := repo.Snapshot(ctx, false); snap.Commit != before.Commit {
		t.Error("a refresh for a time before the last fetch fetched")
	}
	if err := repo.Refresh(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if snap, _ := repo.Snapshot(ctx, false); snap.Commit == before.Commit {
		t.Error("a refresh for a time after the last fetch did not fetch")
	}
}
