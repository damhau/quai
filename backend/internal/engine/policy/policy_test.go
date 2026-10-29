package policy

import (
	"os"
	"slices"
	"testing"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/expr"
)

func examples(t *testing.T) *definitions.Repository {
	t.Helper()
	repo, problems := definitions.Load(os.DirFS("../../../../examples/gitops-repo"))
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	return repo
}

func names(rules []Rule) []string {
	var out []string
	for _, r := range rules {
		out = append(out, r.Names...)
	}
	return out
}

// Ported from the prototype's "approval policy: matching rules, thresholds, self-approval,
// denial", on the VM-first examples: a large production VM requested through linux-vm.
func TestPolicy(t *testing.T) {
	repo := examples(t)
	vm, bp := repo.Types["vm"], repo.Blueprints["linux-vm"]
	rules, err := Match(
		RuleSet{Source: SourceType, Rules: vm.Spec.Approval.Rules, Vars: expr.Vars{
			Inputs: map[string]any{"cpu": 16}, Params: map[string]any{"env": "prod", "name": "web02"},
		}},
		RuleSet{Source: SourceBlueprint, Rules: bp.Spec.Approval.Rules, Vars: expr.Vars{
			Inputs: map[string]any{"environment": "prod", "size": "large"},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	// type/large-vm and blueprint/large-vm-cost both need finops: one requirement.
	if got := names(rules); !slices.Equal(got, []string{"type/production", "type/large-vm", "blueprint/large-vm-cost"}) || len(rules) != 2 {
		t.Fatalf("rules = %v (%d)", got, len(rules))
	}
	admin := Review{Reviewer: "alice", Groups: []string{"platform-admins"}, Decision: Approve}
	finops := Review{Reviewer: "fiona", Groups: []string{"finops"}, Decision: Approve}
	ctx := Context{Requester: "bob"}
	if got := Evaluate(rules, []Review{admin}, ctx).State; got != Pending {
		t.Errorf("one of two approvals: %s", got)
	}
	out := Evaluate(rules, []Review{admin, finops}, ctx)
	if out.State != Approved || !slices.Equal(out.Rules[0].Approvals, []string{"alice"}) || !slices.Equal(out.Rules[1].Approvals, []string{"fiona"}) {
		t.Errorf("both approvals: %+v", out)
	}
	if got := Evaluate(rules, []Review{admin, finops}, Context{Requester: "alice"}).State; got != Pending {
		t.Errorf("the requester approved their own request: %s", got)
	}
	deny := finops
	deny.Decision = Deny
	if got := Evaluate(rules, []Review{admin, deny}, ctx).State; got != Denied {
		t.Errorf("a denial: %s", got)
	}
	// A denial counts only from someone who could have approved.
	stranger := Review{Reviewer: "sam", Groups: []string{"interns"}, Decision: Deny}
	if got := Evaluate(rules, []Review{admin, finops, stranger}, ctx).State; got != Approved {
		t.Errorf("a stranger's denial: %s", got)
	}
	// A reviewer's latest decision counts.
	if got := Evaluate(rules, []Review{admin, deny, finops}, ctx).State; got != Approved {
		t.Errorf("a denial changed to an approval: %s", got)
	}
	if got := Evaluate(nil, nil, ctx).State; got != Approved {
		t.Errorf("no rules: %s", got)
	}

	// In dev with a small VM, no rule applies.
	none, err := Match(
		RuleSet{Source: SourceType, Rules: vm.Spec.Approval.Rules, Vars: expr.Vars{Inputs: map[string]any{"cpu": 2}, Params: map[string]any{"env": "dev"}}},
		RuleSet{Source: SourceBlueprint, Rules: bp.Spec.Approval.Rules, Vars: expr.Vars{Inputs: map[string]any{"size": "small"}}},
	)
	if err != nil || len(none) != 0 {
		t.Errorf("dev rules = %v, %v", none, err)
	}
}

func rule(name string, approvers []string, min int, opts ...func(*definitions.Rule)) definitions.Rule {
	r := definitions.Rule{Name: name, Approvers: approvers, Min: min}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func TestDistinctApprovers(t *testing.T) {
	both := Review{Reviewer: "alice", Groups: []string{"platform-admins", "finops"}, Decision: Approve}
	fiona := Review{Reviewer: "fiona", Groups: []string{"finops"}, Decision: Approve}
	rules, err := Match(RuleSet{Source: SourceType, Rules: []definitions.Rule{
		rule("prod", []string{"group:platform-admins"}, 1),
		rule("cost", []string{"group:finops"}, 1),
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := Context{Requester: "bob"}
	if got := Evaluate(rules, []Review{both}, ctx).State; got != Pending {
		t.Errorf("one person for two rules: %s", got)
	}
	// The matching puts alice on prod and fiona on cost, in either review order.
	for _, reviews := range [][]Review{{both, fiona}, {fiona, both}} {
		if got := Evaluate(rules, reviews, ctx).State; got != Approved {
			t.Errorf("reviews %v: %s", reviews, got)
		}
	}
	notDistinct := false
	rules, _ = Match(RuleSet{Source: SourceType, Rules: []definitions.Rule{
		rule("prod", []string{"group:platform-admins"}, 1),
		rule("cost", []string{"group:finops"}, 1, func(r *definitions.Rule) { r.Distinct = &notDistinct }),
	}})
	if got := Evaluate(rules, []Review{both}, ctx); got.State != Approved || !slices.Equal(got.Rules[1].Approvals, []string{"alice"}) {
		t.Errorf("distinct: false: %+v", got)
	}
	// min: 2 needs two people.
	rules, _ = Match(RuleSet{Source: SourceFloor, Rules: []definitions.Rule{rule("two", []string{"group:finops"}, 2)}})
	if got := Evaluate(rules, []Review{both}, ctx).State; got != Pending {
		t.Errorf("min 2 with one approval: %s", got)
	}
	if got := Evaluate(rules, []Review{both, fiona}, ctx).State; got != Approved {
		t.Errorf("min 2 with two approvals: %s", got)
	}
}

func TestMatchMergesAndOrders(t *testing.T) {
	notDistinct := false
	rules, err := Match(
		RuleSet{Source: SourceFloor, Rules: []definitions.Rule{rule("production-floor", []string{"group:platform-admins"}, 1)}},
		RuleSet{Source: SourceType, Rules: []definitions.Rule{
			rule("production", []string{"group:platform-admins", "group:platform-admins"}, 2, func(r *definitions.Rule) { r.Distinct = &notDistinct }),
			rule("ticket", nil, 0, func(r *definitions.Rule) { r.External = "servicenow" }),
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || !slices.Equal(rules[0].Names, []string{"floor/production-floor", "type/production"}) || rules[0].Min != 2 || rules[0].Distinct || !slices.Equal(rules[0].Approvers, []string{"group:platform-admins"}) {
		t.Errorf("merged = %+v", rules)
	}
	if rules[1].Min != 0 || rules[1].External != "servicenow" {
		t.Errorf("external rule = %+v", rules[1])
	}
	env, _ := expr.NewEnv()
	bad, _ := env.CompileCondition("inputs.missing")
	if _, err := Match(RuleSet{Source: SourceType, Rules: []definitions.Rule{{Name: "x", Approvers: []string{"owners"}, Condition: bad}}}); err == nil {
		t.Error("a failing condition was ignored")
	}
}

func TestApproverKinds(t *testing.T) {
	ctx := Context{
		Requester:  "bob",
		Owners:     []string{"carol", "team:shop"},
		Codeowners: []string{"dave", "group:dba", "user:erin"},
		Manager:    "mia",
		External:   map[string]string{"servicenow": "approved", "jira": "rejected"},
	}
	r := func(approvers ...string) Rule { return Rule{Approvers: approvers, Min: 1, Distinct: true} }
	tests := []struct {
		rule   Rule
		name   string
		groups []string
		want   bool
	}{
		{r("user:zoe"), "zoe", nil, true},
		{r("user:zoe"), "zed", nil, false},
		{r("owners"), "carol", nil, true},
		{r("owners"), "sam", []string{"shop"}, true},
		{r("owners"), "sam", []string{"other"}, false},
		{r("codeowners"), "dave", nil, true},
		{r("codeowners"), "x", []string{"dba"}, true},
		{r("codeowners"), "erin", nil, true},
		{r("manager"), "mia", nil, true},
		{r("manager"), "bob", nil, false},
		{r("owners"), "bob", []string{"shop"}, false}, // the requester never reviews
		{r("group:a"), "", []string{"a"}, false},
	}
	for _, tt := range tests {
		if got := CanReview(tt.rule, tt.name, tt.groups, ctx); got != tt.want {
			t.Errorf("CanReview(%v, %s, %v) = %v", tt.rule.Approvers, tt.name, tt.groups, got)
		}
	}
	ext := []Rule{{Names: []string{"t/ticket"}, External: "servicenow"}}
	if got := Evaluate(ext, nil, ctx).State; got != Approved {
		t.Errorf("approved external: %s", got)
	}
	if got := Evaluate([]Rule{{External: "jira"}}, nil, ctx).State; got != Denied {
		t.Errorf("rejected external: %s", got)
	}
	if got := Evaluate([]Rule{{External: "other"}}, nil, ctx).State; got != Pending {
		t.Errorf("pending external: %s", got)
	}
	if got := Evaluate([]Rule{{Approvers: []string{"group:a"}, Min: 0, Distinct: true}}, nil, ctx).State; got != Approved {
		t.Errorf("min 0: %s", got)
	}
	ignored := []Review{{Reviewer: "", Decision: Approve}, {Reviewer: "bob", Groups: []string{"a"}, Decision: Deny}}
	if got := Evaluate([]Rule{r("group:a")}, ignored, ctx).State; got != Pending {
		t.Errorf("anonymous and self reviews: %s", got)
	}
}

// TestPlanRules: a rule whose condition reads the plan waits for one, apart from rules with the
// same approvers; approvals count for it meanwhile; the plan decides it, and each new plan again
// (docs/design/05-delivery-and-approvals.md#rules-on-the-plan).
func TestPlanRules(t *testing.T) {
	env, err := expr.NewEnv(expr.WithPlan())
	if err != nil {
		t.Fatal(err)
	}
	when := func(src string) func(*definitions.Rule) {
		return func(r *definitions.Rule) {
			r.When = src
			if r.Condition, err = env.CompileCondition(src); err != nil {
				t.Fatal(err)
			}
		}
	}
	rules, err := Match(RuleSet{Source: SourceType, Vars: expr.Vars{Params: map[string]any{"env": "prod"}}, Rules: []definitions.Rule{
		rule("production", []string{"group:finops"}, 1, when("params.env == 'prod'")),
		rule("cost", []string{"group:finops"}, 1, when("params.env == 'prod' && plan.cost.monthlyDelta > 500")),
	}})
	if err != nil || len(rules) != 2 || rules[0].Plan != nil || !rules[1].Undecided() || rules[1].Plan.Vars.Params["env"] != "prod" {
		t.Fatalf("rules = %+v, %v", rules, err)
	}
	ctx := Context{Requester: "bob"}
	fin := []Review{{Reviewer: "fiona", Groups: []string{"finops"}, Decision: Approve}}
	// One approval fills the production rule; the cost rule waits for the plan whatever it gets.
	if out := Evaluate(rules, fin, ctx); out.State != Pending || !out.Rules[0].Satisfied || out.Rules[1].Satisfied {
		t.Errorf("before the plan = %+v", out)
	}

	cheap, changed, err := Decide(rules, map[string]any{"add": 1, "change": 0, "destroy": 0, "destroys": 0, "cost": map[string]any{"currency": "CHF", "monthlyDelta": 142.4}})
	if err != nil || !changed || !cheap[1].NotApplicable() || rules[1].Plan.Applies != nil {
		t.Fatalf("a cheap plan = %+v, %v, %v", cheap[1].Plan, changed, err)
	}
	if out := Evaluate(cheap, fin, ctx); out.State != Approved || !out.Rules[1].Satisfied {
		t.Errorf("after a cheap plan = %+v", out)
	}
	if _, changed, _ := Decide(cheap, map[string]any{"cost": map[string]any{"currency": "CHF", "monthlyDelta": 100}}); changed {
		t.Error("another cheap plan changed a decision")
	}

	// A new plan that costs more: the rule applies, and needs a second finops approver.
	costly, changed, _ := Decide(cheap, map[string]any{"cost": map[string]any{"currency": "CHF", "monthlyDelta": 900}})
	if !changed || costly[1].NotApplicable() || costly[1].Undecided() {
		t.Fatalf("a costly plan = %+v", costly[1].Plan)
	}
	if out := Evaluate(costly, fin, ctx); out.State != Pending {
		t.Errorf("one approver for two rules = %+v", out)
	}
	both := append(fin, Review{Reviewer: "frank", Groups: []string{"finops"}, Decision: Approve})
	if out := Evaluate(costly, both, ctx); out.State != Approved {
		t.Errorf("two approvers = %+v", out)
	}

	// No cost reported: the condition fails, and the rule applies.
	unknown, _, _ := Decide(rules, map[string]any{"add": 1})
	if unknown[1].NotApplicable() || unknown[1].Undecided() || unknown[1].Plan.Error == "" {
		t.Errorf("no cost = %+v", unknown[1].Plan)
	}
	// A denial from a rule's approvers counts while it waits for the plan, and no longer once the
	// plan says it does not apply.
	destroys, _ := Match(RuleSet{Source: SourceType, Rules: []definitions.Rule{rule("destroys", []string{"group:security"}, 1, when("plan.destroys > 0"))}})
	sam := []Review{{Reviewer: "sam", Groups: []string{"security"}, Decision: Deny}}
	if out := Evaluate(destroys, sam, ctx); out.State != Denied {
		t.Errorf("a denial before the plan = %+v", out)
	}
	none, _, _ := Decide(destroys, map[string]any{"destroys": 0})
	if out := Evaluate(none, sam, ctx); out.State != Approved {
		t.Errorf("a denial on a rule that does not apply = %+v", out)
	}
}

// TestPlanRulesDecidedEarly: a condition reading the plan waits for one only when its result
// depends on it.
func TestPlanRulesDecidedEarly(t *testing.T) {
	env, err := expr.NewEnv(expr.WithPlan())
	if err != nil {
		t.Fatal(err)
	}
	rule := func(name, when string) definitions.Rule {
		c, err := env.CompileCondition(when)
		if err != nil {
			t.Fatal(err)
		}
		return definitions.Rule{Name: name, When: when, Condition: c, Approvers: []string{"group:finops"}}
	}
	rules := []definitions.Rule{
		rule("prod-cost", "params.env == 'prod' && plan.cost.monthlyDelta > 500"),
		rule("dev-or-no-destroys", "params.env == 'dev' || plan.destroys == 0"),
		rule("costed", "has(plan.cost) && plan.cost.monthlyDelta > 500"),
	}
	got, err := Match(RuleSet{Source: SourceType, Rules: rules, Vars: expr.Vars{Params: map[string]any{"env": "dev"}}})
	if err != nil {
		t.Fatal(err)
	}
	// In dev: the first does not apply, the second applies whatever the plan, the third waits.
	if len(got) != 2 || got[0].Names[0] != "type/dev-or-no-destroys" || got[0].Plan != nil || !got[1].Undecided() {
		t.Errorf("in dev = %+v", got)
	}
	got, _ = Match(RuleSet{Source: SourceType, Rules: rules, Vars: expr.Vars{Params: map[string]any{"env": "prod"}}})
	if len(got) != 3 || !got[0].Undecided() || !got[1].Undecided() || !got[2].Undecided() {
		t.Errorf("in prod, all three wait: %+v", got)
	}
}
