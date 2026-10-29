package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// RunSummary counts a plan's or an apply's resource changes.
type RunSummary struct {
	Add     int `json:"add" minimum:"0"`
	Change  int `json:"change" minimum:"0"`
	Destroy int `json:"destroy" minimum:"0"`
}

// RunPolicy is one policy check's result in a run.
type RunPolicy struct {
	Name    string `json:"name" minLength:"1"`
	Result  string `json:"result" enum:"passed,warning,failed"`
	Message string `json:"message,omitempty"`
}

// RunCost is a run's cost estimate.
type RunCost struct {
	Currency     string  `json:"currency" pattern:"^[A-Z]{3}$"`
	MonthlyDelta float64 `json:"monthlyDelta"`
}

// RunView is a run reported by a runner.
type RunView struct {
	ID            string      `json:"id"`
	Repository    string      `json:"repository"`
	Stack         string      `json:"stack"`
	Commit        string      `json:"commit"`
	PullRequest   string      `json:"pullRequest,omitempty"`
	Request       string      `json:"request,omitempty" doc:"The request the run belongs to: its pull request's, or the one whose merge commit it ran at"`
	Kind          string      `json:"kind"`
	Status        string      `json:"status"`
	URL           string      `json:"url,omitempty"`
	Summary       *RunSummary `json:"summary,omitempty"`
	Policies      []RunPolicy `json:"policies,omitempty"`
	Cost          *RunCost    `json:"cost,omitempty"`
	StartedAt     time.Time   `json:"startedAt"`
	FinishedAt    *time.Time  `json:"finishedAt,omitempty"`
	RequestNumber int64       `json:"requestNumber,omitempty"`
}

type runInput struct {
	Authorization string `header:"Authorization" required:"true" doc:"Bearer and the repository's run token"`
	Body          struct {
		Repository  string      `json:"repository"`
		Stack       string      `json:"stack"`
		Commit      string      `json:"commit" pattern:"^[0-9a-f]{7,64}$"`
		PullRequest string      `json:"pullRequest,omitempty" doc:"The pull request the run is for, as the platform numbers it"`
		Kind        string      `json:"kind" enum:"plan,apply,sync,drift-check"`
		Status      string      `json:"status" enum:"running,succeeded,failed"`
		StartedAt   *time.Time  `json:"startedAt,omitempty"`
		FinishedAt  *time.Time  `json:"finishedAt,omitempty"`
		URL         string      `json:"url,omitempty" format:"uri"`
		Summary     *RunSummary `json:"summary,omitempty"`
		Policies    []RunPolicy `json:"policies,omitempty"`
		Cost        *RunCost    `json:"cost,omitempty"`
	}
}

type runsInput struct {
	Repository string `path:"name"`
	Stack      string `query:"stack" doc:"Only this stack's runs"`
	Kind       string `query:"kind" enum:"plan,apply,sync,drift-check," doc:"Only runs of this kind"`
	Limit      int    `query:"limit" minimum:"1" maximum:"500" default:"100"`
}

func registerRuns(a huma.API, reg *repositories.Registry, s *store.Store) {
	// authz: public — runners authenticate with the repository's run token, checked here.
	// The path is auth.RunsPath.
	huma.Register(a, huma.Operation{
		OperationID: "report-run", Method: http.MethodPost, Path: Prefix + "/runs",
		Summary: "Report a plan, apply, sync or drift check (runners)", Tags: []string{"Runs"}, DefaultStatus: http.StatusAccepted,
		Description: "A report continues the running run of the same repository, stack and kind that has the same url or, when " +
			"either has none, the same commit and pull request; otherwise it is a new run.",
	}, func(ctx context.Context, in *runInput) (*out[RunView], error) {
		repo, err := reg.Get(in.Body.Repository)
		if err != nil {
			return nil, huma.Error401Unauthorized("invalid run token")
		}
		token, _ := strings.CutPrefix(in.Authorization, "Bearer ")
		want, ok, err := repo.RunToken(ctx)
		if err != nil {
			return nil, err
		}
		// The secret holds the token's SHA-256 in hex (docs/design/06-runs-status-and-drift.md),
		// never the token: what is sent is hashed and compared in constant time.
		sent := sha256.Sum256([]byte(token))
		if !ok || subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sent[:])), []byte(strings.ToLower(strings.TrimSpace(want.Reveal())))) != 1 {
			return nil, huma.Error401Unauthorized("invalid run token")
		}
		if s == nil {
			return nil, huma.Error503ServiceUnavailable("no database")
		}
		r, err := report(ctx, s, repo.Name, in)
		if err != nil {
			return nil, err
		}
		return &out[RunView]{Body: runView(r)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "list-runs", Method: http.MethodGet, Path: Prefix + "/repositories/{name}/runs",
		Summary: "List a repository's runs, newest first", Tags: []string{"Runs"},
	}, func(ctx context.Context, in *runsInput) (*out[[]RunView], error) {
		repo, err := reg.Get(in.Repository)
		if err != nil {
			return nil, huma.Error404NotFound(err.Error())
		}
		if _, err := authorize(ctx, authz.ReadInventory, authz.Object{Repository: repo.Name}); err != nil {
			return nil, err
		}
		views := []RunView{}
		if s == nil {
			return &out[[]RunView]{Body: views}, nil
		}
		runs, err := s.ListRuns(ctx, db.ListRunsParams{Repository: repo.Name, Stack: in.Stack, Kind: in.Kind, MaxRows: int32(in.Limit)}) // #nosec G115 -- bounded by the schema
		if err != nil {
			return nil, err
		}
		numbers := map[uuid.UUID]int64{}
		for _, r := range runs {
			v := runView(r)
			if r.RequestID.Valid {
				id := uuid.UUID(r.RequestID.Bytes)
				if _, ok := numbers[id]; !ok {
					if req, err := s.GetRequest(ctx, id); err == nil {
						numbers[id] = req.Number
					}
				}
				v.RequestNumber = numbers[id]
			}
			views = append(views, v)
		}
		return &out[[]RunView]{Body: views}, nil
	})
}

// report records a run report: it continues the running run it describes, or starts a run.
func report(ctx context.Context, s *store.Store, repository string, in *runInput) (db.Run, error) {
	b := in.Body
	request, err := runRequest(ctx, s, repository, b.PullRequest, b.Commit)
	if err != nil {
		return db.Run{}, err
	}
	summary, policies, cost := []byte(`{}`), []byte(`[]`), []byte(`{}`)
	if b.Summary != nil {
		summary, _ = json.Marshal(b.Summary)
	}
	if len(b.Policies) > 0 {
		policies, _ = json.Marshal(b.Policies)
	}
	if b.Cost != nil {
		cost, _ = json.Marshal(b.Cost)
	}
	var run db.Run
	err = s.Tx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		prev, err := q.RunningRun(ctx, db.RunningRunParams{Repository: repository, Stack: b.Stack, Kind: b.Kind, Url: b.URL, Commit: b.Commit, PullRequest: b.PullRequest})
		switch {
		case err == nil:
			run, err = q.UpdateRun(ctx, db.UpdateRunParams{ID: prev.ID, Status: b.Status, Url: b.URL, RequestID: request, Summary: summary,
				Policies: policies, Cost: cost, StartedAt: timestamp(b.StartedAt), FinishedAt: timestamp(b.FinishedAt)})
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		run, err = q.InsertRun(ctx, db.InsertRunParams{ID: uuid.New(), Repository: repository, Stack: b.Stack, Commit: b.Commit, Kind: b.Kind,
			Status: b.Status, Url: b.URL, PullRequest: b.PullRequest, RequestID: request, Summary: summary, Policies: policies, Cost: cost,
			StartedAt: timestamp(b.StartedAt), FinishedAt: timestamp(b.FinishedAt)})
		return err
	})
	return run, err
}

// runRequest finds the request a run belongs to: its pull request's, or the one whose change
// set the run's commit landed.
func runRequest(ctx context.Context, s *store.Store, repository, pullRequest, commit string) (pgtype.UUID, error) {
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

func runView(r db.Run) RunView {
	v := RunView{ID: r.ID.String(), Repository: r.Repository, Stack: r.Stack, Commit: r.Commit, PullRequest: r.PullRequest, Kind: r.Kind,
		Status: r.Status, URL: r.Url, StartedAt: r.StartedAt.Time}
	if r.RequestID.Valid {
		v.Request = uuid.UUID(r.RequestID.Bytes).String()
	}
	if r.FinishedAt.Valid {
		t := r.FinishedAt.Time
		v.FinishedAt = &t
	}
	var summary RunSummary
	if string(r.Summary) != "{}" && json.Unmarshal(r.Summary, &summary) == nil {
		v.Summary = &summary
	}
	_ = json.Unmarshal(r.Policies, &v.Policies)
	var cost RunCost
	if string(r.Cost) != "{}" && json.Unmarshal(r.Cost, &cost) == nil {
		v.Cost = &cost
	}
	return v
}
