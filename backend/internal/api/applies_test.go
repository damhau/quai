package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/store/db"
)

func eventKinds(r RequestView) []string {
	var kinds []string
	for _, e := range r.Events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// TestRequestsAfterMerge: with a runner, a committed request waits for the apply that includes
// its commit (docs/architecture/domain-model.md#request): one apply settles two requests; a
// failed apply fails the request, which is not delivered again; the timeline has the plans and
// the applies, and the resource's page its applies.
func TestRequestsAfterMerge(t *testing.T) {
	h := newHarnessRunner(t, "")
	bob := h.as("bob")
	head := func() string { return strings.TrimSpace(git(t, h.bare, "rev-parse", "main")) }
	create := func(name string) RequestView {
		t.Helper()
		var r RequestView
		if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", newVM("dev", name, 2), &r); status != http.StatusCreated {
			t.Fatalf("submit %s = %d %s", name, status, body)
		}
		return bob.await(r.ID, "applying")
	}
	apply := func(status, commit, url string) {
		t.Helper()
		if code, _, body := h.reportRun(runToken, runReport(map[string]any{"stack": "envs/dev", "status": status, "commit": commit, "url": url})); code != http.StatusAccepted {
			t.Fatalf("apply report = %d %s", code, body)
		}
	}

	first := create("api01")
	firstCommit := head()
	second := create("api02")
	apply("running", head(), "https://ci.example.com/1")
	if s := bob.read(first.ID).State; s != "applying" {
		t.Errorf("while the apply runs, the request is %s", s)
	}
	// One apply at the second commit includes both.
	apply("succeeded", head(), "https://ci.example.com/1")
	for _, r := range []RequestView{first, second} {
		got := bob.read(r.ID)
		if got.State != "applied" || !slices.Contains(eventKinds(got), "applied") || !slices.Contains(eventKinds(got), "committed") {
			t.Errorf("request #%d after the apply: %s, %v", got.Number, got.State, eventKinds(got))
		}
	}
	var d ResourceDetail
	bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/vm/dev/api01", nil, &d)
	if len(d.Applies) != 1 || d.Applies[0].Kind != "applied" || d.Applies[0].Request != first.ID || !strings.Contains(string(d.Applies[0].Data), "https://ci.example.com/1") {
		t.Errorf("the resource's applies = %+v", d.Applies)
	}

	// An apply of another stack settles nothing; a failed apply fails the request.
	third := create("api03")
	if code, _, body := h.reportRun(runToken, runReport(map[string]any{"stack": "envs/prod", "commit": head()})); code != http.StatusAccepted {
		t.Fatalf("prod apply = %d %s", code, body)
	}
	if s := bob.read(third.ID).State; s != "applying" {
		t.Errorf("after another stack's apply: %s", s)
	}
	apply("failed", head(), "https://ci.example.com/2")
	got := bob.read(third.ID)
	if got.State != "failed" || !slices.Contains(eventKinds(got), "apply_failed") || got.CanRetry {
		t.Errorf("after a failed apply: %s, %v, can retry %v", got.State, eventKinds(got), got.CanRetry)
	}
	if status, _ := bob.do(http.MethodPost, Prefix+"/requests/"+third.ID+"/retry", nil, nil); status != http.StatusConflict {
		t.Errorf("retrying a failed apply = %d", status)
	}
	if s := bob.read(first.ID).State; s != "applied" {
		t.Errorf("an earlier request after a later failure: %s", s)
	}

	// A plan of a request's pull request goes on its timeline, with its summary and cost.
	if _, err := h.store.PutDelivery(t.Context(), db.PutDeliveryParams{RequestID: uuid.MustParse(first.ID), Repository: "infra-live", Branch: "quai/r-1", PullRequest: "41"}); err != nil {
		t.Fatal(err)
	}
	h.reportRun(runToken, runReport(map[string]any{"kind": "plan", "stack": "envs/dev", "commit": firstCommit, "pullRequest": "41",
		"summary": map[string]any{"add": 1, "change": 0, "destroy": 0}, "cost": map[string]any{"currency": "CHF", "monthlyDelta": 142.4},
		"policies": []map[string]any{{"name": "no-public-ip", "result": "warning"}}}))
	got = bob.read(first.ID)
	i := slices.IndexFunc(got.Events, func(e EventView) bool { return e.Kind == "plan_reported" })
	if i < 0 || !strings.Contains(string(got.Events[i].Data), `"monthlyDelta":142.4`) || !strings.Contains(string(got.Events[i].Data), `"warning":1`) {
		t.Errorf("the plan on the timeline: %+v", got.Events)
	}
}

// TestNoRunner: without spec.runner, a committed request is applied.
func TestNoRunner(t *testing.T) {
	h := newHarness(t)
	bob := h.as("bob")
	var r RequestView
	bob.do(http.MethodPost, Prefix+"/requests/type", newVM("dev", "api01", 2), &r)
	if got := bob.await(r.ID, "applied"); slices.Contains(eventKinds(got), "applied") {
		t.Errorf("an apply event without a runner: %v", eventKinds(got))
	}
}
