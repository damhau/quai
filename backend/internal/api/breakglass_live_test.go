package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/adotest"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/azuredevops"
	"github.com/damhau/quai/backend/internal/githubtest"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
)

// TestAzureDevOpsBreakGlass: on the Azure DevOps sandbox, main requiring quai/approval from
// Quai, a production request pending approval outside its change window is broken by dave:
// quai/approval turns success with "Break-glass by dave", Quai sets auto-complete and Azure
// DevOps completes the pull request. Skipped without the sandbox's PAT.
func TestAzureDevOpsBreakGlass(t *testing.T) {
	sb := adotest.New(t)
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
	}, withWindows(officeHours), withBreakGlass)
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	requireApproval(t, sb, repo, id)
	liveBreakGlass(t, h, repo)
}

// TestGitHubBreakGlass: TestAzureDevOpsBreakGlass on the GitHub sandbox, main requiring
// quai/approval from the app, with auto-merge allowed. Skipped without the sandbox's key.
func TestGitHubBreakGlass(t *testing.T) {
	s := githubtest.New(t)
	ctx := context.Background()
	name, spec := githubRepository(t, s)
	if err := s.AllowAutoMerge(ctx, name, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequireCheck(ctx, name, "quai/approval"); err != nil {
		t.Fatal(err)
	}
	h := start(t, t.TempDir(), spec+`
  delivery: { mode: pull-request, merge: auto, mergeMethod: squash }
  sync: { minInterval: 1ns }`, githubOptions(s), withWindows(officeHours), withBreakGlass)
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	liveBreakGlass(t, h, repo)
}

// liveBreakGlass submits a production request on a Saturday, to the test clock, outside the
// office-hours window, and has dave break glass for it before anyone approves: quai/approval turns
// success with "Break-glass by dave" on the pull request's head, and the platform merges it, as
// Quai's merge (not a bypass), its review open.
func liveBreakGlass(t *testing.T, h *harness, repo *repositories.Repo) {
	t.Helper()
	ctx := context.Background()
	bob, dave := h.as("bob"), h.as("dave")
	h.clock.set(saturday)
	r := bob.submit(resizeBody(6))
	r = bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" && v.Delivery.Head != "" })
	v := dave.read(r.ID)
	if v.State != "pending_approval" || !v.CanBreakGlass {
		t.Fatalf("dave cannot break glass for %s: %q", v.State, v.CannotBreakGlass)
	}
	status, raw, broken := dave.breakGlass(v, "Checkout is down: the VM needs more CPU", "INC-2001")
	if status != http.StatusOK || broken.State != "approved" || broken.BreakGlass == nil {
		t.Fatalf("break-glass = %d %s", status, raw)
	}

	statuses := repo.Connector.Statuses()
	var approval connectors.Status
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(2 * time.Second) {
		list, err := statuses.List(ctx, connectors.StatusTarget{PullRequest: v.Delivery.PullRequest, Commit: v.Delivery.Head})
		for _, st := range list {
			if st.Name == "quai/approval" {
				approval = st
			}
		}
		if err == nil && approval.State == connectors.StatusSuccess {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("quai/approval: %+v, %v\nlog:\n%s", approval, err, h.log.String())
		}
	}
	if approval.Description != "Break-glass by dave" {
		t.Errorf("quai/approval's description = %q", approval.Description)
	}

	// The platform merges in its own time.
	for deadline := time.Now().Add(4 * time.Minute); ; time.Sleep(2 * time.Second) {
		bob.do(http.MethodGet, Prefix+"/requests/"+r.ID, nil, &r)
		if r.State == "applied" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not applied: %s, %+v\nlog:\n%s", r.State, r.Delivery, h.log.String())
		}
		_ = h.ingest.Poll(ctx, "infra-live")
	}
	if r.Delivery.Head != v.Delivery.Head {
		t.Errorf("merged head %s, broken for %s", r.Delivery.Head, v.Delivery.Head)
	}
	if kinds := h.eventKinds(r.ID); count(kinds, "break_glass") != 1 || count(kinds, "merged") != 1 || count(kinds, "approved") != 0 {
		t.Errorf("events = %v", kinds)
	}
	if page := h.auditLog("limit=500&action=request.merged_without_approval"); len(page.Events) != 0 {
		t.Errorf("a bypass was recorded: %+v", page.Events)
	}
	if page := h.auditLog("limit=500&action=request.merged_outside_window"); len(page.Events) != 0 {
		t.Errorf("a merge outside the window was recorded: %+v", page.Events)
	}
	if r.BreakGlass == nil || !r.BreakGlass.Open {
		t.Errorf("the review after the merge: %+v", r.BreakGlass)
	}
}
