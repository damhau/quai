package gitea

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/conformance"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/giteatest"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/secrets"
)

func TestMain(m *testing.M) { os.Exit(giteatest.Main(m)) }

// harness gives the suite repositories on the test Gitea.
type harness struct{}

func (harness) Setup(t *testing.T, client *http.Client) *conformance.Repository {
	g := giteatest.Get(t)
	r := g.Repo(t, "conformance")
	c := connector(t, r, client)
	return &conformance.Repository{
		Connector:     c,
		URL:           r.CloneURL,
		DefaultBranch: r.DefaultBranch,
		RequireCheck: func(_ context.Context, branch, check string) error {
			return r.Protect(branch, check)
		},
		RemoveEnforcement: func(_ context.Context, branch string) error { return r.Unprotect(branch) },
		AddOwner: func(_ context.Context, pattern string) (string, error) {
			// Gitea's CODEOWNERS patterns are regular expressions.
			return giteatest.Admin, r.Commit(r.DefaultBranch, ".gitea/CODEOWNERS", strings.TrimPrefix(pattern, "/")+".* @"+giteatest.Admin+"\n")
		},
		WebhooksReachable: true,
	}
}

func connector(t *testing.T, r *giteatest.Repo, client *http.Client) connectors.Connector {
	t.Helper()
	g := giteatest.Get(t)
	c, err := New(connectors.Config{URL: r.CloneURL, HTTP: client,
		Credentials: func(context.Context) (secrets.Secret, bool, error) { return secrets.FromValue(g.Token), true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConformance(t *testing.T) { conformance.Run(t, harness{}) }

// TestAutoComplete: Gitea merges once the required check succeeds.
func TestAutoComplete(t *testing.T) {
	ctx := context.Background()
	g := giteatest.Get(t)
	r := g.Repo(t, "auto")
	c := connector(t, r, nil)
	if err := r.Protect("main", "quai/approval"); err != nil {
		t.Fatal(err)
	}
	m, err := gitx.OpenMirror(ctx, r.CloneURL, filepath.Join(t.TempDir(), "m.git"), gitx.Options{Committer: gitx.Signature{Name: "Quai", Email: "quai@example.com"}, Auth: c.GitAuth})
	if err != nil {
		t.Fatal(err)
	}
	head, err := m.CommitAndPush(ctx, gitx.Commit{Branch: "quai/r-1", Base: "main", Message: "Resize web01\n", Author: gitx.Signature{Name: "Bob", Email: "bob@example.com"},
		Changes: []plan.Change{{Op: plan.OpCreate, Path: "web01.yaml", After: []byte("cpu: 8\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := c.PullRequests().Open(ctx, connectors.NewPullRequest{Source: "quai/r-1", Target: "main", Title: "Resize web01", Labels: []string{"quai", "env:prod"}, Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Draft || len(pr.Labels) != 2 {
		t.Errorf("a draft with labels: %+v", pr)
	}
	set := func(state connectors.StatusState) {
		if err := c.Statuses().Set(ctx, connectors.StatusTarget{PullRequest: pr.ID}, connectors.Status{Name: "quai/approval", State: state}); err != nil {
			t.Fatal(err)
		}
	}
	set(connectors.StatusPending)
	draft := "Resize web01"
	if _, err := c.PullRequests().Update(ctx, pr.ID, connectors.PullRequestUpdate{Title: &draft}); err != nil {
		t.Fatal(err)
	}
	if err := c.PullRequests().AutoComplete(ctx, pr.ID, connectors.MergeOptions{Head: head, Method: connectors.Squash, Message: "Resize web01\n", DeleteBranch: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.PullRequests().Get(ctx, pr.ID); got.State != connectors.PROpen {
		t.Fatalf("merged before the check succeeded: %+v", got)
	}
	set(connectors.StatusSuccess)
	deadline := time.Now().Add(30 * time.Second)
	for {
		got, err := c.PullRequests().Get(ctx, pr.ID)
		if err == nil && got.State == connectors.PRMerged {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not merged after the check succeeded: %+v, %v", got, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestIdentitiesAndErrors(t *testing.T) {
	ctx := context.Background()
	g := giteatest.Get(t)
	r := g.Repo(t, "ids")
	c := connector(t, r, nil)
	u := g.User(t, "alice")
	if got, err := c.Identities().ByEmail(ctx, strings.ToUpper(u.Name)+"@EXAMPLE.COM"); err != nil || got.Name != u.Name {
		t.Errorf("ByEmail = %+v, %v", got, err)
	}
	if _, err := c.Identities().ByEmail(ctx, "nobody@example.com"); !errors.Is(err, connectors.ErrNotFound) {
		t.Errorf("an unknown email: %v", err)
	}
	if _, err := c.PullRequests().Get(ctx, "999"); !errors.Is(err, connectors.ErrNotFound) {
		t.Errorf("a missing pull request: %v", err)
	}
	if _, err := c.PullRequests().Get(ctx, "x"); !errors.Is(err, connectors.ErrNotFound) {
		t.Errorf("an invalid ID: %v", err)
	}
	if info := c.Info(); info.Edition != "Gitea "+giteatest.Version || !info.Has(connectors.CapPullRequests) || info.Has(connectors.CapEnforcedStatus) {
		t.Errorf("info = %+v", info)
	}
	if rules, err := c.Owners().Rules(ctx, "main"); err != nil || rules != nil {
		t.Errorf("no CODEOWNERS: %v, %v", rules, err)
	}
	if p, err := c.Policies().Protection(ctx, "main"); err != nil || p.Protected {
		t.Errorf("an unprotected branch: %+v, %v", p, err)
	}
	if e, err := c.Policies().Verify(ctx, "main", "quai/approval"); err != nil || e.Enforced || !strings.Contains(e.Problems[0], "not protected") {
		t.Errorf("verify an unprotected branch: %+v, %v", e, err)
	}
	if err := r.Protect("main"); err != nil {
		t.Fatal(err)
	}
	if e, _ := c.Policies().Verify(ctx, "main", "quai/approval"); e.Enforced || !strings.Contains(e.Problems[0], "does not require") {
		t.Errorf("verify without the check: %+v", e)
	}
}

func TestURLs(t *testing.T) {
	for raw, want := range map[string][3]string{
		"https://git.example.com/quai/infra-live.git":    {"https://git.example.com", "quai", "infra-live"},
		"https://git.example.com/gitea/quai/infra-live":  {"https://git.example.com/gitea", "quai", "infra-live"},
		"http://quai:tok@gitea:3000/quai/infra-live.git": {"http://gitea:3000", "quai", "infra-live"},
	} {
		c, err := New(connectors.Config{URL: raw})
		if err != nil {
			t.Fatal(err)
		}
		g := c.(*Connector)
		if got := [3]string{g.base, g.owner, g.repo}; got != want {
			t.Errorf("%s: %v", raw, got)
		}
	}
	for _, raw := range []string{"git@git.example.com:quai/infra.git", "https://git.example.com/infra.git", "::"} {
		if _, err := New(connectors.Config{URL: raw}); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
	if _, err := New(connectors.Config{URL: "https://quai:s3cret@git.example.com/infra.git"}); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the error shows the URL's credentials: %v", err)
	}
}

func TestCodeOwners(t *testing.T) {
	rules := parseCodeOwners("# owners\n\nenvs/prod/.* @platform @org/team-a # prod\ndocs/.*\nREADME.md @alice\n")
	if len(rules) != 2 || rules[0].Pattern != "envs/prod/.*" || strings.Join(rules[0].Owners, ",") != "platform,org/team-a" || rules[1].Owners[0] != "alice" {
		t.Errorf("rules = %+v", rules)
	}
}

func TestWebhookPayloads(t *testing.T) {
	c, err := New(connectors.Config{URL: "https://git.example.com/quai/infra.git"})
	if err != nil {
		t.Fatal(err)
	}
	secret := secrets.FromValue("hook-secret")
	deliver := func(event, body string) (connectors.Delivery, error) {
		mac := hmac.New(sha256.New, []byte("hook-secret"))
		mac.Write([]byte(body))
		h := http.Header{"X-Gitea-Event": {event}, "X-Gitea-Delivery": {"d-1"}, "X-Gitea-Signature": {hex.EncodeToString(mac.Sum(nil))}}
		return c.Webhooks().Parse(h, []byte(body), secret)
	}
	d, err := deliver("push", `{"repository":{"full_name":"Quai/Infra"},"ref":"refs/heads/main","before":"a","after":"b",
		"commits":[{"added":["x"],"modified":["y"]},{"removed":["x"]}]}`)
	if err != nil || d.ID != "d-1" || len(d.Events) != 1 {
		t.Fatalf("push = %+v, %v", d, err)
	}
	if p := d.Events[0].(connectors.Push); p.After != "b" || strings.Join(p.Paths, ",") != "x,y" {
		t.Errorf("push = %+v", p)
	}
	for action, want := range map[string]connectors.PullRequestAction{"opened": connectors.PROpened, "synchronized": connectors.PRUpdated, "closed": connectors.PRClosed} {
		d, err := deliver("pull_request", `{"repository":{"full_name":"quai/infra"},"action":"`+action+`","number":7,"pull_request":{"head":{"ref":"quai/r-7","sha":"c"},"base":{"ref":"main"}}}`)
		if err != nil || len(d.Events) != 1 || d.Events[0].(connectors.PullRequestChanged).Action != want || d.Events[0].(connectors.PullRequestChanged).ID != "7" {
			t.Errorf("%s = %+v, %v", action, d, err)
		}
	}
	d, _ = deliver("pull_request", `{"repository":{"full_name":"quai/infra"},"action":"closed","number":7,"pull_request":{"merged":true}}`)
	if d.Events[0].(connectors.PullRequestChanged).Action != connectors.PRCompleted {
		t.Errorf("merged = %+v", d)
	}
	d, _ = deliver("status", `{"repository":{"full_name":"quai/infra"},"sha":"c","context":"quai/approval","state":"warning"}`)
	if s := d.Events[0].(connectors.StatusChanged); s.Commit != "c" || s.Status.State != connectors.StatusFailure {
		t.Errorf("status = %+v", s)
	}
	for name, body := range map[string]string{
		"another repository": `{"repository":{"full_name":"quai/other"},"ref":"refs/heads/main"}`,
		"a label":            `{"repository":{"full_name":"quai/infra"},"action":"label_updated","number":7,"pull_request":{}}`,
	} {
		if d, err := deliver(map[string]string{"another repository": "push", "a label": "pull_request"}[name], body); err != nil || len(d.Events) != 0 {
			t.Errorf("%s: %+v, %v", name, d, err)
		}
	}
	if _, err := c.Webhooks().Parse(http.Header{"X-Gitea-Event": {"push"}}, []byte(`{}`), secret); !errors.Is(err, connectors.ErrUnauthenticated) {
		t.Errorf("an unsigned delivery: %v", err)
	}
	if _, err := c.Webhooks().Parse(http.Header{"X-Gitea-Signature": {""}}, []byte(`{}`), secrets.FromValue("")); !errors.Is(err, connectors.ErrUnauthenticated) {
		t.Errorf("an empty secret: %v", err)
	}
}
