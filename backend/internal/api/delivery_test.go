package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"code.gitea.io/sdk/gitea"

	"github.com/damhau/quai/backend/internal/connectors"
	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/giteatest"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
)

// pushExample commits the example GitOps repository to main through git, with auth.
func pushExample(t *testing.T, url string, auth func(context.Context) (gitx.Credentials, error), proxy func(*url.URL) (*url.URL, error)) {
	t.Helper()
	pushEdited(t, url, auth, proxy, nil)
}

// pushEdited is pushExample with edit changing files first, by path.
func pushEdited(t *testing.T, url string, auth func(context.Context) (gitx.Credentials, error), proxy func(*url.URL) (*url.URL, error), edit func(path string, b []byte) []byte) {
	t.Helper()
	ctx := context.Background()
	m, err := gitx.OpenMirror(ctx, url, filepath.Join(t.TempDir(), "seed.git"), gitx.Options{Committer: gitx.Signature{Name: "Seed", Email: "seed@example.com"}, Auth: auth, Proxy: proxy})
	if err != nil {
		t.Fatal(err)
	}
	src := os.DirFS("../../../examples/gitops-repo")
	var changes []plan.Change
	_ = fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(src, p)
		if edit != nil {
			b = edit(p, b)
		}
		changes = append(changes, plan.Change{Op: plan.OpCreate, Path: p, After: b})
		return err
	})
	if _, err := m.CommitAndPush(ctx, gitx.Commit{Branch: "main", Message: "The existing infrastructure\n", Author: gitx.Signature{Name: "Alice", Email: "alice@example.com"}, Changes: changes}); err != nil {
		t.Fatal(err)
	}
}

// giteaHarness serves Quai with the example repository on Gitea, delivered by pull requests,
// main protected by quai/approval.
func giteaHarness(t *testing.T, merge string) (*harness, *giteatest.Repo) {
	t.Helper()
	return giteaHarnessWith(t, merge, "", nil)
}

// giteaHarnessWith is giteaHarness with more spec lines and the example's files edited.
func giteaHarnessWith(t *testing.T, merge, spec string, edit func(path string, b []byte) []byte) (*harness, *giteatest.Repo) {
	t.Helper()
	g := giteatest.Get(t)
	r := g.Repo(t, "infra")
	pushEdited(t, r.CloneURL, func(context.Context) (gitx.Credentials, error) { return gitx.Credentials{Header: g.GitHeader()}, nil }, nil, edit)
	if err := r.Protect("main", "quai/approval"); err != nil {
		t.Fatal(err)
	}
	h := start(t, t.TempDir(), `connector: gitea
  url: `+r.CloneURL+`
  credentials: { ref: env:GITEA_TOKEN }
  delivery: { mode: pull-request, merge: `+merge+`, mergeMethod: squash }
  sync: { minInterval: 1ns }`+spec, repositories.Options{
		Secrets: secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) {
			if n == "INFRA_LIVE_RUN_TOKEN" {
				sum := sha256.Sum256([]byte(runToken))
				return hex.EncodeToString(sum[:]), true
			}
			return g.Token, n == "GITEA_TOKEN"
		}}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"gitea": giteaconn.New}},
	})
	return h, r
}

// awaitDelivery waits until the request's delivery satisfies ok, polling the repository as the
// scheduler would.
func (u *user) awaitDelivery(id string, what string, ok func(RequestView) bool) RequestView {
	u.h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var r RequestView
		u.do(http.MethodGet, Prefix+"/requests/"+id, nil, &r)
		if ok(r) {
			return r
		}
		if time.Now().After(deadline) {
			u.h.t.Fatalf("request %s: still not %s: %+v (delivery %+v)\nlog:\n%s", id, what, r.State, r.Delivery, u.h.log.String())
		}
		_ = u.h.ingest.Poll(context.Background(), "infra-live")
		time.Sleep(300 * time.Millisecond)
	}
}

func resizeBody(cpu int) map[string]any {
	return map[string]any{"repository": "infra-live", "type": "vm", "kind": "action", "key": "prod/web01", "action": "resize",
		"actionInputs": map[string]any{"cpu": cpu, "memory_gb": 16}, "reason": "Black Friday traffic"}
}

// TestPullRequestDelivery: a request opens a pull request with quai/approval pending; the
// approval turns it to success and Quai merges through the platform; the merge applies the
// request. A cancelled request's pull request is closed.
func TestPullRequestDelivery(t *testing.T) {
	h, remote := giteaHarness(t, "auto")
	g := giteatest.Get(t)
	bob, alice := h.as("bob"), h.as("alice")

	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resizeBody(6), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	if r.Delivery == nil || r.Delivery.Branch != "quai/r-1-web01-resize" {
		t.Fatalf("delivery = %+v", r.Delivery)
	}
	r = bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	number := int64(1)
	pr, _, err := g.Client.GetPullRequest(remote.Owner, remote.Name, number)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Head.Ref != "quai/r-1-web01-resize" || !strings.Contains(pr.Body, "Black Friday traffic") || !strings.Contains(pr.Body, connectors.Marker(r.ID)) ||
		!strings.Contains(pr.Body, "https://quai.example.com/requests/"+r.ID) || len(pr.Labels) == 0 || pr.Labels[0].Name != "quai" || r.Delivery.URL != pr.HTMLURL {
		t.Errorf("the pull request: %+v", pr)
	}
	status := func(want gitea.StatusState) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			combined, _, err := g.Client.GetCombinedStatus(remote.Owner, remote.Name, pr.Head.Sha)
			if err == nil && len(combined.Statuses) > 0 && combined.Statuses[0].Context == "quai/approval" && combined.Statuses[0].State == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("quai/approval is not %s: %+v, %v", want, combined, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	status(gitea.StatusPending)
	// The protection blocks a merge while the check is pending.
	if merged, _, _ := g.Client.MergePullRequest(remote.Owner, remote.Name, number, gitea.MergePullRequestOption{Style: gitea.MergeStyleSquash}); merged {
		t.Fatal("merged while quai/approval is pending")
	}

	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("approve = %d %s", status, body)
	}
	r = bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	if r.Delivery.MergeCommit == "" {
		t.Errorf("no merge commit: %+v", r.Delivery)
	}
	// The squash commit carries the approvals.
	if c, _, err := g.Client.GetSingleCommit(remote.Owner, remote.Name, r.Delivery.MergeCommit); err != nil || !strings.Contains(c.RepoCommit.Message, "Approved-by: alice") ||
		!strings.Contains(c.RepoCommit.Message, "Quai-Request: #1") {
		t.Errorf("the squash commit: %v, %v", c, err)
	}
	content, _, err := g.Client.GetFile(remote.Owner, remote.Name, "main", "envs/prod/vms/web01.yaml")
	if err != nil || !strings.Contains(string(content), "cpu: 6") {
		t.Errorf("main after the merge: %q, %v", content, err)
	}
	if _, resp, _ := g.Client.GetRepoBranch(remote.Owner, remote.Name, "quai/r-1-web01-resize"); resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Error("the branch was not deleted")
	}

	// A cancelled request's pull request is closed and its branch deleted.
	var second RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resizeBody(8), &second); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	second = bob.awaitDelivery(second.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/"+second.ID+"/cancel", nil, nil); status != http.StatusOK {
		t.Fatalf("cancel = %d %s", status, body)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		p, _, err := g.Client.GetPullRequest(remote.Owner, remote.Name, 2)
		if err == nil && p.State == gitea.StateClosed && !p.HasMerged {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cancelled request's pull request is still open: %+v, %v", p, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestManualMerge: with merge: manual, the approval only turns quai/approval to success; the
// person who merges applies the request.
func TestManualMerge(t *testing.T) {
	h, remote := giteaHarness(t, "manual")
	g := giteatest.Get(t)
	bob, alice := h.as("bob"), h.as("alice")
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resizeBody(6), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil)
	time.Sleep(time.Second)
	if bob.await(r.ID, "approved").State != "approved" {
		t.Fatal("not approved")
	}
	// Someone merges, once Gitea checked the pull request.
	deadline := time.Now().Add(30 * time.Second)
	for {
		merged, _, err := g.Client.MergePullRequest(remote.Owner, remote.Name, 1, gitea.MergePullRequestOption{Style: gitea.MergeStyleMerge})
		if merged {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cannot merge: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	r = bob.awaitDelivery(r.ID, "applied by the merge", func(v RequestView) bool { return v.State == "applied" })
	if r.Delivery.MergeCommit == "" {
		t.Errorf("delivery = %+v", r.Delivery)
	}
	// A person merged: the approvals are a final comment on the pull request.
	comments, _, err := g.Client.ListIssueComments(remote.Owner, remote.Name, 1, gitea.ListIssueCommentOptions{})
	if err != nil || len(comments) != 1 || !strings.Contains(comments[0].Body, "Approved-by: alice") {
		t.Errorf("comments = %+v, %v", comments, err)
	}
}

// giteaHead returns a branch's head on Gitea.
func (h *harness) giteaHead(t *testing.T, r *giteatest.Repo, branch string) string {
	t.Helper()
	b, _, err := giteatest.Get(t).Client.GetRepoBranch(r.Owner, r.Name, branch)
	if err != nil {
		t.Fatal(err)
	}
	return b.Commit.ID
}

// awaitStatus waits until quai/approval is in state on a commit, polling the repository.
func (h *harness) awaitStatus(t *testing.T, r *giteatest.Repo, sha string, want gitea.StatusState) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		combined, _, err := giteatest.Get(t).Client.GetCombinedStatus(r.Owner, r.Name, sha)
		if err == nil {
			for _, st := range combined.Statuses {
				if st.Context == "quai/approval" && st.State == want {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("quai/approval on %.12s is not %s: %+v, %v", sha, want, combined, err)
		}
		_ = h.ingest.Poll(context.Background(), "infra-live")
		time.Sleep(200 * time.Millisecond)
	}
}

// awaitPullRequest waits until ok holds for the pull request, polling the repository.
func (h *harness) awaitPullRequest(t *testing.T, r *giteatest.Repo, number int64, ok func(*gitea.PullRequest) bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		pr, _, err := giteatest.Get(t).Client.GetPullRequest(r.Owner, r.Name, number)
		if err == nil && ok(pr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pull request %d: still not as expected: %+v, %v", number, pr, err)
		}
		_ = h.ingest.Poll(context.Background(), "infra-live")
		time.Sleep(300 * time.Millisecond)
	}
}

// TestWebhooksThroughTheServer: Gitea's deliveries reach the ingestion through the server's
// /webhooks/ mount: a push is recorded as a webhook, and fetched.
func TestWebhooksThroughTheServer(t *testing.T) {
	g := giteatest.Get(t)
	r := g.Repo(t, "infra")
	pushExample(t, r.CloneURL, func(context.Context) (gitx.Credentials, error) { return gitx.Credentials{Header: g.GitHeader()}, nil }, nil)
	vars := map[string]string{"GITEA_TOKEN": g.Token, "HOOK_SECRET": "a webhook secret"}
	h := start(t, t.TempDir(), `connector: gitea
  url: `+r.CloneURL+`
  credentials: { ref: env:GITEA_TOKEN }
  webhooks: { secret: { ref: env:HOOK_SECRET } }
  delivery: { mode: direct }
  sync: { minInterval: 1ns }`, repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { v, ok := vars[n]; return v, ok }}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"gitea": giteaconn.New}},
	})
	r.Webhook(t, h.srv.URL+"/webhooks/infra-live", vars["HOOK_SECRET"])
	if err := r.Commit("main", "docs/notes.md", "# Notes\n"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		s, err := h.store.GetRepositorySync(context.Background(), "infra-live")
		if err == nil && s.LastWebhookAt.Valid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no webhook recorded: %+v, %v\nlog:\n%s", s, err, h.log.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	resp, err := http.Post(h.srv.URL+"/webhooks/elsewhere", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a delivery for no repository = %d", resp.StatusCode)
	}
}
