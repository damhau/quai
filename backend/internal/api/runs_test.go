package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/store/db"
)

// reportRun sends a run report to infra-live with token, and decodes the run Quai answers.
func (h *harness) reportRun(token string, body map[string]any) (int, RunView, string) {
	h.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+Prefix+"/runs", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	var run RunView
	if resp.StatusCode == http.StatusAccepted {
		if err := json.Unmarshal(buf.Bytes(), &run); err != nil {
			h.t.Fatal(err)
		}
	}
	return resp.StatusCode, run, buf.String()
}

// runReport is a run report on infra-live's envs/prod stack, changed by change.
func runReport(change map[string]any) map[string]any {
	r := map[string]any{"repository": "infra-live", "stack": "envs/prod", "commit": "abcdef1", "kind": "apply", "status": "succeeded"}
	for k, v := range change {
		if v == nil {
			delete(r, k)
		} else {
			r[k] = v
		}
	}
	return r
}

// Ported from "sync webhook updates deployment status": runners report runs with the
// repository's run token, and resources show their stack's latest apply.
func TestRunReports(t *testing.T) {
	h := newHarnessRunner(t, "")
	sent := runReport(map[string]any{"url": "https://ci.example.com/1"})
	if status, _, _ := h.reportRun("wrong", sent); status != http.StatusUnauthorized {
		t.Errorf("wrong token = %d", status)
	}
	if status, _, _ := h.reportRun("", sent); status != http.StatusUnprocessableEntity {
		t.Errorf("no token = %d", status)
	}
	if status, _, body := h.reportRun(runToken, sent); status != http.StatusAccepted {
		t.Fatalf("report = %d %s", status, body)
	}
	var d ResourceDetail
	h.as("bob").do(http.MethodGet, Prefix+"/resource?id=infra-live/vm/prod/web01", nil, &d)
	if d.LastApply == nil || d.LastApply.Status != "succeeded" || d.LastApply.Commit != "abcdef1" {
		t.Errorf("last apply = %+v", d.LastApply)
	}
	var inv []ResourceView
	h.as("bob").do(http.MethodGet, Prefix+"/inventory", nil, &inv)
	for _, r := range inv {
		if (r.Stack == "envs/prod") != (r.LastApply != nil) {
			t.Errorf("%s: last apply %+v", r.ID, r.LastApply)
		}
	}
}

// TestRunReportProtocol: the report of docs/design/06-runs-status-and-drift.md is validated;
// a run reported running and then finished is one run; runs are listed per repository.
func TestRunReportProtocol(t *testing.T) {
	h := newHarnessRunner(t, "")
	for name, change := range map[string]map[string]any{
		"Phase 0's drift kind":     {"kind": "drift"},
		"a commit that is not one": {"commit": "main"},
		"a policy result":          {"policies": []map[string]any{{"name": "tags", "result": "maybe"}}},
		"a currency":               {"cost": map[string]any{"currency": "chf", "monthlyDelta": 1}},
		"a negative count":         {"summary": map[string]any{"add": -1, "change": 0, "destroy": 0}},
		"a start time":             {"startedAt": "yesterday"},
	} {
		if status, _, body := h.reportRun(runToken, runReport(change)); status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, status, body)
		}
	}

	// Running, then finished, with the same url: one run, with what the second report says.
	status, started, body := h.reportRun(runToken, runReport(map[string]any{"kind": "plan", "status": "running", "pullRequest": "412",
		"url": "https://ci.example.com/88121", "startedAt": "2026-10-26T09:12:03Z"}))
	if status != http.StatusAccepted || started.Status != "running" || started.FinishedAt != nil || started.PullRequest != "412" {
		t.Fatalf("running = %d %s", status, body)
	}
	_, finished, body := h.reportRun(runToken, runReport(map[string]any{"kind": "plan", "pullRequest": "412", "url": "https://ci.example.com/88121",
		"finishedAt": "2026-10-26T09:13:40Z", "summary": map[string]any{"add": 1, "change": 0, "destroy": 0},
		"policies": []map[string]any{{"name": "vm-tags-required", "result": "passed"}, {"name": "no-public-ip", "result": "warning", "message": "a public IP"}},
		"cost":     map[string]any{"currency": "CHF", "monthlyDelta": 142.4}}))
	if finished.ID != started.ID || finished.Status != "succeeded" || finished.FinishedAt == nil || finished.FinishedAt.UTC().Format("15:04:05") != "09:13:40" ||
		finished.StartedAt.UTC().Format("15:04:05") != "09:12:03" || finished.Summary == nil || finished.Summary.Add != 1 || len(finished.Policies) != 2 ||
		finished.Policies[1].Message != "a public IP" || finished.Cost == nil || finished.Cost.MonthlyDelta != 142.4 {
		t.Errorf("finished = %s", body)
	}
	// The same report again, as an adapter polling sees it, is the same run; so is a re-run, which
	// keeps its page.
	if _, again, _ := h.reportRun(runToken, runReport(map[string]any{"kind": "plan", "pullRequest": "412", "url": "https://ci.example.com/88121"})); again.ID != started.ID {
		t.Errorf("the same report again started a run")
	}
	if _, rerun, _ := h.reportRun(runToken, runReport(map[string]any{"kind": "plan", "status": "running", "pullRequest": "412", "url": "https://ci.example.com/88121"})); rerun.ID != started.ID || rerun.Status != "running" {
		t.Errorf("a re-run = %+v", rerun)
	}
	h.reportRun(runToken, runReport(map[string]any{"kind": "plan", "pullRequest": "412", "url": "https://ci.example.com/88121"}))
	// Another pipeline run at the same commit, with its own url, is another run.
	_, other, _ := h.reportRun(runToken, runReport(map[string]any{"kind": "plan", "status": "running", "pullRequest": "412", "url": "https://ci.example.com/88122"}))
	if other.ID == started.ID {
		t.Error("a second pipeline run continued the first")
	}
	// Without a url, the commit and the pull request say which run a report finishes.
	_, a, _ := h.reportRun(runToken, runReport(map[string]any{"commit": "1234567", "status": "running"}))
	_, b, _ := h.reportRun(runToken, runReport(map[string]any{"commit": "1234567", "status": "failed"}))
	_, c, _ := h.reportRun(runToken, runReport(map[string]any{"commit": "1234567", "status": "running", "stack": "envs/dev"}))
	if a.ID != b.ID || b.Status != "failed" || c.ID == a.ID {
		t.Errorf("without a url: %+v, %+v, %+v", a, b, c)
	}

	bob := h.as("bob")
	var runs []RunView
	if status, body := bob.do(http.MethodGet, Prefix+"/repositories/infra-live/runs", nil, &runs); status != http.StatusOK || len(runs) != 4 {
		t.Fatalf("runs = %d %s", status, body)
	}
	bob.do(http.MethodGet, Prefix+"/repositories/infra-live/runs?stack=envs/prod&kind=plan", nil, &runs)
	if len(runs) != 2 || runs[0].ID != other.ID || runs[1].ID != started.ID {
		t.Errorf("plans of envs/prod, newest first = %+v", runs)
	}
	if status, _ := h.as("").do(http.MethodGet, Prefix+"/repositories/infra-live/runs", nil, nil); status != http.StatusUnauthorized {
		t.Errorf("anonymous = %d", status)
	}
	if status, _ := bob.do(http.MethodGet, Prefix+"/repositories/nope/runs", nil, nil); status != http.StatusNotFound {
		t.Errorf("unknown repository = %d", status)
	}
}

// TestRunRequests: a run belongs to the request whose commit it ran at, or whose pull request
// it names.
func TestRunRequests(t *testing.T) {
	h := newHarnessRunner(t, "")
	bob := h.as("bob")
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", newVM("dev", "api01", 2), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	bob.await(r.ID, "applying") // committed; the runner applies it
	commit := strings.TrimSpace(git(t, h.bare, "rev-parse", "main"))

	_, run, body := h.reportRun(runToken, runReport(map[string]any{"stack": "envs/dev", "commit": commit[:7]}))
	if run.Request != r.ID {
		t.Errorf("an apply at the request's commit = %s", body)
	}
	if _, run, _ := h.reportRun(runToken, runReport(map[string]any{"stack": "envs/dev", "commit": "0000000"})); run.Request != "" {
		t.Errorf("an apply at another commit belongs to %s", run.Request)
	}
	if _, err := h.store.PutDelivery(t.Context(), db.PutDeliveryParams{RequestID: uuid.MustParse(r.ID), Repository: "infra-live",
		Branch: "quai/r-1", PullRequest: "412"}); err != nil {
		t.Fatal(err)
	}
	_, plan, body := h.reportRun(runToken, runReport(map[string]any{"stack": "envs/dev", "commit": "fedcba9", "kind": "plan", "pullRequest": "412"}))
	if plan.Request != r.ID {
		t.Errorf("a plan of the request's pull request = %s", body)
	}
	var runs []RunView
	bob.do(http.MethodGet, Prefix+"/repositories/infra-live/runs?kind=plan", nil, &runs)
	if len(runs) != 1 || runs[0].RequestNumber != r.Number {
		t.Errorf("listed = %+v", runs)
	}
}

func recordedPlan(t *testing.T, name string) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("../engine/tfplan/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRunPlans: a report's plan is kept as what it changes, by resource; the summary follows
// from it; values only when the repository keeps them, and never sensitive ones.
func TestRunPlans(t *testing.T) {
	h := newHarnessRunner(t, "")
	_, run, body := h.reportRun(runToken, runReport(map[string]any{"kind": "plan",
		"plan": map[string]any{"format": "terraform-json", "content": recordedPlan(t, "prod.json")}}))
	if run.Plan == nil || len(run.Plan.Resources) != 4 || run.Summary == nil || *run.Summary != (RunSummary{Add: 2, Change: 1, Destroy: 2}) {
		t.Fatalf("the run = %s", body)
	}
	web := run.Plan.Resources[3]
	if web.Resource != "infra-live/vm/prod/web01" || web.Changes[0].Action != "update" || !slices.Equal(web.Changes[0].Attributes, []string{"input.cpu", "output"}) ||
		web.Changes[0].Values != nil {
		t.Errorf("web01 = %+v", web)
	}
	if strings.Contains(body, `"prior_state"`) || strings.Contains(body, `"configuration"`) {
		t.Error("the plan itself is kept")
	}
	var runs []RunView
	h.as("bob").do(http.MethodGet, Prefix+"/repositories/infra-live/runs", nil, &runs)
	if len(runs) != 1 || runs[0].Plan == nil || len(runs[0].Plan.Resources) != 4 {
		t.Errorf("listed = %+v", runs)
	}
	if status, _, body := h.reportRun(runToken, runReport(map[string]any{"kind": "plan",
		"plan": map[string]any{"format": "terraform-json", "content": map[string]any{"resources": []any{}}}})); status != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "body.plan.content") {
		t.Errorf("not a plan = %d %s", status, body)
	}
	// A plan in the run's artifacts needs a platform with pipelines to read it from.
	if status, _, body := h.reportRun(runToken, runReport(map[string]any{"kind": "plan", "url": "https://ci.example.com/_build/results?buildId=7",
		"plan": map[string]any{"format": "terraform-json", "ref": "artifact://plan/plan.json"}})); status != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "body.plan.ref") || !strings.Contains(body, "has no pipelines") {
		t.Errorf("a plan ref without pipelines = %d %s", status, body)
	}

	h = newHarnessRunner(t, "planValues: true")
	_, run, _ = h.reportRun(runToken, runReport(map[string]any{"kind": "plan", "plan": map[string]any{"format": "terraform-json", "content": recordedPlan(t, "prod.json")}}))
	if v := run.Plan.Resources[3].Changes[0].Values["input.cpu"]; v.Before != 4.0 || v.After != 8.0 {
		t.Errorf("with values, input.cpu = %+v", v)
	}
	_, run, body = h.reportRun(runToken, runReport(map[string]any{"kind": "plan", "stack": "apps/prod",
		"plan": map[string]any{"format": "terraform-json", "content": recordedPlan(t, "nested-update.json")}}))
	if len(run.Plan.Unmapped) != 2 || strings.Contains(body, "fixture-secret-value") || strings.Contains(body, "fixture-new-secret") {
		t.Errorf("a sensitive update = %s", body)
	}
}
