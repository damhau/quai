package policy

import (
	"slices"
	"testing"

	"github.com/damhau/quai/backend/internal/engine/definitions"
)

// TestDefinitionAuthors: a definition author the separation of duties excludes never reviews,
// like the requester: their approval does not count, their denial does not reject, and they are
// no rule's approver, whatever their groups.
func TestDefinitionAuthors(t *testing.T) {
	rules, err := Match(RuleSet{Source: SourceType, Rules: []definitions.Rule{rule("platform", []string{"group:platform", "owners"}, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := Context{Requester: "bob", Owners: []string{"dora"}, DefinitionAuthors: []string{"dora"}}
	dora := Person{Name: "dora", Groups: []string{"platform"}}
	if CanReview(rules[0], dora, ctx) || Eligible(rules, 0, dora, ctx) {
		t.Error("a definition author may review")
	}
	if !CanReview(rules[0], dora, Context{Requester: "bob"}) {
		t.Error("dora may not review a request whose definitions she did not change")
	}
	approves := []Review{{Reviewer: "dora", Groups: []string{"platform"}, Decision: Approve}}
	if out := Evaluate(rules, approves, ctx); out.State != Pending || len(out.Rules[0].Approvals) != 0 {
		t.Errorf("a definition author's approval = %+v", out)
	}
	denies := []Review{{Reviewer: "dora", Groups: []string{"platform"}, Decision: Deny}}
	if out := Evaluate(rules, denies, ctx); out.State != Pending {
		t.Errorf("a definition author's denial = %+v", out)
	}
	alice := append(approves, Review{Reviewer: "alice", Groups: []string{"platform"}, Decision: Approve})
	if out := Evaluate(rules, alice, ctx); out.State != Approved || !slices.Equal(out.Rules[0].Approvals, []string{"alice"}) {
		t.Errorf("another approver = %+v", out)
	}
	// distinct: false lets one person count for several rules, never an excluded one for any.
	loose := false
	rules, _ = Match(RuleSet{Source: SourceType, Rules: []definitions.Rule{rule("platform", []string{"group:platform"}, 1, func(r *definitions.Rule) { r.Distinct = &loose })}})
	if out := Evaluate(rules, approves, ctx); out.State != Pending {
		t.Errorf("a definition author's approval for a distinct: false rule = %+v", out)
	}
}

// TestRequireTwo: with destroys: requireTwo, a request whose plan destroys something needs two
// different people among its approvers, never the requester or a definition author, and one
// person never counts twice, distinct: false or not. Without a plan the rule applies; a plan
// that destroys nothing makes it not apply, and each new plan decides again.
func TestRequireTwo(t *testing.T) {
	platform := rule("platform", []string{"group:platform"}, 1)
	rules, err := Match(RuleSet{Source: SourceType, Rules: []definitions.Rule{platform}})
	if err != nil {
		t.Fatal(err)
	}
	rules = append(rules, RequireTwo([]string{"owners"}))
	ctx := Context{Requester: "bob", Owners: []string{"olga"}, DefinitionAuthors: []string{"dora"}}
	review := func(name, decision string, groups ...string) Review {
		return Review{Reviewer: name, Groups: groups, Decision: decision}
	}
	alice := review("alice", Approve, "platform")
	carl := review("carl", Approve, "platform")
	// The requester, a definition author, an owner who is no other rule's approver, and alice
	// approving twice: one person.
	one := []Review{alice, review("bob", Approve, "platform"), review("dora", Approve, "platform"), review("olga", Approve), alice}

	// No plan yet: treated as destroying.
	if r := rules[1]; r.Undecided() || r.NotApplicable() || r.Plan.Error != NoPlanYet {
		t.Fatalf("the rule before a plan = %+v", r.Plan)
	}
	if out := Evaluate(rules, one, ctx); out.State != Pending || !out.Rules[0].Satisfied || out.Rules[1].Satisfied ||
		!slices.Equal(out.Rules[1].Approvals, []string{"alice"}) {
		t.Errorf("one approver before a plan = %+v", out)
	}
	if out := Evaluate(rules, append(one, carl), ctx); out.State != Approved || !slices.Equal(out.Rules[1].Approvals, []string{"alice", "carl"}) {
		t.Errorf("two approvers before a plan = %+v", out)
	}

	// Where no plan can come, the rule says so, and applies.
	if direct := WithoutPlan(rules, "no plan comes before a direct commit"); direct[1].NotApplicable() ||
		direct[1].Plan.Error != "no plan comes before a direct commit" || rules[1].Plan.Error != NoPlanYet {
		t.Errorf("without a plan = %+v", direct[1].Plan)
	}

	// A plan that destroys nothing: one approver is enough.
	safe, changed, err := Decide(rules, map[string]any{"add": 2, "destroy": 0, "destroys": 0})
	if err != nil || !changed || !safe[1].NotApplicable() || safe[1].Plan.Error != "" {
		t.Fatalf("a plan that destroys nothing = %+v, %v, %v", safe[1].Plan, changed, err)
	}
	if out := Evaluate(safe, one, ctx); out.State != Approved {
		t.Errorf("one approver, nothing destroyed = %+v", out)
	}

	// A new plan that destroys: two again; the approvals given stand, as for any rule on the plan.
	destroying, changed, _ := Decide(safe, map[string]any{"destroy": 1, "destroys": 1})
	if !changed || destroying[1].NotApplicable() || destroying[1].Undecided() || destroying[1].Plan.Error != "" {
		t.Fatalf("a plan that destroys = %+v", destroying[1].Plan)
	}
	if out := Evaluate(destroying, one, ctx); out.State != Pending {
		t.Errorf("one approver, something destroyed = %+v", out)
	}
	if out := Evaluate(destroying, append(one, carl), ctx); out.State != Approved {
		t.Errorf("two approvers, something destroyed = %+v", out)
	}

	// distinct: false lets alice count for both rules; requireTwo still wants a second person.
	loose := false
	nd := func(r *definitions.Rule) { r.Distinct = &loose }
	two, _ := Match(RuleSet{Source: SourceType, Rules: []definitions.Rule{
		rule("platform", []string{"group:platform"}, 1, nd), rule("network", []string{"group:platform", "group:network"}, 1, nd),
	}})
	if out := Evaluate(two, one, ctx); out.State != Approved {
		t.Fatalf("distinct: false without requireTwo = %+v", out)
	}
	two = append(two, RequireTwo([]string{"owners"}))
	if out := Evaluate(two, one, ctx); out.State != Pending {
		t.Errorf("distinct: false, one person for requireTwo = %+v", out)
	}
	if out := Evaluate(two, append(one, review("nina", Approve, "network")), ctx); out.State != Approved {
		t.Errorf("distinct: false, two people = %+v", out)
	}

	// Approvers of no rule that applies do not count: a rule the plan rules out, an external one.
	withExternal := append(slices.Clone(destroying[:1]), Rule{Names: []string{"type/cab"}, External: "servicenow"}, destroying[1])
	ext := ctx
	ext.External = map[string]string{"servicenow": "approved"}
	if out := Evaluate(withExternal, append(one, review("sven", Approve, "servicenow")), ext); out.State != Pending {
		t.Errorf("an external approval counts as a person = %+v", out)
	}
	no := false
	ruledOut := append(slices.Clone(destroying[:1]), Rule{Names: []string{"type/cost"}, Approvers: []string{"group:finops"}, Min: 1, Distinct: true,
		Plan: &PlanCondition{When: "plan.cost.monthlyDelta > 500", Applies: &no}}, destroying[1])
	if out := Evaluate(ruledOut, append(one, review("fiona", Approve, "finops")), ctx); out.State != Pending {
		t.Errorf("the approver of a rule the plan rules out counts = %+v", out)
	}

	// No other rule takes approvals from people: the rule's own approvers count, two of them.
	alone := []Rule{RequireTwo([]string{"owners"})}
	owners := Context{Requester: "bob", Owners: []string{"olga", "oscar", "dora"}, DefinitionAuthors: []string{"dora"}}
	olga := review("olga", Approve)
	if !Eligible(alone, 0, Person{Name: "olga"}, owners) || Eligible(alone, 0, Person{Name: "alice", Groups: []string{"platform"}}, owners) ||
		Eligible(alone, 0, Person{Name: "dora"}, owners) {
		t.Error("eligibility without other rules")
	}
	if out := Evaluate(alone, []Review{olga, alice, review("dora", Approve)}, owners); out.State != Pending {
		t.Errorf("one owner = %+v", out)
	}
	if out := Evaluate(alone, []Review{olga, review("oscar", Approve)}, owners); out.State != Approved {
		t.Errorf("two owners = %+v", out)
	}
	if out := Evaluate(alone, []Review{olga, review("oscar", Deny)}, owners); out.State != Denied {
		t.Errorf("an owner denies = %+v", out)
	}
	// With other rules, the rule's own approvers do not count: olga is only an owner.
	if Eligible(rules, 1, Person{Name: "olga"}, ctx) || !Eligible(rules, 1, Person{Name: "alice", Groups: []string{"platform"}}, ctx) {
		t.Error("eligibility with other rules")
	}
	if out := Evaluate(rules, []Review{review("olga", Deny)}, ctx); out.State != Pending {
		t.Errorf("a denial from someone who approves for no rule = %+v", out)
	}
}
