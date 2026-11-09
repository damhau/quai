package api

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/adotest"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/azuredevops"
	"github.com/damhau/quai/backend/internal/connectors/github"
	"github.com/damhau/quai/backend/internal/githubtest"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/secrets"
)

// TestAzureDevOpsExpiry: on the Azure DevOps sandbox, a request pending approval past its vm
// type's expiry expires: quai/approval fails, Quai says why on its pull request, once, and
// abandons it. Skipped without the sandbox's PAT.
func TestAzureDevOpsExpiry(t *testing.T) {
	sb := adotest.New(t)
	name, id := sb.Repository(t)
	pushEdited(t, sb.URL(name), func(ctx context.Context) (gitx.Credentials, error) {
		pat, _, _ := sb.PAT(ctx)
		return gitx.Credentials{Header: "Authorization: " + azuredevops.BasicAuth(pat.Reveal())}, nil
	}, sb.Network.Proxy, withExpiry("1d"), nil)
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
	liveExpiry(t, h, repo)
}

// TestGitHubExpiry: TestAzureDevOpsExpiry on the GitHub sandbox, main requiring quai/approval
// from the app. Skipped without the sandbox's key.
func TestGitHubExpiry(t *testing.T) {
	s := githubtest.New(t)
	ctx := context.Background()
	name, _ := s.Repository(t)
	c, err := github.New(connectors.Config{URL: s.URL(name), HTTP: s.HTTP, Credentials: s.PrivateKey, AppID: s.AppID})
	if err != nil {
		t.Fatal(err)
	}
	pushEdited(t, s.URL(name), c.(*github.Connector).GitAuth, s.Network.Proxy, withExpiry("1d"), nil)
	if _, err := s.RequireCheck(ctx, name, "quai/approval"); err != nil {
		t.Fatal(err)
	}
	h := start(t, t.TempDir(), `connector: github
  url: `+s.URL(name)+`
  credentials: { ref: env:GITHUB_KEY, appId: `+strconv.FormatInt(s.AppID, 10)+` }
  delivery: { mode: pull-request, merge: manual, mergeMethod: squash }
  sync: { minInterval: 1ns }`, githubOptions(s))
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	liveExpiry(t, h, repo)
}

// liveExpiry submits a request whose definitions expire in a day, waits for its pull request,
// and sweeps a day later: the request expires, quai/approval fails ("Expired in Quai"), one
// comment says why, and the pull request is closed. Run again, Quai neither comments nor closes
// again.
func liveExpiry(t *testing.T, h *harness, repo *repositories.Repo) {
	t.Helper()
	ctx := context.Background()
	bob := h.as("bob")
	h.as("alice")
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	r := bob.submit(resizeBody(6))
	if r.Expiry != "1d" {
		t.Fatalf("the request's expiry: %q", r.Expiry)
	}
	r = bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	prs := repo.Connector.PullRequests()

	h.sweep(t0.Add(24 * time.Hour))
	bob.await(r.ID, "expired")
	var pr connectors.PullRequest
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(2 * time.Second) {
		got, err := prs.Get(ctx, r.Delivery.PullRequest)
		if err == nil && got.State == connectors.PRAbandoned {
			pr = got
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pull request: %+v, %v\nlog:\n%s", got, err, h.log.String())
		}
	}
	h.awaitJobs(t)
	marker := "<!-- quai:expired=" + r.ID + " -->"
	commented := func() []string {
		t.Helper()
		comments, err := prs.Comments(ctx, pr.ID)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range comments {
			if strings.Contains(c, marker) {
				out = append(out, c)
			}
		}
		return out
	}
	if got := commented(); len(got) != 1 || !strings.Contains(got[0], "waited for approval longer than its expiry (1d)") {
		t.Errorf("the expiry's comments: %q", got)
	}
	statuses, err := repo.Connector.Statuses().List(ctx, connectors.StatusTarget{PullRequest: pr.ID, Commit: pr.Head})
	failed := false
	for _, st := range statuses {
		if st.Name == "quai/approval" {
			failed = st.State == connectors.StatusFailure && st.Description == "Expired in Quai"
			break
		}
	}
	if err != nil || !failed {
		t.Errorf("quai/approval: %+v, %v", statuses, err)
	}

	if _, err := h.jobs.Insert(ctx, requests.StatusArgs{RequestID: uuid.MustParse(r.ID)}, nil); err != nil {
		t.Fatal(err)
	}
	h.awaitJobs(t)
	if got := commented(); len(got) != 1 {
		t.Errorf("the expiry's comments, run again: %q", got)
	}
	kinds := h.eventKinds(r.ID)
	if count(kinds, "pull_request_closed") != 1 || count(kinds, "expiry_commented") != 1 || count(kinds, "pull_request_left") != 0 {
		t.Errorf("events = %q", kinds)
	}
}
