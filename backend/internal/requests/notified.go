package requests

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Awaiting is a rule of a request pending approval that waits for approvals from people, and the
// people who may give them (docs/design/05-delivery-and-approvals.md#notifications).
type Awaiting struct {
	Index int // the rule's position among the request's rules
	Rule  policy.Rule
	// People are the directory's people who may approve or deny for the rule: not deactivated,
	// not the requester nor a definition author the separation of duties excludes, and without a
	// review of the current content. A delegator whose delegate may review for them now is not
	// among them: their delegate is, in their place (For).
	People []db.User
	// For maps those of People who are there in a delegator's place, not in their own right, to
	// the delegator (docs/design/05-delivery-and-approvals.md#delegation).
	For map[string]string
}

// WaitsFor says whether a request waits for someone's approval, and on whose behalf they would
// give it: "" when they may give it in their own right for one of the rules.
func WaitsFor(awaiting []Awaiting, name string) (onBehalfOf string, waits bool) {
	for _, a := range awaiting {
		for _, u := range a.People {
			if u.Name != name {
				continue
			}
			if f := a.For[name]; f == "" {
				return "", true
			} else if !waits {
				onBehalfOf, waits = f, true
			}
		}
	}
	return onBehalfOf, waits
}

// Awaiting returns the rules of a request pending approval that wait for approvals from people:
// not satisfied, not an external system's, not ruled out by the plan (a rule still waiting for the
// plan is one), each with who may approve it, from the directory: the people who signed in, as
// their last login described them. A delegator's place goes to the delegate of their delegation
// active now, when the delegate may review the request for them (policy.Delegated), is not
// deactivated and has not reviewed it; it goes no further, whatever the delegate delegated in
// turn. Nothing when the request is not pending approval.
func (s *Service) Awaiting(ctx context.Context, r db.Request) ([]Awaiting, error) {
	if r.State != PendingApproval {
		return nil, nil
	}
	reviews, outcome, err := s.Reviews(ctx, r)
	if err != nil {
		return nil, err
	}
	reviewed := map[string]bool{}
	for _, v := range reviews {
		reviewed[v.Reviewer] = true
		if v.OnBehalfOf != "" {
			reviewed[v.OnBehalfOf] = true
		}
	}
	var awaiting []Awaiting
	for i, st := range outcome.Rules {
		if !st.Satisfied && st.Rule.External == "" && !st.Rule.NotApplicable() {
			awaiting = append(awaiting, Awaiting{Index: i, Rule: st.Rule})
		}
	}
	if len(awaiting) == 0 {
		return nil, nil
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	pc := contextOf(r)
	rules := make([]policy.Rule, len(outcome.Rules))
	for i, st := range outcome.Rules {
		rules[i] = st.Rule
	}
	// The delegate of each delegator, for the delegations active now.
	byName := map[string]db.User{}
	for _, u := range users {
		byName[u.Name] = u
	}
	active, err := s.store.ActiveDelegations(ctx, instant(s.now()))
	if err != nil {
		return nil, err
	}
	delegateOf := map[string]db.User{}
	for _, d := range active {
		if u, ok := byName[d.Delegate]; ok && delegationOf(d).Active(s.now()) && !u.DeactivatedAt.Valid && !reviewed[u.Name] {
			if _, dup := delegateOf[d.Delegator]; !dup {
				delegateOf[d.Delegator] = u
			}
		}
	}
	for i := range awaiting {
		own, forOf := map[string]bool{}, map[string]string{}
		for _, u := range users {
			if u.DeactivatedAt.Valid || reviewed[u.Name] {
				continue
			}
			p := s.person(u)
			if !policy.Eligible(rules, awaiting[i].Index, p, pc) {
				continue
			}
			if d, ok := delegateOf[u.Name]; ok && policy.Delegated(rules, awaiting[i].Index, s.person(d), p, pc) {
				if _, taken := forOf[d.Name]; !taken {
					forOf[d.Name] = u.Name
				}
				continue
			}
			own[u.Name] = true
		}
		for _, u := range users {
			switch f, delegated := forOf[u.Name]; {
			case own[u.Name]:
				awaiting[i].People = append(awaiting[i].People, u)
			case delegated:
				awaiting[i].People = append(awaiting[i].People, u)
				if awaiting[i].For == nil {
					awaiting[i].For = map[string]string{}
				}
				awaiting[i].For[u.Name] = f
			}
		}
	}
	return awaiting, nil
}

// UseReach says whom notifications reach: reach reports whether a person has an address on a
// configured channel (a verified email, an Entra ID object ID for Teams) and, for a kind of
// notification (not ""), has not turned it off there. Without it, no channel is configured and
// Unnotified says nothing.
func (s *Service) UseReach(reach func(ctx context.Context, u db.User, kind string) (bool, error)) {
	s.reach = reach
}

// Unnotified says whom the notifications of a request do not reach: whether its requester (no
// address on any channel), and the positions of the rules that wait for approvals no one who may
// give them would be told of (no address, or approvals needed turned off on every channel they
// have one). ok is false when no notification channel is configured.
func (s *Service) Unnotified(ctx context.Context, r db.Request) (requester bool, rules []int, ok bool, err error) {
	if s.reach == nil {
		return false, nil, false, nil
	}
	u, err := s.store.GetUser(ctx, r.Requester)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		requester = true
	case err != nil:
		return false, nil, false, err
	case u.DeactivatedAt.Valid:
		requester = true
	default:
		reached, err := s.reach(ctx, u, "")
		if err != nil {
			return false, nil, false, err
		}
		requester = !reached
	}
	awaiting, err := s.Awaiting(ctx, r)
	if err != nil {
		return false, nil, false, err
	}
	for _, a := range awaiting {
		reached := false
		for _, p := range a.People {
			if reached, err = s.reach(ctx, p, "approval_needed"); err != nil {
				return false, nil, false, err
			}
			if reached {
				break
			}
		}
		if !reached {
			rules = append(rules, a.Index)
		}
	}
	return requester, rules, true, nil
}
