package api

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v88/github"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/github"
	"github.com/damhau/quai/backend/internal/githubtest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
)

// sandboxKey resolves env:GITHUB_KEY to the sandbox app's private key.
type sandboxKey struct{ s *githubtest.Sandbox }

func (k sandboxKey) Get(ctx context.Context, name string) (secrets.Secret, error) {
	if name != "GITHUB_KEY" {
		return secrets.Secret{}, secrets.ErrNotFound
	}
	return k.s.Get(ctx, name)
}

// githubOptions connect Quai to the sandbox, as cmd/quai does to github.com.
func githubOptions(s *githubtest.Sandbox) repositories.Options {
	return repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": sandboxKey{s}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"github": github.New}, HTTP: s.HTTP},
		Network:    s.Network,
	}
}

// githubRepository creates a repository of the sandbox holding the example's files, and returns
// the spec of a connection to it.
func githubRepository(t *testing.T, s *githubtest.Sandbox) (name, spec string) {
	t.Helper()
	name, _ = s.Repository(t)
	c, err := github.New(connectors.Config{URL: s.URL(name), HTTP: s.HTTP, Credentials: s.PrivateKey, AppID: s.AppID})
	if err != nil {
		t.Fatal(err)
	}
	pushExample(t, s.URL(name), c.(*github.Connector).GitAuth, s.Network.Proxy)
	return name, `connector: github
  url: ` + s.URL(name) + `
  credentials: { ref: env:GITHUB_KEY, appId: ` + strconv.FormatInt(s.AppID, 10) + ` }`
}

// TestGitHubEnforcement: on the sandbox, a ruleset requiring quai/approval from the app is
// enforcement; made to accept any source, then removed, it is not, which blocks new pull requests;
// restored, then created again, it is. Skipped without the sandbox's key.
func TestGitHubEnforcement(t *testing.T) {
	s := githubtest.New(t)
	ctx := context.Background()
	name, spec := githubRepository(t, s)
	h := start(t, t.TempDir(), spec+`
  delivery: { mode: pull-request, merge: manual }
  sync: { minInterval: 1ns }`, githubOptions(s))
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.RequireCheck(ctx, name, "quai/approval")
	if err != nil {
		t.Fatal(err)
	}
	bob := h.as("bob")
	// verified waits until GitHub shows what want says, has Quai verify and record it, then plans:
	// want "" is enforced.
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
	verified("the ruleset as Quai needs it", "")
	anySource := s.ApprovalRuleset("quai/approval")
	anySource.Rules.RequiredStatusChecks.RequiredStatusChecks[0].IntegrationID = nil
	if _, _, err := s.Client.Repositories.UpdateRuleset(ctx, s.Org, name, id, anySource); err != nil {
		t.Fatal(err)
	}
	verified("any source can set the check", "accepts quai/approval from any source")
	if _, _, err := s.Client.Repositories.UpdateRuleset(ctx, s.Org, name, id, s.ApprovalRuleset("quai/approval")); err != nil {
		t.Fatal(err)
	}
	verified("restored", "")
	if _, err := s.Client.Repositories.DeleteRuleset(ctx, s.Org, name, id); err != nil {
		t.Fatal(err)
	}
	verified("the ruleset removed", "no branch protection or ruleset on main requires quai/approval")
	if _, err := s.RequireCheck(ctx, name, "quai/approval"); err != nil {
		t.Fatal(err)
	}
	verified("created again", "")
	if got := h.timeline(t); !slices.Equal(got, []string{"alert_enforcement", "enforcement_restored", "alert_enforcement", "enforcement_restored"}) {
		t.Errorf("timeline = %v", got)
	}
}

// TestGitHubAccess: on the sandbox, a GitHub connection's access test passes every check and
// leaves no branch or open pull request behind, and the connection is made through the API.
// Skipped without the sandbox's key.
func TestGitHubAccess(t *testing.T) {
	s := githubtest.New(t)
	ctx := context.Background()
	name, spec := githubRepository(t, s)
	if _, err := s.RequireCheck(ctx, name, "quai/approval"); err != nil {
		t.Fatal(err)
	}
	h := start(t, t.TempDir(), spec+`
  sync: { minInterval: 1ns }`, githubOptions(s))
	alice := h.as("alice")
	doc := document("infra-two", map[string]any{"connector": "github", "url": s.URL(name), "credentials": map[string]any{"ref": "env:GITHUB_KEY", "appId": s.AppID},
		"delivery": map[string]any{"mode": "pull-request"}})

	var v AccessView
	if status, body := alice.do(http.MethodPost, Prefix+"/repository-access-tests", doc, &v); status != http.StatusOK {
		t.Fatalf("access test = %d %s", status, body)
	}
	ran := 0
	for _, c := range v.Checks {
		switch {
		case c.Skipped != "" && c.Name != repositories.CheckPipelines:
			t.Errorf("check %s skipped: %s", c.Name, c.Skipped)
		case c.Skipped == "" && !c.OK:
			t.Errorf("check %s: %+v", c.Name, c)
		case c.Skipped == "":
			ran++
		}
	}
	if !v.OK || ran != 8 || v.Scan == nil || !v.Scan.Found {
		t.Errorf("access test = %+v", v)
	}
	refs, _, err := s.Client.Git.ListMatchingRefs(ctx, s.Org, name, "heads/quai/")
	if err != nil || len(refs) != 0 {
		t.Errorf("branches left behind: %v, %v", refs, err)
	}
	prs, _, err := s.Client.PullRequests.List(ctx, s.Org, name, &gh.PullRequestListOptions{State: "all"})
	if err != nil || len(prs) != 1 || prs[0].GetState() != "closed" || prs[0].GetMerged() {
		t.Errorf("the access test's pull request: %v, %v", prs, err)
	}

	if status, body := alice.do(http.MethodPost, Prefix+"/repositories", doc, nil); status != http.StatusCreated {
		t.Fatalf("connect = %d %s", status, body)
	}
	if _, err := h.reg.Get("infra-two"); err != nil {
		t.Errorf("the connection: %v", err)
	}
	if status, body := alice.do(http.MethodDelete, Prefix+"/repositories/infra-two", nil, nil); status != http.StatusNoContent {
		t.Errorf("disconnect = %d %s", status, body)
	}
}
