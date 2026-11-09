package requests

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/store/db"
)

// TestUndecidedPlans: the requireTwo rule applies while a stack of the request has no plan yet or
// its latest plan failed, whatever the succeeded plans of its other stacks say; a failed plan
// older than the stack's latest succeeded one no longer counts, one started at the same time
// does.
func TestUndecidedPlans(t *testing.T) {
	at := func(minute int) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: time.Date(2026, 11, 9, 10, minute, 0, 0, time.UTC), Valid: true}
	}
	decided, _, err := policy.Decide([]policy.Rule{policy.RequireTwo([]string{"owners"})}, map[string]any{"destroys": 0})
	if err != nil || !decided[0].NotApplicable() {
		t.Fatalf("a plan that destroys nothing = %+v, %v", decided[0].Plan, err)
	}
	run := func(stack string, minute int) db.Run { return db.Run{Stack: stack, StartedAt: at(minute)} }
	for name, c := range map[string]struct {
		stacks            []string
		succeeded, failed []db.Run
		want              string // the rule's error; "" when the plans decide it
	}{
		"every stack planned":       {[]string{"a", "b"}, []db.Run{run("a", 1), run("b", 1)}, nil, ""},
		"a stack without a plan":    {[]string{"a", "b"}, []db.Run{run("a", 1)}, nil, "no plan of b yet: two approvers until a plan shows that nothing is destroyed"},
		"a failed plan after":       {[]string{"a"}, []db.Run{run("a", 1)}, []db.Run{run("a", 2)}, "the latest plan of a failed: two approvers until a plan shows that nothing is destroyed"},
		"a failed plan at the same": {[]string{"a"}, []db.Run{run("a", 1)}, []db.Run{run("a", 1)}, "the latest plan of a failed: two approvers until a plan shows that nothing is destroyed"},
		"a failed plan before":      {[]string{"a"}, []db.Run{run("a", 2)}, []db.Run{run("a", 1)}, ""},
		"only a failed plan":        {[]string{"a", "b"}, []db.Run{run("a", 1)}, []db.Run{run("b", 1)}, "the latest plan of b failed: two approvers until a plan shows that nothing is destroyed"},
		"no stack known":            {nil, []db.Run{run("a", 1)}, nil, ""},
	} {
		rules := []policy.Rule{decided[0]}
		got := undecidedPlans(rules, c.stacks, c.succeeded, c.failed)[0]
		if c.want == "" && (!got.NotApplicable() || got.Plan.Error != "") || c.want != "" && (got.NotApplicable() || got.Plan.Error != c.want) {
			t.Errorf("%s: %+v (applies %v)", name, got.Plan, *got.Plan.Applies)
		}
	}
	// Other rules on the plan are left as the plans decided them.
	cost := policy.Rule{Names: []string{"type/cost"}, Plan: &policy.PlanCondition{When: "plan.cost.monthlyDelta > 500", Applies: new(bool)}}
	if got := undecidedPlans([]policy.Rule{cost}, []string{"a"}, nil, nil); *got[0].Plan.Applies || got[0].Plan.Error != "" {
		t.Errorf("another rule = %+v", got[0].Plan)
	}
}
