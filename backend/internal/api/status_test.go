package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// TestResourceStatus follows resources of envs/prod through commits and runs
// (docs/design/06-runs-status-and-drift.md#resource-status): applied, a pending change, in the
// repository after its merge, applying, failed, a person's commit, one apply covering several
// merges, drifted.
func TestResourceStatus(t *testing.T) {
	t.Parallel()
	h := newHarnessRunner(t, "")
	bob, alice := h.as("bob"), h.as("alice")
	head := func() string { return strings.TrimSpace(git(t, h.bare, "rev-parse", "main")) }
	state := func(id string) (string, *RunView) {
		t.Helper()
		var d ResourceDetail
		if status, body := bob.do(http.MethodGet, Prefix+"/resource?id="+id, nil, &d); status != http.StatusOK {
			t.Fatalf("%s = %d %s", id, status, body)
		}
		return d.State, d.StateRun
	}
	expect := func(when, id, want string) {
		t.Helper()
		if got, _ := state(id); got != want {
			t.Errorf("%s: %s is %s, want %s", when, id, got, want)
		}
	}
	apply := func(status, commit, url string) {
		t.Helper()
		if code, _, body := h.reportRun(runToken, runReport(map[string]any{"status": status, "commit": commit, "url": url})); code != http.StatusAccepted {
			t.Fatalf("apply report = %d %s", code, body)
		}
	}
	const web, ftp, db = "infra-live/vm/prod/web01", "infra-live/vm/prod/legacy-ftp", "infra-live/vm/prod/db01"

	expect("before any run", web, "in_repo")
	c0 := head()
	apply("succeeded", c0, "https://ci.example.com/1")
	expect("after the first apply", web, "applied")
	if _, run := state(web); run == nil || run.Commit != c0 {
		t.Errorf("the run behind applied = %+v", run)
	}

	// A request on web01 waits for a platform admin: a pending change.
	var r RequestView
	if code, body := bob.do(http.MethodPost, Prefix+"/requests/type", resizeBody(6), &r); code != http.StatusCreated {
		t.Fatalf("resize = %d %s", code, body)
	}
	expect("while the request waits", web, "pending_change")
	expect("another resource meanwhile", ftp, "applied")
	alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil)
	bob.await(r.ID, "applying")
	c1 := head()
	expect("merged, not applied yet", web, "in_repo")

	// A person changes legacy-ftp: a second merge before the next apply.
	ftpFile, _ := h.file("envs/prod/vms/legacy-ftp.yaml")
	h.push("envs/prod/vms/legacy-ftp.yaml", ftpFile+"# checked by hand\n")
	bob.do(http.MethodPost, Prefix+"/repositories/infra-live/sync", nil, nil)
	c2 := head()
	expect("a person's commit", ftp, "in_repo")

	apply("running", c2, "https://ci.example.com/2")
	expect("applying the two merges", web, "applying")
	apply("failed", c2, "https://ci.example.com/2")
	expect("the apply failed", web, "failed")
	expect("the apply failed", ftp, "failed")

	// A fix elsewhere, then an apply that covers it all.
	dbFile, _ := h.file("envs/prod/vms/db01.yaml")
	h.push("envs/prod/vms/db01.yaml", dbFile+"# fixed\n")
	bob.do(http.MethodPost, Prefix+"/repositories/infra-live/sync", nil, nil)
	c3 := head()
	expect("after the fix's commit", db, "in_repo")
	expect("after the fix's commit", web, "failed")
	apply("succeeded", c3, "https://ci.example.com/3")
	for _, id := range []string{web, ftp, db} {
		expect("one apply for every merge", id, "applied")
	}

	// A drift check whose plan changes web01, db01 and legacy-ftp.
	if code, _, body := h.reportRun(runToken, runReport(map[string]any{"kind": "drift-check", "commit": c3, "url": "https://ci.example.com/4",
		"plan": map[string]any{"format": "terraform-json", "content": recordedPlan(t, "prod.json")}})); code != http.StatusAccepted {
		t.Fatalf("drift check = %d %s", code, body)
	}
	expect("after the drift check", web, "drifted")
	var inv []ResourceView
	bob.do(http.MethodGet, Prefix+"/inventory", nil, &inv)
	got := map[string]string{}
	for _, v := range inv {
		got[v.ID] = v.State
	}
	if got[web] != "drifted" || got["infra-live/vm/dev/build-agent-01"] != "in_repo" {
		t.Errorf("the inventory = %v", got)
	}

	// The history shows the commits by people and by Quai, Quai's with their request.
	var d ResourceDetail
	bob.do(http.MethodGet, Prefix+"/resource?id="+web, nil, &d)
	if i := slices.IndexFunc(d.History, func(c CommitView) bool { return c.SHA == c1 }); i < 0 || d.History[i].Request != r.Number {
		t.Errorf("Quai's commit in the history = %+v", d.History)
	}
	if i := slices.IndexFunc(d.History, func(c CommitView) bool { return c.SHA == c0 }); i >= 0 && d.History[i].Request != 0 {
		t.Errorf("the first commit names a request: %+v", d.History[i])
	}
	_ = c2
}
