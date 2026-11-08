package ingest

import (
	"context"
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

// githubEnv resolves env:GITHUB_KEY to the sandbox app's private key, and env:HOOK_SECRET.
type githubEnv struct{ s *githubtest.Sandbox }

func (e githubEnv) Get(ctx context.Context, name string) (secrets.Secret, error) {
	if name == "GITHUB_KEY" {
		return e.s.Get(ctx, name)
	}
	return secrets.Env{Lookup: func(n string) (string, bool) { return hookSecret, n == "HOOK_SECRET" }}.Get(ctx, name)
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
		Secrets:    secrets.New(map[string]secrets.Backend{"env": githubEnv{s}}),
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
