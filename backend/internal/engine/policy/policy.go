// Package policy evaluates approval rules (docs/design/05-delivery-and-approvals.md#approval-policy).
// Rules come from the repository's floor rules, the type (or the action's own rules, which
// replace the type's) and the blueprint; Match keeps those whose condition holds and merges
// those with the same approvers. Evaluate decides from the reviews: each rule needs `min`
// approvals from its eligible approvers, a requester never approves their own request, nor does
// a definition author the separation of duties excludes, one person counts for one rule only
// unless a rule sets `distinct: false`, and one denial from an eligible reviewer rejects the
// request. An overall rule (the repository's destroys: requireTwo) counts different people
// across the request's rules. A delegate reviews with the delegator's rights (Delegated), and a
// review on someone's behalf is both people's: one human makes one approval (Standing).
//
// Who belongs to a group, owns a resource or manages the requester is resolved by the caller and
// passed in; the package only decides.
package policy

import (
	"fmt"
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/expr"
)

// Rule sources, in the order rules are shown.
const (
	SourceFloor     = "floor"
	SourceType      = "type"
	SourceBlueprint = "blueprint"
)

// RuleSet is the rules of one source, with the variables their conditions see.
type RuleSet struct {
	Source string
	Rules  []definitions.Rule
	Vars   expr.Vars
}

// Rule is a requirement of a request: one or more definition rules with the same approvers.
type Rule struct {
	Names     []string // "source/name" of the merged rules
	Approvers []string
	Min       int
	External  string
	Distinct  bool
	// Plan is the condition of a rule that reads the plan, decided when a plan has reported. Such
	// a rule is never merged with another.
	Plan *PlanCondition `json:",omitempty"`
	// Overall makes the rule count the different people whose approvals count for the request's
	// other rules (those that apply, not an external system's), whatever rule each counts for, and
	// distinct or not: Min of them. When no other rule takes approvals from people, its own
	// approvers count. It takes no place in the other rules' matching
	// (docs/design/05-delivery-and-approvals.md#separation-of-duties).
	Overall bool `json:",omitempty"`
}

// PlanCondition is the condition of a rule that reads the plan (plan.*): undecided until a plan
// has reported, and decided again with each new plan
// (docs/design/05-delivery-and-approvals.md#rules-on-the-plan).
type PlanCondition struct {
	When    string
	Vars    expr.Vars // what the condition sees besides the plan
	Applies *bool     // nil until a plan has reported
	Error   string    // why the condition could not be evaluated; the rule then applies
}

// The destroys: requireTwo rule's name, and why it applies before a plan says otherwise.
const (
	RequireTwoName = "separationOfDuties/destroys"
	NoPlanYet      = "no plan yet: two approvers until a plan shows that nothing is destroyed"
)

// RequireTwo is the rule of the repository's separationOfDuties.destroys: requireTwo
// (docs/design/05-delivery-and-approvals.md#separation-of-duties): two different people among
// the request's approvers when its plan destroys something. Without a plan it applies, unlike a
// rule waiting for the plan: Decide decides it again with each plan. Its approvers count when no
// other rule takes approvals from people.
func RequireTwo(approvers []string) Rule {
	applies := true
	return Rule{Names: []string{RequireTwoName}, Approvers: approvers, Min: 2, Distinct: true, Overall: true,
		Plan: &PlanCondition{When: "plan.destroys > 0", Applies: &applies, Error: NoPlanYet}}
}

// Undecided reports whether a rule waits for a plan to say whether it applies.
func (r Rule) Undecided() bool { return r.Plan != nil && r.Plan.Applies == nil }

// NotApplicable reports whether the plan says the rule does not apply.
func (r Rule) NotApplicable() bool { return r.Plan != nil && r.Plan.Applies != nil && !*r.Plan.Applies }

// Match returns the rules that apply: those whose condition holds, merged when they have the
// same approvers and external system, keeping the highest min. A rule whose condition reads the
// plan, when there is none yet, is kept undecided, apart.
func Match(sets ...RuleSet) ([]Rule, error) {
	var out []Rule
	for _, set := range sets {
		for _, r := range set.Rules {
			// A condition reading the plan, when there is none yet, waits for it only if its
			// result depends on it.
			deferred := false
			if r.Condition != nil && r.Condition.Uses(expr.VarPlan) && set.Vars.Plan == nil {
				ok, known, err := r.Condition.Decided(set.Vars, expr.VarPlan)
				if err != nil {
					return nil, fmt.Errorf("approval rule %s/%s: %w", set.Source, r.Name, err)
				}
				if known && !ok {
					continue
				}
				deferred = !known
			} else if r.Condition != nil {
				ok, err := r.Condition.Test(set.Vars)
				if err != nil {
					return nil, fmt.Errorf("approval rule %s/%s: %w", set.Source, r.Name, err)
				}
				if !ok {
					continue
				}
			}
			approvers := slices.Sorted(slices.Values(r.Approvers))
			approvers = slices.Compact(approvers)
			min := r.Min
			if min == 0 && r.External == "" {
				min = 1
			}
			distinct := r.Distinct == nil || *r.Distinct
			name := set.Source + "/" + r.Name
			if deferred {
				out = append(out, Rule{Names: []string{name}, Approvers: approvers, Min: min, External: r.External, Distinct: distinct,
					Plan: &PlanCondition{When: r.When, Vars: set.Vars}})
				continue
			}
			i := slices.IndexFunc(out, func(o Rule) bool {
				return o.Plan == nil && slices.Equal(o.Approvers, approvers) && o.External == r.External
			})
			if i < 0 {
				out = append(out, Rule{Names: []string{name}, Approvers: approvers, Min: min, External: r.External, Distinct: distinct})
				continue
			}
			out[i].Names = append(out[i].Names, name)
			out[i].Min = max(out[i].Min, min)
			out[i].Distinct = out[i].Distinct && distinct
		}
	}
	return out, nil
}

// Decide evaluates the conditions of the rules that read the plan, with plan the summary the
// plan variable holds. It returns the rules and whether a decision changed. A condition that
// cannot be evaluated, such as one reading a cost the runner did not report, makes its rule
// apply.
func Decide(rules []Rule, plan map[string]any) ([]Rule, bool, error) {
	env, err := expr.NewEnv(expr.WithPlan())
	if err != nil {
		return nil, false, err
	}
	out, changed := slices.Clone(rules), false
	for i, r := range out {
		if r.Plan == nil {
			continue
		}
		pc := *r.Plan
		applies, msg := true, ""
		prg, err := env.CompileCondition(pc.When)
		if err == nil {
			vars := pc.Vars
			vars.Plan = plan
			applies, err = prg.Test(vars)
		}
		if err != nil {
			applies, msg = true, err.Error()
		}
		if pc.Applies == nil || *pc.Applies != applies || pc.Error != msg {
			changed = true
		}
		pc.Applies, pc.Error = &applies, msg
		out[i].Plan = &pc
	}
	return out, changed, nil
}

// WithoutPlan makes the rules that wait for a plan apply, for why: where no plan can come before
// the decision, such as in direct delivery. The requireTwo rule, which applies until a plan says
// otherwise, says why too.
func WithoutPlan(rules []Rule, why string) []Rule {
	out := slices.Clone(rules)
	for i, r := range out {
		if r.Undecided() || r.Overall && r.Plan != nil && r.Plan.Error == NoPlanYet {
			pc, applies := *r.Plan, true
			pc.Applies, pc.Error = &applies, why
			out[i].Plan = &pc
		}
	}
	return out
}

// Review decisions.
const (
	Approve = "approve"
	Deny    = "deny"
)

// Review is one reviewer's decision.
type Review struct {
	Reviewer string
	Email    string   // the reviewer's email when they reviewed, which email: entries match
	Groups   []string // the reviewer's groups when they reviewed, which group: entries match
	Teams    []string // the reviewer's teams when they reviewed, which team: entries match
	Decision string
	// OnBehalfOf is the person whose approvals the reviewer held through a delegation and reviewed
	// for, as the directory described them when the review was made; nil for a review in one's own
	// right. The review then counts with their rights, never the reviewer's own, under the
	// reviewer's own checks (Delegated), and it is both people's review: one human, one approval
	// (docs/design/05-delivery-and-approvals.md#delegation).
	OnBehalfOf *Person
}

// people are the names a review is the review of: its reviewer's, and the delegator's for a
// review on someone's behalf.
func (rv Review) people() []string {
	if rv.OnBehalfOf != nil {
		return []string{rv.Reviewer, rv.OnBehalfOf.Name}
	}
	return []string{rv.Reviewer}
}

// Person is someone who may review: their name, email (empty when unknown), IdP groups and Quai
// teams.
type Person struct {
	Name, Email   string
	Groups, Teams []string
}

// Context holds what the caller resolved for the request.
type Context struct {
	Requester  string
	Owners     []string          // owners of the resources: user names and team:, group:, user: or email: entries
	Codeowners []string          // the platform's owners of the changed paths, as matched: user:, group: or email: entries
	Manager    string            // the requester's manager, as submission resolved them; empty: none
	External   map[string]string // per external system: approved, rejected, or pending when absent
	// DefinitionAuthors are the people who changed the request's definitions in the period
	// before it (separationOfDuties.definitionAuthors), and DefinitionAuthorEmails every email
	// those changes carry: like the requester, none of them reviews it, including someone who
	// gets one of the emails as a verified email later.
	DefinitionAuthors      []string
	DefinitionAuthorEmails []string
}

// definitionAuthor reports whether the separation of duties keeps a person from reviewing.
func (c Context) definitionAuthor(who Person) bool {
	return slices.Contains(c.DefinitionAuthors, who.Name) ||
		who.Email != "" && slices.ContainsFunc(c.DefinitionAuthorEmails, func(e string) bool { return strings.EqualFold(e, who.Email) })
}

// States of an outcome.
const (
	Approved = "approved"
	Pending  = "pending"
	Denied   = "denied"
)

// Status is how far one rule is.
type Status struct {
	Rule      Rule
	Approvals []string // the approvers counted for the rule, sorted: the people who reviewed
	// OnBehalfOf maps an approver of Approvals who approved for someone else, through a delegation,
	// to that person.
	OnBehalfOf map[string]string `json:",omitempty"`
	Satisfied  bool              // a rule the plan says does not apply is satisfied
}

// Outcome is the decision on a request.
type Outcome struct {
	State string
	Rules []Status
}

// CanReview reports whether a person may approve or deny for a rule by its approvers: never the
// requester, nor one of the request's definition authors. Owner entries match them by name
// (user:), group (group:), team (team:) or email (email:). For an overall rule, use Eligible.
func CanReview(r Rule, who Person, ctx Context) bool {
	if who.Name == "" || who.Name == ctx.Requester || ctx.definitionAuthor(who) {
		return false
	}
	member := func(entries []string) bool {
		for _, e := range entries {
			if kind, v, ok := strings.Cut(e, ":"); ok {
				if kind == "group" && slices.Contains(who.Groups, v) || kind == "team" && slices.Contains(who.Teams, v) ||
					kind == "user" && v == who.Name || kind == "email" && who.Email != "" && strings.EqualFold(v, who.Email) {
					return true
				}
			} else if e == who.Name {
				return true
			}
		}
		return false
	}
	for _, a := range r.Approvers {
		kind, v, _ := strings.Cut(a, ":")
		switch {
		case kind == "group" && slices.Contains(who.Groups, v),
			kind == "user" && v == who.Name,
			a == "owners" && member(ctx.Owners),
			a == "codeowners" && member(ctx.Codeowners),
			// Never the requester as their own manager (four eyes).
			a == "manager" && ctx.Manager != "" && ctx.Manager != ctx.Requester && ctx.Manager == who.Name:
			return true
		}
	}
	return false
}

// Eligible reports whether a person may approve or deny for the i-th of a request's rules:
// CanReview, and for an overall rule, CanReview for one of the other rules that take approvals
// from people and apply (one waiting for the plan does), or by its own approvers when there is none.
func Eligible(rules []Rule, i int, who Person, ctx Context) bool {
	if !rules[i].Overall {
		return CanReview(rules[i], who, ctx)
	}
	others := false
	for j, o := range rules {
		if j == i || o.Overall || o.External != "" || o.NotApplicable() {
			continue
		}
		others = true
		if CanReview(o, who, ctx) {
			return true
		}
	}
	return !others && CanReview(rules[i], who, ctx)
}

// Delegated reports whether a delegate may approve or deny for the i-th of a request's rules on a
// delegator's behalf: the delegator is Eligible, and the delegate passes the checks no delegation
// lifts, being neither the requester, nor one of the request's definition authors, nor the
// delegator. The delegate's own rights play no part: a delegation passes on the delegator's rights,
// never what the delegator holds on someone else's behalf.
func Delegated(rules []Rule, i int, delegate, delegator Person, ctx Context) bool {
	if delegate.Name == "" || delegator.Name == "" || delegate.Name == delegator.Name ||
		delegate.Name == ctx.Requester || ctx.definitionAuthor(delegate) {
		return false
	}
	return Eligible(rules, i, delegator, ctx)
}

// Standing returns the positions of the reviews that count, in order of their reviewers' first
// reviews: a review stands while it is the latest review of every person it is the review of, its
// reviewer's and, on someone's behalf, the delegator's. So one human makes one approval: a
// delegate's review for someone replaces the delegate's own review and the delegator's, and either
// one's later review replaces it. The requester's reviews, in their own right or on their behalf,
// never count.
func Standing(reviews []Review, requester string) []int {
	last := map[string]int{}
	first := map[string]int{}
	var kept []int
	for i, rv := range reviews {
		if rv.Reviewer == "" || rv.OnBehalfOf != nil && rv.OnBehalfOf.Name == "" || slices.Contains(rv.people(), requester) {
			continue
		}
		kept = append(kept, i)
		if _, ok := first[rv.Reviewer]; !ok {
			first[rv.Reviewer] = i
		}
		for _, p := range rv.people() {
			last[p] = i
		}
	}
	var out []int
	for _, i := range kept {
		if !slices.ContainsFunc(reviews[i].people(), func(p string) bool { return last[p] != i }) {
			out = append(out, i)
		}
	}
	slices.SortStableFunc(out, func(a, b int) int { return first[reviews[a].Reviewer] - first[reviews[b].Reviewer] })
	return out
}

// Evaluate decides a request from its reviews. With no rules, a request is approved.
func Evaluate(rules []Rule, reviews []Review, ctx Context) Outcome {
	// The latest decision of each person counts (Standing); the requester's never does.
	var standing []Review
	for _, i := range Standing(reviews, ctx.Requester) {
		standing = append(standing, reviews[i])
	}
	eligible := func(i int, rv Review) bool {
		who := Person{Name: rv.Reviewer, Email: rv.Email, Groups: rv.Groups, Teams: rv.Teams}
		if rv.OnBehalfOf != nil {
			return Delegated(rules, i, who, *rv.OnBehalfOf, ctx)
		}
		return Eligible(rules, i, who, ctx)
	}

	statuses := make([]Status, len(rules))
	for i, r := range rules {
		statuses[i].Rule = r
	}
	denied := false
	for i, r := range rules {
		if r.NotApplicable() {
			continue
		}
		if r.External != "" && ctx.External[r.External] == "rejected" {
			denied = true
		}
		for _, rv := range standing {
			if rv.Decision == Deny && eligible(i, rv) {
				denied = true
			}
		}
	}
	if denied {
		return Outcome{State: Denied, Rules: statuses}
	}

	// Rules with distinct approvers share the pool of approvals: each person fills one slot of
	// one rule. A maximum matching decides who counts where, so the order of reviews or rules
	// cannot leave a rule unsatisfied when an assignment exists. An overall rule counts people
	// apart.
	var slots []int // the rule each slot belongs to
	for i, r := range rules {
		if r.Distinct && r.External == "" && !r.Overall && !r.NotApplicable() {
			for range r.Min {
				slots = append(slots, i)
			}
		}
	}
	var approvers []Review
	for _, rv := range standing {
		if rv.Decision == Approve {
			approvers = append(approvers, rv)
		}
	}
	counts := func(st *Status, rv Review) {
		st.Approvals = append(st.Approvals, rv.Reviewer)
		if rv.OnBehalfOf != nil {
			if st.OnBehalfOf == nil {
				st.OnBehalfOf = map[string]string{}
			}
			st.OnBehalfOf[rv.Reviewer] = rv.OnBehalfOf.Name
		}
	}
	slotOf := make([]int, len(approvers)) // approver → slot, or -1
	holder := make([]int, len(slots))     // slot → approver, or -1
	for i := range slotOf {
		slotOf[i] = -1
	}
	for i := range holder {
		holder[i] = -1
	}
	var augment func(a int, seen []bool) bool
	augment = func(a int, seen []bool) bool {
		for s := range slots {
			if seen[s] || !eligible(slots[s], approvers[a]) {
				continue
			}
			seen[s] = true
			if holder[s] < 0 || augment(holder[s], seen) {
				holder[s], slotOf[a] = a, s
				return true
			}
		}
		return false
	}
	for a := range approvers {
		augment(a, make([]bool, len(slots)))
	}
	for s, a := range holder {
		if a >= 0 {
			counts(&statuses[slots[s]], approvers[a])
		}
	}

	state := Approved
	for i, r := range rules {
		st := &statuses[i]
		switch {
		case r.NotApplicable():
			st.Satisfied = true
		case r.External != "":
			st.Satisfied = ctx.External[r.External] == "approved"
		case !r.Distinct || r.Overall:
			// Each person once: approvers holds one review per person (Standing).
			for _, a := range approvers {
				if eligible(i, a) {
					counts(st, a)
				}
			}
			st.Satisfied = len(st.Approvals) >= r.Min
		default:
			st.Satisfied = len(st.Approvals) >= r.Min
		}
		slices.Sort(st.Approvals)
		// Approvals count for a rule that waits for the plan, but it is not satisfied before.
		if r.Undecided() {
			st.Satisfied = false
		}
		if !st.Satisfied {
			state = Pending
		}
	}
	return Outcome{State: state, Rules: statuses}
}
