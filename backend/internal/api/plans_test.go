package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"code.gitea.io/sdk/gitea"

	"github.com/damhau/quai/backend/internal/giteatest"
)

// costRule adds to the vm type a rule on the plan's cost
// (docs/design/05-delivery-and-approvals.md#rules-on-the-plan).
func costRule(path string, b []byte) []byte {
	if path != ".quai/types/vm.yaml" {
		return b
	}
	return []byte(strings.Replace(string(b), "  actions:\n", `      - name: cost
        when: plan.cost.monthlyDelta > 500
        approvers: [group:finops]
  actions:
`, 1))
}

const runner = "\n  runner: { reportToken: { ref: env:INFRA_LIVE_RUN_TOKEN } }"

// planOf reports a succeeded plan of a request's pull request, with a monthly cost.
func (h *harness) planOf(r RequestView, url string, monthlyDelta float64) {
	h.t.Helper()
	if status, _, body := h.reportRun(runToken, runReport(map[string]any{"kind": "plan", "pullRequest": r.Delivery.PullRequest, "url": url,
		"cost": map[string]any{"currency": "CHF", "monthlyDelta": monthlyDelta}, "plan": map[string]any{"format": "terraform-json", "content": recordedPlan(h.t, "prod.json")}})); status != http.StatusAccepted {
		h.t.Fatalf("plan report = %d %s", status, body)
	}
}

func ruleNamed(r RequestView, name string) RuleView {
	for _, rule := range r.Rules {
		if slices.Contains(rule.Names, name) {
			return rule
		}
	}
	return RuleView{}
}

// read gets a request, decoded afresh: decoding into a used RequestView would keep the fields
// the answer leaves out.
func (u *user) read(id string) RequestView {
	u.h.t.Helper()
	var r RequestView
	u.do(http.MethodGet, Prefix+"/requests/"+id, nil, &r)
	return r
}

func resize(key string, cpu int) map[string]any {
	return map[string]any{"repository": "infra-live", "type": "vm", "kind": "action", "key": key, "action": "resize",
		"actionInputs": map[string]any{"cpu": cpu, "memory_gb": 16}, "reason": "more traffic"}
}

// TestPlanRules: a rule on the plan's cost waits for the plan while approvals come in; a costly
// plan makes it apply and FinOps approve; a cheap one leaves it out and the request goes on.
func TestPlanRules(t *testing.T) {
	h, _ := giteaHarnessWith(t, "auto", runner, costRule)
	bob, alice, fiona := h.as("bob"), h.as("alice"), h.as("fiona")

	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resize("prod/web01", 6), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	if cost := ruleNamed(r, "type/cost"); !cost.WaitingForPlan || cost.When != "plan.cost.monthlyDelta > 500" || cost.Satisfied {
		t.Fatalf("the cost rule = %+v", cost)
	}
	r = bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery != nil && v.Delivery.PullRequest != "" })
	alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil)
	if state := bob.read(r.ID).State; state != "pending_approval" {
		t.Fatalf("approved before the plan: %s", state)
	}

	h.planOf(r, "https://ci.example.com/1", 900)
	r = bob.read(r.ID)
	if cost := ruleNamed(r, "type/cost"); r.State != "pending_approval" || cost.WaitingForPlan || cost.NotApplicable || cost.Satisfied {
		t.Errorf("after a costly plan: %s, %+v", r.State, cost)
	}
	if len(r.Plans) != 1 || r.Plans[0].Plan == nil || len(r.Plans[0].Plan.Resources) != 4 || r.Plans[0].Cost.MonthlyDelta != 900 {
		t.Errorf("the request's plans = %+v", r.Plans)
	}
	if !slices.ContainsFunc(r.Events, func(e EventView) bool { return e.Kind == "plan_decided" }) {
		t.Errorf("no plan_decided event: %+v", r.Events)
	}
	if status, body := fiona.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("FinOps approval = %d %s", status, body)
	}
	bob.awaitDelivery(r.ID, "merged", func(v RequestView) bool { return v.State == "applying" }) // waiting for its apply

	// A cheap plan in dev: the cost rule, the only one there, does not apply, and the request
	// goes on without an approval.
	var second RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resize("dev/build-agent-01", 4), &second); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	second = bob.awaitDelivery(second.ID, "opened", func(v RequestView) bool { return v.Delivery != nil && v.Delivery.PullRequest != "" })
	if second.State != "pending_approval" {
		t.Fatalf("before its plan, the request is %s", second.State)
	}
	h.planOf(second, "https://ci.example.com/2", 40)
	second = bob.awaitDelivery(second.ID, "merged", func(v RequestView) bool { return v.State == "applying" })
	if cost := ruleNamed(second, "type/cost"); !cost.NotApplicable || !cost.Satisfied {
		t.Errorf("the cost rule after a cheap plan = %+v", cost)
	}
}

// TestNewPlanNeedsApproval: an approved request waiting for a person to merge it goes back to
// pending approval when a new plan makes a rule apply, and quai/approval with it.
func TestNewPlanNeedsApproval(t *testing.T) {
	h, remote := giteaHarnessWith(t, "manual", runner, costRule)
	g := giteatest.Get(t)
	bob, alice, fiona := h.as("bob"), h.as("alice"), h.as("fiona")
	approval := func(want gitea.StatusState) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			pr, _, err := g.Client.GetPullRequest(remote.Owner, remote.Name, 1)
			if err == nil {
				combined, _, err := g.Client.GetCombinedStatus(remote.Owner, remote.Name, pr.Head.Sha)
				if err == nil && len(combined.Statuses) > 0 && combined.Statuses[0].State == want {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("quai/approval is not %s\nlog:\n%s", want, h.log.String())
			}
			_ = h.ingest.Poll(t.Context(), "infra-live")
			time.Sleep(300 * time.Millisecond)
		}
	}

	var r RequestView
	bob.do(http.MethodPost, Prefix+"/requests/type", resize("prod/web01", 6), &r)
	r = bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery != nil && v.Delivery.PullRequest != "" })
	alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil)
	h.planOf(r, "https://ci.example.com/1", 40)
	r = bob.awaitDelivery(r.ID, "approved", func(v RequestView) bool { return v.State == "approved" })
	approval(gitea.StatusSuccess)

	// The base moved and the plan ran again: it costs more now.
	h.planOf(r, "https://ci.example.com/2", 900)
	if r = bob.read(r.ID); r.State != "pending_approval" || !slices.ContainsFunc(r.Events, func(e EventView) bool { return e.Kind == "approval_needed" }) {
		t.Fatalf("after a costlier plan: %s, %+v", r.State, r.Events)
	}
	approval(gitea.StatusPending)
	if status, body := fiona.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("FinOps approval = %d %s", status, body)
	}
	bob.awaitDelivery(r.ID, "approved again", func(v RequestView) bool { return v.State == "approved" })
	approval(gitea.StatusSuccess)
}

// TestPlanRulesDirect: delivered by a direct commit, a request has no plan before its approval:
// a rule on the plan applies.
func TestPlanRulesDirect(t *testing.T) {
	h := newHarness(t)
	h.push(".quai/types/vm.yaml", string(costRule(".quai/types/vm.yaml", []byte(mustFile(t, h, ".quai/types/vm.yaml")))))
	bob := h.as("bob")
	bob.do(http.MethodPost, Prefix+"/repositories/infra-live/sync", nil, nil)
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resize("prod/web01", 6), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	if cost := ruleNamed(r, "type/cost"); cost.WaitingForPlan || cost.NotApplicable || cost.PlanError != "no plan comes before a direct commit" {
		t.Errorf("the cost rule = %+v", cost)
	}
}

func mustFile(t *testing.T, h *harness, path string) string {
	t.Helper()
	s, ok := h.file(path)
	if !ok {
		t.Fatalf("no %s", path)
	}
	return s
}

// TestPlanBeforeItsPullRequest: a pipeline can plan a pull request as soon as it opens, before
// Quai stores its number; the plan becomes the request's when it does, and decides its rules.
func TestPlanBeforeItsPullRequest(t *testing.T) {
	h, _ := giteaHarnessWith(t, "auto", runner, costRule)
	bob := h.as("bob")
	h.planOf(RequestView{Delivery: &DeliveryView{PullRequest: "1"}}, "https://ci.example.com/1", 40) // the repository's first

	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resize("dev/build-agent-01", 4), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	r = bob.awaitDelivery(r.ID, "merged", func(v RequestView) bool { return v.State == "applying" })
	if r.Delivery.PullRequest != "1" {
		t.Fatalf("the request's pull request is #%s, not the first", r.Delivery.PullRequest)
	}
	if cost := ruleNamed(r, "type/cost"); !cost.NotApplicable || len(r.Plans) != 1 ||
		!slices.ContainsFunc(r.Events, func(e EventView) bool { return e.Kind == "plan_reported" }) {
		t.Errorf("the early plan: cost rule %+v, plans %+v, events %+v", cost, r.Plans, r.Events)
	}
}

// TestCostAfterThePlan: an adapter records a plan from the pipeline's state, without a cost;
// the cost comes later, in a report of the same run, and decides the rules again. The same
// report again changes nothing.
func TestCostAfterThePlan(t *testing.T) {
	h, _ := giteaHarnessWith(t, "auto", runner, costRule)
	bob := h.as("bob")
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resize("dev/build-agent-01", 4), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	r = bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery != nil && v.Delivery.PullRequest != "" })
	url := "https://ci.example.com/_build/results?buildId=9"
	if status, _, body := h.reportRun(runToken, runReport(map[string]any{"kind": "plan", "pullRequest": r.Delivery.PullRequest, "url": url})); status != http.StatusAccepted {
		t.Fatalf("plan = %d %s", status, body)
	}
	if cost := ruleNamed(bob.read(r.ID), "type/cost"); cost.PlanError == "" || cost.NotApplicable {
		t.Fatalf("without a cost, the rule applies: %+v", cost)
	}
	h.planOf(r, url, 40)
	r = bob.awaitDelivery(r.ID, "merged", func(v RequestView) bool { return v.State == "applying" })
	if cost := ruleNamed(r, "type/cost"); !cost.NotApplicable || cost.PlanError != "" {
		t.Errorf("with its cost: %+v", cost)
	}
	reported := func() int {
		n := 0
		for _, e := range bob.read(r.ID).Events {
			if e.Kind == "plan_reported" {
				n++
			}
		}
		return n
	}
	before := reported()
	h.planOf(r, url, 40)
	if after := reported(); after != before {
		t.Errorf("the same report again: %d plan_reported events, then %d", before, after)
	}
}
