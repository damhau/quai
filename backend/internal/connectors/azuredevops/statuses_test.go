package azuredevops

import (
	"testing"

	"github.com/damhau/quai/backend/internal/connectors"
)

func TestStatusMapping(t *testing.T) {
	if got := statusContext("quai/approval"); got["genre"] != "quai" || got["name"] != "approval" {
		t.Errorf("statusContext = %v", got)
	}
	if got := statusContext("plan"); got["genre"] != "" || got["name"] != "plan" {
		t.Errorf("statusContext = %v", got)
	}
	mk := func(id, iteration int, name, state string) status {
		st := status{ID: id, State: state, IterationID: iteration}
		st.Context.Genre, st.Context.Name = "quai", name
		return st
	}
	all := []status{mk(1, 1, "approval", "pending"), mk(2, 1, "approval", "succeeded"), mk(3, 2, "approval", "pending"), mk(4, 1, "plan", "failed")}
	// On iteration 2, only its own status counts: a push made the old ones stale.
	if got := latest(all, 2); len(got) != 1 || got[0].Name != "quai/approval" || got[0].State != connectors.StatusPending {
		t.Errorf("iteration 2: %+v", got)
	}
	if got := latest(all, 1); len(got) != 2 || got[0].State != connectors.StatusFailure || got[1].State != connectors.StatusSuccess {
		t.Errorf("iteration 1: %+v", got)
	}
	for status, want := range map[string]connectors.StatusState{"approved": connectors.StatusSuccess, "notApplicable": connectors.StatusSuccess,
		"rejected": connectors.StatusFailure, "broken": connectors.StatusFailure, "running": connectors.StatusPending, "queued": connectors.StatusPending} {
		if got := (evaluation{status: status}).state(); got != want {
			t.Errorf("%s = %s", status, got)
		}
	}
	if fromState("notApplicable") != connectors.StatusNotApplicable || fromState("succeeded") != connectors.StatusSuccess {
		t.Error("fromState")
	}
	if _, err := completion(connectors.MergeOptions{Method: "fast-forward"}); err == nil {
		t.Error("an unknown merge method")
	}
	if o, _ := completion(connectors.MergeOptions{Method: connectors.MergeCommit, Message: "m\n", DeleteBranch: true}); o["mergeStrategy"] != "noFastForward" || o["bypassPolicy"] != false {
		t.Errorf("completion = %v", o)
	}
}
