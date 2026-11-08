package github

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/connectors"
)

// TestSetStatus: each state is a new check run of the app on the commit; a stale approval is
// action_required, which blocks a merge.
func TestSetStatus(t *testing.T) {
	f := newFakeAPI(t)
	c := f.connector()
	f.answer("POST /repos/o/r/check-runs", http.StatusCreated, `{"id":1}`)
	for state, want := range map[connectors.StatusState][2]string{
		connectors.StatusPending:       {"in_progress", ""},
		connectors.StatusSuccess:       {"completed", "success"},
		connectors.StatusFailure:       {"completed", "failure"},
		connectors.StatusError:         {"completed", "failure"},
		connectors.StatusNotApplicable: {"completed", "action_required"},
	} {
		err := c.Statuses().Set(t.Context(), connectors.StatusTarget{PullRequest: "5", Commit: "abc"},
			connectors.Status{Name: "quai/approval", State: state, Description: "Approved in Quai", URL: "https://quai.example.com/r/1"})
		if err != nil {
			t.Fatal(err)
		}
		body := f.body("POST /repos/o/r/check-runs")
		output, _ := body["output"].(map[string]any)
		if body["name"] != "quai/approval" || body["head_sha"] != "abc" || body["status"] != want[0] || (body["conclusion"] == nil) != (want[1] == "") ||
			want[1] != "" && body["conclusion"] != want[1] || body["details_url"] != "https://quai.example.com/r/1" ||
			output["title"] != "Approved in Quai" || output["summary"] != "Approved in Quai" {
			t.Errorf("%s: %v", state, body)
		}
	}
	// No commit: the pull request's head. No description or link: neither is sent.
	f.answer("GET /repos/o/r/pulls/5", http.StatusOK, prJSON("open", ""))
	if err := c.Statuses().Set(t.Context(), connectors.StatusTarget{PullRequest: "5"}, connectors.Status{Name: "quai/access-test", State: connectors.StatusSuccess}); err != nil {
		t.Fatal(err)
	}
	if body := f.body("POST /repos/o/r/check-runs"); body["head_sha"] != "approved" || body["output"] != nil || body["details_url"] != nil {
		t.Errorf("on the head: %v", body)
	}
	if err := c.Statuses().Set(t.Context(), connectors.StatusTarget{Commit: "abc"}, connectors.Status{Name: "x", State: "warning"}); !errors.Is(err, connectors.ErrUnsupported) {
		t.Errorf("an unknown state: %v", err)
	}
	f.answer("POST /repos/o/r/check-runs", http.StatusForbidden, `{"message":"Resource not accessible by integration"}`)
	if err := c.Statuses().Set(t.Context(), connectors.StatusTarget{Commit: "abc"}, connectors.Status{Name: "x", State: connectors.StatusSuccess}); err == nil ||
		!strings.Contains(err.Error(), "not accessible") {
		t.Errorf("refused: %v", err)
	}
}

// TestListStatuses: the latest check runs, on every page, and the commit statuses, one per name,
// the least favourable standing.
func TestListStatuses(t *testing.T) {
	f := newFakeAPI(t)
	c := f.connector()
	f.handle("GET /repos/o/r/commits/abc/check-runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filter") != "latest" {
			t.Errorf("filter = %q", r.URL.Query().Get("filter"))
		}
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/commits/abc/check-runs?filter=latest&page=2>; rel="next"`, f.srv.URL))
			_, _ = fmt.Fprint(w, `{"total_count":4,"check_runs":[
				{"name":"quai/approval","status":"completed","conclusion":"action_required","details_url":"https://quai/r/1","output":{"title":"Stale"}},
				{"name":"plan","status":"in_progress"},
				{"name":"lint","status":"completed","conclusion":"skipped"}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"total_count":4,"check_runs":[{"name":"plan","status":"completed","conclusion":"timed_out","output":{"summary":"Too long"}}]}`)
	})
	f.answer("GET /repos/o/r/commits/abc/status", http.StatusOK, `{"state":"failure","statuses":[
		{"context":"lint","state":"success"},{"context":"ci/legacy","state":"failure","description":"Broken","target_url":"https://ci"}]}`)
	got, err := c.Statuses().List(t.Context(), connectors.StatusTarget{PullRequest: "5", Commit: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	want := []connectors.Status{
		{Name: "quai/approval", State: connectors.StatusNotApplicable, Description: "Stale", URL: "https://quai/r/1"},
		{Name: "plan", State: connectors.StatusError, Description: "Too long"},
		{Name: "lint", State: connectors.StatusSuccess},
		{Name: "ci/legacy", State: connectors.StatusFailure, Description: "Broken", URL: "https://ci"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("List =\n%+v\nwant\n%+v", got, want)
	}
}

func TestRunState(t *testing.T) {
	for _, c := range []struct {
		status, conclusion string
		want               connectors.StatusState
	}{
		{"queued", "", connectors.StatusPending},
		{"in_progress", "", connectors.StatusPending},
		{"waiting", "", connectors.StatusPending},
		{"completed", "success", connectors.StatusSuccess},
		{"completed", "neutral", connectors.StatusSuccess},
		{"completed", "skipped", connectors.StatusSuccess},
		{"completed", "failure", connectors.StatusFailure},
		{"completed", "action_required", connectors.StatusNotApplicable},
		{"completed", "cancelled", connectors.StatusError},
		{"completed", "stale", connectors.StatusError},
		{"completed", "something new", connectors.StatusError},
	} {
		if got := runState(c.status, c.conclusion); got != c.want {
			t.Errorf("%s/%s = %s, want %s", c.status, c.conclusion, got, c.want)
		}
	}
}

const notProtected = `{"message":"Branch not protected"}`

// policyAPI answers the branch protection and the rules of main, and the ruleset 7.
func policyAPI(t *testing.T, classic, rules, ruleset answer) *fakeAPI {
	f := newFakeAPI(t)
	f.answer("GET /repos/o/r/branches/main/protection", classic.status, classic.body)
	f.answer("GET /repos/o/r/rules/branches/main", rules.status, rules.body)
	f.handle("GET /repos/o/r/rulesets/7", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("includes_parents") != "true" {
			t.Errorf("an organization's ruleset is not asked for: %s", r.URL)
		}
		w.WriteHeader(ruleset.status)
		_, _ = fmt.Fprint(w, ruleset.body)
	})
	return f
}

// checksRule is a rule requiring checks, of ruleset 7.
func checksRule(checks string) string {
	return `{"type":"required_status_checks","ruleset_source_type":"Organization","ruleset_source":"o","ruleset_id":7,
		"parameters":{"required_status_checks":` + checks + `,"strict_required_status_checks_policy":false}}`
}

func ruleset(enforcement, bypass string) answer {
	return answer{http.StatusOK, `{"id":7,"name":"quai","enforcement":"` + enforcement + `"` + bypass + `}`}
}

// TestProtection: the branch protection and the rulesets together.
func TestProtection(t *testing.T) {
	classic := answer{http.StatusOK, `{"required_status_checks":{"strict":false,"contexts":["quai/approval","ci"],
		"checks":[{"context":"quai/approval","app_id":4242},{"context":"ci","app_id":15368}]},
		"required_pull_request_reviews":{"required_approving_review_count":1,"require_code_owner_reviews":false},"enforce_admins":{"enabled":true}}`}
	rules := answer{http.StatusOK, `[` + checksRule(`[{"context":"quai/approval","integration_id":4242},{"context":"plan","integration_id":15368}]`) + `,
		{"type":"pull_request","ruleset_id":8,"parameters":{"required_approving_review_count":2,"require_code_owner_review":true}},
		{"type":"some_rule_from_the_future","ruleset_id":9}]`}
	f := policyAPI(t, classic, rules, ruleset("active", ""))
	p, err := f.connector().Policies().Protection(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Protected || !slices.Equal(p.RequiredChecks, []string{"quai/approval", "ci", "plan"}) || !p.ChecksRestricted || !p.ResetOnPush ||
		p.RequiredApprovals != 2 || !p.CodeOwnerReview {
		t.Errorf("protection = %+v", p)
	}

	// A check any app can set, as -1 or a legacy context says.
	f = policyAPI(t, answer{http.StatusOK, `{"required_status_checks":{"contexts":["quai/approval"],"checks":[{"context":"quai/approval","app_id":-1}]}}`},
		answer{http.StatusOK, `[]`}, ruleset("active", ""))
	if p, err := f.connector().Policies().Protection(t.Context(), "main"); err != nil || !p.Protected || p.ChecksRestricted || p.CodeOwnerReview {
		t.Errorf("any app: %+v, %v", p, err)
	}
	f = policyAPI(t, answer{http.StatusOK, `{"required_status_checks":{"contexts":["quai/approval"]}}`}, answer{http.StatusOK, `[]`}, ruleset("active", ""))
	if p, err := f.connector().Policies().Protection(t.Context(), "main"); err != nil || !slices.Equal(p.RequiredChecks, []string{"quai/approval"}) || p.ChecksRestricted {
		t.Errorf("legacy contexts: %+v, %v", p, err)
	}

	// Nothing, or rules that do not restrict changes.
	f = policyAPI(t, answer{http.StatusNotFound, notProtected}, answer{http.StatusOK, `[{"type":"deletion","ruleset_id":8},{"type":"non_fast_forward","ruleset_id":8}]`}, ruleset("active", ""))
	if p, err := f.connector().Policies().Protection(t.Context(), "main"); err != nil || p.Protected || len(p.RequiredChecks) != 0 || p.ResetOnPush {
		t.Errorf("unprotected: %+v, %v", p, err)
	}
	f = policyAPI(t, answer{http.StatusNotFound, notProtected}, answer{http.StatusOK, `[{"type":"pull_request","ruleset_id":8,"parameters":{"required_approving_review_count":0}}]`}, ruleset("active", ""))
	if p, err := f.connector().Policies().Protection(t.Context(), "main"); err != nil || !p.Protected || p.CodeOwnerReview {
		t.Errorf("pull requests required: %+v, %v", p, err)
	}

	// What cannot be read is an error: the access test reports it.
	upgrade := answer{http.StatusForbidden, `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature."}`}
	f = policyAPI(t, upgrade, upgrade, ruleset("active", ""))
	if _, err := f.connector().Policies().Protection(t.Context(), "main"); err == nil || !strings.Contains(err.Error(), "Upgrade to GitHub Pro") {
		t.Errorf("a private repository on the free plan: %v", err)
	}
}

// TestVerify: only a check pinned to Quai's app, in a protection or active ruleset that binds the
// app, is enforcement; what Verify cannot read is never counted, and is a problem, not an error.
func TestVerify(t *testing.T) {
	none := answer{http.StatusNotFound, notProtected}
	noRules := answer{http.StatusOK, `[]`}
	pinned := answer{http.StatusOK, `[` + checksRule(`[{"context":"quai/approval","integration_id":4242}]`) + `]`}
	never := ruleset("active", `,"current_user_can_bypass":"never"`)
	classic := func(app, admins string) answer {
		return answer{http.StatusOK, `{"required_status_checks":{"checks":[{"context":"quai/approval"` + app + `}]},"enforce_admins":{"enabled":` + admins + `}}`}
	}
	upgrade := answer{http.StatusForbidden, `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature."}`}
	for _, c := range []struct {
		name                    string
		classic, rules, ruleset answer
		want                    []string // the problems, in part; none is enforced
	}{
		{"a ruleset", none, pinned, never, nil},
		{"a branch protection", classic(`,"app_id":4242`, "true"), noRules, never, nil},
		{"a ruleset, and a protection that is not enough", classic("", "false"), pinned, never, nil},
		{"nothing", none, noRules, never, []string{"no branch protection or ruleset on main requires quai/approval: add a ruleset requiring it from Quai's GitHub App (ID 4242)"}},
		{"a ruleset of other checks", none, answer{http.StatusOK, `[` + checksRule(`[{"context":"ci"}]`) + `]`}, never, []string{"no branch protection or ruleset on main requires quai/approval"}},
		{"a ruleset accepting any source", none, answer{http.StatusOK, `[` + checksRule(`[{"context":"quai/approval"}]`) + `]`}, never,
			[]string{"the ruleset 7 of o accepts quai/approval from any source: set its source to Quai's GitHub App (ID 4242)"}},
		{"a ruleset pinned to another app", none, answer{http.StatusOK, `[` + checksRule(`[{"context":"quai/approval","integration_id":15368}]`) + `]`}, never,
			[]string{"expects quai/approval from another app (ID 15368), not Quai's (ID 4242)"}},
		{"a ruleset Quai's app can bypass", none, pinned, ruleset("active", `,"current_user_can_bypass":"always"`),
			[]string{`Quai's GitHub App can bypass the ruleset "quai" (7) (always): remove it from the bypass list`}},
		{"a ruleset Quai's app can bypass in pull requests", none, pinned, ruleset("active", `,"current_user_can_bypass":"pull_requests_only"`), []string{"can bypass"}},
		{"a ruleset that does not say who bypasses it", none, pinned, ruleset("active", ""), []string{`whether Quai's GitHub App can bypass the ruleset "quai" (7) could not be read: not verified`}},
		{"a ruleset that only evaluates", none, pinned, ruleset("evaluate", `,"current_user_can_bypass":"never"`), []string{`the ruleset "quai" (7) is evaluate, not active`}},
		{"a ruleset that cannot be read", none, pinned, answer{http.StatusNotFound, `{"message":"Not Found"}`}, []string{"the ruleset 7 of o could not be read (GitHub answered 404: Not Found): not verified"}},
		{"a protection accepting any source", classic("", "true"), noRules, never, []string{"the branch protection of main accepts quai/approval from any source"}},
		{"a protection accepting any app", classic(`,"app_id":-1`, "true"), noRules, never, []string{"accepts quai/approval from any source"}},
		{"legacy contexts", answer{http.StatusOK, `{"required_status_checks":{"contexts":["quai/approval"]},"enforce_admins":{"enabled":true}}`}, noRules, never,
			[]string{"accepts quai/approval from any source"}},
		{"a protection pinned to another app", classic(`,"app_id":15368`, "true"), noRules, never, []string{"expects quai/approval from another app (ID 15368)"}},
		{"a protection administrators bypass", classic(`,"app_id":4242`, "false"), noRules, never,
			[]string{`the branch protection of main does not apply to administrators, and Quai's GitHub App is one: it can merge without quai/approval; enable "Do not allow bypassing the above settings"`}},
		{"a private repository on the free plan", upgrade, upgrade, never, []string{
			"the branch protection of main could not be read (GitHub answered 403: Upgrade to GitHub Pro or make this repository public to enable this feature): not verified",
			"the rulesets of main could not be read (GitHub answered 403: Upgrade to GitHub Pro or make this repository public to enable this feature): not verified"}},
		{"no permission to read the protection", answer{http.StatusForbidden, `{"message":"Resource not accessible by integration"}`}, noRules, never,
			[]string{"the branch protection of main could not be read (GitHub answered 403: Resource not accessible by integration): not verified"}},
		{"a branch that is gone", answer{http.StatusNotFound, `{"message":"Branch not found"}`}, answer{http.StatusNotFound, `{"message":"Not Found"}`}, never,
			[]string{"the branch protection of main could not be read (GitHub answered 404: Branch not found)", "the rulesets of main could not be read"}},
		{"an answer Verify does not understand", answer{http.StatusOK, `{"required_status_checks":"yes"}`}, pinned, never, nil},
		{"rules Verify does not understand", none, answer{http.StatusOK, `{"rules":"maybe"}`}, never, []string{"the rulesets of main could not be read (the answer could not be read: "}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := policyAPI(t, c.classic, c.rules, c.ruleset)
			e, err := f.connector().Policies().Verify(t.Context(), "main", "quai/approval")
			if err != nil {
				t.Fatal(err)
			}
			if e.Enforced != (c.want == nil) || len(e.Problems) != len(c.want) {
				t.Fatalf("verify = %+v, want problems %q", e, c.want)
			}
			for i, want := range c.want {
				if !strings.Contains(e.Problems[i], want) {
					t.Errorf("problem %d = %q, want %q", i, e.Problems[i], want)
				}
			}
		})
	}
}

// TestVerifyErrors: a transient error is returned, so the last state recorded stays.
func TestVerifyErrors(t *testing.T) {
	limited := answer{http.StatusForbidden, `{"message":"API rate limit exceeded for installation ID 77."}`}
	for name, f := range map[string]*fakeAPI{
		"a server error on the protection": policyAPI(t, answer{http.StatusBadGateway, `{"message":"Server Error"}`}, answer{http.StatusOK, `[]`}, ruleset("active", "")),
		"a server error on the rules":      policyAPI(t, answer{http.StatusNotFound, notProtected}, answer{http.StatusInternalServerError, `{}`}, ruleset("active", "")),
		"a server error on the ruleset": policyAPI(t, answer{http.StatusNotFound, notProtected},
			answer{http.StatusOK, `[` + checksRule(`[{"context":"quai/approval","integration_id":4242}]`) + `]`}, answer{http.StatusServiceUnavailable, `{}`}),
	} {
		if _, err := f.connector().Policies().Verify(t.Context(), "main", "quai/approval"); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	f := newFakeAPI(t)
	f.handle("GET /repos/o/r/branches/main/protection", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1999999999")
		w.WriteHeader(limited.status)
		_, _ = fmt.Fprint(w, limited.body)
	})
	if _, err := f.connector().Policies().Verify(t.Context(), "main", "quai/approval"); !errors.Is(err, connectors.ErrRateLimited) {
		t.Errorf("rate limited: %v", err)
	}
}

func TestCodeOwnersPattern(t *testing.T) {
	for pattern, paths := range map[string]map[string]bool{
		"*":                {"a.txt": true, "a/b/c.yaml": true},
		"*.js":             {"a.js": true, "src/a.js": true, "a.jsx": false, "a.js/x": true},
		"/.quai/":          {".quai/types/x.yaml": true, ".quai": false, "a/.quai/x": false, ".quaix/y": false},
		".quai/":           {".quai/types/x.yaml": true, "a/.quai/x": true},
		"/.quai":           {".quai/types/x.yaml": true, ".quai": true, ".quaix": false},
		"/envs/prod/":      {"envs/prod/main.tf": true, "envs/prod/a/b.tf": true, "envs/production/x": false, "a/envs/prod/x": false},
		"envs/prod":        {"envs/prod/main.tf": true, "a/envs/prod/main.tf": false},
		"docs/*":           {"docs/a.md": true, "docs/b/c.md": false, "x/docs/a.md": false},
		"apps/":            {"apps/a": true, "x/apps/a/b": true, "apps": false},
		"**/logs":          {"logs/a": true, "a/b/logs/c": true, "logs": true, "xlogs/a": false},
		"/build/logs/":     {"build/logs/a": true, "x/build/logs/a": false},
		"docs/**":          {"docs/a": true, "docs/a/b": true, "docs": false},
		"a/**/b":           {"a/b": true, "a/x/y/b": true, "a/x/b/c": true, "ab": false},
		"?.txt":            {"a.txt": true, "ab.txt": false, "a/b.txt": true},
		"/README":          {"README": true, "readme": false},
		`/path\ with\ sp/`: {"path with sp/a": true},
		`\#notes`:          {"#notes": true},
		`/a.b`:             {"a.b": true, "axb": false},
		`\[x\]`:            {"[x]": true},
	} {
		re := codeOwnersPattern(pattern)
		if re == nil {
			t.Errorf("%s: refused", pattern)
			continue
		}
		for path, want := range paths {
			if got := re.MatchString(path); got != want {
				t.Errorf("%s matches %s = %v, want %v (%s)", pattern, path, got, want, re)
			}
		}
	}
	for _, pattern := range []string{"/bad[/", "a]", "!/.quai/", "/", ""} {
		if codeOwnersPattern(pattern) != nil {
			t.Errorf("%q was accepted", pattern)
		}
	}
}

func TestParseCodeOwners(t *testing.T) {
	lines := parseCodeOwners("# Owners\n\n*       @org/everyone  # a comment\r\n  /.quai/ @alice\tbob@example.com  @org/platform\n/docs/\n" +
		`\#x @carol` + "\n/a nobody @dave\n")
	type line struct {
		number  int
		pattern string
		owners  []ownerToken
		others  []int
	}
	var got []line
	for _, l := range lines {
		got = append(got, line{l.number, l.pattern, l.owners, l.others})
	}
	want := []line{
		{3, "*", []ownerToken{{"org/everyone", 9}}, nil},
		{4, "/.quai/", []ownerToken{{"alice", 11}, {"bob@example.com", 18}, {"org/platform", 35}}, nil},
		{5, "/docs/", nil, nil},
		{6, `\#x`, []ownerToken{{"carol", 5}}, nil},
		{7, "/a", []ownerToken{{"dave", 11}}, []int{4}},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("parsed\n%v\nwant\n%v", got, want)
	}
}

// codeOwnersAPI serves CODEOWNERS at file, and GitHub's check of it.
func codeOwnersAPI(t *testing.T, file, content string, errors answer) *fakeAPI {
	f := newFakeAPI(t)
	f.handle("GET /repos/o/r/contents/"+file, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != "main" {
			t.Errorf("ref = %q", r.URL.Query().Get("ref"))
		}
		_, _ = fmt.Fprintf(w, `{"type":"file","encoding":"base64","path":%q,"content":%q}`, file, base64.StdEncoding.EncodeToString([]byte(content)))
	})
	f.handle("GET /repos/o/r/codeowners/errors", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != "main" {
			t.Errorf("ref = %q", r.URL.Query().Get("ref"))
		}
		w.WriteHeader(errors.status)
		_, _ = fmt.Fprint(w, errors.body)
	})
	return f
}

// TestOwners: the first CODEOWNERS file GitHub would read, without what GitHub ignores in it; the
// last rule matching a path is the one that applies.
func TestOwners(t *testing.T) {
	src := "* @org/everyone\n/.quai/ @org/platform @nobody\n/.quai/blueprints/\n/envs/ @ghost\n/bad[/ @org/platform\n/docs/ @org/docs\n/docs/api/ @whoever\n"
	errs := answer{http.StatusOK, `{"errors":[
		{"line":2,"column":23,"kind":"Unknown owner","path":"CODEOWNERS"},
		{"line":4,"column":8,"kind":"Unknown owner","path":"CODEOWNERS"},
		{"line":5,"column":5,"kind":"Invalid pattern","path":"CODEOWNERS"},
		{"line":6,"column":9,"kind":"Something new","path":"CODEOWNERS"},
		{"line":7,"column":1,"kind":"Unknown owner","path":"CODEOWNERS"},
		{"line":1,"column":3,"kind":"Unknown owner","path":"docs/CODEOWNERS"}]}`}
	f := codeOwnersAPI(t, "CODEOWNERS", src, errs)
	rules, err := f.connector().Owners().Rules(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if f.count("GET /repos/o/r/contents/.github/CODEOWNERS") != 1 || f.count("GET /repos/o/r/contents/docs/CODEOWNERS") != 0 {
		t.Errorf("calls = %v", f.calls)
	}
	var got []string
	for _, r := range rules {
		got = append(got, r.Pattern+" "+strings.Join(r.Owners, ","))
	}
	if want := []string{"* org/everyone", "/.quai/ org/platform", "/.quai/blueprints/ ", "/envs/ ", "/docs/ ", "/docs/api/ "}; !slices.Equal(got, want) {
		t.Errorf("rules = %q, want %q", got, want)
	}
	for path, want := range map[string]bool{
		"README.md":               true,  // *
		".quai/types/x.yaml":      true,  // /.quai/, the known owner
		".quai/blueprints/x.yaml": false, // the last rule that matches has no owner
		"envs/prod/main.tf":       false, // @ghost is unknown
		"bad/x":                   true,  // the invalid line is skipped: * applies
		"docs/a.md":               false, // an error Rules does not know: no owner
		"docs/api/a.md":           false, // the owner was not found where GitHub says
	} {
		if got := connectors.Covered(rules, path); got != want {
			t.Errorf("%s covered = %v, want %v", path, got, want)
		}
	}

	// GitHub has not checked a new file yet.
	f = codeOwnersAPI(t, ".github/CODEOWNERS", src, answer{http.StatusNotFound, `{"message":"Not Found"}`})
	if _, err := f.connector().Owners().Rules(t.Context(), "main"); err == nil || !strings.Contains(err.Error(), "not checked .github/CODEOWNERS") {
		t.Errorf("not checked yet: %v", err)
	}
	// No file.
	f = newFakeAPI(t)
	if rules, err := f.connector().Owners().Rules(t.Context(), "main"); err != nil || rules != nil ||
		f.count("GET /repos/o/r/contents/docs/CODEOWNERS") != 1 || f.count("GET /repos/o/r/codeowners/errors") != 0 {
		t.Errorf("no file: %v, %v, %v", rules, err, f.calls)
	}
	f.answer("GET /repos/o/r/contents/.github/CODEOWNERS", http.StatusBadGateway, `{}`)
	if _, err := f.connector().Owners().Rules(t.Context(), "main"); err == nil {
		t.Error("a server error is not an error")
	}
}
