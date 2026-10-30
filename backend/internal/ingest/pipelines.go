package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/runs"
	"github.com/riverqueue/river"
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

// polledRuns is how many of a pipeline's latest runs a poll reads.
const polledRuns = 5

// pollPipelineRuns records the latest runs of the repository's apply and drift-check pipelines on its
// default branch, oldest first: what their service hooks say, for when those cannot reach Quai
// (docs/design/06-runs-status-and-drift.md#adapters). A run already recorded changes nothing.
func (s *Service) pollPipelineRuns(ctx context.Context, repo *repositories.Repo) error {
	p := repo.Connector.Pipelines()
	if p == nil {
		return nil
	}
	var errs []error
	for _, pl := range repo.Document.Spec.Runner.Pipelines {
		if pl.Kind != "apply" && pl.Kind != "drift-check" {
			continue
		}
		list, err := p.Runs(ctx, pl.Name, repo.Branch, polledRuns)
		if err != nil {
			errs = append(errs, fmt.Errorf("the runs of %s: %w", pl.Name, err))
			continue
		}
		for _, run := range slices.Backward(list) {
			errs = append(errs, s.recordRun(ctx, repo, run))
		}
	}
	return errors.Join(errs...)
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
		stacks, err := runs.Stacks(ctx, repo, pl.Stacks)
		if err != nil {
			return err
		}
		// A run queued for one stack (template parameter stack) covers that one.
		if st := run.Parameters["stack"]; st != "" && slices.Contains(stacks, st) {
			stacks = []string{st}
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

func hasDriftCheck(repo *repositories.Repo) bool {
	return slices.ContainsFunc(repo.Document.Spec.Runner.Pipelines, func(p repositories.Pipeline) bool { return p.Kind == "drift-check" })
}

type driftWorker struct {
	river.WorkerDefaults[DriftArgs]
	s *Service
}

func (w *driftWorker) Work(ctx context.Context, job *river.Job[DriftArgs]) error {
	return w.s.QueueDueDrift(ctx, job.Args.Repository)
}

// QueueDueDrift queues the drift checks of the repository's stacks that are due
// (docs/design/06-runs-status-and-drift.md#drift): those whose latest check started a drift
// interval ago or more, or never.
func (s *Service) QueueDueDrift(ctx context.Context, name string) error {
	repo, err := s.reg.Get(name)
	if err != nil {
		return nil
	}
	now := s.now()
	due, err := runs.DueDrift(ctx, s.store, repo, now)
	if errors.Is(err, runs.ErrNoDriftCheck) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, stack := range due {
		run, err := runs.QueueDrift(ctx, s.store, repo, stack, now)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.log.InfoContext(ctx, "drift check queued", slog.String("repository", name), slog.String("stack", stack), slog.String("run", run.Url))
	}
	return errors.Join(errs...)
}
