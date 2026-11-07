package githubtest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-github/v88/github"
)

// TestSandbox checks that the sandbox holds what docs/plan/phase-3-backlog.md says it does: the
// app's installation, its permissions and events, its webhook and the team. Skipped without the
// app's key.
func TestSandbox(t *testing.T) {
	s := New(t)
	ctx := context.Background()
	in, _, err := s.App.Apps.GetInstallation(ctx, s.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	if in.GetAccount().GetLogin() != s.Org || in.GetRepositorySelection() != "all" {
		t.Errorf("installed on %s, %s repositories", in.GetAccount().GetLogin(), in.GetRepositorySelection())
	}
	b, err := json.Marshal(in.GetPermissions())
	if err != nil {
		t.Fatal(err)
	}
	var permissions map[string]string
	if err := json.Unmarshal(b, &permissions); err != nil {
		t.Fatal(err)
	}
	for name, level := range map[string]string{
		"administration": "write", "checks": "write", "contents": "write", "merge_queues": "write",
		"pull_requests": "write", "statuses": "write", "members": "read", "metadata": "read",
	} {
		if permissions[name] != level {
			t.Errorf("permission %s = %q, want %q", name, permissions[name], level)
		}
	}
	for _, event := range []string{"branch_protection_rule", "check_run", "check_suite", "merge_group", "pull_request", "pull_request_review", "push", "repository_ruleset", "status"} {
		if !slices.Contains(in.Events, event) {
			t.Errorf("the app is not subscribed to %s: %v", event, in.Events)
		}
	}
	hook, _, err := s.App.Apps.GetHookConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hook.GetContentType() != "json" || hook.GetInsecureSSL() != "0" {
		t.Errorf("the webhook sends %s, insecure_ssl %s", hook.GetContentType(), hook.GetInsecureSSL())
	}
	if _, _, err := s.Client.Teams.GetTeamBySlug(ctx, s.Org, Team); err != nil {
		t.Errorf("the team: %v", err)
	}
}

// TestRepository creates a repository and sees it gone once the test that made it ends.
func TestRepository(t *testing.T) {
	s := New(t)
	ctx := context.Background()
	var name string
	t.Run("created", func(t *testing.T) {
		var id int64
		name, id = s.Repository(t)
		repo, _, err := s.Client.Repositories.Get(ctx, s.Org, name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(name, Prefix) || repo.GetID() != id || repo.GetPrivate() || repo.GetDefaultBranch() != Branch {
			t.Errorf("repository %s: %+v", name, repo)
		}
		sha, err := s.AddFiles(ctx, name, Branch, "Two files", map[string]string{"a.txt": "a\n", "b/c.txt": "c\n"})
		if err != nil {
			t.Fatal(err)
		}
		commit, _, err := s.Client.Repositories.GetCommit(ctx, s.Org, name, sha, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(commit.Files) != 2 || len(commit.Parents) != 1 {
			t.Errorf("commit %s: %d files, %d parents", sha, len(commit.Files), len(commit.Parents))
		}
	})
	_, resp, err := s.Client.Repositories.Get(ctx, s.Org, name)
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("%s after the test: %v", name, err)
	}
}

// TestDeliveries reads back the push GitHub tried to deliver, and checks that its body is what
// GitHub signed, with the secret on this machine: a replay carries GitHub's own signature. The
// commit's message and file name are not plain ASCII, where a payload re-encoded for the history
// would differ. Skipped without the webhook's secret.
func TestDeliveries(t *testing.T) {
	s := New(t)
	secret := s.WebhookSecret(t).Reveal()
	ctx := context.Background()
	name, id := s.Repository(t)
	const message = "Résumé <quai> & \"co\" ✓"
	sha, err := s.AddFiles(ctx, name, Branch, message, map[string]string{"docs/é.txt": "quai\n"})
	if err != nil {
		t.Fatal(err)
	}
	type push struct {
		After      string `json:"after"`
		HeadCommit struct {
			Message string `json:"message"`
		} `json:"head_commit"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	read := func(d Delivery) (p push) {
		if err := json.Unmarshal(d.Body, &p); err != nil {
			t.Fatalf("delivery %d: %v", d.ID, err)
		}
		return p
	}
	ds := s.Deliveries(t, id, func(ds []Delivery) bool {
		return slices.ContainsFunc(ds, func(d Delivery) bool { return d.Event == "push" && read(d).After == sha })
	})
	if !HasEvent(ds, "push", "") || HasEvent(ds, "push", "opened") {
		t.Errorf("HasEvent on %d deliveries", len(ds))
	}
	for _, d := range ds {
		if d.Header.Get("X-GitHub-Event") != d.Event {
			t.Errorf("delivery %d: event %s, header %s", d.ID, d.Event, d.Header.Get("X-GitHub-Event"))
		}
		if got := signature(secret, d.Body); !hmac.Equal([]byte(got), []byte(d.Header.Get("X-Hub-Signature-256"))) {
			t.Errorf("delivery %d (%s): the body is not what GitHub signed with this secret", d.ID, d.Event)
		}
		if d.Event != "push" || read(d).After != sha {
			continue
		}
		if p := read(d); p.HeadCommit.Message != message || p.Repository.FullName != s.Org+"/"+name {
			t.Errorf("delivery %d: %+v", d.ID, p)
		}
	}
}

// TestNewDelivery reads a delivery as the history's API returns it: the headers made canonical,
// the payload's bytes kept as they are, or its signature would no longer hold.
func TestNewDelivery(t *testing.T) {
	const payload = `{"ref":"refs/heads/main",  "message":"café <é>"}`
	var d github.HookDelivery
	if err := json.Unmarshal([]byte(`{"id":7,"event":"pull_request","action":"opened","redelivery":false,
		"request":{"headers":{"x-github-event":"pull_request","X-Hub-Signature-256":"sha256=00"},"payload":`+payload+`}}`), &d); err != nil {
		t.Fatal(err)
	}
	got := newDelivery(&d)
	if got.ID != 7 || got.Event != "pull_request" || got.Action != "opened" {
		t.Errorf("delivery %+v", got)
	}
	if got.Header.Get("X-GitHub-Event") != "pull_request" || got.Header.Get("X-Hub-Signature-256") != "sha256=00" {
		t.Errorf("headers %v", got.Header)
	}
	if !bytes.Equal(got.Body, []byte(payload)) {
		t.Errorf("body %s", got.Body)
	}
	if !HasEvent([]Delivery{got}, "pull_request", "opened") || !HasEvent([]Delivery{got}, "pull_request", "") || HasEvent([]Delivery{got}, "pull_request", "closed") || HasEvent([]Delivery{got}, "push", "") {
		t.Error("HasEvent")
	}
}

// signature is X-Hub-Signature-256 of body: the HMAC-SHA256 with the secret.
func signature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
