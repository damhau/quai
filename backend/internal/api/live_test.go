package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/adotest"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/azuredevops"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
)

// sandboxPAT resolves env:ADO_PAT to the sandbox's PAT.
type sandboxPAT struct{ sb *adotest.Sandbox }

func (p sandboxPAT) Get(ctx context.Context, name string) (secrets.Secret, error) {
	if name != "ADO_PAT" {
		return secrets.Secret{}, secrets.ErrNotFound
	}
	return p.sb.Get(ctx, name)
}

// approvalPolicy is the branch policy Quai needs on the sandbox repository's main: a required
// "Status check" for quai/approval that only Quai's identity (author) satisfies, reset on new
// changes.
type approvalPolicy struct {
	sb     *adotest.Sandbox
	id     int
	body   map[string]any
	author string
}

// requireApproval creates the approval policy on the repository's main.
func requireApproval(t *testing.T, sb *adotest.Sandbox, repo *repositories.Repo, repositoryID string) *approvalPolicy {
	t.Helper()
	me, err := repo.Connector.(*azuredevops.Connector).Client().Detect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p := &approvalPolicy{sb: sb, author: me.User.ID, body: map[string]any{"isEnabled": true, "isBlocking": true, "type": map[string]string{"id": "cbdc66da-9728-4af8-aada-9a5a32e4a226"},
		"settings": map[string]any{"statusGenre": "quai", "statusName": "approval", "authorId": me.User.ID, "invalidateOnSourceUpdate": true,
			"scope": []map[string]any{{"repositoryId": repositoryID, "refName": "refs/heads/main", "matchKind": "exact"}}}}}
	var created struct {
		ID int `json:"id"`
	}
	if err := sb.Do(context.Background(), http.MethodPost, adotest.Path(sb.Project, "_apis", "policy", "configurations"), nil, p.body, &created); err != nil {
		t.Fatal(err)
	}
	p.id = created.ID
	return p
}

// update sets the policy's authorized identity ("" for any identity).
func (p *approvalPolicy) update(t *testing.T, author string) {
	t.Helper()
	p.body["settings"].(map[string]any)["authorId"] = author
	if author == "" {
		delete(p.body["settings"].(map[string]any), "authorId")
	}
	if err := p.sb.Do(context.Background(), http.MethodPut, adotest.Path(p.sb.Project, "_apis", "policy", "configurations", strconv.Itoa(p.id)), nil, p.body, nil); err != nil {
		t.Fatal(err)
	}
}

func (p *approvalPolicy) delete(t *testing.T) {
	t.Helper()
	if err := p.sb.Do(context.Background(), http.MethodDelete, adotest.Path(p.sb.Project, "_apis", "policy", "configurations", strconv.Itoa(p.id)), nil, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// land puts a change on main as the approval policy allows: through a pull request whose
// quai/approval succeeded, completed. With the sandbox's PAT, the test is Quai's identity.
func land(t *testing.T, repo *repositories.Repo, c plan.Change) {
	t.Helper()
	ctx := context.Background()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	branch := "test/" + hex.EncodeToString(b)
	sha, err := repo.Mirror.CommitAndPush(ctx, gitx.Commit{Branch: branch, Base: "main", Message: "Edit " + c.Path + "\n", Changes: []plan.Change{c},
		Author: gitx.Signature{Name: "Carol", Email: "carol@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	prs := repo.Connector.PullRequests()
	pr, err := prs.Open(ctx, connectors.NewPullRequest{Source: branch, Target: "main", Title: "Edit " + c.Path})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Connector.Statuses().Set(ctx, connectors.StatusTarget{PullRequest: pr.ID, Commit: sha},
		connectors.Status{Name: "quai/approval", State: connectors.StatusSuccess, Description: "A test's change"}); err != nil {
		t.Fatal(err)
	}
	// The policy is evaluated shortly after the status.
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(2 * time.Second) {
		_, err := prs.Merge(ctx, pr.ID, connectors.MergeOptions{Head: sha, Method: connectors.Squash, Message: "Edit " + c.Path, DeleteBranch: true})
		if got, gerr := prs.Get(ctx, pr.ID); gerr == nil && got.State == connectors.PRMerged {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("landing %s: %v", c.Path, err)
		}
	}
}

// TestAzureDevOpsDelivery: on the sandbox, a request opens a pull request with quai/approval
// pending, required by a status check policy that only Quai's identity satisfies; the approval
// sets the status and auto-complete, Azure DevOps completes the pull request, and the poll
// applies the request. Skipped without the sandbox's PAT.
func TestAzureDevOpsDelivery(t *testing.T) {
	sb := adotest.New(t)
	ctx := context.Background()
	name, id := sb.Repository(t)
	pushExample(t, sb.URL(name), func(ctx context.Context) (gitx.Credentials, error) {
		pat, _, _ := sb.PAT(ctx)
		return gitx.Credentials{Header: "Authorization: " + azuredevops.BasicAuth(pat.Reveal())}, nil
	}, sb.Network.Proxy)
	h := start(t, t.TempDir(), `connector: azure-devops
  url: `+sb.URL(name)+`
  credentials: { ref: env:ADO_PAT }
  delivery: { mode: pull-request, merge: auto, mergeMethod: squash }
  sync: { minInterval: 1ns }`, repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": sandboxPAT{sb}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"azure-devops": azuredevops.New}, HTTP: sb.HTTP},
		Network:    sb.Network,
	})
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	requireApproval(t, sb, repo, id)

	bob, alice := h.as("bob"), h.as("alice")
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resizeBody(6), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	r = bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	pr, err := repo.Connector.PullRequests().Get(ctx, r.Delivery.PullRequest)
	if err != nil || pr.Source != "quai/r-1-web01-resize" || !strings.Contains(pr.Description, connectors.Marker(r.ID)) || len(pr.Labels) == 0 {
		t.Errorf("the pull request: source %q, labels %v, marker %v, %v", pr.Source, pr.Labels, strings.Contains(pr.Description, connectors.Marker(r.ID)), err)
	}
	statuses, err := repo.Connector.Statuses().List(ctx, connectors.StatusTarget{PullRequest: pr.ID})
	if err != nil || len(statuses) == 0 || statuses[0].Name != "quai/approval" || statuses[0].State != connectors.StatusPending {
		t.Errorf("statuses: %+v, %v", statuses, err)
	}

	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("approve = %d %s", status, body)
	}
	r = bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	if r.Delivery.MergeCommit == "" {
		t.Errorf("delivery = %+v", r.Delivery)
	}
	// The squash commit carries the approvals; comments reach the pull request.
	if err := repo.Mirror.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if log, err := repo.Mirror.Log(ctx, "main", "", 1); err != nil || len(log) == 0 || !strings.Contains(log[0].Body, "Approved-by: alice") {
		t.Errorf("the squash commit: %+v, %v", log, err)
	}
	if err := repo.Connector.PullRequests().Comment(ctx, pr.ID, "Approved-by: alice"); err != nil {
		t.Errorf("comment: %v", err)
	}
}

// TestAzureDevOpsContent: approvals bound to content on the sandbox. The base moves (another
// pull request completes): the branch is rebuilt on it and the approval kept. Someone pushes to
// the branch: the approvals are void, quai/approval pending again. An owned region changes on the
// base: the request is stale, quai/approval not applicable. Skipped without the sandbox's PAT.
func TestAzureDevOpsContent(t *testing.T) {
	sb := adotest.New(t)
	ctx := context.Background()
	name, id := sb.Repository(t)
	pushExample(t, sb.URL(name), func(ctx context.Context) (gitx.Credentials, error) {
		pat, _, _ := sb.PAT(ctx)
		return gitx.Credentials{Header: "Authorization: " + azuredevops.BasicAuth(pat.Reveal())}, nil
	}, sb.Network.Proxy)
	h := start(t, t.TempDir(), `connector: azure-devops
  url: `+sb.URL(name)+`
  credentials: { ref: env:ADO_PAT }
  delivery: { mode: pull-request, merge: manual, mergeMethod: squash }
  sync: { minInterval: 1ns }`, repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": sandboxPAT{sb}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"azure-devops": azuredevops.New}, HTTP: sb.HTTP},
		Network:    sb.Network,
	})
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	requireApproval(t, sb, repo, id)
	// someone pushes a change to branch, as a person would.
	someone := func(branch string, c plan.Change) {
		t.Helper()
		if _, err := repo.Mirror.CommitAndPush(ctx, gitx.Commit{Branch: branch, Message: "Edit " + c.Path + "\n", Changes: []plan.Change{c},
			Author: gitx.Signature{Name: "Carol", Email: "carol@example.com"}}); err != nil {
			t.Fatal(err)
		}
	}
	// file reads a file on branch, as the remote has it.
	file := func(branch, path string) string {
		t.Helper()
		if err := repo.Mirror.Fetch(ctx); err != nil {
			t.Fatal(err)
		}
		head, err := repo.Mirror.Head(ctx, branch)
		if err != nil {
			t.Fatal(err)
		}
		b, err := repo.Mirror.ReadFile(ctx, head, path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// status waits until the pull request's latest quai/approval is in state.
	status := func(id string, want connectors.StatusState, description string) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for {
			got, err := repo.Connector.Statuses().List(ctx, connectors.StatusTarget{PullRequest: id})
			if err == nil && len(got) > 0 && got[0].State == want && strings.Contains(got[0].Description, description) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("quai/approval is not %s (%q): %+v, %v", want, description, got, err)
			}
			_ = h.ingest.Poll(ctx, "infra-live")
			time.Sleep(time.Second)
		}
	}

	r := approved(t, h)
	before, err := repo.Connector.PullRequests().Get(ctx, r.Delivery.PullRequest)
	if err != nil {
		t.Fatal(err)
	}
	land(t, repo, plan.Change{Op: plan.OpCreate, Path: "docs/notes.md", After: []byte("# Notes\n")})
	r = h.as("bob").awaitDelivery(r.ID, "rebuilt", func(v RequestView) bool { return hasEvent(v, "rebuilt", "") })
	if r.State != "approved" || len(r.Reviews) != 1 {
		t.Errorf("after the rebuild: %s, reviews %+v", r.State, r.Reviews)
	}
	// Azure DevOps updates the pull request's source commit shortly after the push.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		pr, err := repo.Connector.PullRequests().Get(ctx, r.Delivery.PullRequest)
		if err == nil && pr.Head != before.Head {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pull request's head did not move from %.12s: %v", before.Head, err)
		}
	}
	status(r.Delivery.PullRequest, connectors.StatusSuccess, "Approved")

	web01 := file(r.Delivery.Branch, "envs/prod/vms/web01.yaml")
	someone(r.Delivery.Branch, plan.Change{Op: plan.OpUpdate, Path: "envs/prod/vms/web01.yaml", After: []byte(strings.Replace(web01, "cpu: 6", "cpu: 7", 1))})
	r = h.as("bob").awaitDelivery(r.ID, "modified outside Quai", func(v RequestView) bool { return v.State == "pending_approval" })
	if !hasEvent(r, "modified_outside_quai", "approvals are void") || len(r.Reviews) != 0 || !strings.Contains(diffText(r.Changes[0]), "+cpu: 7") {
		t.Errorf("after the outside push: events %+v, reviews %+v", r.Events, r.Reviews)
	}
	status(r.Delivery.PullRequest, connectors.StatusPending, "Modified outside Quai")

	main := file("main", "envs/prod/vms/web01.yaml")
	land(t, repo, plan.Change{Op: plan.OpUpdate, Path: "envs/prod/vms/web01.yaml", After: []byte(strings.Replace(main, "memory_gb: 16", "memory_gb: 24", 1))})
	r = h.as("bob").awaitDelivery(r.ID, "stale", func(v RequestView) bool { return v.State == "stale" })
	if !hasEvent(r, "stale", "envs/prod/vms/web01.yaml") {
		t.Errorf("events = %+v", r.Events)
	}
	status(r.Delivery.PullRequest, connectors.StatusNotApplicable, "Stale")
}

// TestAzureDevOpsEnforcement: on the sandbox, removing the approval policy's authorized identity,
// then the policy, is detected and blocks new pull requests; restoring it unblocks. Skipped
// without the sandbox's PAT.
func TestAzureDevOpsEnforcement(t *testing.T) {
	sb := adotest.New(t)
	ctx := context.Background()
	name, id := sb.Repository(t)
	pushExample(t, sb.URL(name), func(ctx context.Context) (gitx.Credentials, error) {
		pat, _, _ := sb.PAT(ctx)
		return gitx.Credentials{Header: "Authorization: " + azuredevops.BasicAuth(pat.Reveal())}, nil
	}, sb.Network.Proxy)
	h := start(t, t.TempDir(), `connector: azure-devops
  url: `+sb.URL(name)+`
  credentials: { ref: env:ADO_PAT }
  delivery: { mode: pull-request, merge: manual }
  sync: { minInterval: 1ns }`, repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": sandboxPAT{sb}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"azure-devops": azuredevops.New}, HTTP: sb.HTTP},
		Network:    sb.Network,
	})
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	policy := requireApproval(t, sb, repo, id)
	bob := h.as("bob")
	// verified waits until the platform shows what want says (policies change shortly after the
	// API returns), has Quai verify and record it, then plans: want "" is enforced.
	verified := func(what, want string) {
		t.Helper()
		for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(2 * time.Second) {
			problems, err := repo.VerifyEnforcement(ctx)
			if err == nil && (want == "" && len(problems) == 0 || want != "" && strings.Contains(strings.Join(problems, "; "), want)) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: problems %v, %v", what, problems, err)
			}
		}
		if err := h.ingest.Verify(ctx, "infra-live"); err != nil {
			t.Fatal(err)
		}
		status, body := bob.do(http.MethodPost, Prefix+"/plans/type", planBody(6), nil)
		if want == "" && status != http.StatusOK || want != "" && (status != http.StatusConflict || !strings.Contains(body, want)) {
			t.Errorf("%s: plan = %d %s", what, status, body)
		}
	}
	verified("the policy as Quai needs it", "")
	policy.update(t, "")
	verified("any identity can set the status", "any identity can set quai/approval")
	policy.update(t, policy.author)
	verified("restored", "")
	policy.delete(t)
	verified("the policy removed", "no status check policy on main requires quai/approval")
	if got := h.timeline(t); !slices.Equal(got, []string{"alert_enforcement", "enforcement_restored", "alert_enforcement"}) {
		t.Errorf("timeline = %v", got)
	}
}
