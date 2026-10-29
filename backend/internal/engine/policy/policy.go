// Package policy evaluates approval rules (docs/design/05-delivery-and-approvals.md#approval-policy).
// Rules come from the repository's floor rules, the type (or the action's own rules, which
// replace the type's) and the blueprint; Match keeps those whose condition holds and merges
// those with the same approvers. Evaluate decides from the reviews: each rule needs `min`
// approvals from its eligible approvers, a requester never approves their own request, one
// person counts for one rule only unless a rule sets `distinct: false`, and one denial from an
// eligible reviewer rejects the request.
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
// the decision, such as in direct delivery.
func WithoutPlan(rules []Rule, why string) []Rule {
	out := slices.Clone(rules)
	for i, r := range out {
		if r.Undecided() {
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
	Groups   []string // the reviewer's groups when they reviewed
	Decision string
}

// Context holds what the caller resolved for the request.
type Context struct {
	Requester  string
	Owners     []string          // owners of the resources: user names and team:<name> entries
	Codeowners []string          // CODEOWNERS entries for the changed paths: user names and group:<name>
	Manager    string            // the requester's manager
	External   map[string]string // per external system: approved, rejected, or pending when absent
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
	Approvals []string // the approvers counted for the rule, sorted
	Satisfied bool     // a rule the plan says does not apply is satisfied
}

// Outcome is the decision on a request.
type Outcome struct {
	State string
	Rules []Status
}

// CanReview reports whether a person may approve or deny for a rule.
func CanReview(r Rule, name string, groups []string, ctx Context) bool {
	if name == "" || name == ctx.Requester {
		return false
	}
	member := func(entries []string) bool {
		for _, e := range entries {
			if kind, v, ok := strings.Cut(e, ":"); ok {
				if (kind == "team" || kind == "group") && slices.Contains(groups, v) || kind == "user" && v == name {
					return true
				}
			} else if e == name {
				return true
			}
		}
		return false
	}
	for _, a := range r.Approvers {
		kind, v, _ := strings.Cut(a, ":")
		switch {
		case kind == "group" && slices.Contains(groups, v),
			kind == "user" && v == name,
			a == "owners" && member(ctx.Owners),
			a == "codeowners" && member(ctx.Codeowners),
			a == "manager" && ctx.Manager != "" && ctx.Manager == name:
			return true
		}
	}
	return false
}

// Evaluate decides a request from its reviews. With no rules, a request is approved.
func Evaluate(rules []Rule, reviews []Review, ctx Context) Outcome {
	// The latest decision of each reviewer counts; the requester's never does.
	latest := map[string]Review{}
	var order []string
	for _, rv := range reviews {
		if rv.Reviewer == "" || rv.Reviewer == ctx.Requester {
			continue
		}
		if _, seen := latest[rv.Reviewer]; !seen {
			order = append(order, rv.Reviewer)
		}
		latest[rv.Reviewer] = rv
	}
	eligible := func(r Rule, rv Review) bool { return CanReview(r, rv.Reviewer, rv.Groups, ctx) }

	statuses := make([]Status, len(rules))
	for i, r := range rules {
		statuses[i].Rule = r
	}
	denied := false
	for _, r := range rules {
		if r.NotApplicable() {
			continue
		}
		if r.External != "" && ctx.External[r.External] == "rejected" {
			denied = true
		}
		for _, name := range order {
			if rv := latest[name]; rv.Decision == Deny && eligible(r, rv) {
				denied = true
			}
		}
	}
	if denied {
		return Outcome{State: Denied, Rules: statuses}
	}

	// Rules with distinct approvers share the pool of approvals: each person fills one slot of
	// one rule. A maximum matching decides who counts where, so the order of reviews or rules
	// cannot leave a rule unsatisfied when an assignment exists.
	var slots []int // the rule each slot belongs to
	for i, r := range rules {
		if r.Distinct && r.External == "" && !r.NotApplicable() {
			for range r.Min {
				slots = append(slots, i)
			}
		}
	}
	var approvers []Review
	for _, name := range order {
		if rv := latest[name]; rv.Decision == Approve {
			approvers = append(approvers, rv)
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
			if seen[s] || !eligible(rules[slots[s]], approvers[a]) {
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
			st := &statuses[slots[s]]
			st.Approvals = append(st.Approvals, approvers[a].Reviewer)
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
		case !r.Distinct:
			for _, a := range approvers {
				if eligible(r, a) {
					st.Approvals = append(st.Approvals, a.Reviewer)
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
