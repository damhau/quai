package policy

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/engine/definitions"
)

var (
	alice = Person{Name: "alice", Email: "alice@example.com", Groups: []string{"platform-admins"}}
	fiona = Person{Name: "fiona", Email: "fiona@example.com", Groups: []string{"finops"}}
	gina  = Person{Name: "gina", Groups: []string{"interns"}}
)

// review is who's decision, on behalf of a delegator when one is given.
func review(who Person, decision string, onBehalfOf ...Person) Review {
	rv := Review{Reviewer: who.Name, Email: who.Email, Groups: who.Groups, Teams: who.Teams, Decision: decision}
	if len(onBehalfOf) > 0 {
		rv.OnBehalfOf = &onBehalfOf[0]
	}
	return rv
}

func platformRules(t *testing.T, min int) []Rule {
	t.Helper()
	rules, err := Match(RuleSet{Source: SourceType, Rules: []definitions.Rule{rule("production", []string{"group:platform-admins"}, min)}})
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

// TestDelegateWithinTheRange: alice hands her approvals to fiona for a week. Within it, fiona
// approves what alice could, on her behalf, and the approval says so; before it, at its end and
// after it, or once revoked, fiona has only her own rights, which do not reach the rule.
func TestDelegateWithinTheRange(t *testing.T) {
	rules := platformRules(t, 1)
	ctx := Context{Requester: "bob"}
	start := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	d := Delegation{Delegator: "alice", Delegate: "fiona", Starts: start, Ends: start.Add(7 * 24 * time.Hour)}
	for _, c := range []struct {
		name   string
		at     time.Time
		active bool
	}{
		{"at the start", start, true},
		{"within", start.Add(3 * 24 * time.Hour), true},
		{"just before the end", d.Ends.Add(-time.Nanosecond), true},
		{"before", start.Add(-time.Second), false},
		{"at the end", d.Ends, false},
		{"after", d.Ends.Add(time.Hour), false},
	} {
		if got := d.Active(c.at); got != c.active {
			t.Errorf("%s: active = %t", c.name, got)
		}
		// The service makes a review on alice's behalf only while the delegation is active.
		rv := review(fiona, Approve)
		if d.Active(c.at) {
			rv = review(fiona, Approve, alice)
		}
		out := Evaluate(rules, []Review{rv}, ctx)
		if c.active != (out.State == Approved) {
			t.Errorf("%s: %+v", c.name, out)
		}
		if c.active && (!slices.Equal(out.Rules[0].Approvals, []string{"fiona"}) || !maps.Equal(out.Rules[0].OnBehalfOf, map[string]string{"fiona": "alice"})) {
			t.Errorf("%s: approvals %v on behalf of %v", c.name, out.Rules[0].Approvals, out.Rules[0].OnBehalfOf)
		}
	}
	revoked := d
	revoked.Revoked = true
	if revoked.Active(start.Add(time.Hour)) {
		t.Error("a revoked delegation is active")
	}
	for _, bad := range []Delegation{{Delegator: "alice", Delegate: "alice", Starts: d.Starts, Ends: d.Ends}, {Delegator: "alice", Starts: d.Starts, Ends: d.Ends}} {
		if bad.Active(start.Add(time.Hour)) {
			t.Errorf("%+v is active", bad)
		}
	}
	// A denial on alice's behalf rejects, as hers would.
	if out := Evaluate(rules, []Review{review(fiona, Deny, alice)}, ctx); out.State != Denied {
		t.Errorf("a denial on alice's behalf: %+v", out)
	}
	// Without the delegation fiona's own denial is a stranger's.
	if out := Evaluate(rules, []Review{review(fiona, Deny)}, ctx); out.State != Pending {
		t.Errorf("fiona's own denial: %+v", out)
	}
}

// TestDelegateRefused: a delegation never lifts the checks of the delegate, nor adds to the
// delegator's rights: no review on behalf on the delegate's own request, nor on the delegator's,
// nor from a definition author, nor for a delegator who could not review.
func TestDelegateRefused(t *testing.T) {
	rules := platformRules(t, 1)
	for _, c := range []struct {
		name     string
		ctx      Context
		who      Person
		onBehalf Person
	}{
		{"the delegate's own request", Context{Requester: "fiona"}, fiona, alice},
		{"the delegator's request", Context{Requester: "alice"}, fiona, alice},
		{"the delegate changed the definitions", Context{Requester: "bob", DefinitionAuthors: []string{"fiona"}}, fiona, alice},
		{"the delegate's email changed the definitions", Context{Requester: "bob", DefinitionAuthorEmails: []string{"FIONA@example.com"}}, fiona, alice},
		{"the delegator changed the definitions", Context{Requester: "bob", DefinitionAuthors: []string{"alice"}}, fiona, alice},
		{"the delegator cannot review", Context{Requester: "bob"}, alice, fiona},
		{"oneself", Context{Requester: "bob"}, alice, alice},
		{"nobody", Context{Requester: "bob"}, Person{}, alice},
	} {
		if Delegated(rules, 0, c.who, c.onBehalf, c.ctx) {
			t.Errorf("%s: Delegated", c.name)
		}
		for _, decision := range []string{Approve, Deny} {
			if out := Evaluate(rules, []Review{review(c.who, decision, c.onBehalf)}, c.ctx); out.State != Pending {
				t.Errorf("%s: %s = %+v", c.name, decision, out)
			}
		}
	}
	if !Delegated(rules, 0, fiona, alice, Context{Requester: "bob"}) {
		t.Error("fiona may not review on alice's behalf a request bob made")
	}
}

// TestDelegationCountsOnce: one human makes one approval. A review on someone's behalf is the
// delegate's and the delegator's: it replaces either one's earlier review, and either one's later
// review replaces it, so two approvals never come from one person through a delegation, with min:
// 2, across distinct rules, or for requireTwo.
func TestDelegationCountsOnce(t *testing.T) {
	ctx := Context{Requester: "bob"}
	carol := Person{Name: "carol", Groups: []string{"platform-admins"}}
	fionaAdmin := Person{Name: "fiona", Groups: []string{"platform-admins", "finops"}}
	two := platformRules(t, 2)
	for _, c := range []struct {
		name    string
		reviews []Review
		state   string
		counted []string
	}{
		{"the delegate, then for the delegator", []Review{review(fionaAdmin, Approve), review(fionaAdmin, Approve, alice)}, Pending, []string{"fiona"}},
		{"for the delegator, then the delegate", []Review{review(fionaAdmin, Approve, alice), review(fionaAdmin, Approve)}, Pending, []string{"fiona"}},
		{"for the delegator, then the delegator", []Review{review(fiona, Approve, alice), review(alice, Approve)}, Pending, []string{"alice"}},
		{"the delegator, then for the delegator", []Review{review(alice, Approve), review(fiona, Approve, alice)}, Pending, []string{"fiona"}},
		{"twice for the delegator", []Review{review(fiona, Approve, alice), review(gina, Approve, alice)}, Pending, []string{"gina"}},
		{"for the delegator, and someone else", []Review{review(fiona, Approve, alice), review(carol, Approve)}, Approved, []string{"carol", "fiona"}},
	} {
		out := Evaluate(two, c.reviews, ctx)
		if out.State != c.state || !slices.Equal(out.Rules[0].Approvals, c.counted) {
			t.Errorf("min 2, %s: %+v", c.name, out)
		}
	}
	// A delegate's later denial for themselves withdraws their approval on the delegator's behalf.
	if out := Evaluate(platformRules(t, 1), []Review{review(fiona, Approve, alice), review(fiona, Deny)}, ctx); out.State != Pending {
		t.Errorf("an approval on behalf, then the delegate's own denial: %+v", out)
	}

	// Distinct rules: fiona fills one slot, for alice or for herself, never both.
	distinct, err := Match(RuleSet{Source: SourceType, Rules: []definitions.Rule{
		rule("production", []string{"group:platform-admins"}, 1),
		rule("cost", []string{"group:finops"}, 1),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if out := Evaluate(distinct, []Review{review(fiona, Approve), review(fiona, Approve, alice)}, ctx); out.State != Pending {
		t.Errorf("distinct rules from one person through a delegation: %+v", out)
	}
	finops := Person{Name: "frank", Groups: []string{"finops"}}
	if out := Evaluate(distinct, []Review{review(fiona, Approve, alice), review(finops, Approve)}, ctx); out.State != Approved ||
		!slices.Equal(out.Rules[0].Approvals, []string{"fiona"}) || out.Rules[0].OnBehalfOf["fiona"] != "alice" || !slices.Equal(out.Rules[1].Approvals, []string{"frank"}) {
		t.Errorf("distinct rules from two people: %+v", out)
	}

	// requireTwo counts different people: the delegate and the delegator are one.
	withTwo := append(platformRules(t, 1), RequireTwo([]string{"owners"}))
	for _, reviews := range [][]Review{
		{review(alice, Approve), review(fiona, Approve, alice)},
		{review(fiona, Approve, alice), review(alice, Approve)},
		{review(fionaAdmin, Approve), review(fionaAdmin, Approve, alice)},
	} {
		if out := Evaluate(withTwo, reviews, ctx); out.State != Pending || len(out.Rules[1].Approvals) != 1 {
			t.Errorf("requireTwo from one person through a delegation %v: %+v", reviews, out)
		}
	}
	if out := Evaluate(withTwo, []Review{review(fiona, Approve, alice), review(carol, Approve)}, ctx); out.State != Approved {
		t.Errorf("requireTwo from two people: %+v", out)
	}
}

// TestNoChains: a delegate reviews with the delegator's own rights. alice delegated to fiona and
// fiona to gina: gina's review on fiona's behalf has fiona's rights, finops, never alice's.
func TestNoChains(t *testing.T) {
	rules := platformRules(t, 1)
	ctx := Context{Requester: "bob"}
	if Delegated(rules, 0, gina, fiona, ctx) {
		t.Error("gina reviews with alice's rights through fiona")
	}
	if out := Evaluate(rules, []Review{review(gina, Approve, fiona)}, ctx); out.State != Pending {
		t.Errorf("a chain: %+v", out)
	}
	// A review on behalf that names nobody counts for nobody.
	if out := Evaluate(rules, []Review{review(fiona, Approve, Person{})}, ctx); out.State != Pending {
		t.Errorf("on behalf of nobody: %+v", out)
	}
}

func TestBounds(t *testing.T) {
	now := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	for _, c := range []struct {
		name           string
		starts, ends   time.Time
		start          time.Time
		startErr, wErr bool
	}{
		{"now for a week", now, now.Add(7 * day), now, false, false},
		{"tomorrow for 90 days", now.Add(day), now.Add(91 * day), now.Add(day), false, false},
		{"a skewed clock", now.Add(-30 * time.Second), now.Add(day), now, false, false},
		{"in the past", now.Add(-2 * time.Minute), now.Add(day), time.Time{}, true, false},
		{"ended", now.Add(-2 * day), now.Add(-day), time.Time{}, true, false},
		{"over 90 days", now, now.Add(90*day + time.Second), time.Time{}, false, true},
		{"backwards", now.Add(2 * day), now.Add(day), time.Time{}, false, true},
		{"empty", now.Add(day), now.Add(day), time.Time{}, false, true},
		{"over a year ahead", now.Add(366 * day), now.Add(367 * day), time.Time{}, true, false},
	} {
		start, _, fields := Bounds(c.starts, c.ends, now)
		if _, bad := fields["startsAt"]; bad != c.startErr {
			t.Errorf("%s: starts: %v", c.name, fields)
		}
		if _, bad := fields["endsAt"]; bad != c.wErr {
			t.Errorf("%s: ends: %v", c.name, fields)
		}
		if fields == nil && !start.Equal(c.start) {
			t.Errorf("%s: starts at %s", c.name, start)
		}
	}
}

func TestConflict(t *testing.T) {
	now := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	week := func(delegator, delegate string, from int) Delegation {
		return Delegation{Delegator: delegator, Delegate: delegate, Starts: now.Add(time.Duration(from) * day), Ends: now.Add(time.Duration(from+7) * day)}
	}
	d := week("alice", "fiona", 0)
	for _, c := range []struct {
		name     string
		others   []Delegation
		conflict bool
	}{
		{"nothing else", nil, false},
		{"the delegator delegates for part of it", []Delegation{week("alice", "gina", 6)}, true},
		{"the delegator delegates afterwards", []Delegation{week("alice", "gina", 7)}, false},
		{"revoked", []Delegation{{Delegator: "alice", Delegate: "gina", Starts: now, Ends: now.Add(day), Revoked: true}}, false},
		{"a cycle", []Delegation{week("fiona", "alice", 3)}, true},
		{"the delegate is away", []Delegation{week("fiona", "gina", -3)}, true},
		{"the delegate is away before", []Delegation{week("fiona", "gina", -7)}, false},
		// The delegator holds someone else's approvals: what fiona gets is alice's own.
		{"the delegator holds another's", []Delegation{week("gina", "alice", 0)}, false},
		{"the delegate holds another's", []Delegation{week("gina", "fiona", 0)}, false},
	} {
		if got := Conflict(d, c.others); (got != "") != c.conflict {
			t.Errorf("%s: %q", c.name, got)
		}
	}
}
