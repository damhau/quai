package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/connectors"
)

// fakeGraphQL answers GitHub's GraphQL endpoint on a fakeAPI: the pull request's state, and the
// auto-merge and merge queue mutations, refused with the message refuse names.
type fakeGraphQL struct {
	state     string            // the pull request, as JSON; empty: GitHub finds none
	refuse    map[string]string // a mutation → GitHub's refusal
	errorType string            // the type of the query's error, when set
	mutations []string          // the mutations asked, in order
	vars      map[string]map[string]any
}

const graphqlKey = "POST /graphql"

func (g *fakeGraphQL) install(f *fakeAPI) {
	g.vars = map[string]map[string]any{}
	f.handle(graphqlKey, func(w http.ResponseWriter, _ *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(f.bodies[graphqlKey], &req); err != nil { // serve holds the lock
			f.t.Errorf("a GraphQL request: %v", err)
		}
		name := ""
		for _, m := range []string{"enablePullRequestAutoMerge", "disablePullRequestAutoMerge", "enqueuePullRequest", "dequeuePullRequest"} {
			if strings.Contains(req.Query, m+"(") {
				name = m
			}
		}
		if name == "" {
			switch {
			case g.errorType != "":
				_, _ = fmt.Fprintf(w, `{"data":{"repository":null},"errors":[{"type":%q,"message":"no"}]}`, g.errorType)
			case g.state == "":
				_, _ = fmt.Fprint(w, `{"data":{"repository":{"pullRequest":null}}}`)
			default:
				_, _ = fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":%s}}}`, g.state)
			}
			return
		}
		g.mutations = append(g.mutations, name)
		g.vars[name] = req.Variables
		if message, ok := g.refuse[name]; ok {
			_, _ = fmt.Fprintf(w, `{"data":{%q:null},"errors":[{"type":"UNPROCESSABLE","path":[%q],"message":%q}]}`, name, name, message)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":{%q:{"clientMutationId":null}}}`, name)
	})
}

// prGraphQL is a pull request as pullRequestState reads it.
func prGraphQL(extra string) string {
	return `{"id":"PR_5","state":"OPEN","merged":false,"isDraft":false,"headRefOid":"approved","isMergeQueueEnabled":false,"autoMergeRequest":null,"mergeQueueEntry":null` + extra + `}`
}

func TestAutoComplete(t *testing.T) {
	const (
		message = "Resize web01\n\nApproved-by: Alice <alice@example.com>\n"
		runs    = "GET /repos/o/r/commits/group/check-runs"
		status  = "GET /repos/o/r/commits/group/status"
		post    = "POST /repos/o/r/check-runs"
		put     = "PUT /repos/o/r/pulls/5/merge"
	)
	approval := connectors.Status{Name: "quai/approval", State: connectors.StatusSuccess, Description: "Approved in Quai", URL: "https://quai.example.com/requests/1"}
	queued := `,"isMergeQueueEnabled":true`
	for name, tc := range map[string]struct {
		state     string
		refuse    map[string]string
		errorType string
		method    connectors.MergeMethod
		group     string // the merge group's check runs, as GitHub lists them
		merge     string // the merge call's answer, when there is one
		err       error  // nil: success
		plain     bool   // an error that is none of the connectors' own
		mutations []string
		published bool // the approval is published on the merge group's commit
	}{
		"merged":                   {state: strings.Replace(prGraphQL(""), `"merged":false`, `"merged":true`, 1)},
		"closed":                   {state: strings.Replace(prGraphQL(""), "OPEN", "CLOSED", 1), plain: true},
		"no such pull request":     {err: connectors.ErrNotFound},
		"not found":                {errorType: "NOT_FOUND", err: connectors.ErrNotFound},
		"rate limited":             {errorType: "RATE_LIMITED", err: connectors.ErrRateLimited},
		"a draft":                  {state: prGraphQL(`,"isDraft":true`), err: connectors.ErrBlocked},
		"an unknown method":        {state: prGraphQL(""), method: "fast-forward", err: connectors.ErrUnsupported},
		"auto-merge enabled":       {state: prGraphQL(""), mutations: []string{"enablePullRequestAutoMerge"}},
		"auto-merge kept":          {state: prGraphQL(`,"autoMergeRequest":{"mergeMethod":"SQUASH"}`)},
		"auto-merge's method":      {state: prGraphQL(`,"autoMergeRequest":{"mergeMethod":"MERGE"}`), mutations: []string{"disablePullRequestAutoMerge", "enablePullRequestAutoMerge"}},
		"mergeable now":            {state: prGraphQL(""), refuse: map[string]string{"enablePullRequestAutoMerge": "Pull request Pull request is in clean status"}, merge: `{"sha":"m","merged":true}`, mutations: []string{"enablePullRequestAutoMerge"}},
		"auto-merge not allowed":   {state: prGraphQL(""), refuse: map[string]string{"enablePullRequestAutoMerge": "Pull request Auto merge is not allowed for this repository"}, merge: "check", err: connectors.ErrBlocked, mutations: []string{"enablePullRequestAutoMerge"}},
		"a forbidden method":       {state: prGraphQL(""), refuse: map[string]string{"enablePullRequestAutoMerge": "Merge method squash merging is not allowed on this repository"}, err: connectors.ErrUnsupported, mutations: []string{"enablePullRequestAutoMerge"}},
		"another refusal":          {state: prGraphQL(""), refuse: map[string]string{"enablePullRequestAutoMerge": "Failed to add PR #5: expected head oid does not match the current head oid"}, err: connectors.ErrBlocked, mutations: []string{"enablePullRequestAutoMerge"}},
		"the head moved":           {state: prGraphQL(`,"headRefOid":"newer","autoMergeRequest":{"mergeMethod":"SQUASH"}`), err: connectors.ErrBlocked, mutations: []string{"disablePullRequestAutoMerge"}},
		"the head moved, queued":   {state: prGraphQL(queued + `,"headRefOid":"newer","mergeQueueEntry":{"state":"QUEUED","headCommit":null}`), err: connectors.ErrBlocked, mutations: []string{"dequeuePullRequest"}},
		"enqueued":                 {state: prGraphQL(queued), mutations: []string{"enqueuePullRequest"}},
		"enqueue refused":          {state: prGraphQL(queued), refuse: map[string]string{"enqueuePullRequest": `Pull request Required status check "ci" is in progress.`}, err: connectors.ErrBlocked, mutations: []string{"enqueuePullRequest"}},
		"queued, no group yet":     {state: prGraphQL(queued + `,"mergeQueueEntry":{"state":"QUEUED","headCommit":null}`)},
		"the group awaits checks":  {state: prGraphQL(queued + `,"mergeQueueEntry":{"state":"AWAITING_CHECKS","headCommit":{"oid":"group"}}`), group: `[]`, published: true},
		"the group approved":       {state: prGraphQL(queued + `,"mergeQueueEntry":{"state":"AWAITING_CHECKS","headCommit":{"oid":"group"}}`), group: `[{"name":"quai/approval","status":"completed","conclusion":"success","app":{"id":4242}}]`},
		"the group's other checks": {state: prGraphQL(queued + `,"mergeQueueEntry":{"state":"AWAITING_CHECKS","headCommit":{"oid":"group"}}`), group: `[{"name":"ci","status":"completed","conclusion":"success"}]`, published: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAPI(t)
			c := f.connector()
			g := &fakeGraphQL{state: tc.state, refuse: tc.refuse, errorType: tc.errorType}
			g.install(f)
			f.answer(runs, http.StatusOK, fmt.Sprintf(`{"total_count":1,"check_runs":%s}`, tc.group))
			f.answer(status, http.StatusOK, `{"state":"pending","statuses":[]}`)
			f.answer(post, http.StatusCreated, `{"id":1}`)
			f.answer("GET /repos/o/r/pulls/5", http.StatusOK, prJSON("open", `,"mergeable":true`))
			f.answer("DELETE /repos/o/r/git/refs/heads/quai/x", http.StatusNoContent, "")
			switch tc.merge {
			case "check":
				f.answer(put, http.StatusMethodNotAllowed, `{"message":"Required status check \"quai/approval\" is expected."}`)
			case "":
			default:
				f.answer(put, http.StatusOK, tc.merge)
			}
			method := tc.method
			if method == "" {
				method = connectors.Squash
			}
			err := c.PullRequests().AutoComplete(t.Context(), "5", connectors.MergeOptions{Head: "approved", Method: method, Message: message, DeleteBranch: true, Approval: approval})
			switch {
			case tc.err != nil:
				if !errors.Is(err, tc.err) {
					t.Errorf("err = %v, want %v", err, tc.err)
				}
			case tc.plain:
				if err == nil || errors.Is(err, connectors.ErrBlocked) || errors.Is(err, connectors.ErrUnsupported) || errors.Is(err, connectors.ErrNotFound) {
					t.Errorf("err = %v, want a failure of its own", err)
				}
			case err != nil:
				t.Fatal(err)
			}
			f.set(func() {
				if strings.Join(g.mutations, ",") != strings.Join(tc.mutations, ",") {
					t.Errorf("mutations %v, want %v", g.mutations, tc.mutations)
				}
				if v, ok := g.vars["enablePullRequestAutoMerge"]; ok && (v["method"] != "SQUASH" || v["head"] != "approved" || v["id"] != "PR_5" ||
					v["title"] != "Resize web01" || v["body"] != "Approved-by: Alice <alice@example.com>") {
					t.Errorf("auto-merge asked with %v", v)
				}
				if v, ok := g.vars["enqueuePullRequest"]; ok && (v["head"] != "approved" || v["id"] != "PR_5") {
					t.Errorf("enqueued with %v", v)
				}
			})
			if got := f.count(post) == 1; got != tc.published {
				t.Errorf("published on the merge group: %v, want %v", got, tc.published)
			}
			if tc.published {
				if body := f.body(post); body["head_sha"] != "group" || body["name"] != "quai/approval" || body["conclusion"] != "success" || body["details_url"] != approval.URL {
					t.Errorf("the merge group's check run: %v", body)
				}
			}
			if merges := f.count(put); (tc.merge != "") != (merges > 0) {
				t.Errorf("%d merges asked", merges)
			}
		})
	}
}

// TestAutoCompleteRebase: a rebase keeps Quai's commits, so auto-merge is asked without a message.
func TestAutoCompleteRebase(t *testing.T) {
	f := newFakeAPI(t)
	c := f.connector()
	g := &fakeGraphQL{state: prGraphQL("")}
	g.install(f)
	if err := c.PullRequests().AutoComplete(t.Context(), "5", connectors.MergeOptions{Method: connectors.Rebase, Message: "Resize\n\nApproved-by: Alice\n"}); err != nil {
		t.Fatal(err)
	}
	f.set(func() {
		v := g.vars["enablePullRequestAutoMerge"]
		if v["method"] != "REBASE" || v["head"] != nil || v["title"] != nil || v["body"] != nil {
			t.Errorf("auto-merge asked with %v", v)
		}
	})
}
