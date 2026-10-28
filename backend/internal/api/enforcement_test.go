package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/ingest"
	"github.com/damhau/quai/backend/internal/store/db"
)

// planBody is resizeBody without the reason, for /plans/type.
func planBody(cpu int) map[string]any {
	b := resizeBody(cpu)
	delete(b, "reason")
	return b
}

// timeline returns the kinds of the repository's events, oldest first.
func (h *harness) timeline(t *testing.T) []string {
	t.Helper()
	list, err := h.store.ListTargetEvents(context.Background(), db.ListTargetEventsParams{Target: "repository:infra-live", MaxRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for i := len(list) - 1; i >= 0; i-- {
		kinds = append(kinds, list[i].Kind)
	}
	return kinds
}

// TestEnforcement: without the branch protection requiring quai/approval, Quai opens no pull
// request: planning and submitting say why (verified at first use), the repository shows it and
// an alert is recorded. Restoring the protection unblocks; removing the required check blocks
// again.
func TestEnforcement(t *testing.T) {
	h, remote := giteaHarness(t, "manual")
	ctx := context.Background()
	bob := h.as("bob")
	if err := remote.Unprotect("main"); err != nil {
		t.Fatal(err)
	}
	refused := func(what string, want string) {
		t.Helper()
		status, body := bob.do(http.MethodPost, Prefix+"/plans/type", planBody(6), nil)
		if status != http.StatusConflict || !strings.Contains(body, "approvals are not enforced on infra-live") || !strings.Contains(body, want) {
			t.Errorf("%s: plan = %d %s", what, status, body)
		}
		status, body = bob.do(http.MethodPost, Prefix+"/requests/type", resizeBody(6), nil)
		if status != http.StatusConflict || !strings.Contains(body, want) {
			t.Errorf("%s: submit = %d %s", what, status, body)
		}
	}
	refused("unprotected", "branch main is not protected")
	var repos []RepositoryView
	bob.do(http.MethodGet, Prefix+"/repositories", nil, &repos)
	if len(repos) != 1 || !repos[0].Enforcement.Required || !repos[0].Enforcement.Checked || len(repos[0].Enforcement.Problems) != 1 {
		t.Errorf("the repository's enforcement = %+v", repos)
	}
	if got := h.timeline(t); !slices.Equal(got, []string{"alert_enforcement"}) {
		t.Errorf("timeline = %v", got)
	}

	// Restored, as a policy change says: requests go through again.
	if err := remote.Protect("main", "quai/approval"); err != nil {
		t.Fatal(err)
	}
	if err := h.ingest.Process(ctx, "infra-live", []ingest.Event{{Policy: &connectors.PolicyChanged{Branch: "main"}}}); err != nil {
		t.Fatal(err)
	}
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/type", planBody(6), nil); status != http.StatusOK {
		t.Errorf("enforced again: plan = %d %s", status, body)
	}
	r := opened(t, h)
	if got := h.timeline(t); !slices.Equal(got, []string{"alert_enforcement", "enforcement_restored"}) {
		t.Errorf("timeline = %v", got)
	}

	// The protection no longer requires the check, as the hourly verification finds.
	if err := remote.Unprotect("main"); err != nil {
		t.Fatal(err)
	}
	if err := remote.Protect("main"); err != nil {
		t.Fatal(err)
	}
	if err := h.ingest.Verify(ctx, "infra-live"); err != nil {
		t.Fatal(err)
	}
	refused("without the check", "does not require the status check quai/approval")
	// The request already open is not affected: its pull request stays, and it can be approved.
	if status, body := h.as("alice").do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Errorf("approving the open request = %d %s", status, body)
	}
}
