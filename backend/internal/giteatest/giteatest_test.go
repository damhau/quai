package giteatest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"code.gitea.io/sdk/gitea"

	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/gitx"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// TestPullRequest pushes a branch over HTTP with the admin's token, opens a pull request and
// reads it back; then the helpers protect the branch, add a user and a webhook.
func TestPullRequest(t *testing.T) {
	ctx := context.Background()
	g := Get(t)
	r := g.Repo(t, "infra")
	auth := func(context.Context) (gitx.Credentials, error) { return gitx.Credentials{Header: g.GitHeader()}, nil }
	m, err := gitx.OpenMirror(ctx, r.CloneURL, filepath.Join(t.TempDir(), "m.git"), gitx.Options{Committer: gitx.Signature{Name: "Quai", Email: "quai@example.com"}, Auth: auth})
	if err != nil {
		t.Fatal(err)
	}
	sha, err := m.CommitAndPush(ctx, gitx.Commit{Branch: "quai/r-1-web01", Base: r.DefaultBranch, Message: "Resize web01\n",
		Author:  gitx.Signature{Name: "Bob", Email: "bob@example.com"},
		Changes: []plan.Change{{Op: plan.OpCreate, Path: "envs/prod/vms/web01.yaml", After: []byte("cpu: 8\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	pr, _, err := g.Client.CreatePullRequest(r.Owner, r.Name, gitea.CreatePullRequestOption{Head: "quai/r-1-web01", Base: r.DefaultBranch, Title: "Resize web01"})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := g.Client.GetPullRequest(r.Owner, r.Name, pr.Index)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Resize web01" || got.Head.Sha != sha || got.Head.Ref != "quai/r-1-web01" || got.Base.Ref != "main" || got.State != gitea.StateOpen {
		t.Errorf("read back %+v (head %+v)", got, got.Head)
	}

	r.Protect(t, "main", "quai/approval")
	bp, _, err := g.Client.GetBranchProtection(r.Owner, r.Name, "main")
	if err != nil || !bp.EnableStatusCheck || len(bp.StatusCheckContexts) != 1 || bp.StatusCheckContexts[0] != "quai/approval" {
		t.Errorf("protection = %+v, %v", bp, err)
	}
	r.Unprotect(t, "main")

	u := g.User(t, "intruder")
	r.AddCollaborator(t, u)
	if _, _, err := u.Client.GetRepo(r.Owner, r.Name); err != nil {
		t.Errorf("the collaborator cannot read the repository: %v", err)
	}
	r.Webhook(t, "http://127.0.0.1:1/webhooks/infra", "secret")
	if hooks, _, err := g.Client.ListRepoHooks(r.Owner, r.Name, gitea.ListHooksOptions{}); err != nil || len(hooks) != 1 {
		t.Errorf("hooks = %v, %v", hooks, err)
	}
}
