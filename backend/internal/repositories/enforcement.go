package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Enforcement (docs/design/01-repository-connections.md#health): in pull-request mode, the target
// branch must require the check (quai/approval) as Quai needs it, or approvals are not enforced
// and Quai opens no pull request on the repository. It is verified at registration (the first
// time Quai needs it), every EnforcementInterval, and when a branch policy changes.

// EnforcementInterval is how often enforcement is verified besides.
const EnforcementInterval = time.Hour

// ErrNotEnforced is returned when Quai must not open pull requests on a repository.
var ErrNotEnforced = errors.New("approvals are not enforced")

// EnforcementState is a repository's enforcement as last verified.
type EnforcementState struct {
	Required  bool // pull-request delivery
	Checked   bool
	Problems  []string // what is missing; none when enforced
	CheckedAt time.Time
}

// Err returns ErrNotEnforced with what is missing, or nil when Quai may open pull requests.
func (e EnforcementState) Err(repository string) error {
	if !e.Required || len(e.Problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w on %s: %s. Quai opens no pull request on it until this is fixed", ErrNotEnforced, repository, strings.Join(e.Problems, "; "))
}

// VerifyEnforcement asks the platform whether the target branch requires the check: what is
// missing, none when enforced or not required.
func (repo *Repo) VerifyEnforcement(ctx context.Context) ([]string, error) {
	spec := repo.Document.Spec
	if spec.Delivery.Mode != "pull-request" {
		return nil, nil
	}
	pol := repo.Connector.Policies()
	if pol == nil {
		return []string{fmt.Sprintf("the %s connector cannot verify that %s requires %s", repo.Connector.Info().Platform, repo.Branch, spec.Enforcement.RequiredCheck)}, nil
	}
	e, err := pol.Verify(ctx, repo.Branch, spec.Enforcement.RequiredCheck)
	if err != nil {
		return nil, fmt.Errorf("enforcement on %s: %w", repo.Branch, err)
	}
	if e.Enforced {
		return nil, nil
	}
	return e.Problems, nil
}

// CheckEnforcement verifies the repository's enforcement and records it. A change is an event on
// the repository's timeline: alert_enforcement when it is lost (or what is missing changes),
// enforcement_restored when it is back.
func CheckEnforcement(ctx context.Context, s *store.Store, repo *Repo) (EnforcementState, error) {
	problems, err := repo.VerifyEnforcement(ctx)
	if err != nil {
		return EnforcementState{}, err
	}
	if problems == nil {
		problems = []string{}
	}
	required := repo.Document.Spec.Delivery.Mode == "pull-request"
	err = s.Tx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		before, err := q.GetRepositorySync(ctx, repo.Name)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var was []string
		_ = json.Unmarshal(before.EnforcementProblems, &was)
		b, err := json.Marshal(problems)
		if err != nil {
			return err
		}
		if err := q.RecordEnforcement(ctx, db.RecordEnforcementParams{Repository: repo.Name, EnforcementProblems: b}); err != nil {
			return err
		}
		kind := ""
		switch {
		case !required || slices.Equal(was, problems):
		case len(problems) > 0:
			kind = "alert_enforcement"
		case before.EnforcementCheckedAt.Valid:
			kind = "enforcement_restored"
		}
		if kind == "" {
			return nil
		}
		data, err := json.Marshal(map[string]any{"branch": repo.Branch, "check": repo.Document.Spec.Enforcement.RequiredCheck, "problems": problems})
		if err != nil {
			return err
		}
		_, err = q.AddEvent(ctx, db.AddEventParams{RequestID: pgtype.UUID{}, Target: "repository:" + repo.Name, Kind: kind, Data: data})
		return err
	})
	if err != nil {
		return EnforcementState{}, err
	}
	return EnforcementState{Required: required, Checked: true, Problems: problems, CheckedAt: time.Now()}, nil
}

// EnforcementOf returns the repository's enforcement as last verified, verifying it first if it
// never was. Without a database, it is verified each time.
func EnforcementOf(ctx context.Context, s *store.Store, repo *Repo) (EnforcementState, error) {
	if repo.Document.Spec.Delivery.Mode != "pull-request" {
		return EnforcementState{Problems: []string{}}, nil
	}
	if s == nil {
		problems, err := repo.VerifyEnforcement(ctx)
		return EnforcementState{Required: true, Checked: err == nil, Problems: problems, CheckedAt: time.Now()}, err
	}
	row, err := s.GetRepositorySync(ctx, repo.Name)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return EnforcementState{}, err
	}
	if !row.EnforcementCheckedAt.Valid {
		return CheckEnforcement(ctx, s, repo)
	}
	e := EnforcementState{Required: true, Checked: true, Problems: []string{}, CheckedAt: row.EnforcementCheckedAt.Time}
	_ = json.Unmarshal(row.EnforcementProblems, &e.Problems)
	return e, nil
}
