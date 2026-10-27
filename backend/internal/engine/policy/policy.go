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
}

// Match returns the rules that apply: those whose condition holds, merged when they have the
// same approvers and external system, keeping the highest min.
func Match(sets ...RuleSet) ([]Rule, error) {
	var out []Rule
	for _, set := range sets {
		for _, r := range set.Rules {
			if r.Condition != nil {
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
			i := slices.IndexFunc(out, func(o Rule) bool {
				return slices.Equal(o.Approvers, approvers) && o.External == r.External
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
	Satisfied bool
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
		if r.Distinct && r.External == "" {
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
		if !st.Satisfied {
			state = Pending
		}
	}
	return Outcome{State: state, Rules: statuses}
}
