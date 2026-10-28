package azuredevops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/secrets"
)

// sandbox is the Azure DevOps project the live tests use (docs/plan/phase-1-backlog.md#test-infrastructure):
// QUAI_TEST_AZURE_DEVOPS_URL names it, QUAI_TEST_AZURE_DEVOPS_PAT_FILE the file holding the PAT.
// Without them the live tests are skipped. The PAT is read from the file only, and never shown.
type sandbox struct {
	collection, project string
	pat                 func(context.Context) (secrets.Secret, bool, error)
	network             *netx.Network
	http                *http.Client
	client              *Client
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	file, project := os.Getenv("QUAI_TEST_AZURE_DEVOPS_PAT_FILE"), os.Getenv("QUAI_TEST_AZURE_DEVOPS_URL")
	if file == "" || project == "" {
		t.Skip("QUAI_TEST_AZURE_DEVOPS_PAT_FILE and QUAI_TEST_AZURE_DEVOPS_URL are not set: no live Azure DevOps tests")
	}
	b, err := os.ReadFile(file) // #nosec G304 -- the file the developer named
	if err != nil {
		t.Fatalf("the PAT file: %v", err)
	}
	pat := secrets.FromValue(strings.TrimSpace(string(b)))
	network, err := netx.New("")
	if err != nil {
		t.Fatal(err)
	}
	i := strings.LastIndex(strings.TrimSuffix(project, "/"), "/")
	s := &sandbox{collection: project[:i], project: strings.Trim(project[i+1:], "/"), network: network,
		pat:  func(context.Context) (secrets.Secret, bool, error) { return pat, true, nil },
		http: connectors.NewHTTPClient(network.Transport(), nil)}
	if s.project, err = url.PathUnescape(s.project); err != nil {
		t.Fatal(err)
	}
	s.client = NewClient(s.collection, s.http, s.pat)
	return s
}

// repository creates a repository with a first commit on main, deleted (and purged from the
// recycle bin) when the test ends, failed or not. Its name starts with quai-test-.
func (s *sandbox) repository(t *testing.T) (name, id string) {
	t.Helper()
	ctx := context.Background()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name = "quai-test-" + hex.EncodeToString(b)
	var repo struct {
		ID string `json:"id"`
	}
	if err := s.client.Do(ctx, http.MethodPost, path(s.project, "_apis", "git", "repositories"), APIVersion, nil, map[string]any{"name": name}, &repo); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.client.Do(ctx, http.MethodDelete, path(s.project, "_apis", "git", "repositories", repo.ID), APIVersion, nil, nil, nil); err != nil {
			t.Errorf("delete %s: %v", name, err)
			return
		}
		if err := s.client.Do(ctx, http.MethodDelete, path(s.project, "_apis", "git", "recycleBin", "repositories", repo.ID), APIVersion, nil, nil, nil); err != nil {
			t.Errorf("purge %s: %v", name, err)
		}
	})
	push := map[string]any{
		"refUpdates": []map[string]any{{"name": "refs/heads/main", "oldObjectId": strings.Repeat("0", 40)}},
		"commits": []map[string]any{{"comment": "Initial commit", "changes": []map[string]any{{"changeType": "add",
			"item": map[string]any{"path": "/README.md"}, "newContent": map[string]any{"content": "# Quai test\n", "contentType": "rawtext"}}}}},
	}
	if err := s.client.Do(ctx, http.MethodPost, path(s.project, "_apis", "git", "repositories", repo.ID, "pushes"), APIVersion, nil, push, nil); err != nil {
		t.Fatalf("initialize %s: %v", name, err)
	}
	return name, repo.ID
}

// url returns the repository's Git URL.
func (s *sandbox) url(name string) string {
	return s.collection + path(s.project, "_git", name)
}

// TestLive resolves a repository of the sandbox and pushes to it with the PAT.
func TestLive(t *testing.T) {
	s := newSandbox(t)
	ctx := context.Background()
	name, id := s.repository(t)
	c, err := New(connectors.Config{URL: s.url(name), HTTP: s.http, Credentials: s.pat})
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

	m, err := gitx.OpenMirror(ctx, s.url(name), filepath.Join(t.TempDir(), "m.git"),
		gitx.Options{Committer: gitx.Signature{Name: "Quai", Email: "quai@example.com"}, Auth: c.GitAuth, Proxy: s.network.Proxy})
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
