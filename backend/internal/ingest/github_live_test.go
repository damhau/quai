package ingest

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	gh "github.com/google/go-github/v88/github"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/github"
	"github.com/damhau/quai/backend/internal/githubtest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
)

// githubEnv resolves env:GITHUB_KEY to the sandbox app's private key, and env:HOOK_SECRET to hook,
// or hookSecret when it is empty.
type githubEnv struct {
	s    *githubtest.Sandbox
	hook string
}

func (e githubEnv) Get(ctx context.Context, name string) (secrets.Secret, error) {
	if name == "GITHUB_KEY" {
		return e.s.Get(ctx, name)
	}
	hook := e.hook
	if hook == "" {
		hook = hookSecret
	}
	return secrets.Env{Lookup: func(n string) (string, bool) { return hook, n == "HOOK_SECRET" }}.Get(ctx, name)
}

// TestGitHubReplay: GitHub's deliveries, replayed to Quai's endpoint as the app's history recorded
// them, signature included, are taken once and processed: the push is fetched, the pull request
// read again. A forged signature is refused. Subscribe installs a repository webhook only with
// the Webhooks permission, which the sandbox's app does not have: its error says so. Skipped
// without the sandbox's key.
func TestGitHubReplay(t *testing.T) {
	s := githubtest.New(t)
	secret := s.WebhookSecret(t)
	ctx := context.Background()
	name, id := s.Repository(t)
	e := start(t, name, `connector: github
  url: `+s.URL(name)+`
  credentials: { ref: env:GITHUB_KEY, appId: `+strconv.FormatInt(s.AppID, 10)+` }`, repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": githubEnv{s, secret.Reveal()}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"github": github.New}, HTTP: s.HTTP},
		Network:    s.Network,
	})
	e.proxy = s.Network.Proxy
	hook := "https://quai-ingest.invalid/webhooks/" + name
	switch err := e.repo().Connector.Webhooks().Subscribe(ctx, hook, secret); {
	case err == nil:
		hooks, _, err := s.Client.Repositories.ListHooks(ctx, s.Org, name, nil)
		if err != nil || len(hooks) != 1 || hooks[0].GetConfig().GetURL() != hook {
			t.Errorf("the webhook installed: %v, %v", hooks, err)
		}
	case !strings.Contains(err.Error(), "Webhooks (read and write)"):
		t.Errorf("subscribe without the permission: %v", err)
	}

	pr := e.openPR("quai/r-3-web01")
	deliveries := s.Deliveries(t, id, func(ds []githubtest.Delivery) bool {
		return githubtest.HasEvent(ds, "push", "") && githubtest.HasEvent(ds, "pull_request", "opened")
	})
	for _, d := range deliveries {
		if status, out := e.send("/webhooks/"+name, delivery{d.Header, d.Body}); status != http.StatusAccepted || out["duplicate"] == true {
			t.Errorf("%s.%s: %d %v", d.Event, d.Action, status, out)
		}
	}
	e.eventually("the pull request is re-read from GitHub", func() bool { return e.handler.seen(pr.ID, connectors.PROpen) })
	e.eventually("the push is fetched", func() bool {
		s, err := e.store.GetRepositorySync(ctx, name)
		return err == nil && s.LastWebhookAt.Valid && s.LastFetchAt.Valid && s.Head != ""
	})
	if status, out := e.send("/webhooks/"+name, delivery{deliveries[0].Header, deliveries[0].Body}); status != http.StatusAccepted || out["duplicate"] != true {
		t.Errorf("a redelivery: %d %v", status, out)
	}
	forged := deliveries[0].Header.Clone()
	forged.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	if status, _ := e.send("/webhooks/"+name, delivery{forged, deliveries[0].Body}); status != http.StatusUnauthorized {
		t.Errorf("a forged signature = %d", status)
	}
}

// TestGitHubTrust: on GitHub, the definitions are trusted when CODEOWNERS covers .quai/ and the
// branch requires code owners' review; CODEOWNERS alone only requests reviews. Skipped without the
// sandbox's key.
func TestGitHubTrust(t *testing.T) {
	s := githubtest.New(t)
	ctx := context.Background()
	name, _ := s.Repository(t)
	e := start(t, name, `connector: github
  url: `+s.URL(name)+`
  credentials: { ref: env:GITHUB_KEY, appId: `+strconv.FormatInt(s.AppID, 10)+` }`, repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": githubEnv{s: s}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"github": github.New}, HTTP: s.HTTP},
		Network:    s.Network,
	})
	if err := e.svc.Poll(ctx, name); err != nil {
		t.Fatal(err)
	}
	if problems, ok := e.trust(); !ok || len(problems) != 3 {
		t.Fatalf("an open repository: %v, %v", problems, ok)
	}

	// The owners first: the ruleset then keeps anyone from pushing to main.
	if err := s.AddTeam(ctx, name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFiles(ctx, name, githubtest.Branch, "Owners", map[string]string{".github/CODEOWNERS": "/.quai/ @" + s.Org + "/" + githubtest.Team + "\n"}); err != nil {
		t.Fatal(err)
	}
	e.eventually("CODEOWNERS covers .quai/, but only requests reviews", func() bool {
		if err := e.svc.Poll(ctx, name); err != nil {
			t.Fatal(err)
		}
		problems, ok := e.trust()
		return ok && len(problems) == 2 && strings.Contains(problems[1], "does not require code owners' review")
	})
	review := s.ApprovalRuleset("quai/approval")
	review.Name = "review"
	review.Rules = &gh.RepositoryRulesetRules{PullRequest: &gh.PullRequestRuleParameters{RequireCodeOwnerReview: true, RequiredApprovingReviewCount: 1,
		AllowedMergeMethods: []gh.PullRequestMergeMethod{gh.PullRequestMergeMethodMerge, gh.PullRequestMergeMethodSquash, gh.PullRequestMergeMethodRebase}}}
	if _, _, err := s.Client.Repositories.CreateRuleset(ctx, s.Org, name, review); err != nil {
		t.Fatal(err)
	}
	e.eventually("the ruleset requires code owners' review", func() bool {
		if err := e.svc.Poll(ctx, name); err != nil {
			t.Fatal(err)
		}
		problems, ok := e.trust()
		return ok && len(problems) == 0
	})
}
