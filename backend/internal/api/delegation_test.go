package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/chatlink"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/giteatest"
	"github.com/damhau/quai/backend/internal/mailtest"
	"github.com/damhau/quai/backend/internal/store/db"
	"github.com/damhau/quai/backend/internal/teamstest"
)

// delegate hands the caller's approvals to someone through the API: the status, the raw answer
// and the delegation made.
func (u *user) delegate(to string, starts, ends time.Time) (int, string, DelegationView) {
	u.h.t.Helper()
	var d DelegationView
	status, raw := u.do(http.MethodPost, Prefix+"/me/delegations", map[string]any{"delegate": to, "startsAt": starts, "endsAt": ends}, &d)
	return status, raw, d
}

// mustDelegate is delegate, failing the test unless the delegation is made.
func (u *user) mustDelegate(to string, starts, ends time.Time) DelegationView {
	u.h.t.Helper()
	status, raw, d := u.delegate(to, starts, ends)
	if status != http.StatusCreated {
		u.h.t.Fatalf("delegate to %s = %d %s", to, status, raw)
	}
	return d
}

// delegations lists the delegations the caller gave or received.
func (u *user) delegations() DelegationsOutput {
	u.h.t.Helper()
	var out DelegationsOutput
	if status, raw := u.do(http.MethodGet, Prefix+"/me/delegations", nil, &out.Body); status != http.StatusOK {
		u.h.t.Fatalf("delegations = %d %s", status, raw)
	}
	return out
}

// reviewFor reviews a request on someone's behalf ("" in one's own right): the status, the raw
// answer and the request after.
func (u *user) reviewFor(id, onBehalfOf, decision string) (int, string, RequestView) {
	u.h.t.Helper()
	body := map[string]string{"decision": decision}
	if onBehalfOf != "" {
		body["onBehalfOf"] = onBehalfOf
	}
	if decision == "deny" {
		body["comment"] = "not this week"
	}
	var r RequestView
	status, raw := u.do(http.MethodPost, Prefix+"/requests/"+id+"/review", body, &r)
	return status, raw, r
}

// inboxOf returns the IDs of the requests of someone's inbox.
func (u *user) inboxOf() []string {
	u.h.t.Helper()
	var inbox []RequestView
	if status, raw := u.do(http.MethodGet, Prefix+"/requests?view=inbox", nil, &inbox); status != http.StatusOK {
		u.h.t.Fatalf("inbox = %d %s", status, raw)
	}
	var ids []string
	for _, r := range inbox {
		ids = append(ids, r.ID)
	}
	return ids
}

// bearer sends a request with an API token: the status and the raw answer.
func bearer(t *testing.T, h *harness, token, method, path string, body any) (int, string) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestDelegation: an approver hands their approvals to a colleague for a period, in a browser
// session, from now for at most 90 days: never to themselves, to someone who never signed in or
// was deactivated, nor to someone who delegated their own (no cycles); one delegate at a time; no
// field names anyone else, and an API token can do none of it. During the period the delegate
// reviews on the delegator's behalf what the delegator could review: never their own request, nor
// the delegator's. The review names both people on the request, its timeline, the audit log and
// the commit's trailers. The delegator revokes it at any time, as may a platform admin, nobody
// else; before its start and from its end it gives nothing.
func TestDelegation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	h.clock.set(now)
	alice, bob, fiona, carol := h.as("alice"), h.as("bob"), h.as("fiona"), h.as("carol")
	week := now.Add(7 * 24 * time.Hour)

	if _, err := h.store.Pool.Exec(context.Background(), "UPDATE users SET deactivated_at = now() WHERE name = 'carol'"); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		to           string
		starts, ends time.Time
		field        string
	}{
		"nobody":         {"zed", now, week, "body.delegate"},
		"oneself":        {"alice", now, week, "body.delegate"},
		"deactivated":    {"carol", now, week, "body.delegate"},
		"91 days":        {"fiona", now, now.Add(91 * 24 * time.Hour), "body.endsAt"},
		"backwards":      {"fiona", week, now, "body.endsAt"},
		"in the past":    {"fiona", now.Add(-time.Hour), week, "body.startsAt"},
		"years from now": {"fiona", now.AddDate(2, 0, 0), now.AddDate(2, 0, 7), "body.startsAt"},
	} {
		status, raw, _ := alice.delegate(tt.to, tt.starts, tt.ends)
		if status != http.StatusUnprocessableEntity || !strings.Contains(raw, `"location":"`+tt.field+`"`) {
			t.Errorf("%s = %d %s, want 422 on %s", name, status, raw, tt.field)
		}
	}
	if _, err := h.store.Pool.Exec(context.Background(), "UPDATE users SET deactivated_at = NULL WHERE name = 'carol'"); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]map[string]any{
		"a delegator": {"delegate": "fiona", "startsAt": now, "endsAt": week, "delegator": "bob"},
		"a user":      {"delegate": "fiona", "startsAt": now, "endsAt": week, "user": "bob"},
	} {
		if status, raw := alice.do(http.MethodPost, Prefix+"/me/delegations", body, nil); status != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d %s, want 422: no field names someone else", name, status, raw)
		}
	}
	for _, path := range []string{"/users/bob/delegations", "/me/delegations/bob"} {
		if status, _ := alice.do(http.MethodPost, Prefix+path, map[string]any{"delegate": "fiona", "startsAt": now, "endsAt": week}, nil); status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d: no path names someone else", path, status)
		}
	}
	if status, _ := h.as("").do(http.MethodPost, Prefix+"/me/delegations", map[string]any{"delegate": "fiona", "startsAt": now, "endsAt": week}, nil); status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Errorf("anonymous = %d", status)
	}
	var tok CreatedTokenOutput
	if status, raw := alice.do(http.MethodPost, Prefix+"/tokens", map[string]any{"name": "all", "scopes": []string{"catalog:read", "inventory:read", "requests:read", "requests:write"}},
		&tok.Body); status != http.StatusCreated {
		t.Fatalf("token = %d %s", status, raw)
	}
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/me/delegations", map[string]any{"delegate": "fiona", "startsAt": now, "endsAt": week}},
		{http.MethodGet, "/me/delegations", nil},
		{http.MethodGet, "/delegations", nil},
	} {
		if status, raw := bearer(t, h, tok.Body.Token, c.method, Prefix+c.path, c.body); status != http.StatusForbidden || !strings.Contains(raw, "scopes") {
			t.Errorf("%s %s with a token = %d %s, want 403", c.method, c.path, status, raw)
		}
	}

	// alice delegates to fiona for a week from now.
	d := alice.mustDelegate("fiona", now, week)
	if d.Delegator != "alice" || d.Delegate != "fiona" || !d.StartsAt.Equal(now) || !d.EndsAt.Equal(week) || !d.Active {
		t.Errorf("delegation = %+v", d)
	}
	ev, personal := h.audited("delegation.created", d.ID)
	byPerson(t, ev[0], personal, "alice")
	if personal[ev[0].Details["delegator"].(string)] != "alice" || personal[ev[0].Details["delegate"].(string)] != "fiona" ||
		ev[0].Details["starts"] != now.Format(audit.AtFormat) || ev[0].Details["ends"] != week.Format(audit.AtFormat) {
		t.Errorf("created = %+v", ev[0].Details)
	}
	// One delegate at a time; no cycle.
	if status, raw, _ := alice.delegate("carol", now.Add(24*time.Hour), week.Add(24*time.Hour)); status != http.StatusConflict || !strings.Contains(raw, "revoke that delegation first") {
		t.Errorf("a second delegate = %d %s", status, raw)
	}
	if status, raw, _ := fiona.delegate("alice", now, week); status != http.StatusConflict {
		t.Errorf("fiona back to alice = %d %s", status, raw)
	}
	if status, raw, _ := carol.delegate("alice", now, week); status != http.StatusConflict || !strings.Contains(raw, "alice has delegated their own approvals") {
		t.Errorf("carol to alice, who delegated hers = %d %s", status, raw)
	}
	// Both see it, nobody else; platform admins list everyone's.
	if l := alice.delegations().Body; len(l.Delegations) != 1 || l.Delegations[0].ID != d.ID || l.MaxDays != 90 || l.StepUp {
		t.Errorf("alice's = %+v", l)
	}
	if l := fiona.delegations().Body.Delegations; len(l) != 1 || l[0].ID != d.ID {
		t.Errorf("fiona's = %+v", l)
	}
	if l := bob.delegations().Body.Delegations; len(l) != 0 {
		t.Errorf("bob's = %+v", l)
	}
	var all DelegationsOutput
	if status, raw := alice.do(http.MethodGet, Prefix+"/delegations", nil, &all.Body); status != http.StatusOK || len(all.Body.Delegations) != 1 {
		t.Errorf("everyone's = %d %s", status, raw)
	}
	if status, _ := bob.do(http.MethodGet, Prefix+"/delegations", nil, nil); status != http.StatusForbidden {
		t.Errorf("everyone's for bob = %d", status)
	}

	// bob's production VM waits for a platform admin: fiona, none, may review it for alice.
	r := bob.submit(newVM("prod", "deleg-01", 2))
	v := fiona.view(r.ID)
	if !v.CanReview || v.CanReviewOwn || len(v.ReviewFor) != 1 || v.ReviewFor[0].Delegator != "alice" || v.ReviewFor[0].Delegation != d.ID || !v.ReviewFor[0].Until.Equal(week) {
		t.Errorf("fiona's view = %v %v %+v", v.CanReview, v.CanReviewOwn, v.ReviewFor)
	}
	if !slices.Contains(fiona.inboxOf(), r.ID) || slices.Contains(carol.inboxOf(), r.ID) {
		t.Errorf("inboxes: fiona %v, carol %v", fiona.inboxOf(), carol.inboxOf())
	}
	for _, c := range []struct {
		who        *user
		onBehalfOf string
		want       int
	}{
		{fiona, "", http.StatusForbidden},      // not in her own right
		{fiona, "bob", http.StatusForbidden},   // bob delegated nothing
		{carol, "alice", http.StatusForbidden}, // not to carol
		{bob, "alice", http.StatusForbidden},   // the requester
	} {
		if status, raw, _ := c.who.reviewFor(r.ID, c.onBehalfOf, "approve"); status != c.want {
			t.Errorf("review on behalf of %q = %d %s, want %d", c.onBehalfOf, status, raw, c.want)
		}
	}
	status, raw, after := fiona.reviewFor(r.ID, "alice", "approve")
	if status != http.StatusOK || after.State != "approved" {
		t.Fatalf("fiona for alice = %d %s", status, raw)
	}
	if len(after.Reviews) != 1 || after.Reviews[0].Reviewer != "fiona" || after.Reviews[0].OnBehalfOf != "alice" || after.Reviews[0].Delegation != d.ID ||
		!slices.Equal(after.Rules[0].Approvals, []string{"fiona"}) || after.Rules[0].OnBehalfOf["fiona"] != "alice" {
		t.Errorf("the review = %+v, rules %+v", after.Reviews, after.Rules)
	}
	var reviewed bool
	for _, e := range after.Events {
		if e.Kind == "approve" && e.Actor == "fiona" && strings.Contains(string(e.Data), `"onBehalfOf":"alice"`) && strings.Contains(string(e.Data), d.ID) {
			reviewed = true
		}
	}
	if !reviewed {
		t.Errorf("the timeline does not say on whose behalf: %+v", after.Events)
	}
	ev, personal = h.audited("request.reviewed", r.ID)
	byPerson(t, ev[0], personal, "fiona")
	if personal[ev[0].Details["onBehalfOf"].(string)] != "alice" || ev[0].Details["delegation"] != d.ID {
		t.Errorf("reviewed = %+v", ev[0].Details)
	}
	ev, personal = h.audited("request.approved", r.ID)
	behalf, _ := ev[0].Details["onBehalfOf"].([]any)
	if len(behalf) != 1 {
		t.Fatalf("approved = %+v", ev[0].Details)
	}
	if b := behalf[0].(map[string]any); personal[b["approver"].(string)] != "fiona" || personal[b["onBehalfOf"].(string)] != "alice" || b["delegation"] != d.ID {
		t.Errorf("approved = %+v", ev[0].Details)
	}
	bob.await(r.ID, "applied")
	if msg := git(t, "", "--git-dir", h.bare, "log", "-1", "--format=%B", "main"); !strings.Contains(msg, "Approved-by: fiona (on behalf of alice)\n") {
		t.Errorf("the commit's message:\n%s", msg)
	}

	// Never on her own request, nor on alice's.
	own := fiona.submit(newVM("prod", "deleg-fiona", 2))
	if v := fiona.view(own.ID); v.CanReview || len(v.ReviewFor) != 0 {
		t.Errorf("fiona's own request: %v %+v", v.CanReview, v.ReviewFor)
	}
	if status, raw, _ := fiona.reviewFor(own.ID, "alice", "approve"); status != http.StatusForbidden {
		t.Errorf("fiona for alice on her own request = %d %s", status, raw)
	}
	alices := alice.submit(newVM("prod", "deleg-alice", 2))
	if status, raw, _ := fiona.reviewFor(alices.ID, "alice", "approve"); status != http.StatusForbidden {
		t.Errorf("fiona for alice on alice's request = %d %s", status, raw)
	}

	// One human, one approval: a VM both production and large waits for a platform admin and
	// FinOps; fiona gives either, for alice or herself, never both.
	big := bob.submit(newVM("prod", "deleg-big", 12))
	if v := fiona.view(big.ID); !v.CanReviewOwn || len(v.ReviewFor) != 1 {
		t.Errorf("fiona on the large VM: %v %+v", v.CanReviewOwn, v.ReviewFor)
	}
	for i, onBehalfOf := range []string{"alice", "", "alice"} {
		status, raw, v := fiona.reviewFor(big.ID, onBehalfOf, "approve")
		if status != http.StatusOK || v.State != "pending_approval" {
			t.Fatalf("fiona's review %d = %d %s", i, status, raw)
		}
		production, large := len(v.Rules[0].Approvals), len(v.Rules[1].Approvals)
		if onBehalfOf == "alice" && (production != 1 || large != 0) || onBehalfOf == "" && (production != 0 || large != 1) {
			t.Errorf("after fiona's review %d (for %q): %+v", i, onBehalfOf, v.Rules)
		}
	}
	// alice's own decision replaces fiona's for her; fiona can no longer review for her.
	alice.approve(big.ID)
	if status, raw, _ := fiona.reviewFor(big.ID, "alice", "approve"); status != http.StatusConflict || !strings.Contains(raw, "their decision stands") {
		t.Errorf("fiona for alice after alice = %d %s", status, raw)
	}
	if status, raw, v := fiona.reviewFor(big.ID, "", "approve"); status != http.StatusOK || v.State != "approved" ||
		!slices.Equal(v.Rules[0].Approvals, []string{"alice"}) || !slices.Equal(v.Rules[1].Approvals, []string{"fiona"}) || len(v.Rules[0].OnBehalfOf) != 0 {
		t.Errorf("fiona herself after alice = %d %s", status, raw)
	}

	// Only alice, or a platform admin, revokes it.
	for _, u := range []*user{bob, fiona} {
		if status, raw := u.do(http.MethodDelete, Prefix+"/delegations/"+d.ID, nil, nil); status != http.StatusNotFound {
			t.Errorf("revoked by someone else = %d %s", status, raw)
		}
	}
	if status, raw := bearer(t, h, tok.Body.Token, http.MethodDelete, Prefix+"/delegations/"+d.ID, nil); status != http.StatusForbidden {
		t.Errorf("revoked with a token = %d %s", status, raw)
	}
	pending := bob.submit(newVM("prod", "deleg-02", 2))
	if status, raw := alice.do(http.MethodDelete, Prefix+"/delegations/"+d.ID, nil, nil); status != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", status, raw)
	}
	if status, raw := alice.do(http.MethodDelete, Prefix+"/delegations/"+d.ID, nil, nil); status != http.StatusConflict {
		t.Errorf("revoke again = %d %s", status, raw)
	}
	ev, personal = h.audited("delegation.revoked", d.ID)
	byPerson(t, ev[0], personal, "alice")
	if ev[0].Details["by"] != "delegator" {
		t.Errorf("revoked = %+v", ev[0].Details)
	}
	if v := fiona.view(r.ID); len(v.Reviews) != 1 || v.Reviews[0].OnBehalfOf != "alice" {
		t.Errorf("the review made before the revocation: %+v", v.Reviews)
	}
	if v := fiona.view(pending.ID); v.CanReview || len(v.ReviewFor) != 0 {
		t.Errorf("after the revocation: %v %+v", v.CanReview, v.ReviewFor)
	}
	if status, raw, _ := fiona.reviewFor(pending.ID, "alice", "approve"); status != http.StatusForbidden {
		t.Errorf("fiona for alice after the revocation = %d %s", status, raw)
	}
	if l := fiona.delegations().Body.Delegations; len(l) != 0 {
		t.Errorf("fiona's after the revocation = %+v", l)
	}

	// A platform admin revokes fiona's to carol, and makes none for anyone else.
	fc := fiona.mustDelegate("carol", now, week)
	if status, raw := alice.do(http.MethodDelete, Prefix+"/delegations/"+fc.ID, nil, nil); status != http.StatusNoContent {
		t.Fatalf("admin revoke = %d %s", status, raw)
	}
	ev, personal = h.audited("delegation.revoked", fc.ID)
	byPerson(t, ev[0], personal, "alice")
	if ev[0].Details["by"] != "admin" || personal[ev[0].Details["delegator"].(string)] != "fiona" {
		t.Errorf("revoked by an admin = %+v", ev[0].Details)
	}

	// From its start, until its end.
	later := alice.mustDelegate("fiona", now.Add(time.Hour), now.Add(25*time.Hour))
	if later.Active {
		t.Errorf("a delegation from next hour is active: %+v", later)
	}
	if status, raw, _ := fiona.reviewFor(pending.ID, "alice", "approve"); status != http.StatusForbidden {
		t.Errorf("before the start = %d %s", status, raw)
	}
	h.clock.set(now.Add(time.Hour))
	if v := fiona.view(pending.ID); len(v.ReviewFor) != 1 || !slices.Contains(fiona.inboxOf(), pending.ID) {
		t.Errorf("from the start: %+v", v.ReviewFor)
	}
	h.clock.set(now.Add(25 * time.Hour))
	if v := fiona.view(pending.ID); v.CanReview || len(v.ReviewFor) != 0 || slices.Contains(fiona.inboxOf(), pending.ID) {
		t.Errorf("at the end: %+v", v.ReviewFor)
	}
	if status, raw, _ := fiona.reviewFor(pending.ID, "alice", "approve"); status != http.StatusForbidden {
		t.Errorf("at the end = %d %s", status, raw)
	}
	if status, raw := alice.do(http.MethodDelete, Prefix+"/delegations/"+later.ID, nil, nil); status != http.StatusConflict {
		t.Errorf("revoke an ended delegation = %d %s", status, raw)
	}

	// The delegator deactivated: their delegate holds nothing.
	alice.mustDelegate("fiona", now.Add(25*time.Hour), week)
	if v := fiona.view(pending.ID); len(v.ReviewFor) != 1 {
		t.Fatalf("again: %+v", v.ReviewFor)
	}
	if _, err := h.store.Pool.Exec(context.Background(), "UPDATE users SET deactivated_at = now() WHERE name = 'alice'"); err != nil {
		t.Fatal(err)
	}
	if status, raw, _ := fiona.reviewFor(pending.ID, "alice", "approve"); status != http.StatusForbidden {
		t.Errorf("for a deactivated delegator = %d %s", status, raw)
	}
}

// TestDelegationStepUp: with auth.stepUp, making a delegation needs a recent sign-in (open
// question 75); a review on the delegator's behalf of a request that asks for a step-up needs the
// delegate's, the human acting.
func TestDelegationStepUp(t *testing.T) {
	t.Parallel()
	h, clk := stepUpHarness(t, &config.StepUp{MaxAge: 5 * time.Minute})
	vm, err := os.ReadFile(filepath.Join(h.work, ".quai/types/vm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	h.push(".quai/types/vm.yaml", strings.Replace(string(vm), "  approval:\n    rules:\n", "  approval:\n    stepUp: \"params.env == 'prod'\"\n    rules:\n", 1))
	bob, alice, fiona := h.as("bob"), h.as("alice"), h.as("fiona")
	now := time.Now().UTC()
	clk.add(6 * time.Minute)
	if l := alice.delegations().Body; !l.StepUp {
		t.Errorf("the list does not say a step-up is needed: %+v", l)
	}
	status, body, _ := alice.delegate("fiona", now.Add(time.Minute), now.Add(48*time.Hour))
	stepUpRefused(t, "alice's old session delegating", status, body, "more than 5m0s ago")
	ev, _ := h.audited("auth.step_up_refused", "alice")
	if ev[0].Details["attempted"] != "delegation.create" {
		t.Errorf("refusal = %+v", ev[0])
	}
	alice = h.as("alice")
	alice.mustDelegate("fiona", now.Add(time.Minute), now.Add(48*time.Hour))
	h.clock.set(now.Add(2 * time.Minute))

	r := bob.submit(newVM("prod", "deleg-stepup", 2))
	status, body, _ = fiona.reviewFor(r.ID, "alice", "approve")
	stepUpRefused(t, "fiona's old session for alice", status, body, "more than 5m0s ago")
	fiona = h.as("fiona")
	status, body, v := fiona.reviewFor(r.ID, "alice", "approve")
	if status != http.StatusOK || v.State != "approved" || len(v.Reviews) != 1 || !v.Reviews[0].StepUp || v.Reviews[0].OnBehalfOf != "alice" {
		t.Errorf("fiona, signed in again, for alice = %d %s", status, body)
	}
}

// TestDelegationOnGitea: an approval on someone's behalf is in the squash commit's trailers, both
// people named.
func TestDelegationOnGitea(t *testing.T) {
	t.Parallel()
	h, remote := giteaHarness(t, "auto")
	g := giteatest.Get(t)
	bob, alice, fiona := h.as("bob"), h.as("alice"), h.as("fiona")
	now := time.Now().UTC()
	alice.mustDelegate("fiona", now, now.Add(24*time.Hour))
	r := bob.submit(resizeBody(6))
	bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	if status, raw, _ := fiona.reviewFor(r.ID, "alice", "approve"); status != http.StatusOK {
		t.Fatalf("fiona for alice = %d %s", status, raw)
	}
	r = bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	c, _, err := g.Client.GetSingleCommit(remote.Owner, remote.Name, r.Delivery.MergeCommit)
	if err != nil {
		t.Fatal(err)
	}
	if msg := c.RepoCommit.Message; !strings.Contains(msg, "Approved-by: fiona (on behalf of alice)\n") || strings.Contains(msg, "Approved-by: alice\n") {
		t.Errorf("the squash commit:\n%s", msg)
	}
	if v := bob.view(r.ID); len(v.Reviews) != 1 || v.Reviews[0].Reviewer != "fiona" || v.Reviews[0].OnBehalfOf != "alice" {
		t.Errorf("the review = %+v", v.Reviews)
	}
}

// TestDelegationNotified: while a delegation is active, the delegator's approvals needed go to the
// delegate, saying on whose behalf; the delegator still gets alerts. Revoked, they go to the
// delegator again.
func TestDelegationNotified(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h := notifiedHarness(t, mail, "{ name: infra-live, title: Infrastructure, owners: [group:platform-admins] }")
	bob, alice := h.as("bob"), h.as("alice")
	h.as("pat")
	h.as("fiona")
	now := time.Now().UTC()
	d := alice.mustDelegate("fiona", now, now.Add(24*time.Hour))
	h.quiet(t)

	r := bob.submit(newVM("prod", "deleg-notified", 2))
	h.quiet(t)
	if got := inbox(t, mail, "alice@example.com"); len(got) != 0 {
		t.Errorf("alice's messages = %q", got)
	}
	if got := inbox(t, mail, "pat@example.com"); len(got) != 1 {
		t.Errorf("pat's messages = %q", got)
	}
	got := mail.To(t, "fiona@example.com")
	if len(got) != 1 {
		t.Fatalf("fiona's messages = %+v", got)
	}
	if m := mail.Message(t, got[0].ID); !strings.Contains(m.Text, "You hold alice's approvals through a delegation") || !strings.Contains(m.Text, "/requests/"+r.ID) {
		t.Errorf("fiona's message:\n%s", m.Text)
	}

	// Alerts cannot be passed on: alice gets hers.
	if err := h.store.Tx(context.Background(), func(tx pgx.Tx, _ *db.Queries) error {
		_, err := h.store.RecordEvent(context.Background(), tx, db.AddEventParams{Target: "repository:infra-live", Kind: "alert_bypass",
			Data: json.RawMessage(`{"number": 12, "state": "pending_approval", "pullRequest": "7", "commit": "0a1b2c3"}`)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.quiet(t)
	if got := inbox(t, mail, "alice@example.com"); len(got) != 1 || !strings.Contains(got[0], "Alert") {
		t.Errorf("alice's messages after the alert = %q", got)
	}

	// Revoked: alice is asked again.
	if status, raw := alice.do(http.MethodDelete, Prefix+"/delegations/"+d.ID, nil, nil); status != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", status, raw)
	}
	bob.submit(newVM("prod", "deleg-notified-2", 2))
	h.quiet(t)
	if got := inbox(t, mail, "alice@example.com"); len(got) != 2 {
		t.Errorf("alice's messages after the revocation = %q", got)
	}
	if got := inbox(t, mail, "fiona@example.com"); len(got) != 1 {
		t.Errorf("fiona's messages after the revocation = %q", got)
	}
}

// TestDelegationChatLink: the card of an approval needed on a delegator's behalf goes to the
// delegate, its links bound to them and to the delegator: confirmed on that behalf they decide,
// otherwise they are refused.
func TestDelegationChatLink(t *testing.T) {
	t.Parallel()
	fakes := teamstest.Start(t)
	h := chatHarness(t, fakes, fakes.CAFile)
	alice, bob, carol := h.as("alice"), h.as("bob"), h.as("carol")
	h.as("pat")
	now := time.Now().UTC()
	alice.mustDelegate("bob", now, now.Add(24*time.Hour))
	h.quiet(t)

	r := carol.submit(newVM("prod", "deleg-teams", 2))
	h.quiet(t)
	if got := fakes.Connector.To(oidAlice); len(got) != 0 {
		t.Errorf("alice's cards = %+v", got)
	}
	card := cardOf(t, fakes, oidBob)
	if !strings.Contains(card.Card.Text(), "alice") {
		t.Errorf("bob's card does not name alice: %s", card.Card.Text())
	}
	approve := linkOf(t, card, "Approve", r.ID)
	var v ChatLinkView
	if status, raw := bob.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/chat-link", map[string]string{"link": approve}, &v); status != http.StatusOK ||
		v.OnBehalfOf != "alice" || v.Decision != "approve" {
		t.Errorf("bob opens his Approve = %d %s", status, raw)
	}
	status, raw := bob.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve", "link": approve}, nil)
	refused(t, "the link in bob's own right", status, raw, "on someone else's behalf")
	ev, _ := h.audited("request.link_refused", r.ID)
	if ev[len(ev)-1].Details["code"] != chatlink.Behalf {
		t.Errorf("refusal = %+v", ev[len(ev)-1])
	}
	var after RequestView
	if status, raw := bob.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve", "link": approve, "onBehalfOf": "alice"},
		&after); status != http.StatusOK || after.State != "approved" || len(after.Reviews) != 1 || after.Reviews[0].OnBehalfOf != "alice" {
		t.Errorf("bob confirms for alice = %d %s", status, raw)
	}
}
