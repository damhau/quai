package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/repositories"
)

// testClock is a clock the test moves, for step-ups: no test sleeps through maxAge.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// stepUpHarness serves the example repository with auth.stepUp set to stepUp (none when nil),
// judged by the clock it returns.
func stepUpHarness(t *testing.T, stepUp *config.StepUp) (*harness, *testClock) {
	t.Helper()
	dir := t.TempDir()
	bare, work := localRemote(t, dir)
	h := start(t, dir, "connector: generic\n  url: "+bare+"\n  sync: { minInterval: 1ns }", repositories.Options{}, func(c *config.Config) {
		c.Auth.StepUp = stepUp
	})
	h.bare, h.work = bare, work
	clk := &testClock{t: time.Now().Truncate(time.Second)}
	h.auth.UseClock(clk.now)
	return h, clk
}

// problem is an RFC 9457 problem as the API answers it.
type problem struct {
	Type   string
	Status int
	Detail string
}

// stepUpRefused checks that an answer is the step-up problem, naming why.
func stepUpRefused(t *testing.T, what string, status int, body, reason string) {
	t.Helper()
	var p problem
	_ = json.Unmarshal([]byte(body), &p)
	if status != http.StatusForbidden || p.Type != auth.StepUpProblemType || p.Status != http.StatusForbidden || !strings.Contains(p.Detail, reason) {
		t.Errorf("%s = %d %s, want the step-up problem with %q", what, status, body, reason)
	}
}

// TestStepUpApprovals: when a type's approval asks for a step-up, the request's reviews count only
// from people who authenticated within maxAge, checked on the server when they review, with the
// clock the test moves; others are refused with the reason and the refusal is audited. An API
// token never steps up. Approvals without stepUp are unchanged.
func TestStepUpApprovals(t *testing.T) {
	t.Parallel()
	h, clk := stepUpHarness(t, &config.StepUp{MaxAge: 5 * time.Minute})
	vm, err := os.ReadFile(filepath.Join(h.work, ".quai/types/vm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const rules = "  approval:\n    rules:\n"
	if !strings.Contains(string(vm), rules) {
		t.Fatal("the example vm type changed its approval")
	}
	h.push(".quai/types/vm.yaml", strings.Replace(string(vm), rules, "  approval:\n    stepUp: \"params.env == 'prod'\"\n    rules:\n", 1))
	bob, alice, fiona := h.as("bob"), h.as("alice"), h.as("fiona")

	// The plan says so, before the request is submitted.
	var plan PlanView
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/type", newVM("prod", "stepup-plan", 2), &plan); status != http.StatusOK || !plan.StepUp {
		t.Errorf("prod plan = %d %s", status, body)
	}
	var devPlan PlanView
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/type", newVM("dev", "stepup-plan", 2), &devPlan); status != http.StatusOK || devPlan.StepUp {
		t.Errorf("dev plan = %d %s", status, body)
	}
	submit := func(env, name string, cpu int) RequestView {
		t.Helper()
		var r RequestView
		if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", newVM(env, name, cpu), &r); status != http.StatusCreated {
			t.Fatalf("submit %s/%s = %d %s", env, name, status, body)
		}
		return r
	}
	approved, denied, large := submit("prod", "stepup-01", 2), submit("prod", "stepup-02", 2), submit("dev", "stepup-03", 16)
	if !approved.StepUp || !denied.StepUp || large.StepUp || large.State != "pending_approval" {
		t.Fatalf("step-up of the requests: %v %v %v (%s)", approved.StepUp, denied.StepUp, large.StepUp, large.State)
	}

	// Six minutes after alice and fiona signed in.
	clk.add(6 * time.Minute)
	var me MeOutput
	if alice.do(http.MethodGet, Prefix+"/me", nil, &me.Body); me.Body.StepUp.Satisfied || me.Body.StepUp.Until != nil || me.Body.StepUp.MaxAge != 300 ||
		!strings.Contains(me.Body.StepUp.Reason, "more than 5m0s ago") {
		t.Errorf("alice's step-up = %+v", me.Body.StepUp)
	}
	status, body := alice.do(http.MethodPost, Prefix+"/requests/"+approved.ID+"/review", map[string]string{"decision": "approve"}, nil)
	stepUpRefused(t, "alice's old approval", status, body, "more than 5m0s ago")
	// A denial too (open question 50).
	status, body = alice.do(http.MethodPost, Prefix+"/requests/"+denied.ID+"/review", map[string]string{"decision": "deny", "comment": "not now"}, nil)
	stepUpRefused(t, "alice's old denial", status, body, "more than 5m0s ago")
	var r RequestView
	if alice.do(http.MethodGet, Prefix+"/requests/"+approved.ID, nil, &r); r.State != "pending_approval" || len(r.Reviews) != 0 {
		t.Errorf("after the refusal: %s %+v", r.State, r.Reviews)
	}
	ev, personal := h.audited("auth.step_up_refused", approved.ID)
	byPerson(t, ev[0], personal, "alice")
	if ev[0].Details["attempted"] != "request.review" || !strings.Contains(ev[0].Details["reason"].(string), "more than 5m0s ago") ||
		ev[0].Object.Type != "request" || ev[0].Object.Resource != "infra-live/vm/prod/stepup-01" {
		t.Errorf("refusal = %+v", ev[0])
	}

	// An API token never steps up, its owner's session however fresh.
	alice = h.as("alice")
	var tok CreatedTokenOutput
	if status, body := alice.do(http.MethodPost, Prefix+"/tokens", map[string]any{"name": "ci", "scopes": []string{"requests:write"}}, &tok.Body); status != http.StatusCreated {
		t.Fatalf("create token = %d %s", status, body)
	}
	status, body = withToken(t, h, tok.Body.Token, Prefix+"/requests/"+approved.ID+"/review", map[string]string{"decision": "approve"})
	stepUpRefused(t, "an approval with a token", status, body, "API token")

	// A request without step-up takes fiona's six-minute-old approval, which keeps it was no
	// step-up.
	if status, body := fiona.do(http.MethodPost, Prefix+"/requests/"+large.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("fiona's approval = %d %s", status, body)
	}
	if r = bob.await(large.ID, "applied"); len(r.Reviews) != 1 || r.Reviews[0].StepUp {
		t.Errorf("fiona's review = %+v", r.Reviews)
	}

	// Signed in again, alice approves; the review keeps that she had stepped up.
	if alice.do(http.MethodGet, Prefix+"/me", nil, &me.Body); !me.Body.StepUp.Satisfied || me.Body.StepUp.Until == nil || !me.Body.StepUp.Until.Equal(clk.now().Add(5*time.Minute)) {
		t.Errorf("alice's step-up after signing in again = %+v", me.Body.StepUp)
	}
	clk.add(4 * time.Minute)
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+approved.ID+"/review", map[string]string{"decision": "approve", "comment": "fresh"}, nil); status != http.StatusOK {
		t.Fatalf("alice's fresh approval = %d %s", status, body)
	}
	r = bob.await(approved.ID, "applied")
	if len(r.Reviews) != 1 || !r.Reviews[0].StepUp || r.Reviews[0].AuthTime == nil || !r.Reviews[0].AuthTime.Equal(clk.now().Add(-4*time.Minute)) {
		t.Errorf("alice's review = %+v", r.Reviews)
	}
	ev, _ = h.audited("request.reviewed", approved.ID)
	if ev[0].Details["stepUp"] != true {
		t.Errorf("reviewed = %+v", ev[0])
	}
	var stored bool
	if err := h.store.Pool.QueryRow(t.Context(), "SELECT step_up FROM reviews WHERE request_id = $1", approved.ID).Scan(&stored); err != nil || !stored {
		t.Errorf("the stored review's step_up = %v, %v", stored, err)
	}
	// Still pending: alice denies it with her fresh authentication.
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+denied.ID+"/review", map[string]string{"decision": "deny", "comment": "not now"}, nil); status != http.StatusOK {
		t.Fatalf("alice's fresh denial = %d %s", status, body)
	}
}

// TestStepUpAdmin: with auth.stepUp.admin, changing repository connections needs a step-up, on the
// server; without it, it does not.
func TestStepUpAdmin(t *testing.T) {
	t.Parallel()
	for _, admin := range []bool{true, false} {
		h, clk := stepUpHarness(t, &config.StepUp{MaxAge: 5 * time.Minute, Admin: admin})
		alice := h.as("alice")
		var me MeOutput
		if alice.do(http.MethodGet, Prefix+"/me", nil, &me.Body); me.Body.StepUp.Admin != admin {
			t.Errorf("me says admin %v, want %v", me.Body.StepUp.Admin, admin)
		}
		bare, _ := localRemote(t, t.TempDir())
		dns := document("dns", map[string]any{"connector": "generic", "url": bare, "delivery": map[string]any{"mode": "direct"}})
		if status, body := alice.do(http.MethodPost, Prefix+"/repositories", dns, nil); status != http.StatusCreated {
			t.Fatalf("admin %v: fresh connect = %d %s", admin, status, body)
		}
		clk.add(5*time.Minute + time.Second)
		changes := []struct {
			what, method, path string
			body               any
			ok                 int
		}{
			{"connect", http.MethodPost, "/repositories", document("dns2", dns["spec"].(map[string]any)), http.StatusCreated},
			{"change", http.MethodPut, "/repositories/dns", dns, http.StatusOK},
			{"generate a run token", http.MethodPost, "/repositories/dns/run-token", nil, http.StatusOK},
			{"disconnect", http.MethodDelete, "/repositories/dns", nil, http.StatusNoContent},
		}
		for _, c := range changes {
			status, body := alice.do(c.method, Prefix+c.path, c.body, nil)
			if admin {
				stepUpRefused(t, "an old "+c.what, status, body, "more than 5m0s ago")
			} else if status != c.ok {
				t.Errorf("without admin, an old %s = %d %s", c.what, status, body)
			}
		}
		if !admin {
			continue
		}
		ev, personal := h.audited("auth.step_up_refused", "dns")
		byPerson(t, ev[0], personal, "alice")
		if ev[0].Details["attempted"] != "repository.update" || ev[0].Object.Type != "repository" {
			t.Errorf("refusal = %+v", ev[0])
		}
		// Reading needs none.
		if status, body := alice.do(http.MethodGet, Prefix+"/repositories/dns", nil, nil); status != http.StatusOK {
			t.Errorf("an old read = %d %s", status, body)
		}
		alice = h.as("alice")
		for _, c := range changes {
			if status, body := alice.do(c.method, Prefix+c.path, c.body, nil); status != c.ok {
				t.Errorf("a fresh %s = %d %s", c.what, status, body)
			}
		}
	}
}
