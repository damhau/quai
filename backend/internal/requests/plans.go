package requests

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

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
// the approvals do not satisfy. The requireTwo rule applies while a stack of the request has no
// plan yet or its latest plan failed (docs/design/05-delivery-and-approvals.md#separation-of-duties).
// A request pending approval whose repository's separation of duties became stricter goes stale
// instead.
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
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	if err != nil || len(sets) == 0 {
		return err
	}
	if why := s.separationTightened(r, sets[0].Repository); why != "" && r.State == PendingApproval {
		_, err := s.transition(ctx, r, PendingApproval, Stale, "stale", map[string]any{"separation": why})
		return err
	}
	decided, _, err := policy.Decide(rules, PlanVars(runs))
	if err != nil {
		return err
	}
	failed, err := s.store.LatestRequestRuns(ctx, db.LatestRequestRunsParams{RequestID: pgtype.UUID{Bytes: id, Valid: true}, Kind: "plan", Status: "failed"})
	if err != nil {
		return err
	}
	decided = undecidedPlans(decided, r.Stacks, runs, failed)
	if !planChanged(rules, decided) {
		return nil
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
		if err := s.event(ctx, tx, r, "plan_decided", "", map[string]any{"decisions": said}); err != nil {
			return err
		}
		all, err := q.ListReviews(ctx, r.ID)
		if err != nil {
			return err
		}
		outcome := policy.Evaluate(decided, toReviews(counted(r, all, sets[0].Hash), s.dir), contextOf(r))
		switch {
		case r.State == PendingApproval && outcome.State == policy.Approved:
			if _, err := q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: PendingApproval, ToState: Approved}); err != nil {
				return conflict(err, r)
			}
			if err := s.event(ctx, tx, r, "approved", "", nil); err != nil {
				return err
			}
			details := approvedDetails(sets[0].Hash, r, counted(r, all, sets[0].Hash))
			details["reason"] = "the plan makes no rule that needs more approvals apply"
			if err := recordRequest(ctx, tx, r, "approved", details); err != nil {
				return err
			}
			return jobs.InsertTx(ctx, s.jobs, tx, DeliverArgs{RequestID: r.ID}, nil)
		case r.State == Approved && outcome.State != policy.Approved && !broken(r):
			// A request broken by break-glass merges without its approvals, whatever a new plan
			// of the same content makes apply.
			next, err := q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: Approved, ToState: PendingApproval})
			if err != nil {
				return conflict(err, r)
			}
			if err := s.event(ctx, tx, r, "approval_needed", "", map[string]any{"reason": "a new plan makes a rule apply"}); err != nil {
				return err
			}
			return s.statusJob(ctx, tx, q, next)
		}
		return s.statusJob(ctx, tx, q, r)
	})
}

// undecidedPlans makes the requireTwo rule apply while what the request destroys is unknown, as
// before any plan (open question 73): a stack of the request has no succeeded plan yet, or its
// latest finished plan failed (one started no earlier than its latest succeeded one).
func undecidedPlans(rules []policy.Rule, stacks []string, succeeded, failed []db.Run) []policy.Rule {
	var why []string
	for _, st := range stacks {
		if !slices.ContainsFunc(succeeded, func(r db.Run) bool { return r.Stack == st }) &&
			!slices.ContainsFunc(failed, func(r db.Run) bool { return r.Stack == st }) {
			why = append(why, "no plan of "+st+" yet")
		}
	}
	for _, f := range failed {
		i := slices.IndexFunc(succeeded, func(r db.Run) bool { return r.Stack == f.Stack })
		if i < 0 || !f.StartedAt.Time.Before(succeeded[i].StartedAt.Time) {
			why = append(why, "the latest plan of "+f.Stack+" failed")
		}
	}
	if len(why) == 0 {
		return rules
	}
	slices.Sort(why)
	for i, r := range rules {
		if r.Overall && r.Plan != nil {
			pc, applies := *r.Plan, true
			pc.Applies, pc.Error = &applies, strings.Join(why, "; ")+": two approvers until a plan shows that nothing is destroyed"
			rules[i].Plan = &pc
		}
	}
	return rules
}

// planChanged reports whether a decision of the rules that read the plan differs.
func planChanged(before, after []policy.Rule) bool {
	for i, a := range after {
		b := before[i]
		if a.Plan == nil || b.Plan == nil {
			continue
		}
		if (a.Plan.Applies == nil) != (b.Plan.Applies == nil) || a.Plan.Applies != nil && *a.Plan.Applies != *b.Plan.Applies || a.Plan.Error != b.Plan.Error {
			return true
		}
	}
	return false
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
