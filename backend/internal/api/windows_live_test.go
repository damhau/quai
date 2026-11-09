package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
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

// TestAzureDevOpsChangeWindow: on the Azure DevOps sandbox, an approved request outside its
// change window waits: quai/approval pending with the reason, auto-complete not set; when the
// window opens, Quai sets it and Azure DevOps completes the pull request. Skipped without the
// sandbox's PAT.
func TestAzureDevOpsChangeWindow(t *testing.T) {
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
	}, withWindows(officeHours))
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	requireApproval(t, sb, repo, id)
	liveChangeWindow(t, h, repo, nil)
}

// TestGitHubChangeWindow: TestAzureDevOpsChangeWindow on the GitHub sandbox, main requiring
// quai/approval from the app, with auto-merge allowed or a merge queue: nothing is armed and no
// merge group built before the window opens. Skipped without the sandbox's key.
func TestGitHubChangeWindow(t *testing.T) {
	s := githubtest.New(t)
	for _, queue := range []bool{false, true} {
		t.Run(map[bool]string{false: "auto-merge", true: "merge queue"}[queue], func(t *testing.T) {
			ctx := context.Background()
			name, spec := githubRepository(t, s)
			if queue {
				if _, err := s.MergeQueue(ctx, name, "quai/approval"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := s.AllowAutoMerge(ctx, name, true); err != nil {
					t.Fatal(err)
				}
				if _, err := s.RequireCheck(ctx, name, "quai/approval"); err != nil {
					t.Fatal(err)
				}
			}
			h := start(t, t.TempDir(), spec+`
  delivery: { mode: pull-request, merge: auto, mergeMethod: squash }
  sync: { minInterval: 1ns }`, githubOptions(s), withWindows(officeHours))
			repo, err := h.reg.Get("infra-live")
			if err != nil {
				t.Fatal(err)
			}
			liveChangeWindow(t, h, repo, func(pr connectors.PullRequest) string {
				// A merge group is a branch under gh-readonly-queue/.
				refs, _, err := s.Client.Git.ListMatchingRefs(ctx, s.Org, name, "heads/gh-readonly-queue/")
				if err != nil || len(refs) > 0 {
					return fmt.Sprintf("merge groups %d, %v", len(refs), err)
				}
				return ""
			})
		})
	}
}

// liveChangeWindow approves a production request on a Saturday, to the test clock, outside the
// office-hours window: quai/approval stays pending with the reason, the pull request open and
// nothing armed on the platform (auto-complete, or what armed says, "" for nothing). On Monday,
// the windows' sweep releases it, and the platform merges it.
func liveChangeWindow(t *testing.T, h *harness, repo *repositories.Repo, armed func(connectors.PullRequest) string) {
	t.Helper()
	ctx := context.Background()
	bob, alice := h.as("bob"), h.as("alice")
	h.clock.set(saturday)
	r := bob.submit(resizeBody(6))
	r = bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("approve = %d %s", status, body)
	}
	r = held(bob, r.ID)
	prs := repo.Connector.PullRequests()
	reason := "Approved; waits for the change window, opens Mon 08:00 Europe/Zurich"
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(2 * time.Second) {
		pr, err := prs.Get(ctx, r.Delivery.PullRequest)
		if err != nil {
			t.Fatal(err)
		}
		statuses, err := repo.Connector.Statuses().List(ctx, connectors.StatusTarget{PullRequest: pr.ID, Commit: pr.Head})
		var approval connectors.Status
		for _, st := range statuses {
			if st.Name == "quai/approval" {
				approval = st
			}
		}
		if err == nil && approval.State == connectors.StatusPending && strings.Contains(approval.Description, reason) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("quai/approval: %+v, %v\nlog:\n%s", approval, err, h.log.String())
		}
	}
	h.awaitJobs(t)
	pr, err := prs.Get(ctx, r.Delivery.PullRequest)
	if err != nil || pr.State != connectors.PROpen || pr.AutoComplete {
		t.Fatalf("outside the window, the pull request: %+v, %v", pr, err)
	}
	if armed != nil {
		if got := armed(pr); got != "" {
			t.Fatalf("outside the window, armed: %s", got)
		}
	}
	if kinds := h.eventKinds(r.ID); count(kinds, "merging") != 0 || count(kinds, "window_waiting") != 1 {
		t.Fatalf("events outside the window: %v", kinds)
	}

	h.clock.set(monday)
	if err := h.requests.SweepWindows(ctx); err != nil {
		t.Fatal(err)
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
	if kinds := h.eventKinds(r.ID); count(kinds, "window_opened") != 1 || count(kinds, "merged") != 1 {
		t.Errorf("events = %v", kinds)
	}
}
