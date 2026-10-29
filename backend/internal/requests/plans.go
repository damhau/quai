package requests

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/store/db"
)

// PlanReported decides the rules of a request that read the plan, from its latest succeeded plan
// of each stack, and moves the request on when that decides it
// (docs/design/05-delivery-and-approvals.md#rules-on-the-plan): approved when the last rule it
// waited for does not apply, back to pending approval when a new plan makes a rule apply that
// the approvals do not satisfy.
func (s *Service) PlanReported(ctx context.Context, id uuid.UUID) error {
	r, err := s.get(ctx, id)
	if err != nil {
		return err
	}
	if r.State != PendingApproval && r.State != Approved {
		return nil
	}
	var rules []policy.Rule
	_ = json.Unmarshal(r.Rules, &rules)
	if !slices.ContainsFunc(rules, func(r policy.Rule) bool { return r.Plan != nil }) {
		return nil
	}
	runs, err := s.store.LatestRequestRuns(ctx, db.LatestRequestRunsParams{RequestID: pgtype.UUID{Bytes: id, Valid: true}, Kind: "plan", Status: "succeeded"})
	if err != nil || len(runs) == 0 {
		return err
	}
	decided, changed, err := policy.Decide(rules, PlanVars(runs))
	if err != nil || !changed {
		return err
	}
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(decided)
	return s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := q.SetRequestRules(ctx, db.SetRequestRulesParams{ID: r.ID, Rules: b}); err != nil {
			return err
		}
		var said []map[string]any
		for _, rule := range decided {
			if rule.Plan != nil {
				d := map[string]any{"rules": rule.Names, "applies": *rule.Plan.Applies}
				if rule.Plan.Error != "" {
					d["error"] = rule.Plan.Error
				}
				said = append(said, d)
			}
		}
		if err := event(ctx, q, r, "plan_decided", "", map[string]any{"decisions": said}); err != nil {
			return err
		}
		all, err := q.ListReviews(ctx, r.ID)
		if err != nil {
			return err
		}
		outcome := policy.Evaluate(decided, toReviews(bound(all, sets[0].Hash)), policy.Context{Requester: r.Requester, Owners: r.Owners})
		switch {
		case r.State == PendingApproval && outcome.State == policy.Approved:
			if _, err := q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: PendingApproval, ToState: Approved}); err != nil {
				return conflict(err, r)
			}
			if err := event(ctx, q, r, "approved", "", nil); err != nil {
				return err
			}
			return jobs.InsertTx(ctx, s.jobs, tx, DeliverArgs{RequestID: r.ID}, nil)
		case r.State == Approved && outcome.State != policy.Approved:
			next, err := q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: Approved, ToState: PendingApproval})
			if err != nil {
				return conflict(err, r)
			}
			if err := event(ctx, q, r, "approval_needed", "", map[string]any{"reason": "a new plan makes a rule apply"}); err != nil {
				return err
			}
			return s.statusJob(ctx, tx, q, next)
		}
		return s.statusJob(ctx, tx, q, r)
	})
}

// PlanVars is what the plan variable of approval rules holds for a request's latest plans, one
// per stack: the counts summed, destroys the number of resources destroyed, the cost when every
// plan reported one in the same currency, and the policies' results.
func PlanVars(runs []db.Run) map[string]any {
	var add, change, destroy int
	var currency string
	var delta float64
	costed := len(runs) > 0
	var policies []any
	for _, r := range runs {
		var sum struct{ Add, Change, Destroy int }
		_ = json.Unmarshal(r.Summary, &sum)
		add, change, destroy = add+sum.Add, change+sum.Change, destroy+sum.Destroy
		var cost struct {
			Currency     string  `json:"currency"`
			MonthlyDelta float64 `json:"monthlyDelta"`
		}
		if json.Unmarshal(r.Cost, &cost) != nil || cost.Currency == "" || currency != "" && cost.Currency != currency {
			costed = false
		} else {
			currency, delta = cost.Currency, delta+cost.MonthlyDelta
		}
		var ps []map[string]any
		_ = json.Unmarshal(r.Policies, &ps)
		for _, p := range ps {
			policies = append(policies, map[string]any{"name": p["name"], "result": p["result"]})
		}
	}
	vars := map[string]any{"add": add, "change": change, "destroy": destroy, "destroys": destroy, "policies": policies}
	if costed {
		vars["cost"] = map[string]any{"currency": currency, "monthlyDelta": delta}
	}
	return vars
}
