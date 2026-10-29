// Package runs records run reports (docs/design/06-runs-status-and-drift.md#run-report-protocol):
// from runners, through POST /api/v1/runs, and from the platforms' pipelines, through their
// adapters. A report continues the running run it describes, or starts one, and belongs to the
// request whose pull request or landing commit it names.
package runs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/engine/tfplan"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Summary counts a plan's or an apply's resource changes.
type Summary struct {
	Add     int `json:"add"`
	Change  int `json:"change"`
	Destroy int `json:"destroy"`
}

// Policy is one policy check's result in a run.
type Policy struct {
	Name    string `json:"name"`
	Result  string `json:"result"`
	Message string `json:"message,omitempty"`
}

// Cost is a run's cost estimate.
type Cost struct {
	Currency     string  `json:"currency"`
	MonthlyDelta float64 `json:"monthlyDelta"`
}

// Report is what a runner, or a platform's pipeline, says of a run.
type Report struct {
	Stack, Commit, PullRequest, Kind, Status, URL string
	StartedAt, FinishedAt                         *time.Time
	Summary                                       *Summary
	Policies                                      []Policy
	Cost                                          *Cost
	// Plan is the output of terraform show -json, when the report carries it; PlanRef names it
	// in the run's artifacts otherwise (artifact://<artifact>/<file>).
	Plan       []byte
	PlanFormat string
	PlanRef    string
}

// Plan is what Quai keeps of a plan (runs.plan): what it changes by resource, never the plan.
type Plan struct {
	Format string `json:"format"`
	tfplan.Mapping
}

// PlanError is a report whose plan cannot be read: Field says from where.
type PlanError struct {
	Field string // plan.content or plan.ref
	Err   error
}

func (e *PlanError) Error() string { return e.Err.Error() }
func (e *PlanError) Unwrap() error { return e.Err }

// Record records a report of a run of repo: it continues the run of the same stack and kind
// that has the same url, or, when either has none, the running one with the same commit and
// pull request, or starts a run. What a later report leaves out is kept. changed says whether
// the report started the run, changed its status or added to what it says (its plan, cost or
// policies, after the adapter recorded it from the pipeline's state): the same report again
// changes nothing.
func Record(ctx context.Context, s *store.Store, repo *repositories.Repo, r Report) (run db.Run, changed bool, err error) {
	request, err := requestOf(ctx, s, repo.Name, r.PullRequest, r.Commit)
	if err != nil {
		return db.Run{}, false, err
	}
	summary, policies, cost, planned := []byte(`{}`), []byte(`[]`), []byte(`{}`), []byte(`{}`)
	if len(r.Plan) == 0 && r.PlanRef != "" {
		if r.Plan, err = artifactPlan(ctx, repo, r.URL, r.PlanRef); err != nil {
			return db.Run{}, false, &PlanError{Field: "plan.ref", Err: err}
		}
	}
	if len(r.Plan) > 0 {
		changes, err := tfplan.Parse(r.Plan, tfplan.Options{Values: repo.Document.Spec.Runner.PlanValues})
		if err != nil {
			return db.Run{}, false, &PlanError{Field: "plan.content", Err: err}
		}
		// The types of the repository's default branch say which resources own what.
		snap, err := repo.Snapshot(ctx, false)
		if err != nil {
			return db.Run{}, false, err
		}
		planned, _ = json.Marshal(Plan{Format: r.PlanFormat, Mapping: tfplan.Map(changes, r.Stack, tfplan.Claims(snap.Definitions.Types))})
		if r.Summary == nil {
			add, change, destroy := tfplan.Summary(changes)
			r.Summary = &Summary{Add: add, Change: change, Destroy: destroy}
		}
	}
	if r.Summary != nil {
		summary, _ = json.Marshal(r.Summary)
	}
	if len(r.Policies) > 0 {
		policies, _ = json.Marshal(r.Policies)
	}
	if r.Cost != nil {
		cost, _ = json.Marshal(r.Cost)
	}
	err = s.Tx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		prev, err := q.ReportedRun(ctx, db.ReportedRunParams{Repository: repo.Name, Stack: r.Stack, Kind: r.Kind, Url: r.URL, Commit: r.Commit, PullRequest: r.PullRequest})
		switch {
		case err == nil:
			changed = prev.Status != r.Status
			finished := timestamp(r.FinishedAt)
			if !changed && prev.FinishedAt.Valid && !finished.Valid {
				finished = prev.FinishedAt // the same report again: it finished then
			}
			run, err = q.UpdateRun(ctx, db.UpdateRunParams{ID: prev.ID, Status: r.Status, Commit: r.Commit, PullRequest: r.PullRequest, Url: r.URL, RequestID: request, Summary: summary,
				Policies: policies, Cost: cost, Plan: planned, StartedAt: timestamp(r.StartedAt), FinishedAt: finished})
			changed = changed || !bytes.Equal(prev.Summary, run.Summary) || !bytes.Equal(prev.Policies, run.Policies) ||
				!bytes.Equal(prev.Cost, run.Cost) || !bytes.Equal(prev.Plan, run.Plan)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		changed = true
		run, err = q.InsertRun(ctx, db.InsertRunParams{ID: uuid.New(), Repository: repo.Name, Stack: r.Stack, Commit: r.Commit, Kind: r.Kind,
			Status: r.Status, Url: r.URL, PullRequest: r.PullRequest, RequestID: request, Summary: summary, Policies: policies, Cost: cost,
			Plan: planned, StartedAt: timestamp(r.StartedAt), FinishedAt: timestamp(r.FinishedAt)})
		return err
	})
	return run, changed, err
}

// artifactPlan reads the plan ref names (artifact://<artifact>/<file>) from the artifacts of the
// pipeline run whose page is page: an Azure Pipelines results page, which names it as buildId.
func artifactPlan(ctx context.Context, repo *repositories.Repo, page, ref string) ([]byte, error) {
	rest, ok := strings.CutPrefix(ref, "artifact://")
	artifact, file, _ := strings.Cut(rest, "/")
	if !ok || artifact == "" || file == "" {
		return nil, fmt.Errorf("%q: Quai reads artifact://<artifact>/<file>", ref)
	}
	p := repo.Connector.Pipelines()
	if p == nil {
		return nil, fmt.Errorf("%q: %s has no pipelines to read it from; send the plan as plan.content", ref, repo.Name)
	}
	u, err := url.Parse(page)
	id := ""
	if err == nil {
		id = u.Query().Get("buildId")
	}
	if id == "" {
		return nil, fmt.Errorf("%q: url must be the run's page (its results page) to say which run", ref)
	}
	return p.Artifact(ctx, id, artifact, file)
}

// requestOf finds the request a run belongs to: its pull request's, or the one whose change set
// the run's commit landed.
func requestOf(ctx context.Context, s *store.Store, repository, pullRequest, commit string) (pgtype.UUID, error) {
	var id uuid.UUID
	var err error
	if pullRequest != "" {
		var d db.Delivery
		d, err = s.GetDeliveryByPullRequest(ctx, db.GetDeliveryByPullRequestParams{Repository: repository, PullRequest: pullRequest})
		id = d.RequestID
	} else {
		id, err = s.RequestAtCommit(ctx, db.RequestAtCommitParams{Repository: repository, Commit: commit})
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return pgtype.UUID{}, nil
	case err != nil:
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: id, Valid: true}, nil
}

func timestamp(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}
