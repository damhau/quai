package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"code.gitea.io/sdk/gitea"

	"github.com/damhau/quai/backend/internal/config"
	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/giteatest"
	"github.com/damhau/quai/backend/internal/repositories"
)

// sodUsers are the users of the separation of duties' tests: alice, pat and pia administer the
// platform, pat and pia with verified emails, alice without one; bob requests.
func sodUsers(c *config.Config) {
	c.Auth.Dev.Users = []config.DevUser{
		{Name: "alice", Groups: []string{"platform-admins"}},
		{Name: "pat", Email: "pat@example.com", Groups: []string{"platform-admins"}},
		{Name: "pia", Email: "pia@example.com", Groups: []string{"platform-admins"}},
		{Name: "bob", Email: "bob@example.com", Groups: []string{"developers", "shop"}},
	}
}

// submit requests a change, and fails the test unless it is created.
func (u *user) submit(body map[string]any) RequestView {
	u.h.t.Helper()
	var r RequestView
	if status, raw := u.do(http.MethodPost, Prefix+"/requests/type", body, &r); status != http.StatusCreated {
		u.h.t.Fatalf("submit = %d %s", status, raw)
	}
	return r
}

// TestSeparationDefinitionAuthors: with definitionAuthors: 7d, whoever changed a request's
// definitions in the 7 days before it cannot review it, matched by the verified emails of the
// commits' authors, committers and co-authors, whatever dates the commits carry, signed in before
// the request or after; the request says who and why. A commit to other files excludes no one.
// The period is measured on the definitions Quai recorded: once those it had 7 days before
// include a change, its authors review again.
func TestSeparationDefinitionAuthors(t *testing.T) {
	t.Parallel()
	h, remote := giteaHarnessOver(t, "manual", "\n  enforcement: { separationOfDuties: { definitionAuthors: 7d } }", nil, nil, giteaconn.New, sodUsers)
	g := giteatest.Get(t)
	alice, pat, bob := h.as("alice"), h.as("pat"), h.as("bob")

	// pat changes the vm type in the web UI with pia, the commit dated a month back; mallory
	// changes a file no definition reads.
	const typeFile = ".quai/types/vm.yaml"
	existing, _, err := g.Client.GetContents(remote.Owner, remote.Name, "main", typeFile)
	if err != nil {
		t.Fatal(err)
	}
	month := time.Now().AddDate(0, -1, 0)
	res, _, err := g.Client.UpdateFile(remote.Owner, remote.Name, typeFile, gitea.UpdateFileOptions{
		FileOptions: gitea.FileOptions{BranchName: "main", Message: "Tune the vm type\n\nCo-authored-by: Pia <pia@example.com>\n",
			Author: gitea.Identity{Name: "Pat", Email: "PAT@example.com"}, Dates: gitea.CommitDateOptions{Author: month, Committer: month}},
		SHA: existing.SHA, Content: base64.StdEncoding.EncodeToString([]byte("# tuned\n" + mainFile(t, remote, typeFile))),
	})
	if err != nil {
		t.Fatal(err)
	}
	changed := res.Commit.SHA
	if _, _, err := g.Client.CreateFile(remote.Owner, remote.Name, "notes.md", gitea.CreateFileOptions{
		FileOptions: gitea.FileOptions{BranchName: "main", Message: "Notes", Author: gitea.Identity{Name: "Mallory", Email: "mallory@example.net"}},
		Content:     base64.StdEncoding.EncodeToString([]byte("notes\n")),
	}); err != nil {
		t.Fatal(err)
	}
	h.poll()

	// Quai recorded no definitions 7 days ago: the whole history counts, the example's first
	// commit, by alice@example.com, which no one has as a verified email, with it.
	var plan PlanView
	body := resize("prod/web01", 6)
	delete(body, "reason")
	if status, raw := bob.do(http.MethodPost, Prefix+"/plans/type", body, &plan); status != http.StatusOK || plan.Separation == nil ||
		len(plan.Separation.Authors) != 1 || plan.Separation.Authors[0].Name != "pat" {
		t.Fatalf("plan = %d %s", status, raw)
	}
	r := bob.submit(resize("prod/web01", 6))
	sep := r.Separation
	if sep == nil || sep.DefinitionAuthors != "7d" || sep.From != "" || sep.Since == nil || time.Since(*sep.Since) < 7*24*time.Hour || sep.Commit == "" ||
		len(sep.Authors) != 1 || sep.Authors[0].Name != "pat" || !slices.Equal(sep.Authors[0].Commits, []string{changed}) ||
		!slices.Contains(sep.Unmatched, "Pia <pia@example.com>") || !slices.Contains(sep.Unmatched, "Alice <alice@example.com>") ||
		slices.ContainsFunc(sep.Unmatched, func(s string) bool { return strings.Contains(s, "mallory") }) {
		t.Fatalf("the request's separation = %+v", sep)
	}

	v := pat.read(r.ID)
	if v.CanReview || !strings.Contains(v.CannotReview, changed[:12]) || !strings.Contains(v.CannotReview, "7d") {
		t.Errorf("pat: canReview %v, %q", v.CanReview, v.CannotReview)
	}
	var inbox []RequestView
	if pat.do(http.MethodGet, Prefix+"/requests?view=inbox", nil, &inbox); len(inbox) != 0 {
		t.Errorf("pat's inbox = %+v", inbox)
	}
	if status := pat.tryApprove(r.ID); status != http.StatusForbidden {
		t.Errorf("pat's approval = %d", status)
	}
	// pia signs in after the request was planned: her verified email is the co-author's.
	pia := h.as("pia")
	if v := pia.read(r.ID); v.CanReview || !strings.Contains(v.CannotReview, changed[:12]) {
		t.Errorf("pia: canReview %v, %q", v.CanReview, v.CannotReview)
	}
	if status := pia.tryApprove(r.ID); status != http.StatusForbidden {
		t.Errorf("pia's approval = %d", status)
	}
	if v := bob.read(r.ID); v.CannotReview != "" {
		t.Errorf("bob, whom no rule names: %q", v.CannotReview)
	}
	if v := alice.read(r.ID); !v.CanReview || v.CannotReview != "" {
		t.Errorf("alice: canReview %v, %q", v.CanReview, v.CannotReview)
	}
	if status := alice.tryApprove(r.ID); status != http.StatusOK {
		t.Fatalf("alice's approval = %d", status)
	}
	if state := bob.read(r.ID).State; state != "approved" {
		t.Errorf("after alice's approval: %s", state)
	}
	ev, personal := h.audited("request.submitted", r.ID)
	d, _ := ev[0].Details["separationOfDuties"].(map[string]any)
	excluded, _ := d["excluded"].([]any)
	if d["definitionAuthors"] != "7d" || len(excluded) != 1 {
		t.Fatalf("the audit's separation = %+v", ev[0].Details)
	}
	if person, _ := excluded[0].(map[string]any)["person"].(string); personal[person] != "pat" {
		t.Errorf("the excluded person in the audit log = %q, %v", person, personal)
	}

	// 8 days on, by what Quai recorded: the definitions it had 7 days before include pat's
	// change, and no one is excluded.
	if _, err := h.store.Pool.Exec(context.Background(), "UPDATE definition_sets SET created_at = created_at - interval '8 days'"); err != nil {
		t.Fatal(err)
	}
	second := bob.submit(newVM("prod", "sod02", 4))
	if sep := second.Separation; sep == nil || sep.From == "" || len(sep.Authors) != 0 || len(sep.Unmatched) != 0 {
		t.Fatalf("8 days on, the separation = %+v", sep)
	}
	if v := pat.read(second.ID); !v.CanReview || v.CannotReview != "" {
		t.Errorf("pat 8 days on: canReview %v, %q", v.CanReview, v.CannotReview)
	}
	if status := pat.tryApprove(second.ID); status != http.StatusOK {
		t.Errorf("pat's approval 8 days on = %d", status)
	}
}

// TestSeparationRequireTwo: with destroys: requireTwo, a request delivered by a pull request needs
// two different approvers until a plan shows that it destroys nothing, and each plan decides
// again: one that destroys, or a failed one, needs a second approver again.
func TestSeparationRequireTwo(t *testing.T) {
	t.Parallel()
	h, _ := giteaHarnessOver(t, "manual", runner+"\n  enforcement: { separationOfDuties: { destroys: requireTwo } }", nil, nil, giteaconn.New, sodUsers)
	alice, pat, bob := h.as("alice"), h.as("pat"), h.as("bob")

	r := bob.submit(resize("prod/web01", 6))
	if two := ruleNamed(r, policy.RequireTwoName); !two.Overall || two.Min != 2 || two.WaitingForPlan || two.NotApplicable ||
		two.PlanError != policy.NoPlanYet || !slices.Equal(two.Approvers, []string{"owners"}) {
		t.Fatalf("the requireTwo rule = %+v", two)
	}
	if r.Separation == nil || r.Separation.Destroys != "requireTwo" || r.Separation.DefinitionAuthors != "" {
		t.Errorf("the request's separation = %+v", r.Separation)
	}
	r = bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery != nil && v.Delivery.PullRequest != "" })
	if status := alice.tryApprove(r.ID); status != http.StatusOK {
		t.Fatalf("alice's approval = %d", status)
	}
	if state := bob.read(r.ID).State; state != "pending_approval" {
		t.Fatalf("one approver before a plan: %s", state)
	}

	plan := func(n, status string, destroy int) {
		t.Helper()
		body := map[string]any{"kind": "plan", "status": status, "pullRequest": r.Delivery.PullRequest, "url": "https://ci.example.com/" + n,
			"summary": map[string]any{"add": 0, "change": 1, "destroy": destroy}}
		if status == "failed" {
			body["summary"] = nil
		}
		if code, _, raw := h.reportRun(runToken, runReport(body)); code != http.StatusAccepted {
			t.Fatalf("plan %s = %d %s", n, code, raw)
		}
	}
	plan("1", "succeeded", 0)
	v := bob.await(r.ID, "approved")
	if two := ruleNamed(v, policy.RequireTwoName); !two.NotApplicable || two.PlanError != "" {
		t.Errorf("after a plan that destroys nothing: %+v", two)
	}

	plan("2", "succeeded", 1)
	v = bob.await(r.ID, "pending_approval")
	if two := ruleNamed(v, policy.RequireTwoName); two.NotApplicable || two.Satisfied || two.PlanError != "" {
		t.Errorf("after a plan that destroys: %+v", two)
	}
	plan("3", "succeeded", 0)
	bob.await(r.ID, "approved")
	plan("4", "failed", 0)
	v = bob.await(r.ID, "pending_approval")
	if two := ruleNamed(v, policy.RequireTwoName); two.NotApplicable || !strings.Contains(two.PlanError, "the latest plan of envs/prod failed") {
		t.Errorf("after a failed plan: %+v", two)
	}
	if status := alice.tryApprove(r.ID); status != http.StatusOK || bob.read(r.ID).State != "pending_approval" {
		t.Errorf("alice approving again = %d", status)
	}
	if status := pat.tryApprove(r.ID); status != http.StatusOK {
		t.Fatalf("pat's approval = %d", status)
	}
	if v := bob.await(r.ID, "approved"); !slices.Equal(ruleNamed(v, policy.RequireTwoName).Approvals, []string{"alice", "pat"}) {
		t.Errorf("the requireTwo rule's approvals = %+v", ruleNamed(v, policy.RequireTwoName))
	}
}

// sodHarness serves the example repository delivered by direct commits, with the enforcement
// given and sodUsers; reconfigure replaces the enforcement, as a new repository document would.
func sodHarness(t *testing.T, enforcement string) (h *harness, reconfigure func(string)) {
	t.Helper()
	dir := t.TempDir()
	bare, work := localRemote(t, dir)
	spec := func(e string) string {
		return "connector: generic\n  url: " + bare + "\n  enforcement: " + e + "\n  sync: { minInterval: 1ns }"
	}
	h = start(t, dir, spec(enforcement), repositories.Options{}, sodUsers)
	h.bare, h.work = bare, work
	return h, func(e string) {
		t.Helper()
		docs, err := repositories.Parse([]byte("apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: infra-live, title: Infrastructure }\nspec:\n  " + spec(e) + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if err := repositories.Reconcile(context.Background(), h.store, docs); err != nil {
			t.Fatal(err)
		}
		if err := h.reg.Reload(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSeparationDirect: delivered by a direct commit, a request has no plan: with requireTwo it
// needs two different approvers. A request keeps the separation of duties it was planned with: a
// looser one leaves it as it was; a stricter one makes it stale at its next review, unreviewed.
func TestSeparationDirect(t *testing.T) {
	t.Parallel()
	h, reconfigure := sodHarness(t, "{ separationOfDuties: { destroys: requireTwo } }")
	alice, pat, bob := h.as("alice"), h.as("pat"), h.as("bob")

	r := bob.submit(resize("prod/web01", 6))
	if two := ruleNamed(r, policy.RequireTwoName); two.NotApplicable || two.PlanError != "no plan comes before a direct commit" {
		t.Errorf("the requireTwo rule = %+v", two)
	}
	if status := alice.tryApprove(r.ID); status != http.StatusOK || bob.read(r.ID).State != "pending_approval" {
		t.Fatalf("alice's approval = %d", status)
	}
	if status := pat.tryApprove(r.ID); status != http.StatusOK {
		t.Fatalf("pat's approval = %d", status)
	}
	bob.await(r.ID, "applied")

	stricter, looser := bob.submit(newVM("prod", "sod02", 4)), bob.submit(newVM("prod", "sod03", 4))
	reconfigure("{ separationOfDuties: { destroys: requireTwo, definitionAuthors: 7d } }")
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+stricter.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("a review after a stricter separation = %d %s", status, body)
	}
	v := bob.read(stricter.ID)
	if v.State != "stale" || len(v.Reviews) != 0 || !slices.ContainsFunc(v.Events, func(e EventView) bool {
		return e.Kind == "stale" && strings.Contains(string(e.Data), "definition authors of the last 7d cannot approve")
	}) {
		t.Errorf("after a stricter separation: %s, reviews %+v, events %+v", v.State, v.Reviews, v.Events)
	}

	// A platform admin requesting a change cannot approve it either.
	own := alice.submit(newVM("prod", "sod04", 4))
	if v := alice.read(own.ID); v.CanReview || v.CannotReview != "You requested this change: someone else approves it." {
		t.Errorf("alice's own request: canReview %v, %q", v.CanReview, v.CannotReview)
	}

	reconfigure("{}")
	if status := alice.tryApprove(looser.ID); status != http.StatusOK || bob.read(looser.ID).State != "pending_approval" {
		t.Fatalf("alice's approval after a looser separation = %d", status)
	}
	if status := pat.tryApprove(looser.ID); status != http.StatusOK {
		t.Fatalf("pat's approval = %d", status)
	}
	bob.await(looser.ID, "applied")
}
