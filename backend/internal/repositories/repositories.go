// Package repositories holds Quai's repository connections (docs/design/01-repository-connections.md):
// for each, its mirror and a snapshot of the default branch's head with the definitions read
// from it. A snapshot is reloaded only when the head moves; reads fetch the mirror at most once
// per sync interval, and an explicit sync fetches at once.
package repositories

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/gitx"
)

// ErrNotFound is returned for a repository that is not connected.
var ErrNotFound = errors.New("no such repository")

// Repo is one connected repository.
type Repo struct {
	Name       string
	Title      string
	Branch     string
	Delivery   string
	FloorRules []definitions.Rule
	Mirror     *gitx.Mirror
	// RunTokenSHA256 is the SHA-256 of the token runners report with; empty when none may.
	RunTokenSHA256 string

	minInterval time.Duration
	log         *slog.Logger
	mu          sync.Mutex
	lastFetch   time.Time
	snap        *Snapshot
}

// Snapshot is a repository at one commit: its files and the definitions read from them.
type Snapshot struct {
	Repository  string
	Commit      string
	Definitions *definitions.Repository
	Problems    definitions.Problems
	FS          fs.FS
	worktree    *gitx.Worktree
}

// Registry is the set of connected repositories.
type Registry struct {
	repos []*Repo
	log   *slog.Logger
}

// worktreeGrace is how long a replaced snapshot's files stay readable for requests still using
// it.
const worktreeGrace = 2 * time.Minute

// Open connects the repositories of the configuration: it clones or fetches their mirrors and
// compiles their floor rules.
func Open(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Registry, error) {
	cache := cfg.Git.CacheDir
	if cache == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		cache = filepath.Join(dir, "quai", "mirrors")
	}
	env, err := expr.NewEnv()
	if err != nil {
		return nil, err
	}
	reg := &Registry{log: log}
	for _, rc := range cfg.Repositories {
		m, err := gitx.OpenMirror(ctx, rc.URL, filepath.Join(cache, rc.Name+".git"), gitx.Signature{Name: cfg.Git.Committer.Name, Email: cfg.Git.Committer.Email})
		if err != nil {
			return nil, fmt.Errorf("repository %s: %w", rc.Name, err)
		}
		r := &Repo{Name: rc.Name, Title: rc.Title, Branch: rc.DefaultBranch, Delivery: rc.Delivery.Mode, Mirror: m,
			RunTokenSHA256: rc.Runner.TokenSHA256, minInterval: rc.Sync.MinInterval, log: log, lastFetch: time.Now()}
		for _, f := range rc.FloorRules {
			rule := definitions.Rule{Name: f.Name, When: f.When, Approvers: f.Approvers, Min: f.Min}
			if f.When != "" {
				if rule.Condition, err = env.CompileCondition(f.When); err != nil {
					return nil, fmt.Errorf("repository %s: floor rule %s: %w", rc.Name, f.Name, err)
				}
			}
			r.FloorRules = append(r.FloorRules, rule)
		}
		reg.repos = append(reg.repos, r)
	}
	return reg, nil
}

// List returns the repositories in configuration order.
func (r *Registry) List() []*Repo { return r.repos }

// Get returns a repository by name.
func (r *Registry) Get(name string) (*Repo, error) {
	for _, repo := range r.repos {
		if repo.Name == name {
			return repo, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// Close removes the snapshots' worktrees.
func (r *Registry) Close() {
	for _, repo := range r.repos {
		repo.mu.Lock()
		if repo.snap != nil {
			_ = repo.snap.worktree.Close()
			repo.snap = nil
		}
		repo.mu.Unlock()
	}
}

// Snapshot returns the repository at the head of its branch, fetching first when the last fetch
// is older than the sync interval, or always when sync is set.
func (repo *Repo) Snapshot(ctx context.Context, sync bool) (*Snapshot, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if sync || time.Since(repo.lastFetch) >= repo.minInterval {
		if err := repo.Mirror.Fetch(ctx); err != nil {
			if repo.snap == nil {
				return nil, err
			}
			// Serve the last snapshot while the remote is unreachable.
			repo.log.WarnContext(ctx, "fetch failed; serving the last snapshot", slog.String("repository", repo.Name), slog.Any("error", err))
		} else {
			repo.lastFetch = time.Now()
		}
	}
	head, err := repo.Mirror.Head(ctx, repo.Branch)
	if err != nil {
		return nil, err
	}
	if repo.snap != nil && repo.snap.Commit == head {
		return repo.snap, nil
	}
	wt, err := repo.Mirror.Worktree(ctx, head)
	if err != nil {
		return nil, err
	}
	defs, problems := definitions.Load(wt.FS())
	snap := &Snapshot{Repository: repo.Name, Commit: head, Definitions: defs, Problems: problems, FS: wt.FS(), worktree: wt}
	if old := repo.snap; old != nil {
		time.AfterFunc(worktreeGrace, func() { _ = old.worktree.Close() })
	}
	repo.snap = snap
	repo.log.InfoContext(ctx, "definitions loaded", slog.String("repository", repo.Name), slog.String("commit", head),
		slog.Int("types", len(defs.Types)), slog.Int("blueprints", len(defs.Blueprints)), slog.Int("problems", len(problems)))
	return snap, nil
}
