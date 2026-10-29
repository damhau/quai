package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/generic"
	"github.com/damhau/quai/backend/internal/connectors/pipelinestest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
)

// driftHarness serves infra-live with a runner whose drift-check pipeline covers envs/{env}, on
// fake pipelines.
func driftHarness(t *testing.T) (*harness, *pipelinestest.Pipelines) {
	t.Helper()
	dir := t.TempDir()
	bare, work := localRemote(t, dir)
	fake := &pipelinestest.Pipelines{}
	h := start(t, dir, `connector: generic
  url: `+bare+`
  runner:
    reportToken: { ref: env:INFRA_LIVE_RUN_TOKEN }
    pipelines: [{ name: infra-drift, kind: drift-check, stacks: "envs/{env}" }]
  sync: { minInterval: 1ns }`, repositories.Options{
		Secrets: secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(name string) (string, bool) {
			sum := sha256.Sum256([]byte(runToken))
			return hex.EncodeToString(sum[:]), name == "INFRA_LIVE_RUN_TOKEN"
		}}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"generic": pipelinestest.Factory(generic.New, fake)}},
	})
	h.bare, h.work = bare, work
	return h, fake
}

// TestDriftChecks: a repository admin queues the drift checks of the drift-check pipeline's
// stacks, each with its stack as a template parameter; a drift report marks the resources whose
// Terraform resources changed as Drifted, with what differs, and a clean one clears them
// (docs/design/06-runs-status-and-drift.md#drift).
func TestDriftChecks(t *testing.T) {
	h, fake := driftHarness(t)
	alice, bob := h.as("alice"), h.as("bob")
	// The stack was applied before the drift checks run.
	head := strings.TrimSpace(git(t, h.bare, "rev-parse", "main"))
	h.reportRun(runToken, runReport(map[string]any{"commit": head, "url": "https://ci.example.com/apply/1"}))
	var queued []RunView
	if status, body := alice.do(http.MethodPost, Prefix+"/repositories/infra-live/drift-checks", map[string]any{}, &queued); status != http.StatusAccepted || len(queued) != 2 {
		t.Fatalf("queue = %d %s", status, body)
	}
	triggered := fake.Triggered()
	if len(triggered) != 2 || triggered[0].Parameters["stack"] != "envs/dev" || triggered[1].Parameters["stack"] != "envs/prod" || triggered[0].Pipeline != "infra-drift" {
		t.Errorf("triggered = %+v", triggered)
	}
	if q := queued[1]; q.Kind != "drift-check" || q.Status != "running" || q.Stack != "envs/prod" || q.URL != triggered[1].URL {
		t.Errorf("the queued run = %+v", q)
	}
	if status, _ := alice.do(http.MethodPost, Prefix+"/repositories/infra-live/drift-checks", map[string]any{"stack": "envs/prod"}, &queued); status != http.StatusAccepted || len(queued) != 1 {
		t.Errorf("one stack = %d %+v", status, queued)
	}
	if status, body := alice.do(http.MethodPost, Prefix+"/repositories/infra-live/drift-checks", map[string]any{"stack": "platform/dns"}, nil); status != http.StatusUnprocessableEntity || !strings.Contains(body, "body.stack") {
		t.Errorf("a stack it does not cover = %d %s", status, body)
	}
	if status, _ := bob.do(http.MethodPost, Prefix+"/repositories/infra-live/drift-checks", map[string]any{}, nil); status != http.StatusForbidden {
		t.Errorf("a requester queues drift checks: %d", status)
	}

	// Drifted by the drift check's report, then clean again.
	const web = "infra-live/vm/prod/web01"
	state := func() ResourceDetail {
		var d ResourceDetail
		bob.do(http.MethodGet, Prefix+"/resource?id="+web, nil, &d)
		return d
	}
	if s := state(); s.State != "applied" {
		t.Fatalf("while the drift check runs: %s", s.State)
	}
	h.reportRun(runToken, runReport(map[string]any{"kind": "drift-check", "commit": head, "url": queued[0].URL,
		"plan": map[string]any{"format": "terraform-json", "content": recordedPlan(t, "prod.json")}}))
	d := state()
	if d.State != "drifted" || d.StateRun == nil || d.StateRun.URL != queued[0].URL {
		t.Fatalf("after a drift: %s, %+v", d.State, d.StateRun)
	}
	var drifted []string
	for _, r := range d.StateRun.Plan.Resources {
		if r.Resource == web {
			drifted = r.Changes[0].Attributes
		}
	}
	if strings.Join(drifted, ",") != "input.cpu,output" {
		t.Errorf("what differs = %v", drifted)
	}
	clean := json.RawMessage(`{"format_version": "1.2", "terraform_version": "1.16.4", "resource_changes": []}`)
	h.reportRun(runToken, runReport(map[string]any{"kind": "drift-check", "commit": head, "url": "https://ci.example.com/drift/2",
		"plan": map[string]any{"format": "terraform-json", "content": clean}}))
	if s := state(); s.State != "applied" {
		t.Errorf("after a clean drift check: %s", s.State)
	}
}

// TestNoDriftCheck: without a drift-check pipeline, there is nothing to queue.
func TestNoDriftCheck(t *testing.T) {
	h := newHarnessRunner(t, "")
	if status, _ := h.as("alice").do(http.MethodPost, Prefix+"/repositories/infra-live/drift-checks", map[string]any{}, nil); status != http.StatusConflict {
		t.Errorf("queue without a drift-check pipeline = %d", status)
	}
}
