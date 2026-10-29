package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/status"
	"github.com/damhau/quai/backend/internal/inventory"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/runs"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// stackRuns is how many of a stack's latest runs a status is derived from.
const stackRuns = 50

// statuses derives resource statuses (docs/design/06-runs-status-and-drift.md#resource-status)
// for one call to the API: each repository's change index, the paths its open requests change
// and its stacks' runs are read once.
type statuses struct {
	s     *store.Store
	repos map[string]*repoStatuses
}

type repoStatuses struct {
	repo    *repositories.Repo
	index   *repositories.ChangeIndex
	pending []string
	runs    map[string][]db.Run // by stack
	after   map[[2]string]bool  // (last change, run commit) → the run is at or after the change
}

func newStatuses(s *store.Store) *statuses {
	return &statuses{s: s, repos: map[string]*repoStatuses{}}
}

// of returns a resource's status and the run it comes from.
func (st *statuses) of(ctx context.Context, repo *repositories.Repo, snap *repositories.Snapshot, r inventory.Resource) (string, *RunView) {
	rs := st.repo(ctx, repo, snap)
	if rs == nil {
		return status.InRepo, nil
	}
	in := status.Input{
		Pending: slices.ContainsFunc(rs.pending, func(p string) bool { return p == r.Path || strings.HasPrefix(p, strings.TrimSuffix(r.Path, "/")+"/") }),
		Runner:  repo.HasRunner(),
	}
	runs := rs.stack(ctx, st.s, r.Stack)
	for _, run := range runs {
		in.Runs = append(in.Runs, status.Run{Kind: run.Kind, Status: run.Status, Commit: run.Commit, Drifted: run.Kind == "drift-check" && drifted(run, r)})
	}
	changed := rs.index.Of(r.Path)
	in.After = func(commit string) bool {
		if changed == "" {
			return false
		}
		k := [2]string{changed, commit}
		if v, ok := rs.after[k]; ok {
			return v
		}
		v, err := repo.Mirror.IsAncestor(ctx, changed, commit)
		rs.after[k] = err == nil && v
		return rs.after[k]
	}
	state, i := status.Derive(in)
	if i < 0 {
		return state, nil
	}
	v := runView(runs[i])
	return state, &v
}

func (st *statuses) repo(ctx context.Context, repo *repositories.Repo, snap *repositories.Snapshot) *repoStatuses {
	if rs, ok := st.repos[repo.Name]; ok {
		return rs
	}
	rs := &repoStatuses{repo: repo, runs: map[string][]db.Run{}, after: map[[2]string]bool{}}
	st.repos[repo.Name] = rs
	if repo.Mirror == nil {
		return rs
	}
	rs.index, _ = repo.Changes(ctx, snap.Commit) // without it, every resource is in the repository
	if st.s != nil {
		rs.pending, _ = st.s.OpenChangePaths(ctx, repo.Name)
	}
	return rs
}

func (rs *repoStatuses) stack(ctx context.Context, s *store.Store, stack string) []db.Run {
	if s == nil || stack == "" {
		return nil
	}
	if runs, ok := rs.runs[stack]; ok {
		return runs
	}
	runs, _ := s.StackRuns(ctx, db.StackRunsParams{Repository: rs.repo.Name, Stack: stack, MaxRows: stackRuns})
	rs.runs[stack] = runs
	return runs
}

// drifted reports whether a drift check's plan changes the resource.
func drifted(run db.Run, r inventory.Resource) bool {
	var sp runs.Plan
	if json.Unmarshal(run.Plan, &sp) != nil {
		return false
	}
	for _, res := range sp.Resources {
		if res.Type == r.Type && res.Key == r.Key && len(res.Changes) > 0 {
			return true
		}
	}
	return false
}
