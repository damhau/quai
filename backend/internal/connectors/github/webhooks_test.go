package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/secrets"
)

const hookSecret = "hook-secret"

// delivery is a delivery as GitHub sends it, signed with secret.
func delivery(event, body, secret string) http.Header {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return http.Header{"X-Github-Event": {event}, "X-Github-Delivery": {"d-1"}, "Content-Type": {"application/json"},
		"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}
}

// repository is the payload's repository: the connector's, o/r, with the ID 1.
const repository = `"repository":{"id":1,"full_name":"O/r"}`

func TestParse(t *testing.T) {
	c, err := New(connectors.Config{URL: "https://github.com/o/r", AppID: appID})
	if err != nil {
		t.Fatal(err)
	}
	hooks := c.Webhooks()
	for name, tc := range map[string]struct {
		event, body string
		want        []connectors.Event
	}{
		"push": {"push", `{"ref":"refs/heads/main","before":"a","after":"b",` + repository + `,"commits":[{"added":["x"],"modified":["y"]},{"removed":["x"],"modified":["z"]}]}`,
			[]connectors.Event{connectors.Push{Ref: "refs/heads/main", Before: "a", After: "b", Paths: []string{"x", "y", "z"}}}},
		"opened": {"pull_request", `{"action":"opened",` + repository + `,"pull_request":{"number":5,"head":{"ref":"quai/x","sha":"h"},"base":{"ref":"main"}}}`,
			[]connectors.Event{connectors.PullRequestChanged{ID: "5", Action: connectors.PROpened, Head: "h", Source: "quai/x", Target: "main"}}},
		"synchronized": {"pull_request", `{"action":"synchronize",` + repository + `,"pull_request":{"number":5,"head":{"ref":"quai/x","sha":"h2"},"base":{"ref":"main"}}}`,
			[]connectors.Event{connectors.PullRequestChanged{ID: "5", Action: connectors.PRUpdated, Head: "h2", Source: "quai/x", Target: "main"}}},
		"enqueued": {"pull_request", `{"action":"enqueued",` + repository + `,"pull_request":{"number":5,"head":{"ref":"quai/x","sha":"h"},"base":{"ref":"main"}}}`,
			[]connectors.Event{connectors.PullRequestChanged{ID: "5", Action: connectors.PRUpdated, Head: "h", Source: "quai/x", Target: "main"}}},
		"merged": {"pull_request", `{"action":"closed",` + repository + `,"pull_request":{"number":5,"merged":true,"head":{"ref":"quai/x","sha":"h"},"base":{"ref":"main"}}}`,
			[]connectors.Event{connectors.PullRequestChanged{ID: "5", Action: connectors.PRCompleted, Head: "h", Source: "quai/x", Target: "main"}}},
		"closed": {"pull_request", `{"action":"closed",` + repository + `,"pull_request":{"number":5,"merged":false,"head":{"ref":"quai/x","sha":"h"},"base":{"ref":"main"}}}`,
			[]connectors.Event{connectors.PullRequestChanged{ID: "5", Action: connectors.PRClosed, Head: "h", Source: "quai/x", Target: "main"}}},
		"labelled":           {"pull_request", `{"action":"labeled",` + repository + `,"pull_request":{"number":5}}`, nil},
		"another repository": {"push", `{"ref":"refs/heads/main","repository":{"id":2,"full_name":"o/other"}}`, nil},
		"no repository":      {"push", `{"ref":"refs/heads/main"}`, nil},
		"a check run": {"check_run", `{"action":"completed",` + repository + `,"check_run":{"name":"ci","status":"completed","conclusion":"failure","head_sha":"h","details_url":"https://ci",
			"output":{"title":"2 failed"},"app":{"id":15368},"pull_requests":[{"number":5,"base":{"repo":{"id":1}}},{"number":9,"base":{"repo":{"id":3}}}]}}`,
			[]connectors.Event{connectors.StatusChanged{PullRequest: "5", Commit: "h", Status: connectors.Status{Name: "ci", State: connectors.StatusFailure, Description: "2 failed", URL: "https://ci"}}}},
		"a check run on a commit": {"check_run", `{"action":"created",` + repository + `,"check_run":{"name":"ci","status":"queued","head_sha":"g","app":{"id":15368},"pull_requests":[]}}`,
			[]connectors.Event{connectors.StatusChanged{Commit: "g", Status: connectors.Status{Name: "ci", State: connectors.StatusPending}}}},
		"Quai's own check run": {"check_run", `{"action":"completed",` + repository + `,"check_run":{"name":"quai/approval","status":"completed","conclusion":"success","head_sha":"h","app":{"id":4242},"pull_requests":[{"number":5,"base":{"repo":{"id":1}}}]}}`, nil},
		"a status": {"status", `{"sha":"h","context":"ci/legacy","state":"success","description":"ok","target_url":"https://ci",` + repository + `}`,
			[]connectors.Event{connectors.StatusChanged{Commit: "h", Status: connectors.Status{Name: "ci/legacy", State: connectors.StatusSuccess, Description: "ok", URL: "https://ci"}}}},
		"a branch protection rule": {"branch_protection_rule", `{"action":"edited","rule":{"name":"main"},` + repository + `}`,
			[]connectors.Event{connectors.PolicyChanged{Branch: "main"}}},
		"a ruleset":                  {"repository_ruleset", `{"action":"deleted","repository_ruleset":{"id":1},` + repository + `}`, []connectors.Event{connectors.PolicyChanged{}}},
		"an organization's ruleset":  {"repository_ruleset", `{"action":"edited","repository_ruleset":{"id":1},"organization":{"login":"o"}}`, []connectors.Event{connectors.PolicyChanged{}}},
		"another repository's rules": {"repository_ruleset", `{"action":"edited","repository":{"id":2,"full_name":"o/other"}}`, nil},
		"a merge group": {"merge_group", `{"action":"checks_requested",` + repository + `,"merge_group":{"head_sha":"g","head_ref":"refs/heads/gh-readonly-queue/release/1.0/pr-12-0123abc","base_ref":"refs/heads/release/1.0"}}`,
			[]connectors.Event{connectors.PullRequestChanged{ID: "12", Action: connectors.PRUpdated, Target: "release/1.0"}}},
		"a branch of another queue": {"merge_group", `{"action":"checks_requested",` + repository + `,"merge_group":{"head_ref":"refs/heads/other/main/pr-12-0123abc"}}`, nil},
		"a ping":                    {"ping", `{"zen":"Keep it logically awesome.",` + repository + `}`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			d, err := hooks.Parse(delivery(tc.event, tc.body, hookSecret), []byte(tc.body), secrets.FromValue(hookSecret))
			if err != nil {
				t.Fatal(err)
			}
			if d.ID != "d-1" || !reflect.DeepEqual(d.Events, tc.want) {
				t.Errorf("parsed %+v, want %+v", d, tc.want)
			}
		})
	}

	// A webhook set up by hand may send a form: the signature covers the form.
	body := `{"ref":"refs/heads/main","after":"b",` + repository + `}`
	form := "payload=" + url.QueryEscape(body)
	header := delivery("push", form, hookSecret)
	header.Set("Content-Type", "application/x-www-form-urlencoded")
	if d, err := hooks.Parse(header, []byte(form), secrets.FromValue(hookSecret)); err != nil || len(d.Events) != 1 {
		t.Errorf("a form: %+v, %v", d, err)
	}
	if _, err := hooks.Parse(delivery("push", "{", hookSecret), []byte("{"), secrets.FromValue(hookSecret)); err == nil || errors.Is(err, connectors.ErrUnauthenticated) {
		t.Errorf("a signed delivery that is not JSON: %v", err)
	}
}

// TestParseSignature: a delivery is taken only with GitHub's SHA-256 signature, made with the
// secret, over the body as it came.
func TestParseSignature(t *testing.T) {
	c, err := New(connectors.Config{URL: "https://github.com/o/r", AppID: appID})
	if err != nil {
		t.Fatal(err)
	}
	hooks := c.Webhooks()
	body := `{"ref":"refs/heads/main",` + repository + `}`
	signed := delivery("push", body, hookSecret)
	sha1 := signed.Clone()
	sha1.Set("X-Hub-Signature-256", strings.Replace(signed.Get("X-Hub-Signature-256"), "sha256=", "sha1=", 1))
	missing := signed.Clone()
	missing.Del("X-Hub-Signature-256")
	bare := signed.Clone()
	bare.Set("X-Hub-Signature-256", strings.TrimPrefix(signed.Get("X-Hub-Signature-256"), "sha256="))
	garbled := signed.Clone()
	garbled.Set("X-Hub-Signature-256", "sha256=zz")
	for name, tc := range map[string]struct {
		header http.Header
		body   string
		secret string
	}{
		"a wrong secret":           {signed, body, hookSecret + "-wrong"},
		"no secret":                {delivery("push", body, ""), body, ""},
		"no signature":             {missing, body, hookSecret},
		"another algorithm's name": {sha1, body, hookSecret},
		"no algorithm":             {bare, body, hookSecret},
		"not hexadecimal":          {garbled, body, hookSecret},
		"a changed body":           {signed, body + " ", hookSecret},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := hooks.Parse(tc.header, []byte(tc.body), secrets.FromValue(tc.secret)); !errors.Is(err, connectors.ErrUnauthenticated) {
				t.Errorf("err = %v, want ErrUnauthenticated", err)
			}
		})
	}
	if _, err := hooks.Parse(signed, []byte(body), secrets.FromValue(hookSecret)); err != nil {
		t.Errorf("the signed delivery: %v", err)
	}
}

func TestSubscribe(t *testing.T) {
	const (
		list   = "GET /repos/o/r/hooks"
		create = "POST /repos/o/r/hooks"
		edit   = "PATCH /repos/o/r/hooks/9"
		target = "https://quai.example.com/webhooks/infra"
	)
	f := newFakeAPI(t)
	c := f.connector()
	f.answer(list, http.StatusOK, `[{"id":8,"config":{"url":"https://elsewhere.example.com"}}]`)
	f.answer(create, http.StatusCreated, `{"id":9}`)
	if err := c.Webhooks().Subscribe(t.Context(), target, secrets.FromValue(hookSecret)); err != nil {
		t.Fatal(err)
	}
	body := f.body(create)
	config, _ := body["config"].(map[string]any)
	if config["url"] != target || config["content_type"] != "json" || config["secret"] != hookSecret || config["insecure_ssl"] != "0" || body["active"] != true {
		t.Errorf("the webhook created: %v", body)
	}
	if events, _ := body["events"].([]any); len(events) != len(hookEvents) {
		t.Errorf("the webhook's events: %v", body["events"])
	}

	// The webhook delivering there already is updated, with the secret as it is now.
	f.answer(list, http.StatusOK, `[{"id":8,"config":{"url":"https://elsewhere.example.com"}},{"id":9,"config":{"url":"`+target+`"}}]`)
	f.answer(edit, http.StatusOK, `{"id":9}`)
	if err := c.Webhooks().Subscribe(t.Context(), target, secrets.FromValue("rotated")); err != nil {
		t.Fatal(err)
	}
	if config, _ := f.body(edit)["config"].(map[string]any); config["secret"] != "rotated" || f.count(create) != 1 {
		t.Errorf("the webhook updated: %v, %d created", config, f.count(create))
	}

	// Without the Webhooks permission, GitHub refuses: the error says which permission.
	f.answer(list, http.StatusForbidden, `{"message":"Resource not accessible by integration"}`)
	if err := c.Webhooks().Subscribe(t.Context(), target, secrets.FromValue(hookSecret)); err == nil || !strings.Contains(err.Error(), "Webhooks (read and write)") {
		t.Errorf("without the permission: %v", err)
	}
	if err := c.Webhooks().Subscribe(t.Context(), target, secrets.FromValue("")); err == nil {
		t.Error("a webhook without a secret")
	}
}
