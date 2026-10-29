package requests

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/store/db"
)

// runsToSettle is how many of a stack's latest runs settle a request's apply.
const runsToSettle = 50

// RunReported moves requests on after a run report (docs/design/06-runs-status-and-drift.md#runs-and-requests):
// a finished plan of a request's pull request goes on its timeline and decides its rules that
// read the plan; a finished apply settles the requests of the repository that wait for an apply
// of its stack.
func (s *Service) RunReported(ctx context.Context, run db.Run) error {
	if run.Status == "running" {
		return nil
	}
	switch run.Kind {
	case "plan":
		if !run.RequestID.Valid {
			return nil
		}
		id := uuid.UUID(run.RequestID.Bytes)
		r, err := s.get(ctx, id)
		if err != nil {
			return err
		}
		if err := event(ctx, s.store.Queries, r, "plan_reported", "", runData(run)); err != nil {
			return err
		}
		if run.Status == "succeeded" {
			return s.PlanReported(ctx, id)
		}
	case "apply":
		applying, err := s.store.ApplyingRequests(ctx, db.ApplyingRequestsParams{Repository: run.Repository, Stack: run.Stack})
		if err != nil {
			return err
		}
		var errs []error
		for _, r := range applying {
			errs = append(errs, s.settle(ctx, r))
		}
		return errors.Join(errs...)
	}
	return nil
}

// settle moves a request waiting for its apply on, from the latest finished apply of each of
// its stacks that includes the commit that landed it: applied when each succeeded, failed when
// one failed. Without a stack, there is no apply to wait for.
func (s *Service) settle(ctx context.Context, r db.Request) error {
	if r.State != Applying {
		return nil
	}
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	if err != nil || len(sets) == 0 || sets[0].Commit == "" {
		return err
	}
	landed := sets[0].Commit
	if len(r.Stacks) == 0 {
		_, err := s.transition(ctx, r, Applying, Applied, "applied", map[string]any{"commit": landed, "note": "no stack to apply"})
		return err
	}
	repo, err := s.reg.Get(sets[0].Repository)
	if err != nil {
		return err
	}
	// An apply includes the landing commit when it ran at it or after it. This process's mirror
	// may not have the apply's commit yet (another process merged): it fetches once to know.
	fetched := false
	includes := func(commit string) bool {
		if commit != "" && (strings.HasPrefix(landed, commit) || strings.HasPrefix(commit, landed)) {
			return true
		}
		if ok, err := repo.Mirror.IsAncestor(ctx, landed, commit); err == nil && ok {
			return true
		}
		if !fetched {
			fetched = true
			if err := repo.Mirror.Fetch(ctx); err == nil {
				ok, err := repo.Mirror.IsAncestor(ctx, landed, commit)
				return err == nil && ok
			}
		}
		return false
	}
	var applied []db.Run
	for _, st := range r.Stacks {
		runs, err := s.store.StackRuns(ctx, db.StackRunsParams{Repository: repo.Name, Stack: st, MaxRows: runsToSettle})
		if err != nil {
			return err
		}
		var latest *db.Run
		for i, run := range runs { // newest first
			if run.Kind != "apply" || run.Status == "running" {
				continue
			}
			if includes(run.Commit) {
				latest = &runs[i]
				break
			}
		}
		switch {
		case latest == nil:
			return nil // still waiting for this stack
		case latest.Status == "failed":
			_, err := s.transition(ctx, r, Applying, Failed, "apply_failed", runData(*latest))
			return err
		}
		applied = append(applied, *latest)
	}
	_, err = s.transition(ctx, r, Applying, Applied, "applied", runData(applied[len(applied)-1]))
	return err
}

// runData is what a request's timeline says of a run.
func runData(run db.Run) map[string]any {
	d := map[string]any{"run": run.ID.String(), "stack": run.Stack, "commit": run.Commit, "status": run.Status}
	if run.Url != "" {
		d["url"] = run.Url
	}
	for k, raw := range map[string][]byte{"summary": run.Summary, "cost": run.Cost} {
		var v map[string]any
		if json.Unmarshal(raw, &v) == nil && len(v) > 0 {
			d[k] = v
		}
	}
	var policies []map[string]any
	if json.Unmarshal(run.Policies, &policies) == nil {
		counts := map[string]int{}
		for _, p := range policies {
			if res, _ := p["result"].(string); res != "" {
				counts[res]++
			}
		}
		if len(counts) > 0 {
			d["policies"] = counts
		}
	}
	return d
}

// landed reports whether a request's change is in Git already: a failed apply cannot be retried
// by delivering it again, the next apply of its stacks reports on it.
func (s *Service) landed(ctx context.Context, r db.Request) bool {
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	return err == nil && len(sets) > 0 && sets[0].Commit != ""
}

// Landed reports whether a request's change is in Git already.
func (s *Service) Landed(ctx context.Context, r db.Request) bool { return s.landed(ctx, r) }
