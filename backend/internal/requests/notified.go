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
	// review of the current content.
	People []db.User
}

// Awaiting returns the rules of a request pending approval that wait for approvals from people:
// not satisfied, not an external system's, not ruled out by the plan (a rule still waiting for the
// plan is one), each with who may approve it, from the directory: the people who signed in, as
// their last login described them. Nothing when the request is not pending approval.
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
	for _, u := range users {
		if u.DeactivatedAt.Valid || reviewed[u.Name] {
			continue
		}
		p := policy.Person{Name: u.Name, Email: u.Email, Groups: u.Groups, Teams: s.dir.Teams(u.Groups)}
		for i := range awaiting {
			if policy.Eligible(rules, awaiting[i].Index, p, pc) {
				awaiting[i].People = append(awaiting[i].People, u)
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
