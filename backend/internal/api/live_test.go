package api

import (
	"context"
	"net/http"
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
	me, err := repo.Connector.(*azuredevops.Connector).Client().Detect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	policy := map[string]any{"isEnabled": true, "isBlocking": true, "type": map[string]string{"id": "cbdc66da-9728-4af8-aada-9a5a32e4a226"},
		"settings": map[string]any{"statusGenre": "quai", "statusName": "approval", "authorId": me.User.ID, "invalidateOnSourceUpdate": true,
			"scope": []map[string]any{{"repositoryId": id, "refName": "refs/heads/main", "matchKind": "exact"}}}}
	if err := sb.Do(ctx, http.MethodPost, adotest.Path(sb.Project, "_apis", "policy", "configurations"), nil, policy, nil); err != nil {
		t.Fatal(err)
	}

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

// TestAzureDevOpsContent: approvals bound to content on the sandbox. The base moves: the branch
// is rebuilt on it and the approval kept. Someone pushes to the branch: the approvals are void,
// quai/approval pending again. An owned region changes on the base: the request is stale,
// quai/approval not applicable. Skipped without the sandbox's PAT.
func TestAzureDevOpsContent(t *testing.T) {
	sb := adotest.New(t)
	ctx := context.Background()
	name, _ := sb.Repository(t)
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
	someone("main", plan.Change{Op: plan.OpCreate, Path: "docs/notes.md", After: []byte("# Notes\n")})
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
	someone("main", plan.Change{Op: plan.OpUpdate, Path: "envs/prod/vms/web01.yaml", After: []byte(strings.Replace(main, "memory_gb: 16", "memory_gb: 24", 1))})
	r = h.as("bob").awaitDelivery(r.ID, "stale", func(v RequestView) bool { return v.State == "stale" })
	if !hasEvent(r, "stale", "envs/prod/vms/web01.yaml") {
		t.Errorf("events = %+v", r.Events)
	}
	status(r.Delivery.PullRequest, connectors.StatusNotApplicable, "Stale")
}
