package azuredevops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/adotest"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/conformance"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/secrets"
)

// sandbox is the Azure DevOps project of the live tests, with a client of its organization.
type sandbox struct {
	*adotest.Sandbox
	client *Client
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	sb := adotest.New(t)
	return &sandbox{Sandbox: sb, client: NewClient(sb.Collection, sb.HTTP, sb.PAT)}
}

// TestLive resolves a repository of the sandbox and pushes to it with the PAT.
func TestLive(t *testing.T) {
	s := newSandbox(t)
	ctx := context.Background()
	name, id := s.Repository(t)
	c, err := New(connectors.Config{URL: s.URL(name), HTTP: s.HTTP, Credentials: s.PAT})
	if err != nil {
		t.Fatal(err)
	}
	ado := c.(*Connector)
	repo, err := ado.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if repo.ID != id || repo.Name != name || repo.DefaultBranch != "refs/heads/main" || repo.ProjectID == "" {
		t.Errorf("resolved %+v", repo)
	}
	instance, _ := ado.Client().Detect(ctx)
	if !instance.Services || instance.User.ID == "" || !strings.HasPrefix(c.Info().Edition, "Services (API 7.") {
		t.Errorf("instance %+v, info %+v", instance, c.Info())
	}

	m, err := gitx.OpenMirror(ctx, s.URL(name), filepath.Join(t.TempDir(), "m.git"),
		gitx.Options{Committer: gitx.Signature{Name: "Quai", Email: "quai@example.com"}, Auth: c.GitAuth, Proxy: s.Network.Proxy})
	if err != nil {
		t.Fatal(err)
	}
	sha, err := m.CommitAndPush(ctx, gitx.Commit{Branch: "quai/test-push", Base: "main", Message: "Resize web01\n",
		Author:  gitx.Signature{Name: "Bob", Email: "bob@example.com"},
		Changes: []plan.Change{{Op: plan.OpCreate, Path: "envs/prod/vms/web01.yaml", After: []byte("cpu: 8\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	var refs struct {
		Value []struct {
			Name     string `json:"name"`
			ObjectID string `json:"objectId"`
		} `json:"value"`
	}
	if err := s.client.Do(ctx, http.MethodGet, path(repo.ProjectID, "_apis", "git", "repositories", repo.ID, "refs"), APIVersion,
		url.Values{"filter": {"heads/quai/"}}, nil, &refs); err != nil {
		t.Fatal(err)
	}
	if len(refs.Value) != 1 || refs.Value[0].Name != "refs/heads/quai/test-push" || refs.Value[0].ObjectID != sha {
		t.Errorf("refs = %+v, pushed %s", refs.Value, sha)
	}
}

// liveHarness gives the conformance suite repositories of the sandbox, with the policies the
// suite asks for and the service hook deliveries Azure DevOps recorded (it cannot reach this
// machine). There is one PAT, so no second identity: step 4 is skipped.
type liveHarness struct{ s *sandbox }

func (h liveHarness) Setup(t *testing.T, client *http.Client) *conformance.Repository {
	ctx := context.Background()
	name, _ := h.s.Repository(t)
	c, err := New(connectors.Config{URL: h.s.URL(name), HTTP: client, Credentials: h.s.PAT})
	if err != nil {
		t.Fatal(err)
	}
	ado := c.(*Connector)
	repo, err := ado.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in, err := ado.client.Detect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Subscribed before the suite pushes anything, to an address that does not resolve: Azure
	// DevOps keeps what it tried to send.
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	secret, hook := hex.EncodeToString(b), "https://quai-conformance.invalid/webhooks/"+name
	if err := c.Webhooks().Subscribe(ctx, hook, secrets.FromValue(secret)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := (*webhooks)(ado).Unsubscribe(context.Background(), hook); err != nil {
			t.Errorf("unsubscribe: %v", err)
		}
	})
	var statusPolicies []int
	create := func(ctx context.Context, typ string, settings map[string]any) (int, error) {
		settings["scope"] = []map[string]any{{"repositoryId": repo.ID, "refName": "refs/heads/main", "matchKind": "exact"}}
		var p struct {
			ID int `json:"id"`
		}
		err := ado.client.Do(ctx, http.MethodPost, path(repo.ProjectID, "_apis", "policy", "configurations"), APIVersion, nil,
			map[string]any{"isEnabled": true, "isBlocking": true, "type": map[string]string{"id": typ}, "settings": settings}, &p)
		return p.ID, err
	}
	return &conformance.Repository{
		Connector:     c,
		URL:           h.s.URL(name),
		DefaultBranch: "main",
		RequireCheck: func(ctx context.Context, branch, check string) error {
			genre, name, _ := strings.Cut(check, "/")
			id, err := create(ctx, statusPolicy, map[string]any{"statusGenre": genre, "statusName": name, "authorId": in.User.ID, "invalidateOnSourceUpdate": true})
			statusPolicies = append(statusPolicies, id)
			return err
		},
		RemoveEnforcement: func(ctx context.Context, branch string) error {
			for _, id := range statusPolicies {
				if err := ado.client.Do(ctx, http.MethodDelete, path(repo.ProjectID, "_apis", "policy", "configurations", strconv.Itoa(id)), APIVersion, nil, nil, nil); err != nil {
					return err
				}
			}
			return nil
		},
		AddOwner: func(ctx context.Context, pattern string) (string, error) {
			_, err := create(ctx, reviewersPolicy, map[string]any{"requiredReviewerIds": []string{in.User.ID}, "minimumApproverCount": 1,
				"creatorVoteCounts": true, "filenamePatterns": []string{strings.TrimSuffix(pattern, "/") + "/*"}})
			return in.User.Email, err
		},
		Recorded: func(t *testing.T) []conformance.Recorded { return h.recorded(t, hook, secret) },
	}
}

// recorded returns what Azure DevOps tried to deliver to hook, once it holds a push and a pull
// request.
func (h liveHarness) recorded(t *testing.T, hook, secret string) []conformance.Recorded {
	t.Helper()
	ds := h.s.Deliveries(t, hook, SecretHeader, secret, func(ds []adotest.Delivery) bool {
		return adotest.HasEvent(ds, "git.push") && adotest.HasEvent(ds, "git.pullrequest.")
	})
	var out []conformance.Recorded
	for _, d := range ds {
		out = append(out, conformance.Recorded{Header: d.Header, Body: d.Body, Secret: secret})
	}
	return out
}

func TestLiveConformance(t *testing.T) { conformance.Run(t, liveHarness{newSandbox(t)}) }

// TestLiveIdentities finds the PAT's own identity by its email.
func TestLiveIdentities(t *testing.T) {
	s := newSandbox(t)
	ctx := context.Background()
	name, _ := s.Repository(t)
	c, err := New(connectors.Config{URL: s.URL(name), HTTP: s.HTTP, Credentials: s.PAT})
	if err != nil {
		t.Fatal(err)
	}
	in, err := c.(*Connector).Client().Detect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Identities().ByEmail(ctx, "nobody-"+name+"@example.com"); !errors.Is(err, connectors.ErrNotFound) {
		t.Errorf("an unknown email: %v", err)
	}
	if in.User.Email == "" {
		t.Fatal("connectionData gave no email for the token's identity")
	}
	if u, err := c.Identities().ByEmail(ctx, strings.ToUpper(in.User.Email)); err != nil || u.ID != in.User.ID {
		t.Errorf("the token's own identity: %+v, %v (want %s)", u, err, in.User.ID)
	}
}

// TestLiveAutoComplete opens a draft with labels and the marker, then sets auto-complete, which
// Azure DevOps acts on as soon as the policies pass (none, here).
func TestLiveAutoComplete(t *testing.T) {
	s := newSandbox(t)
	ctx := context.Background()
	name, _ := s.Repository(t)
	c, err := New(connectors.Config{URL: s.URL(name), HTTP: s.HTTP, Credentials: s.PAT})
	if err != nil {
		t.Fatal(err)
	}
	m, err := gitx.OpenMirror(ctx, s.URL(name), filepath.Join(t.TempDir(), "m.git"),
		gitx.Options{Committer: gitx.Signature{Name: "Quai", Email: "quai@example.com"}, Auth: c.GitAuth, Proxy: s.Network.Proxy})
	if err != nil {
		t.Fatal(err)
	}
	head, err := m.CommitAndPush(ctx, gitx.Commit{Branch: "quai/r-1-web01", Base: "main", Message: "Resize web01\n",
		Author:  gitx.Signature{Name: "Bob", Email: "bob@example.com"},
		Changes: []plan.Change{{Op: plan.OpCreate, Path: "web01.yaml", After: []byte("cpu: 8\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	desc := strings.Repeat("Black Friday. ", 400) + "\n\n" + connectors.Marker("3b6f2a8c-0d4e-4f5a-9b1c-2d3e4f5a6b7c")
	pr, err := c.PullRequests().Open(ctx, connectors.NewPullRequest{Source: "quai/r-1-web01", Target: "main", Title: "Resize web01",
		Description: desc, Labels: []string{"quai", "env:prod"}, Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Draft || strings.Join(pr.Labels, ",") != "quai,env:prod" || !strings.Contains(pr.URL, "/pullrequest/"+pr.ID) {
		t.Errorf("opened %+v", pr)
	}
	if id, ok := connectors.RequestOf(pr.Description); !ok || id != "3b6f2a8c-0d4e-4f5a-9b1c-2d3e4f5a6b7c" || len(pr.Description) > maxDescription {
		t.Errorf("the marker did not survive the 4000-character limit: %v, %d", ok, len(pr.Description))
	}
	if err := c.PullRequests().AutoComplete(ctx, pr.ID, connectors.MergeOptions{Head: head, Method: connectors.Squash, Message: "Resize web01\n", DeleteBranch: true}); err != nil {
		t.Fatal(err)
	}
	// A draft does not complete: publishing it lets auto-complete act.
	if got, _ := c.PullRequests().Get(ctx, pr.ID); !got.AutoComplete || got.State != connectors.PROpen {
		t.Errorf("auto-complete on a draft: %+v", got)
	}
	at, _, err := c.(*Connector).api(ctx, "pullrequests", pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.(*Connector).client.Do(ctx, http.MethodPatch, at, APIVersion, nil, map[string]any{"isDraft": false}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(CompletionTimeout)
	for {
		got, err := c.PullRequests().Get(ctx, pr.ID)
		if err == nil && got.State == connectors.PRMerged && got.MergeCommit != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not completed: %+v, %v", got, err)
		}
		time.Sleep(time.Second)
	}
}
