package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/config"
	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/engine/window"
	"github.com/damhau/quai/backend/internal/mailtest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/servicenowtest"
)

// ServiceNow change requests, on the fake instance (docs/design/05-delivery-and-approvals.md#itsm).

// changeTicket adds to the vm type a rule that an approved change request in ServiceNow
// satisfies, applying when when holds (always, for "").
func changeTicket(when string) func(path string, b []byte) []byte {
	return func(path string, b []byte) []byte {
		if path != ".quai/types/vm.yaml" {
			return b
		}
		rule := "      - name: change-ticket\n"
		if when != "" {
			rule += "        when: " + when + "\n"
		}
		rule += "        external: servicenow\n"
		return []byte(strings.Replace(string(b), "  actions:\n", rule+"  actions:\n", 1))
	}
}

// withServiceNow configures Quai with the fake instance, its OAuth application's secret at
// env:SN_SECRET, changed by more; Quai trusts the instance's certificate besides the CA file
// configured before it.
func withServiceNow(t *testing.T, sn *servicenowtest.Server, more func(*config.ServiceNow)) func(*config.Config) {
	return func(c *config.Config) {
		cfg := sn.OAuth("env:SN_SECRET")
		if more != nil {
			more(cfg)
		}
		c.ITSM.ServiceNow = cfg
		pem := sn.PEM()
		if c.Network.CAFile != "" {
			b, err := os.ReadFile(c.Network.CAFile)
			if err != nil {
				t.Fatal(err)
			}
			pem = append(append(b, '\n'), pem...)
		}
		ca := filepath.Join(t.TempDir(), "ca.pem")
		if err := os.WriteFile(ca, pem, 0o600); err != nil {
			t.Fatal(err)
		}
		c.Network.CAFile = ca
	}
}

// snHarness serves the example repository delivered by direct commits, its floor rule asking
// platform-admins for production and the vm type's change-ticket rule always applying, with Quai
// configured with the fake instance (changed by more), then by configure.
func snHarness(t *testing.T, sn *servicenowtest.Server, more func(*config.ServiceNow), configure ...func(*config.Config)) *harness {
	t.Helper()
	return snHarnessWith(t, sn, "", more, configure...)
}

// snHarnessWith is snHarness with spec added to the repository's (a runner, whose report token is
// runToken).
func snHarnessWith(t *testing.T, sn *servicenowtest.Server, spec string, more func(*config.ServiceNow), configure ...func(*config.Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	bare, work := localRemote(t, dir)
	configure = append(slices.Clone(configure), withServiceNow(t, sn, more))
	h := startRepo(t, dir, "{ name: infra-live, title: Infrastructure }", `connector: generic
  url: `+bare+`
  enforcement:
    floorRules: [{ name: production-floor, when: "params.env == 'prod'", approvers: [group:platform-admins] }]
  sync: { minInterval: 1ns }`+spec, repositories.Options{Secrets: secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) {
		if n == "INFRA_LIVE_RUN_TOKEN" {
			sum := sha256.Sum256([]byte(runToken))
			return hex.EncodeToString(sum[:]), true
		}
		return sn.ClientSecret, n == "SN_SECRET"
	}}})}, configure...)
	h.bare, h.work = bare, work
	vm, ok := h.file(".quai/types/vm.yaml")
	if !ok {
		t.Fatal("no vm type")
	}
	h.push(".quai/types/vm.yaml", string(changeTicket("")(".quai/types/vm.yaml", []byte(vm))))
	return h
}

// changesOf returns the fake's change requests of a request, in the order Quai made them.
func changesOf(sn *servicenowtest.Server, r RequestView) []servicenowtest.Change {
	var out []servicenowtest.Change
	for _, c := range sn.Changes() {
		if strings.HasPrefix(c.Fields["correlation_id"], "quai:"+r.ID+":") {
			out = append(out, c)
		}
	}
	return out
}

// changeFor returns the fake's change request Quai made for a request's generation-th content.
func changeFor(t *testing.T, sn *servicenowtest.Server, r RequestView, generation int) servicenowtest.Change {
	t.Helper()
	prefix := fmt.Sprintf("quai:%s:%d:", r.ID, generation)
	for _, c := range sn.Changes() {
		if strings.HasPrefix(c.Fields["correlation_id"], prefix) {
			return c
		}
	}
	t.Fatalf("no change request in ServiceNow for request #%d's content %d: %+v", r.Number, generation, sn.Changes())
	return servicenowtest.Change{}
}

// snSweep runs the change requests' sweep with the clock at at, as its periodic job would, once
// the jobs queued before it are done, then waits for the jobs it queued.
func (h *harness) snSweep(at time.Time) {
	h.t.Helper()
	h.awaitJobs(h.t)
	h.clock.set(at)
	if err := h.requests.SweepServiceNow(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.awaitJobs(h.t)
}

// reads counts the readings of a change request the instance received.
func reads(sn *servicenowtest.Server, sysID string) int {
	n := 0
	for _, c := range sn.Calls() {
		if c.Method == http.MethodGet && c.Path == "/api/sn_chg_rest/change/"+sysID {
			n++
		}
	}
	return n
}

// lastEvent returns a request's last event of kind.
func lastEvent(r RequestView, kind string) (EventView, bool) {
	for i := len(r.Events) - 1; i >= 0; i-- {
		if r.Events[i].Kind == kind {
			return r.Events[i], true
		}
	}
	return EventView{}, false
}

func review(t *testing.T, u *user, id, decision string) {
	t.Helper()
	if status, body := u.do(http.MethodPost, Prefix+"/requests/"+id+"/review", map[string]string{"decision": decision}, nil); status != http.StatusOK {
		t.Fatalf("%s = %d %s", decision, status, body)
	}
}

// TestServiceNowNormalChange: a rule on the plan that waits on ServiceNow opens no change request
// before its plan; the plan that makes it apply opens a normal change with the request's fields,
// taken for approval, and notes the plan's result on it. Quai's own rules still apply:
// platform-admins' approval alone does not approve the request; with ServiceNow's, read by the
// sweep, it merges through the gate, ServiceNow the audit log's actor. Merged, the change request
// is read no more.
func TestServiceNowNormalChange(t *testing.T) {
	t.Parallel()
	sn := servicenowtest.Start(t)
	h, _ := giteaHarnessEnv(t, "auto", runner, changeTicket("params.env == 'prod' && plan.cost.monthlyDelta > 500"), nil, giteaconn.New,
		map[string]string{"SN_SECRET": sn.ClientSecret}, withServiceNow(t, sn, nil))
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob, alice := h.as("bob"), h.as("alice")
	r := opened(t, h)
	h.awaitJobs(t)
	if rule := ruleNamed(r, "type/change-ticket"); !rule.WaitingForPlan || rule.External != "servicenow" || r.ChangeRequest != nil || len(sn.Changes()) != 0 {
		t.Fatalf("before its plan: the rule %+v, the change request %+v, ServiceNow's %+v", rule, r.ChangeRequest, sn.Changes())
	}

	h.planOf(r, "https://ci.example.com/sn-1", 900)
	h.awaitJobs(t)
	c := changeFor(t, sn, r, 1)
	r = bob.read(r.ID)
	if c.Type != "normal" || c.Template != "" || c.State != servicenowtest.Assess || c.Approval != "requested" {
		t.Errorf("the change request = %+v", c)
	}
	for k, want := range map[string]string{
		"short_description":   fmt.Sprintf("Quai #%d: resize on infra-live/vm/prod/web01", r.Number),
		"correlation_display": fmt.Sprintf("Quai #%d", r.Number),
		"justification":       "Black Friday traffic",
		"start_date":          "", // no change window: ServiceNow's own planning
	} {
		if c.Fields[k] != want {
			t.Errorf("%s = %q, want %q", k, c.Fields[k], want)
		}
	}
	for k, parts := range map[string][]string{
		"description": {"Requested in Quai by bob: resize on infra-live/vm/prod/web01, in infra-live.", "Type: vm.",
			fmt.Sprintf("Request #%d (%s).", r.Number, r.ID), "https://quai.example.com/requests/" + r.ID},
		"implementation_plan":  {"Change set " + r.ChangeSet + ", 1 files:", "- update envs/prod/vms/web01.yaml (+1 -1)"},
		"risk_impact_analysis": {"type/change-ticket: servicenow's approval", "1 of group:platform-admins"},
	} {
		for _, p := range parts {
			if !strings.Contains(c.Fields[k], p) {
				t.Errorf("%s = %q, without %q", k, c.Fields[k], p)
			}
		}
	}
	if len(c.WorkNotes) != 1 || !strings.HasPrefix(c.WorkNotes[0], "Quai: the plan of envs/prod succeeded at ") ||
		!strings.Contains(c.WorkNotes[0], "Cost: +900.00 CHF a month.") || !strings.Contains(c.WorkNotes[0], "/requests/"+r.ID) {
		t.Errorf("the work notes = %q", c.WorkNotes)
	}
	if v := r.ChangeRequest; v == nil || v.Number != c.Number || v.Kind != "normal" || v.State != "Assess" || v.Approval != "requested" ||
		v.Outcome != "pending" || v.URL != sn.URL+"/change_request.do?sys_id="+c.SysID || v.Failure != "" || v.Problem != "" {
		t.Errorf("the request's change request = %+v", r.ChangeRequest)
	}
	if !hasEvent(r, "change_request_opened", c.Number) {
		t.Errorf("events = %v", h.eventKinds(r.ID))
	}
	if ev, _ := h.audited("request.change_request_opened", r.ID); ev[0].Details["changeRequest"] != c.Number || ev[0].Details["sysId"] != c.SysID ||
		ev[0].Details["kind"] != "normal" || ev[0].Actor.ID != "quai" {
		t.Errorf("the opening in the audit log = %+v", ev[0])
	}

	// platform-admins approve: the request still waits for ServiceNow.
	review(t, alice, r.ID, "approve")
	if r = bob.read(r.ID); r.State != "pending_approval" || ruleNamed(r, "type/change-ticket").Satisfied || !ruleNamed(r, "type/production").Satisfied {
		t.Fatalf("approved in Quai only: %s, %+v", r.State, r.Rules)
	}

	// ServiceNow approves: the sweep reads it, and the request merges.
	sn.Approve(c.SysID)
	h.snSweep(t0.Add(6 * time.Minute))
	r = bob.awaitDelivery(r.ID, "merged", func(v RequestView) bool { return v.State == "applying" })
	if !ruleNamed(r, "type/change-ticket").Satisfied || r.ChangeRequest.Outcome != "approved" || r.ChangeRequest.State != "Scheduled" {
		t.Errorf("merged: the rule %+v, the change request %+v", ruleNamed(r, "type/change-ticket"), r.ChangeRequest)
	}
	if e, ok := lastEvent(r, "approved"); !ok || !strings.Contains(string(e.Data), c.Number) {
		t.Errorf("the approval = %+v", e)
	}
	ev, _ := h.audited("request.approved", r.ID)
	if a := ev[len(ev)-1]; a.Actor.Type != "system" || a.Actor.ID != "servicenow" || a.Details["changeRequest"] == nil || a.Details["changeSet"] != r.ChangeSet {
		t.Errorf("the approval in the audit log = %+v", a)
	}
	if kinds := h.eventKinds(r.ID); count(kinds, "approved") != 1 || count(kinds, "merged") != 1 {
		t.Errorf("events = %v", kinds)
	}

	// Merged: Quai waits on it no more.
	n := reads(sn, c.SysID)
	h.snSweep(t0.Add(20 * time.Minute))
	if reads(sn, c.SysID) != n {
		t.Error("a merged request's change request is read again")
	}
}

// TestServiceNowSuperseded: an approval is for the content it was given for. A push to the
// request's branch, while a reading of its change request is on its way, supersedes the change
// request: the reading, approved, changes nothing, a work note says it no longer counts, and a new
// one is opened for the new content. What ServiceNow says of the old one counts no more; the new
// one's approval does.
func TestServiceNowSuperseded(t *testing.T) {
	t.Parallel()
	sn := servicenowtest.Start(t)
	h, remote := giteaHarnessEnv(t, "manual", "", changeTicket("params.env == 'prod'"), nil, giteaconn.New,
		map[string]string{"SN_SECRET": sn.ClientSecret}, withServiceNow(t, sn, nil))
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob, alice := h.as("bob"), h.as("alice")
	r := opened(t, h)
	h.awaitJobs(t)
	first := changeFor(t, sn, r, 1)
	sn.Approve(first.SysID)
	review(t, alice, r.ID, "approve")
	if v := bob.read(r.ID); v.State != "pending_approval" {
		t.Fatalf("approved before ServiceNow's approval was read: %s", v.State)
	}

	// The sweep's reading of the approved change request is held while someone pushes to the
	// branch, within the request's file.
	read := reads(sn, first.SysID)
	arrived, release := sn.HoldRead()
	t.Cleanup(release)
	h.clock.set(t0.Add(6 * time.Minute))
	swept := make(chan error, 1)
	go func() { swept <- h.requests.SweepServiceNow(context.Background()) }()
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the sweep did not read the change request")
	}
	if err := remote.Write(r.Delivery.Branch, "envs/prod/vms/web01.yaml", "owner: bob\ntemplate: ubuntu-24.04\ncpu: 7\nmemory_gb: 16\ndisks: [{ size_gb: 80 }]\nnetwork: { vlan: 120 }\n"); err != nil {
		t.Fatal(err)
	}
	bob.awaitDelivery(r.ID, "modified outside Quai", func(v RequestView) bool { return hasEvent(v, "modified_outside_quai", "approvals are void") })
	release()
	if err := <-swept; err != nil {
		t.Fatal(err)
	}
	h.awaitJobs(t)

	r = bob.read(r.ID)
	second := changeFor(t, sn, r, 2)
	if r.State != "pending_approval" || r.ChangeRequest == nil || r.ChangeRequest.Number != second.Number || r.ChangeRequest.Superseded != 1 ||
		r.ChangeRequest.Outcome != "pending" {
		t.Fatalf("after the push: %s, the change request %+v", r.State, r.ChangeRequest)
	}
	if !strings.Contains(second.Fields["implementation_plan"], "Change set "+r.ChangeSet) || second.Number == first.Number {
		t.Errorf("the new change request = %+v", second)
	}
	old, _ := sn.Change(first.SysID)
	if len(old.WorkNotes) != 1 || !strings.Contains(old.WorkNotes[0], "this change request no longer counts") ||
		!strings.Contains(old.WorkNotes[0], "now change set "+r.ChangeSet[:12]) {
		t.Errorf("the old change request's work notes = %q", old.WorkNotes)
	}
	if !hasEvent(r, "change_request_superseded", first.Number) {
		t.Errorf("events = %v", h.eventKinds(r.ID))
	}
	if ev, _ := h.audited("request.change_request_superseded", r.ID); ev[0].Details["changeRequest"] != first.Number {
		t.Errorf("the supersession in the audit log = %+v", ev[0])
	}
	crs, err := h.store.ListChangeRequests(context.Background(), uuid.MustParse(r.ID))
	if err != nil || len(crs) != 2 || crs[1].SysID.String != first.SysID || !crs[1].SupersededAt.Valid || crs[1].Outcome != "pending" {
		t.Errorf("the held reading of the old change request was recorded: %+v, %v", crs, err)
	}

	// The old change request is rejected in ServiceNow: it counts no more. The new one's approval
	// and platform-admins' again approve the new content.
	sn.Reject(first.SysID)
	review(t, alice, r.ID, "approve")
	sn.Approve(second.SysID)
	h.snSweep(t0.Add(12 * time.Minute))
	if r = bob.read(r.ID); r.State != "approved" || r.ChangeRequest.Outcome != "approved" {
		t.Errorf("with the new content approved: %s, %+v", r.State, r.ChangeRequest)
	}
	if n := reads(sn, first.SysID) - read; n != 1 { // the held reading only
		t.Errorf("the old change request was read %d more times", n)
	}
}

// TestServiceNowRejected: a change request rejected or canceled in ServiceNow rejects its
// request, ServiceNow with the change's number the actor, as a review's rejection is recorded and
// notified. What Quai writes is neutral for ServiceNow: no [code] tag nor HTML gets through.
func TestServiceNowRejected(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	sn := servicenowtest.Start(t)
	h := snHarness(t, sn, nil, withMail(mail))
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob := h.as("bob")
	body := newVM("dev", "sn-rejected", 2)
	body["reason"] = "[code]<img src=x onerror=alert(1)>[/code] for the shop"
	rejected := bob.submit(body)
	cancelled := bob.submit(newVM("dev", "sn-cancelled", 2))
	h.awaitJobs(t)
	a, b := changeFor(t, sn, rejected, 1), changeFor(t, sn, cancelled, 1)
	if got := a.Fields["justification"]; got != "(code)&lt;img src=x onerror=alert(1)&gt;(/code) for the shop" {
		t.Errorf("the justification = %q", got)
	}
	sn.Reject(a.SysID)
	sn.Cancel(b.SysID)
	h.snSweep(t0.Add(6 * time.Minute))

	for _, c := range []struct {
		r       RequestView
		change  servicenowtest.Change
		outcome string
	}{{rejected, a, "rejected"}, {cancelled, b, "cancelled"}} {
		v := bob.read(c.r.ID)
		if v.State != "rejected" || v.ChangeRequest == nil || v.ChangeRequest.Outcome != c.outcome {
			t.Errorf("%s in ServiceNow: %s, %+v", c.outcome, v.State, v.ChangeRequest)
		}
		if e, ok := lastEvent(v, "rejected"); !ok || e.Actor != "ServiceNow ("+c.change.Number+")" || !strings.Contains(string(e.Data), c.outcome) {
			t.Errorf("%s: the rejection = %+v", c.outcome, e)
		}
		ev, _ := h.audited("request.rejected", c.r.ID)
		if ev[0].Actor.Type != "system" || ev[0].Actor.ID != "servicenow" || ev[0].Details["changeRequest"] == nil {
			t.Errorf("%s: the rejection in the audit log = %+v", c.outcome, ev[0])
		}
	}
	h.awaitJobs(t)
	for _, r := range []RequestView{rejected, cancelled} {
		var subject string
		for _, m := range mail.To(t, "bob@example.com") {
			if strings.HasPrefix(m.Subject, fmt.Sprintf("[Quai] Rejected: #%d ", r.Number)) {
				subject = m.ID
			}
		}
		if subject == "" {
			t.Errorf("bob heard nothing of #%d's rejection: %v", r.Number, inbox(t, mail, "bob@example.com"))
			continue
		}
		if m := mail.Message(t, subject); !strings.Contains(m.Text, "was rejected by ServiceNow (CHG") {
			t.Errorf("the rejection's message = %q", m.Text)
		}
	}
}

// TestServiceNowUnreachable: while ServiceNow cannot be reached, a submitted request waits,
// pending, its change request recorded and tried again with a growing wait; its page says why,
// an hour of it is logged as an error, and nothing is approved. When ServiceNow is back, the
// change request is opened and its approval read. A creation whose answer was lost is found by
// its correlation ID: one change request, not two.
func TestServiceNowUnreachable(t *testing.T) {
	t.Parallel()
	sn := servicenowtest.Start(t)
	h := snHarness(t, sn, nil)
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob := h.as("bob")
	sn.Down(true)
	r := bob.submit(newVM("dev", "sn-down", 2))
	h.awaitJobs(t)
	v := bob.read(r.ID)
	if c := v.ChangeRequest; v.State != "pending_approval" || c == nil || c.Number != "" || c.Outcome != "pending" || c.Failure == "" ||
		c.FailingSince == nil || !c.FailingSince.Equal(t0) || strings.Contains(c.Failure, sn.ClientSecret) {
		t.Fatalf("ServiceNow down: %s, %+v", v.State, v.ChangeRequest)
	}
	calls := len(sn.Calls())
	h.snSweep(t0.Add(30 * time.Second))
	if len(sn.Calls()) != calls {
		t.Error("tried again before its wait")
	}
	tries := 0
	for at := t0.Add(2 * time.Minute); at.Before(t0.Add(3 * time.Hour)); at = at.Add(10 * time.Minute) {
		before := len(sn.Calls())
		h.snSweep(at)
		if len(sn.Calls()) > before {
			tries++
		}
	}
	if tries < 4 || tries > 8 {
		t.Errorf("tried %d times in three hours", tries)
	}
	v = bob.read(r.ID)
	if v.State != "pending_approval" || v.ChangeRequest.Failure == "" || len(changesOf(sn, r)) != 0 {
		t.Errorf("after three hours down: %s, %+v", v.State, v.ChangeRequest)
	}
	if log := h.log.String(); !strings.Contains(log, "level=ERROR msg=\"ServiceNow has been failing for over an hour") || strings.Contains(log, sn.ClientSecret) {
		t.Error("an hour down is not logged as an error, or the secret is")
	}

	// ServiceNow is back: opened at the next try, then approved.
	sn.Down(false)
	h.snSweep(t0.Add(4 * time.Hour))
	c := changeFor(t, sn, r, 1)
	if v = bob.read(r.ID); v.ChangeRequest.Number != c.Number || v.ChangeRequest.Failure != "" || v.ChangeRequest.FailingSince != nil || v.ChangeRequest.State != "Assess" {
		t.Errorf("back: %+v", v.ChangeRequest)
	}
	sn.Approve(c.SysID)
	h.snSweep(t0.Add(4*time.Hour + 6*time.Minute))
	bob.await(r.ID, "applied")

	// A creation whose answer is lost: found again by its correlation ID at the next try.
	sn.LoseCreates(1)
	lost := bob.submit(newVM("dev", "sn-lost", 2))
	h.awaitJobs(t)
	if v := bob.read(lost.ID); v.ChangeRequest == nil || v.ChangeRequest.Number != "" || v.ChangeRequest.Failure == "" || len(changesOf(sn, lost)) != 1 {
		t.Fatalf("its answer lost: %+v, ServiceNow's %+v", v.ChangeRequest, changesOf(sn, lost))
	}
	h.snSweep(t0.Add(4*time.Hour + 10*time.Minute))
	made := changesOf(sn, lost)
	if v := bob.read(lost.ID); len(made) != 1 || v.ChangeRequest.Number != made[0].Number || v.ChangeRequest.Failure != "" || made[0].State != servicenowtest.Assess {
		t.Errorf("found again: %+v, ServiceNow's %+v", v.ChangeRequest, made)
	}
}

// TestServiceNowStandardChange: a catalog item mapped to a standard change template gets a
// standard change from it: the template's description and implementation plan kept, Quai's in a
// work note, the configured fields set. It approves the request as soon as ServiceNow reports it
// scheduled. A move ServiceNow refuses leaves the request waiting, its page saying why, until
// someone schedules the change in ServiceNow.
func TestServiceNowStandardChange(t *testing.T) {
	t.Parallel()
	sn := servicenowtest.Start(t)
	tpl := sn.AddTemplate(map[string]string{"description": "A development VM from the golden image.", "implementation_plan": "The pipeline applies it."})
	h := snHarness(t, sn, func(c *config.ServiceNow) {
		c.StandardChanges = []config.StandardChange{{Repository: "infra-live", Type: "vm", Kinds: []string{"create"}, Template: tpl}}
		c.Fields = map[string]string{"assignment_group": "Platform"}
	})
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob := h.as("bob")
	body := newVM("dev", "sn-standard", 2)
	body["reason"] = "A build agent for the shop"
	r := bob.submit(body)
	bob.await(r.ID, "applied")
	h.awaitJobs(t)
	c := changeFor(t, sn, r, 1)
	// Scheduled and approved, it merged; applied, Quai closed it.
	if c.Type != "standard" || c.Template != tpl || c.State != servicenowtest.Closed || c.Approval != "approved" || c.Fields["close_code"] != "successful" {
		t.Errorf("the standard change = %+v", c)
	}
	for k, want := range map[string]string{
		"description":         "A development VM from the golden image.",
		"implementation_plan": "The pipeline applies it.",
		"short_description":   fmt.Sprintf("Quai #%d: Create infra-live/vm/dev/sn-standard", r.Number),
		"justification":       "A build agent for the shop",
		"assignment_group":    "Platform",
	} {
		if c.Fields[k] != want {
			t.Errorf("%s = %q, want %q", k, c.Fields[k], want)
		}
	}
	if len(c.WorkNotes) != 1 || !strings.HasPrefix(c.WorkNotes[0], "Quai: Requested in Quai by bob: Create infra-live/vm/dev/sn-standard") ||
		!strings.Contains(c.WorkNotes[0], "- create envs/dev/vms/sn-standard.yaml (+") {
		t.Errorf("the work notes = %q", c.WorkNotes)
	}
	ev, _ := h.audited("request.approved", r.ID)
	if ev[0].Actor.ID != "servicenow" {
		t.Errorf("the approval in the audit log = %+v", ev[0])
	}

	// ServiceNow refuses to schedule it: the request waits, and says why.
	sn.RefuseMoves(1)
	second := bob.submit(newVM("dev", "sn-standard2", 2))
	h.awaitJobs(t)
	c = changeFor(t, sn, second, 1)
	v := bob.read(second.ID)
	if v.State != "pending_approval" || c.State != servicenowtest.New || v.ChangeRequest.Kind != "standard" ||
		!strings.Contains(v.ChangeRequest.Problem, "ServiceNow did not take "+c.Number+" to scheduled") {
		t.Fatalf("refused: %s, %+v, ServiceNow's %+v", v.State, v.ChangeRequest, c)
	}
	sn.Set(c.SysID, servicenowtest.Scheduled, "approved")
	h.snSweep(t0.Add(6 * time.Minute))
	bob.await(second.ID, "applied")
}

// TestServiceNowWithdrawn: the change request carries the planned window, when the request's
// change windows let it merge next. An approved request that waits for its window goes back to
// pending approval when ServiceNow withdraws the approval, and on when it gives it again.
func TestServiceNowWithdrawn(t *testing.T) {
	t.Parallel()
	sn := servicenowtest.Start(t)
	devHours := window.ChangeWindow{Name: "dev-hours", When: "params.env == 'dev'", Allow: []string{"Mon-Fri 08:00-17:00 Europe/Zurich"}}
	h := snHarness(t, sn, nil, withWindows(devHours))
	h.clock.set(saturday)
	bob := h.as("bob")
	r := bob.submit(newVM("dev", "sn-window", 2))
	h.awaitJobs(t)
	c := changeFor(t, sn, r, 1)
	if c.Fields["start_date"] != "2026-11-16 07:00:00" || c.Fields["end_date"] != "2026-11-16 16:00:00" {
		t.Errorf("the planned window = %q to %q", c.Fields["start_date"], c.Fields["end_date"])
	}
	sn.Approve(c.SysID)
	h.snSweep(saturday.Add(6 * time.Minute))
	held(bob, r.ID)

	sn.Set(c.SysID, servicenowtest.Assess, "requested")
	h.snSweep(saturday.Add(12 * time.Minute))
	v := bob.read(r.ID)
	if e, ok := lastEvent(v, "approval_needed"); v.State != "pending_approval" || !ok || !strings.Contains(string(e.Data), "no longer reports "+c.Number+" approved") {
		t.Fatalf("withdrawn: %s, %+v", v.State, e)
	}
	if ev, _ := h.audited("request.approval_withdrawn", r.ID); ev[0].Actor.ID != "servicenow" {
		t.Errorf("the withdrawal in the audit log = %+v", ev[0])
	}

	sn.Approve(c.SysID)
	h.snSweep(saturday.Add(18 * time.Minute))
	held(bob, r.ID)
	h.sweepWindows(monday)
	bob.await(r.ID, "applied")
}

// TestServiceNowFailsClosed: only an explicit approval of a scheduled or implementing change
// approves. A state or an approval ServiceNow does not document, an approved change under review,
// or a correlation ID that is not Quai's keep the request pending, logged.
func TestServiceNowFailsClosed(t *testing.T) {
	t.Parallel()
	sn := servicenowtest.Start(t)
	h := snHarness(t, sn, nil)
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob := h.as("bob")
	r := bob.submit(newVM("dev", "sn-closed", 2))
	h.awaitJobs(t)
	c := changeFor(t, sn, r, 1)
	at := t0
	for _, s := range []struct {
		what string
		set  func()
	}{
		{what: "approval Approved", set: func() { sn.SetRaw(c.SysID, "-2", "Approved") }},
		{what: "state 9", set: func() { sn.SetRaw(c.SysID, "9", "approved") }},
		{what: "an empty approval", set: func() { sn.SetRaw(c.SysID, "-2", " ") }},
		{what: "under review", set: func() { sn.Set(c.SysID, servicenowtest.Review, "approved") }},
		{what: "another correlation ID", set: func() {
			sn.Approve(c.SysID)
			sn.SetField(c.SysID, "correlation_id", "quai:"+r.ID+":1:0000000000000000")
		}},
	} {
		s.set()
		at = at.Add(6 * time.Minute)
		h.snSweep(at)
		if v := bob.read(r.ID); v.State != "pending_approval" || v.ChangeRequest.Outcome != "pending" {
			t.Errorf("%s: %s, %+v", s.what, v.State, v.ChangeRequest)
		}
	}
	log := h.log.String()
	if !strings.Contains(log, "a state or an approval of the change request that Quai does not know") ||
		!strings.Contains(log, "another correlation ID on the change request than Quai set") {
		t.Errorf("not logged:\n%s", log)
	}
	sn.SetField(c.SysID, "correlation_id", c.Fields["correlation_id"])
	h.snSweep(at.Add(6 * time.Minute))
	bob.await(r.ID, "applied")
}

// TestServiceNowBreakGlass: break-glass lifts the rule that waits on ServiceNow: the request
// merges, a work note records the break-glass, who and for which incident (never the reason), and
// Quai reads the change request no more. Applied, the change request, never approved, gets the
// evidence and a work note, and is left to its change manager.
func TestServiceNowBreakGlass(t *testing.T) {
	t.Parallel()
	sn := servicenowtest.Start(t)
	h := snHarness(t, sn, nil, withBreakGlass)
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob, dave := h.as("bob"), h.as("dave")
	sn.AddIncident("INC0003001", servicenowtest.IncidentInProgress)
	r := bob.submit(newVM("dev", "sn-glass", 2))
	h.awaitJobs(t)
	c := changeFor(t, sn, r, 1)
	r = dave.read(r.ID)
	if status, raw, _ := dave.breakGlass(r, "DNS outage in the shop", "INC0003001"); status != http.StatusOK {
		t.Fatalf("break-glass = %d %s", status, raw)
	}
	bob.await(r.ID, "applied")
	h.awaitJobs(t)
	c, _ = sn.Change(c.SysID)
	if len(c.WorkNotes) != 2 || !strings.Contains(c.WorkNotes[0], "glass was broken for request #") || !strings.Contains(c.WorkNotes[0], "by dave, for incident INC0003001") ||
		strings.Contains(c.WorkNotes[0], "DNS outage") || !strings.Contains(c.WorkNotes[1], "was merged by break-glass without this change request's approval") ||
		strings.Contains(c.WorkNotes[1], "DNS outage") {
		t.Errorf("the work notes = %q", c.WorkNotes)
	}
	if v := bob.read(r.ID).ChangeRequest; c.State != servicenowtest.Assess || v.CloseResult != "left" || len(v.Attachments) != 2 || len(sn.Attachments(c.SysID)) != 2 {
		t.Errorf("left to its change manager: ServiceNow's %+v, Quai's %+v", c, v)
	}
	n := reads(sn, c.SysID)
	sn.Reject(c.SysID)
	h.snSweep(t0.Add(6 * time.Minute))
	if reads(sn, c.SysID) != n || bob.read(r.ID).State != "applied" {
		t.Error("the change request of a broken request is read again")
	}
}

// TestServiceNowNotConfigured: a rule that waits on ServiceNow, without itsm.servicenow, is never
// satisfied: the request waits, its page saying why.
func TestServiceNowNotConfigured(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	vm, _ := h.file(".quai/types/vm.yaml")
	h.push(".quai/types/vm.yaml", string(changeTicket("")(".quai/types/vm.yaml", []byte(vm))))
	bob := h.as("bob")
	r := bob.submit(newVM("dev", "sn-none", 2))
	h.awaitJobs(t)
	if v := bob.read(r.ID); v.State != "pending_approval" || v.ChangeRequest == nil || !strings.Contains(v.ChangeRequest.Problem, "not configured") {
		t.Errorf("without ServiceNow: %s, %+v", v.State, v.ChangeRequest)
	}
}
