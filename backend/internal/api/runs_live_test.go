package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/adotest"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/azuredevops"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/ingest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store/db"
)

// sandboxRunner resolves env:ADO_PAT to the sandbox's PAT and env:INFRA_LIVE_RUN_TOKEN to the
// run token's hash.
type sandboxRunner struct{ sb *adotest.Sandbox }

func (p sandboxRunner) Get(ctx context.Context, name string) (secrets.Secret, error) {
	if name == "INFRA_LIVE_RUN_TOKEN" {
		sum := sha256.Sum256([]byte(runToken))
		return secrets.FromValue(hex.EncodeToString(sum[:])), nil
	}
	return sandboxPAT(p).Get(ctx, name)
}

const (
	echoPipeline = "trigger: none\npool:\n  vmImage: ubuntu-latest\nsteps:\n  - script: echo quai\n"
	failPipeline = "trigger: none\npool:\n  vmImage: ubuntu-latest\nsteps:\n  - script: exit 1\n"
)

// requireBuild makes a pipeline the build validation of pull requests to main.
func requireBuild(t *testing.T, sb *adotest.Sandbox, repositoryID string, pipeline int) {
	t.Helper()
	ctx := context.Background()
	settings := map[string]any{"buildDefinitionId": pipeline, "queueOnSourceUpdateOnly": false, "manualQueueOnly": false, "displayName": "plan",
		"validDuration": 0, "scope": []map[string]any{{"repositoryId": repositoryID, "refName": "refs/heads/main", "matchKind": "exact"}}}
	var policy struct {
		ID int `json:"id"`
	}
	if err := sb.Do(ctx, http.MethodPost, adotest.Path(sb.Project, "_apis", "policy", "configurations"), nil,
		map[string]any{"isEnabled": true, "isBlocking": true, "type": map[string]string{"id": "0609b952-1397-4640-95ec-e00a01b2c241"}, "settings": settings}, &policy); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sb.Do(context.Background(), http.MethodDelete, adotest.Path(sb.Project, "_apis", "policy", "configurations", strconv.Itoa(policy.ID)), nil, nil, nil)
	})
}

// awaitRuns waits, polling the repository as the scheduler would, until the request satisfies
// ok: hosted pipelines take minutes.
func (u *user) awaitRuns(id, what string, ok func(RequestView) bool) RequestView {
	u.h.t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for {
		r := u.read(id)
		if ok(r) {
			return r
		}
		if time.Now().After(deadline) {
			u.h.t.Fatalf("request %s: still not %s: %s, events %+v\nlog:\n%s", id, what, r.State, r.Events, u.h.log.String())
		}
		_ = u.h.ingest.Poll(context.Background(), "infra-live")
		time.Sleep(5 * time.Second)
	}
}

// apply runs an apply pipeline on main and hands Quai its run as the service hook would: Azure
// DevOps cannot reach the test.
func (h *harness) apply(sb *adotest.Sandbox, pipeline int) {
	h.t.Helper()
	ctx := context.Background()
	run, err := sb.Run(ctx, pipeline, "main")
	if err != nil {
		h.t.Fatal(err)
	}
	sb.Await(h.t, pipeline, run.ID, 10*time.Minute)
	if err := h.ingest.Process(ctx, "infra-live", []ingest.Event{{Run: &connectors.PipelineRunChanged{Run: connectors.Run{ID: strconv.Itoa(run.ID)}}}}); err != nil {
		h.t.Fatal(err)
	}
}

func (u *user) resource(id string) ResourceDetail {
	u.h.t.Helper()
	var r ResourceDetail
	if status, body := u.do(http.MethodGet, Prefix+"/resource?id="+id, nil, &r); status != http.StatusOK {
		u.h.t.Fatalf("resource %s = %d %s", id, status, body)
	}
	return r
}

// TestAzureDevOpsRuns is the end-to-end flow with runs on the sandbox, with the Azure Pipelines
// adapter (docs/design/06-runs-status-and-drift.md): a plan pipeline validates each request's
// pull request, and its plan, with the cost its quai report step sends, is on the request while
// the approvals wait and decides a floor rule on the cost; once merged, the request and its
// resource are applied by the apply pipeline's run, and failed by a failing one's. The quai
// report step, like the service hook, cannot reach the test from Azure DevOps: the test sends
// what they would. Skipped without the sandbox's PAT.
func TestAzureDevOpsRuns(t *testing.T) {
	sb := adotest.New(t)
	ctx := context.Background()
	name, id := sb.Repository(t)
	pushExample(t, sb.URL(name), func(ctx context.Context) (gitx.Credentials, error) {
		pat, _, _ := sb.PAT(ctx)
		return gitx.Credentials{Header: "Authorization: " + azuredevops.BasicAuth(pat.Reveal())}, nil
	}, sb.Network.Proxy)
	if err := sb.AddFiles(ctx, id, "main", "Pipelines", map[string]string{"plan.yml": echoPipeline, "apply.yml": echoPipeline, "apply-dev.yml": failPipeline}); err != nil {
		t.Fatal(err)
	}
	planID, applyID, failID := sb.Pipeline(t, name, id, "plan.yml"), sb.Pipeline(t, name, id, "apply.yml"), sb.Pipeline(t, name, id, "apply-dev.yml")
	h := start(t, t.TempDir(), `connector: azure-devops
  url: `+sb.URL(name)+`
  credentials: { ref: env:ADO_PAT }
  delivery: { mode: pull-request, merge: auto, mergeMethod: squash }
  enforcement:
    floorRules: [{ name: cost, when: "params.env == 'prod' && plan.cost.monthlyDelta > 500", approvers: [group:finops] }]
  sync: { minInterval: 1ns }
  runner:
    reportToken: { ref: env:INFRA_LIVE_RUN_TOKEN }
    pipelines:
      - { name: `+name+`-plan, kind: plan, stacks: "envs/{env}" }
      - { name: `+name+`-apply, kind: apply, stacks: envs/prod }
      - { name: `+name+`-apply-dev, kind: apply, stacks: envs/dev }`, repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": sandboxRunner{sb}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"azure-devops": azuredevops.New}, HTTP: sb.HTTP},
		Network:    sb.Network,
	})
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	requireApproval(t, sb, repo, id)
	requireBuild(t, sb, id, planID)
	bob, alice, fiona := h.as("bob"), h.as("alice"), h.as("fiona")

	// A production resize: its pull request's build validation is its plan, which costs 900 a
	// month; FinOps approve too.
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resizeBody(6), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	if cost := ruleNamed(r, "floor/cost"); !cost.WaitingForPlan {
		t.Fatalf("the cost rule = %+v", cost)
	}
	// The plan pipeline covers envs/{env}: its run is a plan of each stack, envs/dev and envs/prod.
	r = bob.awaitRuns(r.ID, "planned", func(v RequestView) bool {
		return len(v.Plans) == 2 && !slices.ContainsFunc(v.Plans, func(p RunView) bool { return p.Status != "succeeded" })
	})
	for _, plan := range r.Plans {
		if plan.PullRequest != r.Delivery.PullRequest || plan.URL != azuredevops.RunURL(sb.Collection, sb.Project, mustBuildID(t, plan.URL)) {
			t.Errorf("the plan = %+v", plan)
		}
		// The pipeline's quai report step reports each stack it planned: production costs 900 more.
		report := map[string]any{"kind": "plan", "stack": plan.Stack, "pullRequest": plan.PullRequest, "commit": plan.Commit, "url": plan.URL,
			"cost": map[string]any{"currency": "CHF", "monthlyDelta": 0}}
		if plan.Stack == "envs/prod" {
			report["cost"] = map[string]any{"currency": "CHF", "monthlyDelta": 900}
			report["plan"] = map[string]any{"format": "terraform-json", "content": recordedPlan(t, "prod.json")}
		}
		if status, _, body := h.reportRun(runToken, runReport(report)); status != http.StatusAccepted {
			t.Fatalf("the quai report step of %s = %d %s", plan.Stack, status, body)
		}
	}
	r = bob.read(r.ID)
	prod := slices.IndexFunc(r.Plans, func(p RunView) bool { return p.Stack == "envs/prod" })
	if cost := ruleNamed(r, "floor/cost"); r.State != "pending_approval" || cost.WaitingForPlan || cost.NotApplicable || cost.PlanError != "" ||
		prod < 0 || r.Plans[prod].Cost == nil || r.Plans[prod].Cost.MonthlyDelta != 900 || r.Plans[prod].Plan == nil {
		t.Fatalf("after the plan: %s, cost rule %+v, plans %+v", r.State, cost, r.Plans)
	}
	alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil)
	if state := bob.read(r.ID).State; state != "pending_approval" {
		t.Fatalf("approved without FinOps: %s", state)
	}
	if status, body := fiona.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("FinOps approval = %d %s", status, body)
	}
	bob.awaitRuns(r.ID, "merged", func(v RequestView) bool { return v.State == "applying" })
	h.apply(sb, applyID)
	r = bob.awaitRuns(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	if !slices.ContainsFunc(r.Events, func(e EventView) bool { return e.Kind == "applied" }) {
		t.Errorf("no applied event: %+v", r.Events)
	}
	if res := bob.resource("infra-live/vm/prod/web01"); res.State != "applied" || len(res.Applies) == 0 || res.Applies[0].Kind != "applied" {
		t.Errorf("web01: %s, applies %+v", res.State, res.Applies)
	}

	// A dev resize, approved by no one once its plan validates the pull request: its apply fails.
	var dev RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resize("dev/build-agent-01", 4), &dev); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	bob.awaitRuns(dev.ID, "merged", func(v RequestView) bool { return v.State == "applying" })
	h.apply(sb, failID)
	dev = bob.awaitRuns(dev.ID, "failed", func(v RequestView) bool { return v.State == "failed" })
	if !slices.ContainsFunc(dev.Events, func(e EventView) bool { return e.Kind == "apply_failed" }) {
		t.Errorf("no apply_failed event: %+v", dev.Events)
	}
	if res := bob.resource("infra-live/vm/dev/build-agent-01"); res.State != "failed" {
		t.Errorf("build-agent-01: %s", res.State)
	}
	if applies, _ := h.store.ListRuns(ctx, db.ListRunsParams{Repository: "infra-live", Kind: "apply", MaxRows: 10}); len(applies) != 2 {
		t.Errorf("applies = %+v", applies)
	}
}

// mustBuildID returns the run of an Azure Pipelines results page.
func mustBuildID(t *testing.T, page string) int {
	t.Helper()
	u, err := url.Parse(page)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(u.Query().Get("buildId"))
	if err != nil {
		t.Fatalf("%s: not a results page", page)
	}
	return n
}
