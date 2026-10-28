package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"code.gitea.io/sdk/gitea"

	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/giteatest"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Approvals bound to content, on Gitea (docs/design/05-delivery-and-approvals.md#approvals-bound-to-content).

// opened submits bob's resize of prod/web01 and waits for its pull request; approved approves it
// too (with merge: manual, it stays open).
func opened(t *testing.T, h *harness) RequestView {
	t.Helper()
	var r RequestView
	if status, body := h.as("bob").do(http.MethodPost, Prefix+"/requests/type", resizeBody(6), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	return h.as("bob").awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
}

func approved(t *testing.T, h *harness) RequestView {
	t.Helper()
	r := opened(t, h)
	if status, body := h.as("alice").do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("approve = %d %s", status, body)
	}
	return h.as("bob").awaitDelivery(r.ID, "approved", func(v RequestView) bool { return v.State == "approved" })
}

func diffText(c ChangeView) string {
	var b strings.Builder
	for _, l := range c.Diff {
		b.WriteString(l.Op + l.Text + "\n")
	}
	return b.String()
}

func hasEvent(r RequestView, kind, text string) bool {
	for _, e := range r.Events {
		if e.Kind == kind && strings.Contains(string(e.Data), text) {
			return true
		}
	}
	return false
}

// The base moves, the owned region unchanged: the branch is rebuilt on it; same hash, the
// approval stays.
func TestRebuildOnNewBase(t *testing.T) {
	h, remote := giteaHarness(t, "manual")
	g := giteatest.Get(t)
	r := approved(t, h)
	before, _, err := g.Client.GetPullRequest(remote.Owner, remote.Name, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write("main", "README.md", "# Infrastructure, edited\n"); err != nil {
		t.Fatal(err)
	}
	main := h.giteaHead(t, remote, "main")
	r = h.as("bob").awaitDelivery(r.ID, "rebuilt", func(v RequestView) bool { return hasEvent(v, "rebuilt", main) })
	if r.State != "approved" || len(r.Reviews) != 1 {
		t.Errorf("after the rebuild: %s, reviews %+v", r.State, r.Reviews)
	}
	pr, _, err := g.Client.GetPullRequest(remote.Owner, remote.Name, 1)
	if err != nil || pr.Head.Sha == before.Head.Sha {
		t.Fatalf("the pull request's head did not move: %v", err)
	}
	c, _, err := g.Client.GetSingleCommit(remote.Owner, remote.Name, pr.Head.Sha)
	if err != nil || len(c.Parents) != 1 || c.Parents[0].SHA != main {
		t.Errorf("the rebuilt commit is not on main's head %s: %+v, %v", main, c, err)
	}
	h.awaitStatus(t, remote, pr.Head.Sha, gitea.StatusSuccess)
}

// An owned region changes on the base: the request is stale.
func TestOwnedRegionChanged(t *testing.T) {
	h, remote := giteaHarness(t, "manual")
	r := opened(t, h)
	if err := remote.Write("main", "envs/prod/vms/web01.yaml", "owner: bob\ntemplate: ubuntu-24.04\ncpu: 5\nmemory_gb: 16\n"); err != nil {
		t.Fatal(err)
	}
	r = h.as("bob").awaitDelivery(r.ID, "stale", func(v RequestView) bool { return v.State == "stale" })
	if !hasEvent(r, "stale", "envs/prod/vms/web01.yaml") {
		t.Errorf("the stale event does not name the region: %+v", r.Events)
	}
	pr, _, _ := giteatest.Get(t).Client.GetPullRequest(remote.Owner, remote.Name, 1)
	h.awaitStatus(t, remote, pr.Head.Sha, gitea.StatusFailure)
}

// The definitions change: the request is stale.
func TestDefinitionsChanged(t *testing.T) {
	h, remote := giteaHarness(t, "manual")
	r := opened(t, h)
	original, _, err := giteatest.Get(t).Client.GetFile(remote.Owner, remote.Name, "main", ".quai/types/vm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write("main", ".quai/types/vm.yaml", strings.Replace(string(original), "Virtual machine (Terraform)", "Virtual machine", 1)); err != nil {
		t.Fatal(err)
	}
	r = h.as("bob").awaitDelivery(r.ID, "stale", func(v RequestView) bool { return v.State == "stale" })
	if !hasEvent(r, "stale", "the type vm changed") {
		t.Errorf("events = %+v", r.Events)
	}
}

// Someone pushes to the branch, within the request's file: the approvals are void and the
// request is approved again from the branch's content.
func TestOutsidePushWithin(t *testing.T) {
	h, remote := giteaHarness(t, "manual")
	r := approved(t, h)
	if err := remote.Write(r.Delivery.Branch, "envs/prod/vms/web01.yaml", "owner: bob\ntemplate: ubuntu-24.04\ncpu: 7\nmemory_gb: 16\ndisks: [{ size_gb: 80 }]\nnetwork: { vlan: 120 }\n"); err != nil {
		t.Fatal(err)
	}
	r = h.as("bob").awaitDelivery(r.ID, "modified outside Quai", func(v RequestView) bool { return v.State == "pending_approval" })
	if !hasEvent(r, "modified_outside_quai", "approvals are void") || len(r.Reviews) != 0 || len(r.Changes) != 1 || !strings.Contains(diffText(r.Changes[0]), "+cpu: 7") {
		t.Errorf("after the outside push: events %+v, reviews %+v, changes %+v", r.Events, r.Reviews, r.Changes)
	}
	var again RequestView
	if status, body := h.as("alice").do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, &again); status != http.StatusOK || again.State != "approved" {
		t.Errorf("approving the new content = %d %s", status, body)
	}
}

// Someone replaces the branch's commit with the same content (a reworded commit): the approval
// stands, and quai/approval is published again on the new commit.
func TestOutsidePushSameContent(t *testing.T) {
	h, remote := giteaHarness(t, "manual")
	g := giteatest.Get(t)
	r := approved(t, h)
	pr, _, err := g.Client.GetPullRequest(remote.Owner, remote.Name, 1)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := repo.Mirror.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	web01, err := repo.Mirror.ReadFile(ctx, pr.Head.Sha, "envs/prod/vms/web01.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reworded, err := repo.Mirror.CommitAndPush(ctx, gitx.Commit{Branch: r.Delivery.Branch, Base: "main", Replace: pr.Head.Sha, Message: "Reworded\n",
		Author: gitx.Signature{Name: "Carol", Email: "carol@example.com"}, Changes: []plan.Change{{Op: plan.OpUpdate, Path: "envs/prod/vms/web01.yaml", After: web01}}})
	if err != nil {
		t.Fatal(err)
	}
	h.awaitStatus(t, remote, reworded, gitea.StatusSuccess)
	r = h.as("bob").awaitDelivery(r.ID, "approved", func(v RequestView) bool { return v.State == "approved" })
	if len(r.Reviews) != 1 || hasEvent(r, "modified_outside_quai", "") {
		t.Errorf("after the same content: reviews %+v, events %+v", r.Reviews, r.Events)
	}
}

// Someone pushes to the branch beyond the request's files: the request is closed, and its pull
// request.
func TestOutsidePushBeyond(t *testing.T) {
	h, remote := giteaHarness(t, "manual")
	r := opened(t, h)
	if err := remote.Write(r.Delivery.Branch, "README.md", "# Not the request's\n"); err != nil {
		t.Fatal(err)
	}
	r = h.as("bob").awaitDelivery(r.ID, "closed", func(v RequestView) bool { return v.State == "cancelled" })
	if !hasEvent(r, "modified_outside_quai", "README.md") {
		t.Errorf("events = %+v", r.Events)
	}
	h.awaitPullRequest(t, remote, 1, func(pr *gitea.PullRequest) bool { return pr.State == gitea.StateClosed })
}

// The pull request is closed outside Quai: the request is cancelled.
func TestClosedOutside(t *testing.T) {
	h, remote := giteaHarness(t, "manual")
	r := opened(t, h)
	closed := gitea.StateClosed
	if _, _, err := giteatest.Get(t).Client.EditPullRequest(remote.Owner, remote.Name, 1, gitea.EditPullRequestOption{State: &closed}); err != nil {
		t.Fatal(err)
	}
	r = h.as("bob").awaitDelivery(r.ID, "cancelled", func(v RequestView) bool { return v.State == "cancelled" })
	if !hasEvent(r, "cancelled", "closed outside Quai") {
		t.Errorf("events = %+v", r.Events)
	}
}

// The pull request is merged without Quai's approval (the protection was removed): the request
// is applied, and the bypass recorded as an alert.
func TestMergedWithoutApproval(t *testing.T) {
	h, remote := giteaHarness(t, "manual")
	r := opened(t, h)
	if err := remote.Unprotect("main"); err != nil {
		t.Fatal(err)
	}
	h.awaitPullRequest(t, remote, 1, func(*gitea.PullRequest) bool {
		merged, _, _ := giteatest.Get(t).Client.MergePullRequest(remote.Owner, remote.Name, 1, gitea.MergePullRequestOption{Style: gitea.MergeStyleMerge})
		return merged
	})
	r = h.as("bob").awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	if !hasEvent(r, "merged_without_approval", r.Delivery.PullRequest) {
		t.Errorf("events = %+v", r.Events)
	}
	alerts, err := h.store.ListTargetEvents(context.Background(), db.ListTargetEventsParams{Target: "repository:infra-live", MaxRows: 10})
	if err != nil || len(alerts) != 1 || alerts[0].Kind != "alert_bypass" {
		t.Fatalf("alerts = %+v, %v", alerts, err)
	}
	var data map[string]any
	_ = json.Unmarshal(alerts[0].Data, &data)
	if data["state"] != "pending_approval" {
		t.Errorf("the alert = %v", data)
	}
}
