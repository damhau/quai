package runs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/inventory"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// DefaultDriftInterval is how often a stack's drift is checked without spec.runner.driftInterval.
const DefaultDriftInterval = 24 * time.Hour

// ErrNoDriftCheck is a repository without a drift-check pipeline, or whose connector cannot
// trigger it.
var ErrNoDriftCheck = errors.New("no drift-check pipeline")

// Stacks returns the stacks a pipeline covers: the one it names, or, for a pattern such as
// envs/{env}, each stack of the repository's resources that matches it.
func Stacks(ctx context.Context, repo *repositories.Repo, pattern string) ([]string, error) {
	if !strings.Contains(pattern, "{") {
		return []string{pattern}, nil
	}
	p, err := locate.Compile(pattern)
	if err != nil {
		return nil, err
	}
	snap, err := repo.Snapshot(ctx, false)
	if err != nil {
		return nil, err
	}
	resources, err := inventory.Scan(snap)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range resources {
		if _, ok := p.Match(r.Stack); ok && !slices.Contains(out, r.Stack) {
			out = append(out, r.Stack)
		}
	}
	slices.Sort(out)
	return out, nil
}

// driftPipeline returns the repository's drift-check pipeline.
func driftPipeline(repo *repositories.Repo) (repositories.Pipeline, bool) {
	for _, p := range repo.Document.Spec.Runner.Pipelines {
		if p.Kind == "drift-check" {
			return p, repo.Connector != nil && repo.Connector.Pipelines() != nil
		}
	}
	return repositories.Pipeline{}, false
}

// DriftStacks returns the stacks the repository's drift-check pipeline covers.
func DriftStacks(ctx context.Context, repo *repositories.Repo) ([]string, error) {
	p, ok := driftPipeline(repo)
	if !ok {
		return nil, ErrNoDriftCheck
	}
	return Stacks(ctx, repo, p.Stacks)
}

// DueDrift returns the stacks whose latest drift check started at least the repository's drift
// interval before now, or that never had one.
func DueDrift(ctx context.Context, s *store.Store, repo *repositories.Repo, now time.Time) ([]string, error) {
	stacks, err := DriftStacks(ctx, repo)
	if err != nil {
		return nil, err
	}
	interval := time.Duration(repo.Document.Spec.Runner.DriftInterval)
	if interval == 0 {
		interval = DefaultDriftInterval
	}
	var due []string
	for _, st := range stacks {
		last, err := s.ListRuns(ctx, db.ListRunsParams{Repository: repo.Name, Stack: st, Kind: "drift-check", MaxRows: 1})
		if err != nil {
			return nil, err
		}
		if len(last) == 0 || now.Sub(last[0].StartedAt.Time) >= interval {
			due = append(due, st)
		}
	}
	return due, nil
}

// QueueDrift triggers the drift-check pipeline for stack on the default branch, and records the
// run queued: the next schedule sees it. A pipeline covering several stacks is queued with the
// template parameter stack.
func QueueDrift(ctx context.Context, s *store.Store, repo *repositories.Repo, stack string, now time.Time) (db.Run, error) {
	p, ok := driftPipeline(repo)
	if !ok {
		return db.Run{}, ErrNoDriftCheck
	}
	var params map[string]string
	if strings.Contains(p.Stacks, "{") {
		params = map[string]string{"stack": stack}
	}
	run, err := repo.Connector.Pipelines().Trigger(ctx, p.Name, repo.Branch, params)
	if err != nil {
		return db.Run{}, fmt.Errorf("queue the drift check of %s: %w", stack, err)
	}
	recorded, _, err := Record(ctx, s, repo, Report{Stack: stack, Commit: run.Commit, Kind: "drift-check", Status: "running", URL: run.URL, StartedAt: &now})
	return recorded, err
}
