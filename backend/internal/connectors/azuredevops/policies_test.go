package azuredevops

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/adotest"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/secrets"
)

var quai = User{ID: "11111111-aaaa-bbbb-cccc-000000000001", Name: "svc-quai"}

func statusCheck(blocking bool, change func(*policySettings)) policy {
	p := policy{IsEnabled: true, IsBlocking: blocking}
	p.Type.ID = statusPolicy
	p.Settings = policySettings{StatusGenre: "quai", StatusName: "approval", AuthorID: strings.ToUpper(quai.ID), InvalidateOnSourceUpdate: true}
	if change != nil {
		change(&p.Settings)
	}
	return p
}

func TestVerify(t *testing.T) {
	one := 1
	reviewers := policy{IsEnabled: true, IsBlocking: true}
	reviewers.Type.ID = minReviewersPolicy
	reviewers.Settings.MinimumApproverCount = 2
	for name, tc := range map[string]struct {
		policies []policy
		want     string // a problem containing it; empty: enforced
	}{
		"enforced":            {[]policy{statusCheck(true, nil), reviewers}, ""},
		"no policy":           {[]policy{reviewers}, "no status check policy"},
		"another status":      {[]policy{statusCheck(true, func(s *policySettings) { s.StatusName = "plan" })}, "no status check policy"},
		"optional":            {[]policy{statusCheck(false, nil)}, "optional"},
		"anyone can set it":   {[]policy{statusCheck(true, func(s *policySettings) { s.AuthorID = "" })}, "any identity"},
		"someone else":        {[]policy{statusCheck(true, func(s *policySettings) { s.AuthorID = "2222" })}, "not Quai's"},
		"not reset":           {[]policy{statusCheck(true, func(s *policySettings) { s.InvalidateOnSourceUpdate = false })}, "not reset"},
		"only when posted":    {[]policy{statusCheck(true, func(s *policySettings) { s.PolicyApplicability = &one })}, "only when the status is posted"},
		"only for some paths": {[]policy{statusCheck(true, func(s *policySettings) { s.FilenamePatterns = []string{"/envs/*"} })}, "only to some paths"},
	} {
		e := verify("main", "quai/approval", quai, tc.policies)
		switch {
		case tc.want == "" && (!e.Enforced || len(e.Problems) > 0):
			t.Errorf("%s: %+v", name, e)
		case tc.want != "" && (e.Enforced || !strings.Contains(strings.Join(e.Problems, "\n"), tc.want)):
			t.Errorf("%s: %+v, want a problem about %q", name, e, tc.want)
		}
	}
	p := protection("main", []policy{statusCheck(true, nil), reviewers})
	if !p.Protected || len(p.RequiredChecks) != 1 || p.RequiredChecks[0] != "quai/approval" || !p.ChecksRestricted || !p.ResetOnPush || p.RequiredApprovals != 2 {
		t.Errorf("protection = %+v", p)
	}
	if p := protection("main", nil); p.Protected || p.ChecksRestricted {
		t.Errorf("no policies = %+v", p)
	}
}

func TestParse(t *testing.T) {
	c, err := New(connectors.Config{URL: "https://dev.azure.com/acme/Infrastructure/_git/live"})
	if err != nil {
		t.Fatal(err)
	}
	ado := c.(*Connector)
	ado.resolved = &Repository{ID: "8d305a40-d78b-427e-a3d0-b375906a3fdf", ProjectID: "p-1", WebURL: "https://dev.azure.com/acme/Infrastructure/_git/live"}
	secret := secrets.FromValue("hook-secret")
	deliver := func(body string) (connectors.Delivery, error) {
		return c.Webhooks().Parse(http.Header{SecretHeader: {"hook-secret"}}, []byte(body), secret)
	}
	const repo = `"repository":{"id":"8D305A40-D78B-427E-A3D0-B375906A3FDF","name":"live"}`
	d, err := deliver(`{"id":"e-1","eventType":"git.push","resource":{` + repo + `,"refUpdates":[
		{"name":"refs/heads/main","oldObjectId":"a","newObjectId":"b"},{"name":"refs/heads/quai/r-1","oldObjectId":"c","newObjectId":"0000000000000000000000000000000000000000"}]}}`)
	if err != nil || d.ID != "e-1" || len(d.Events) != 2 || d.Events[0].(connectors.Push).After != "b" || d.Events[1].(connectors.Push).Ref != "refs/heads/quai/r-1" {
		t.Fatalf("push = %+v, %v", d, err)
	}
	for body, want := range map[string]connectors.PullRequestAction{
		`{"id":"e-2","eventType":"git.pullrequest.created","resource":{` + repo + `,"pullRequestId":7,"status":"active","sourceRefName":"refs/heads/quai/r-7","targetRefName":"refs/heads/main","lastMergeSourceCommit":{"commitId":"c"}}}`: connectors.PROpened,
		`{"id":"e-3","eventType":"git.pullrequest.updated","resource":{` + repo + `,"pullRequestId":7,"status":"active"}}`:                                                                                                                  connectors.PRUpdated,
		`{"id":"e-4","eventType":"git.pullrequest.updated","resource":{` + repo + `,"pullRequestId":7,"status":"abandoned"}}`:                                                                                                               connectors.PRClosed,
		`{"id":"e-5","eventType":"git.pullrequest.merged","resource":{` + repo + `,"pullRequestId":7,"status":"completed"}}`:                                                                                                                connectors.PRCompleted,
		`{"id":"e-6","eventType":"git.pullrequest.merged","resource":{` + repo + `,"pullRequestId":7,"status":"active","mergeStatus":"conflicts"}}`:                                                                                         connectors.PRUpdated,
	} {
		d, err := deliver(body)
		if err != nil || len(d.Events) != 1 {
			t.Errorf("%s: %+v, %v", body[:40], d, err)
			continue
		}
		if e := d.Events[0].(connectors.PullRequestChanged); e.Action != want || e.ID != "7" {
			t.Errorf("%s: %+v, want %s", d.ID, e, want)
		}
	}
	if e := mustOne(t, deliver)(`{"id":"e-2","eventType":"git.pullrequest.created","resource":{` + repo + `,"pullRequestId":7,"status":"active","sourceRefName":"refs/heads/quai/r-7","targetRefName":"refs/heads/main","lastMergeSourceCommit":{"commitId":"c"}}}`); e.Source != "quai/r-7" || e.Target != "main" || e.Head != "c" {
		t.Errorf("created = %+v", e)
	}
	for name, body := range map[string]string{
		"another repository": `{"id":"e-7","eventType":"git.push","resource":{"repository":{"id":"other"},"refUpdates":[{"name":"refs/heads/main"}]}}`,
		"another event":      `{"id":"e-8","eventType":"ms.vss-code.git-pullrequest-comment-event","resource":{` + repo + `}}`,
	} {
		if d, err := deliver(body); err != nil || len(d.Events) != 0 {
			t.Errorf("%s: %+v, %v", name, d, err)
		}
	}
	for name, h := range map[string]http.Header{"no secret": {}, "a wrong secret": {SecretHeader: {"hook-secre"}}} {
		if _, err := c.Webhooks().Parse(h, []byte(`{}`), secret); !errors.Is(err, connectors.ErrUnauthenticated) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.Webhooks().Parse(http.Header{SecretHeader: {""}}, []byte(`{}`), secrets.FromValue("")); !errors.Is(err, connectors.ErrUnauthenticated) {
		t.Errorf("an empty secret: %v", err)
	}
}

func mustOne(t *testing.T, deliver func(string) (connectors.Delivery, error)) func(string) connectors.PullRequestChanged {
	return func(body string) connectors.PullRequestChanged {
		t.Helper()
		d, err := deliver(body)
		if err != nil || len(d.Events) != 1 {
			t.Fatalf("%+v, %v", d, err)
		}
		return d.Events[0].(connectors.PullRequestChanged)
	}
}

// TestReplay reads a request as a subscription's history shows it.
func TestReplay(t *testing.T) {
	recorded := "Method: POST\r\nURI: https://quai.example.com/webhooks/live\r\nHTTP Version: 1.1\r\nHeaders:\r\n{\r\n  X-Quai-Webhook-Secret: ********\r\n  Content-Type: application/json; charset=utf-8\r\n}\r\nContent:\r\n{\r\n  \"id\": \"e-1\"\r\n}"
	h, body, ok := adotest.Replay(recorded, SecretHeader, "s3cret")
	if !ok || h.Get(SecretHeader) != "s3cret" || h.Get("Content-Type") != "application/json; charset=utf-8" || string(body) != "{\n  \"id\": \"e-1\"\n}" {
		t.Errorf("replay = %v %q %v", h, body, ok)
	}
	if _, _, ok := adotest.Replay("Method: POST\nno content", SecretHeader, "s"); ok {
		t.Error("a request without content")
	}
}

func TestPathFilter(t *testing.T) {
	for pattern, cases := range map[string]map[string]bool{
		"/.quai/*":   {".quai/types/vm.yaml": true, ".quai/x": true, "envs/.quai/x": false, "README.md": false},
		"*.tf":       {"main.tf": true, "envs/prod/main.tf": true, "main.tfvars": false},
		"*":          {"anything/at/all": true},
		"!/.quai/*":  {".quai/types/vm.yaml": false},
		"/docs/a.md": {"docs/a.md": true, "docs/b.md": false},
	} {
		match := pathFilter(pattern)
		for path, want := range cases {
			if match(path) != want {
				t.Errorf("%s matching %s = %v", pattern, path, !want)
			}
		}
	}
}
