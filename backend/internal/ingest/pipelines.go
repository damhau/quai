package ingest

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/inventory"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/runs"
)

// pipelineRun reads a run of one of the repository's pipelines again, as a service hook named
// it, and records it.
func (s *Service) pipelineRun(ctx context.Context, repo *repositories.Repo, id string) error {
	p := repo.Connector.Pipelines()
	if p == nil || len(repo.Document.Spec.Runner.Pipelines) == 0 {
		return nil
	}
	run, err := p.Get(ctx, id)
	if errors.Is(err, connectors.ErrNotFound) {
		return nil // another repository's pipeline
	}
	if err != nil {
		return err
	}
	return s.recordRun(ctx, repo, run)
}

// pullRequestRuns records the runs of a pull request's build validation.
func (s *Service) pullRequestRuns(ctx context.Context, repo *repositories.Repo, pr string) error {
	p := repo.Connector.Pipelines()
	if p == nil || len(repo.Document.Spec.Runner.Pipelines) == 0 {
		return nil
	}
	list, err := p.PullRequestRuns(ctx, pr)
	if err != nil {
		return err
	}
	var errs []error
	for _, run := range list {
		errs = append(errs, s.recordRun(ctx, repo, run))
	}
	return errors.Join(errs...)
}

// recordRun records a pipeline run as a run of each stack its pipeline covers
// (spec.runner.pipelines; docs/design/06-runs-status-and-drift.md#adapters), and hands what
// changed to the Handler: a report from quai report inside the run continues the same runs.
func (s *Service) recordRun(ctx context.Context, repo *repositories.Repo, run connectors.Run) error {
	for _, pl := range repo.Document.Spec.Runner.Pipelines {
		if pl.Name != run.Pipeline {
			continue
		}
		stacks, err := stacksOf(ctx, repo, pl.Stacks)
		if err != nil {
			return err
		}
		for _, stack := range stacks {
			r := runs.Report{Stack: stack, Commit: run.Commit, PullRequest: run.PullRequest, Kind: pl.Kind, Status: runStatus(run), URL: run.URL,
				StartedAt: when(run.Started), FinishedAt: when(run.Finished)}
			recorded, changed, err := runs.Record(ctx, s.store, repo, r)
			if err != nil {
				return err
			}
			s.log.InfoContext(ctx, "pipeline run", slog.String("repository", repo.Name), slog.String("pipeline", pl.Name), slog.String("stack", stack),
				slog.String("status", recorded.Status), slog.Bool("changed", changed))
			if changed && s.handler != nil {
				if err := s.handler.RunReported(ctx, recorded); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// runStatus is a pipeline run's status as a run report says it.
func runStatus(run connectors.Run) string {
	switch {
	case run.State != "completed":
		return "running"
	case run.Result == "succeeded":
		return "succeeded"
	}
	return "failed" // failed, partially succeeded, canceled
}

func when(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// stacksOf returns the stacks a pipeline covers: the one it names, or, for a pattern such as
// envs/{env}, each stack of the repository's resources that matches it.
func stacksOf(ctx context.Context, repo *repositories.Repo, pattern string) ([]string, error) {
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
