package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"code.gitea.io/sdk/gitea"

	"github.com/damhau/quai/backend/internal/connectors"
	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/giteatest"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
)

// document is a Repository document for the API.
func document(name string, spec map[string]any) map[string]any {
	return map[string]any{"apiVersion": "quai.dev/v1beta1", "kind": "Repository", "metadata": map[string]any{"name": name, "title": "DNS"}, "spec": spec}
}

// TestManageRepositories: repository admins connect, change and disconnect repositories through
// the API; those declared in configuration are read-only there; others may not.
func TestManageRepositories(t *testing.T) {
	h := newHarness(t)
	alice, bob := h.as("alice"), h.as("bob")
	bare, _ := localRemote(t, t.TempDir())
	dns := document("dns", map[string]any{"connector": "generic", "url": bare, "delivery": map[string]any{"mode": "direct"}})

	if status, body := bob.do(http.MethodPost, Prefix+"/repositories", dns, nil); status != http.StatusForbidden {
		t.Errorf("bob connects = %d %s", status, body)
	}
	var c ConnectionView
	if status, body := alice.do(http.MethodPost, Prefix+"/repositories", dns, &c); status != http.StatusCreated || !c.Connected || c.ManagedBy != "api" ||
		c.Document.Spec.DefaultBranch != "main" || c.Document.Spec.Sync.PollInterval == 0 {
		t.Fatalf("connect = %d %s", status, body)
	}
	var repos []RepositoryView
	alice.do(http.MethodGet, Prefix+"/repositories", nil, &repos)
	if !slices.ContainsFunc(repos, func(r RepositoryView) bool { return r.Name == "dns" && r.Types > 0 }) {
		t.Errorf("the new repository is not listed: %+v", repos)
	}
	if status, body := alice.do(http.MethodPost, Prefix+"/repositories", dns, nil); status != http.StatusConflict {
		t.Errorf("connecting it twice = %d %s", status, body)
	}
	if status, body := alice.do(http.MethodPost, Prefix+"/repositories", document("infra-live", dns["spec"].(map[string]any)), nil); status != http.StatusConflict {
		t.Errorf("taking a configured name = %d %s", status, body)
	}
	bad := document("bad", map[string]any{"connector": "svn", "url": bare})
	if status, body := alice.do(http.MethodPost, Prefix+"/repositories", bad, nil); status != http.StatusUnprocessableEntity || !strings.Contains(body, "spec.connector must be one of") {
		t.Errorf("an invalid document = %d %s", status, body)
	}

	dns["metadata"].(map[string]any)["title"] = "Name servers"
	if status, body := alice.do(http.MethodPut, Prefix+"/repositories/dns", dns, &c); status != http.StatusOK || c.Document.Metadata.Title != "Name servers" {
		t.Errorf("change = %d %s", status, body)
	}
	if status, body := alice.do(http.MethodPut, Prefix+"/repositories/other", dns, nil); status != http.StatusUnprocessableEntity || !strings.Contains(body, "metadata.name cannot change") {
		t.Errorf("renaming = %d %s", status, body)
	}
	var live ConnectionView
	if status, body := alice.do(http.MethodGet, Prefix+"/repositories/infra-live", nil, &live); status != http.StatusOK || live.ManagedBy != "config" || live.Document.Spec.URL != h.bare {
		t.Errorf("read the configured one = %d %s", status, body)
	}
	if status, body := alice.do(http.MethodPut, Prefix+"/repositories/infra-live", live.Document, nil); status != http.StatusConflict || !strings.Contains(body, "declared in Quai's configuration") {
		t.Errorf("changing the configured one = %d %s", status, body)
	}
	if status, body := alice.do(http.MethodDelete, Prefix+"/repositories/infra-live", nil, nil); status != http.StatusConflict {
		t.Errorf("deleting the configured one = %d %s", status, body)
	}

	// A remote that cannot be reached is stored, with the reason, and retried.
	gone := document("gone", map[string]any{"connector": "generic", "url": filepath.Join(t.TempDir(), "nowhere.git"), "delivery": map[string]any{"mode": "direct"}})
	if status, body := alice.do(http.MethodPost, Prefix+"/repositories", gone, &c); status != http.StatusCreated || c.Connected || c.Error == "" {
		t.Errorf("an unreachable remote = %d %s", status, body)
	}

	for _, name := range []string{"dns", "gone"} {
		if status, body := alice.do(http.MethodDelete, Prefix+"/repositories/"+name, nil, nil); status != http.StatusNoContent {
			t.Errorf("disconnect %s = %d %s", name, status, body)
		}
	}
	if status, _ := alice.do(http.MethodGet, Prefix+"/repositories/dns", nil, nil); status != http.StatusNotFound {
		t.Errorf("a disconnected repository = %d", status)
	}
	repos = nil
	alice.do(http.MethodGet, Prefix+"/repositories", nil, &repos)
	if len(repos) != 1 {
		t.Errorf("after disconnecting: %+v", repos)
	}
}

// TestListConnections: the connections, connected or not, as their admins see them.
func TestListConnections(t *testing.T) {
	h := newHarness(t)
	alice := h.as("alice")
	gone := document("gone", map[string]any{"connector": "generic", "url": filepath.Join(t.TempDir(), "nowhere.git"), "delivery": map[string]any{"mode": "direct"}})
	if status, body := alice.do(http.MethodPost, Prefix+"/repositories", gone, nil); status != http.StatusCreated {
		t.Fatalf("connect = %d %s", status, body)
	}
	var list []ConnectionView
	if status, body := alice.do(http.MethodGet, Prefix+"/repository-connections", nil, &list); status != http.StatusOK || len(list) != 2 ||
		list[0].Name != "gone" || list[0].Connected || list[0].Error == "" || list[1].Name != "infra-live" || !list[1].Connected || list[1].ManagedBy != "config" {
		t.Errorf("connections = %d %s", status, body)
	}
	if status, _ := h.as("bob").do(http.MethodGet, Prefix+"/repository-connections", nil, nil); status != http.StatusForbidden {
		t.Errorf("bob lists connections = %d", status)
	}
}

// TestHealthAndRunToken: a repository's health says what Quai knows of it; a run token is new
// each time, with its SHA-256 for the secret.
func TestHealthAndRunToken(t *testing.T) {
	h := newHarness(t)
	alice := h.as("alice")
	if err := h.ingest.Poll(t.Context(), "infra-live"); err != nil {
		t.Fatal(err)
	}
	var v HealthView
	if status, body := alice.do(http.MethodGet, Prefix+"/repositories/infra-live/health", nil, &v); status != http.StatusOK {
		t.Fatalf("health = %d %s", status, body)
	}
	if !v.Connected || v.Definitions.Types == 0 || v.Definitions.Commit == "" || v.Sync.LastFetchAt == nil || v.Sync.LastPollAt == nil || v.Sync.Head != v.Definitions.Commit ||
		v.Enforcement.Required || v.Credentials.Readable {
		t.Errorf("health = %+v", v)
	}
	if status, _ := h.as("bob").do(http.MethodGet, Prefix+"/repositories/infra-live/health", nil, nil); status != http.StatusForbidden {
		t.Errorf("bob reads the health = %d", status)
	}
	var first, second RunTokenView
	alice.do(http.MethodPost, Prefix+"/repositories/infra-live/run-token", nil, &first)
	alice.do(http.MethodPost, Prefix+"/repositories/infra-live/run-token", nil, &second)
	sum := sha256.Sum256([]byte(first.Token))
	if first.Token == "" || first.Token == second.Token || first.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("run tokens = %+v, %+v", first, second)
	}
}

// accessHarness serves the Gitea repository r as infra-live, with the tokens in vars as env:
// secrets.
func accessHarness(t *testing.T, r *giteatest.Repo, vars map[string]string) *harness {
	t.Helper()
	g := giteatest.Get(t)
	pushExample(t, r.CloneURL, func(context.Context) (gitx.Credentials, error) { return gitx.Credentials{Header: g.GitHeader()}, nil }, nil)
	if err := r.Protect("main", "quai/approval"); err != nil {
		t.Fatal(err)
	}
	return start(t, t.TempDir(), `connector: gitea
  url: `+r.CloneURL+`
  credentials: { ref: env:GITEA_TOKEN }
  sync: { minInterval: 1ns }`, repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { v, ok := vars[n]; return v, ok }}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"gitea": giteaconn.New}},
	})
}

// TestRepositoryAccess: on Gitea, the access test passes every check with an admin's token and
// leaves nothing behind; with a reader's, the push fails with the permission it needs. The
// webhook is installed through the platform.
func TestRepositoryAccess(t *testing.T) {
	g := giteatest.Get(t)
	r := g.Repo(t, "infra")
	reader := g.User(t, "reader")
	vars := map[string]string{"GITEA_TOKEN": g.Token, "READER_TOKEN": reader.Token, "HOOK_SECRET": "a webhook secret"}
	h := accessHarness(t, r, vars)
	alice := h.as("alice")
	spec := map[string]any{"connector": "gitea", "url": r.CloneURL, "credentials": map[string]any{"ref": "env:GITEA_TOKEN"}}

	var v AccessView
	if status, body := alice.do(http.MethodPost, Prefix+"/repository-access-tests", document("infra-two", spec), &v); status != http.StatusOK {
		t.Fatalf("access test = %d %s", status, body)
	}
	var names []string
	for _, c := range v.Checks {
		if !c.OK && (c.Name != "pipelines" || c.Skipped != "the connection names no pipelines") {
			t.Errorf("check %s: %+v", c.Name, c)
		}
		names = append(names, c.Name)
	}
	if !v.OK || strings.Join(names, ",") != "credentials,connector,read,push,pull-request,status,delete-branch,branch-protection,pipelines" {
		t.Errorf("checks = %v, ok %v", names, v.OK)
	}
	// Pipelines named for a platform whose connector reads none.
	withPipelines := map[string]any{"connector": "gitea", "url": r.CloneURL, "credentials": map[string]any{"ref": "env:GITEA_TOKEN"},
		"runner": map[string]any{"pipelines": []map[string]any{{"name": "plan", "kind": "plan", "stacks": "envs/{env}"}}}}
	var p AccessView
	alice.do(http.MethodPost, Prefix+"/repository-access-tests", document("infra-two", withPipelines), &p)
	if c := p.Checks[len(p.Checks)-1]; p.OK || c.Name != "pipelines" || !strings.Contains(c.Error, "the gitea connector reads none") {
		t.Errorf("pipelines on Gitea = %+v", p.Checks)
	}
	if v.Scan == nil || !v.Scan.Found || v.Scan.Types == 0 {
		t.Errorf("scan = %+v", v.Scan)
	}
	branches, _, _ := g.Client.ListRepoBranches(r.Owner, r.Name, gitea.ListRepoBranchesOptions{})
	open, _, _ := g.Client.ListRepoPullRequests(r.Owner, r.Name, gitea.ListPullRequestsOptions{State: gitea.StateOpen})
	if len(branches) != 1 || len(open) != 0 {
		t.Errorf("left behind: %d branches, %d open pull requests", len(branches), len(open))
	}

	// A reader can read, not push: the checks after the push are skipped.
	spec["credentials"] = map[string]any{"ref": "env:READER_TOKEN"}
	v = AccessView{}
	alice.do(http.MethodPost, Prefix+"/repository-access-tests", document("infra-two", spec), &v)
	byName := map[string]CheckView{}
	for _, c := range v.Checks {
		byName[c.Name] = c
	}
	if v.OK || !byName["read"].OK || byName["push"].OK || !strings.Contains(byName["push"].Permission, "write access to the repository") ||
		byName["pull-request"].Skipped == "" || v.Scan == nil {
		t.Errorf("a reader's access test = %+v", v)
	}
	// A reference that cannot be resolved stops at the credentials.
	spec["credentials"] = map[string]any{"ref": "env:MISSING"}
	v = AccessView{}
	alice.do(http.MethodPost, Prefix+"/repository-access-tests", document("infra-two", spec), &v)
	if v.OK || len(v.Checks) == 0 || v.Checks[0].Name != "credentials" || v.Checks[0].OK || v.Checks[1].Skipped == "" {
		t.Errorf("a missing secret = %+v", v)
	}
	if status, _ := h.as("bob").do(http.MethodPost, Prefix+"/repository-access-tests", document("infra-two", spec), nil); status != http.StatusForbidden {
		t.Errorf("bob tests access = %d", status)
	}

	// Enforcement, verified before saving: the protection requires quai/approval.
	var e EnforcementView
	if status, body := alice.do(http.MethodPost, Prefix+"/repository-enforcement-checks", document("infra-two", map[string]any{"connector": "gitea", "url": r.CloneURL,
		"credentials": map[string]any{"ref": "env:GITEA_TOKEN"}}), &e); status != http.StatusOK || !e.Required || len(e.Problems) != 0 {
		t.Errorf("enforcement = %d %s", status, body)
	}
	if err := r.Unprotect("main"); err != nil {
		t.Fatal(err)
	}
	e = EnforcementView{}
	alice.do(http.MethodPost, Prefix+"/repository-enforcement-checks", document("infra-two", map[string]any{"connector": "gitea", "url": r.CloneURL,
		"credentials": map[string]any{"ref": "env:GITEA_TOKEN"}}), &e)
	if len(e.Problems) != 1 || !strings.Contains(e.Problems[0], "not protected") {
		t.Errorf("enforcement, unprotected = %+v", e)
	}

	// The webhook, once there is a secret.
	if status, body := alice.do(http.MethodPost, Prefix+"/repositories/infra-live/webhook", nil, nil); status != http.StatusConflict || !strings.Contains(body, "webhook secret") {
		t.Errorf("a webhook without a secret = %d %s", status, body)
	}
	spec["credentials"] = map[string]any{"ref": "env:GITEA_TOKEN"}
	spec["webhooks"] = map[string]any{"secret": map[string]any{"ref": "env:HOOK_SECRET"}}
	if status, body := alice.do(http.MethodPost, Prefix+"/repositories", document("infra-two", spec), nil); status != http.StatusCreated {
		t.Fatalf("connect = %d %s", status, body)
	}
	var hook WebhookView
	if status, body := alice.do(http.MethodPost, Prefix+"/repositories/infra-two/webhook", nil, &hook); status != http.StatusOK || !hook.Installed ||
		hook.URL != "https://quai.example.com/webhooks/infra-two" {
		t.Errorf("install the webhook = %d %s", status, body)
	}
	hooks, _, err := g.Client.ListRepoHooks(r.Owner, r.Name, gitea.ListHooksOptions{})
	if err != nil || !slices.ContainsFunc(hooks, func(h *gitea.Hook) bool { return h.Config["url"] == hook.URL }) {
		t.Errorf("hooks = %+v, %v", hooks, err)
	}
}
