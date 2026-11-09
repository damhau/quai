package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/evidence"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/servicenowtest"
)

// Closing ServiceNow change requests with the evidence, and break-glass incidents checked against
// ServiceNow, on the fake instance (docs/design/05-delivery-and-approvals.md#itsm).

// evidenceName is the name of a file of a request's evidence, its token random.
func evidenceName(r RequestView, ext string) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`^quai-evidence-%d-[0-9a-f]{16}\.%s$`, r.Number, ext))
}

// checkEvidence checks the files attached to a change request: the bundle of the request without
// its proof, holding its own events only, and its PDF, as Quai recorded them.
func checkEvidence(t *testing.T, sn *servicenowtest.Server, c servicenowtest.Change, r RequestView, v *ChangeRequestView) {
	t.Helper()
	files := sn.Attachments(c.SysID)
	if len(files) != 2 || v == nil || len(v.Attachments) != 2 {
		t.Fatalf("the attachments: ServiceNow's %d, Quai's %+v", len(files), v)
	}
	for i, ext := range []string{"json", "pdf"} {
		f := files[i]
		sum := sha256.Sum256(f.Data)
		if !evidenceName(r, ext).MatchString(f.FileName) || f.Table != "change_request" || f.Record != c.SysID ||
			v.Attachments[i].Name != f.FileName || v.Attachments[i].SHA256 != hex.EncodeToString(sum[:]) || int(v.Attachments[i].Bytes) != len(f.Data) {
			t.Errorf("attachment %d: %s on %s %s, %s, Quai's %+v", i, f.FileName, f.Table, f.Record, f.ContentType, v.Attachments[i])
		}
		if bytes.Contains(f.Data, []byte(sn.ClientSecret)) {
			t.Errorf("%s holds ServiceNow's secret", f.FileName)
		}
	}
	if files[0].ContentType != "application/json" || files[1].ContentType != "application/pdf" || !bytes.HasPrefix(files[1].Data, []byte("%PDF-")) {
		t.Errorf("the content types: %s, %s", files[0].ContentType, files[1].ContentType)
	}
	b, err := evidence.Parse(bytes.NewReader(files[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	if b.Request.ID != r.ID || len(b.Proof.Segments) != 0 || !strings.Contains(b.Proof.Omitted, "attached to a change request carries no proof") || len(b.Events) == 0 {
		t.Errorf("the bundle: request %s, %d segments, omitted %q, %d events", b.Request.ID, len(b.Proof.Segments), b.Proof.Omitted, len(b.Events))
	}
	for _, e := range b.Events {
		var o struct{ ID string }
		if json.Unmarshal(e.Object, &o) != nil || o.ID != r.ID {
			t.Errorf("the bundle holds another object's event: %s %s", e.Action, e.Object)
		}
	}
	if rep := evidence.Verify(b, nil); len(rep.Failures) != 0 {
		t.Errorf("the attached bundle does not verify: %v", rep.Failures)
	}
}

// statePatches counts the state moves Quai asked of a change request.
func statePatches(sn *servicenowtest.Server, sysID string) []string {
	var out []string
	for _, c := range sn.Calls() {
		if c.Method == http.MethodPatch && c.Path == "/api/sn_chg_rest/change/"+sysID && strings.Contains(c.Body, `"state"`) {
			var in map[string]string
			_ = json.Unmarshal([]byte(c.Body), &in)
			out = append(out, in["state"])
		}
	}
	return out
}

// uploads counts the files uploaded to a change request.
func uploads(sn *servicenowtest.Server, sysID string) int {
	n := 0
	for _, c := range sn.Calls() {
		if c.Method == http.MethodPost && c.Path == "/api/now/attachment/file" && c.Query.Get("table_sys_id") == sysID {
			n++
		}
	}
	return n
}

// approveInServiceNow submits a development VM, has ServiceNow approve its change request, and
// returns the request once Quai read the approval, and its change request.
func approveInServiceNow(t *testing.T, h *harness, sn *servicenowtest.Server, name string, at time.Time) (RequestView, servicenowtest.Change) {
	t.Helper()
	r := h.as("bob").submit(newVM("dev", name, 2))
	h.awaitJobs(t)
	c := changeFor(t, sn, r, 1)
	sn.Approve(c.SysID)
	h.snSweep(at)
	return r, c
}

// TestServiceNowClosed: once the request's content is applied, Quai attaches the evidence (the
// bundle without its proof, and its PDF) to its change request, then takes it Implement, Review
// and Closed, successful, with close notes naming the files; the request's page, its timeline and
// the audit log say so. The change request is read no more. A second request's closing, its
// answers lost on the way, attaches each file once and moves each state once.
func TestServiceNowClosed(t *testing.T) {
	t.Parallel()
	sn := servicenowtest.Start(t)
	h := snHarness(t, sn, nil)
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob := h.as("bob")
	r, c := approveInServiceNow(t, h, sn, "sn-close", t0.Add(6*time.Minute))
	r = bob.await(r.ID, "applied")
	h.awaitJobs(t)
	c, _ = sn.Change(c.SysID)
	if c.State != servicenowtest.Closed || c.Fields["close_code"] != "successful" || !strings.Contains(c.Fields["close_notes"], fmt.Sprintf("request #%d was applied at ", r.Number)) ||
		!strings.Contains(c.Fields["close_notes"], "The evidence is attached: quai-evidence-") || !strings.Contains(c.Fields["close_notes"], "/requests/"+r.ID) {
		t.Errorf("closed: %+v", c)
	}
	if got := statePatches(sn, c.SysID); strings.Join(got, ",") != "assess,implement,review,closed" { // assess when it was opened
		t.Errorf("the moves = %v", got)
	}
	r = bob.read(r.ID)
	v := r.ChangeRequest
	if v == nil || v.CloseOutcome != "successful" || v.CloseResult != "closed" || v.ClosedAt == nil || v.State != "Closed" || v.Problem != "" || v.Failure != "" {
		t.Errorf("the request's change request = %+v", v)
	}
	checkEvidence(t, sn, c, r, v)
	if !hasEvent(r, "change_request_closed", c.Number) {
		t.Errorf("events = %v", h.eventKinds(r.ID))
	}
	ev, _ := h.audited("request.change_request_closed", r.ID)
	files, _ := ev[0].Details["files"].([]any)
	if ev[0].Details["outcome"] != "successful" || ev[0].Details["result"] != "closed" || ev[0].Details["sysId"] != c.SysID || len(files) != 2 {
		t.Errorf("the closing in the audit log = %+v", ev[0])
	}
	n := len(sn.Calls())
	h.snSweep(t0.Add(30 * time.Minute))
	if len(sn.Calls()) != n {
		t.Error("a closed change request is called again")
	}

	// The upload's answer is lost, then the first move's: each is found again, by the file's name
	// and the state ServiceNow reports, at the next tries.
	sn.LoseAttachments(1)
	sn.LoseMoves(1)
	lost, lc := approveInServiceNow(t, h, sn, "sn-close-lost", t0.Add(36*time.Minute))
	bob.await(lost.ID, "applied")
	h.awaitJobs(t)
	if v := bob.read(lost.ID).ChangeRequest; v.ClosedAt != nil || v.Failure == "" || len(sn.Attachments(lc.SysID)) != 1 {
		t.Fatalf("its answer lost: %+v, %d files", v, len(sn.Attachments(lc.SysID)))
	}
	h.snSweep(t0.Add(42 * time.Minute))
	h.snSweep(t0.Add(54 * time.Minute))
	lc, _ = sn.Change(lc.SysID)
	lost = bob.read(lost.ID)
	if lc.State != servicenowtest.Closed || lost.ChangeRequest.CloseResult != "closed" || lost.ChangeRequest.Failure != "" {
		t.Fatalf("after the tries: ServiceNow's %+v, Quai's %+v", lc, lost.ChangeRequest)
	}
	if got := statePatches(sn, lc.SysID); strings.Join(got, ",") != "assess,implement,review,closed" {
		t.Errorf("the moves = %v", got)
	}
	if n := uploads(sn, lc.SysID); n != 2 {
		t.Errorf("%d uploads", n)
	}
	checkEvidence(t, sn, lc, lost, lost.ChangeRequest)
}

// TestServiceNowClosedAfterApply: with a runner, the change request is closed once the apply
// reports: successful, or unsuccessful when it failed, with the evidence both times. While
// ServiceNow cannot be reached, the closing waits, its page saying why, and is tried again with a
// growing wait.
func TestServiceNowClosedAfterApply(t *testing.T) {
	t.Parallel()
	sn := servicenowtest.Start(t)
	h := snHarnessWith(t, sn, runner, nil)
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob := h.as("bob")
	head := func() string { return strings.TrimSpace(git(t, h.bare, "rev-parse", "main")) }
	apply := func(status string) {
		t.Helper()
		if code, _, body := h.reportRun(runToken, runReport(map[string]any{"stack": "envs/dev", "status": status, "commit": head()})); code != http.StatusAccepted {
			t.Fatalf("apply report = %d %s", code, body)
		}
	}

	r, c := approveInServiceNow(t, h, sn, "sn-apply", t0.Add(6*time.Minute))
	bob.await(r.ID, "applying")
	h.awaitJobs(t)
	if c, _ = sn.Change(c.SysID); c.State != servicenowtest.Scheduled {
		t.Fatalf("closed before the apply: %+v", c)
	}
	sn.Down(true)
	apply("succeeded")
	h.awaitJobs(t)
	v := bob.read(r.ID).ChangeRequest
	if v.CloseOutcome != "successful" || v.ClosedAt != nil || v.Failure == "" || v.FailingSince == nil || strings.Contains(v.Failure, sn.ClientSecret) {
		t.Fatalf("ServiceNow down: %+v", v)
	}
	calls := len(sn.Calls())
	h.snSweep(t0.Add(8 * time.Minute)) // within the wait
	if len(sn.Calls()) != calls {
		t.Error("tried again before its wait")
	}
	sn.Down(false)
	h.snSweep(t0.Add(20 * time.Minute))
	c, _ = sn.Change(c.SysID)
	r = bob.read(r.ID)
	if c.State != servicenowtest.Closed || c.Fields["close_code"] != "successful" || r.ChangeRequest.CloseResult != "closed" || r.ChangeRequest.Failure != "" {
		t.Fatalf("back: ServiceNow's %+v, Quai's %+v", c, r.ChangeRequest)
	}
	checkEvidence(t, sn, c, r, r.ChangeRequest)

	// A failed apply closes it unsuccessful, with the evidence too.
	failed, fc := approveInServiceNow(t, h, sn, "sn-apply-failed", t0.Add(26*time.Minute))
	bob.await(failed.ID, "applying")
	apply("failed")
	failed = bob.await(failed.ID, "failed")
	h.awaitJobs(t)
	fc, _ = sn.Change(fc.SysID)
	failed = bob.read(failed.ID)
	if fc.State != servicenowtest.Closed || fc.Fields["close_code"] != "unsuccessful" || !strings.Contains(fc.Fields["close_notes"], fmt.Sprintf("the apply of request #%d failed at ", failed.Number)) ||
		failed.ChangeRequest.CloseOutcome != "unsuccessful" || failed.ChangeRequest.CloseResult != "closed" {
		t.Errorf("a failed apply: ServiceNow's %+v, Quai's %+v", fc, failed.ChangeRequest)
	}
	checkEvidence(t, sn, fc, failed, failed.ChangeRequest)
	if ev, _ := h.audited("request.change_request_closed", failed.ID); ev[0].Details["outcome"] != "unsuccessful" {
		t.Errorf("the closing in the audit log = %+v", ev[0])
	}
}

// TestServiceNowCloseRefused: a move ServiceNow refuses while closing is a problem on the request
// page and a warning, not tried again; the evidence attached before stays. A change request closed
// or canceled in ServiceNow before is left as it is, the evidence attached.
func TestServiceNowCloseRefused(t *testing.T) {
	t.Parallel()
	sn := servicenowtest.Start(t)
	h := snHarness(t, sn, nil)
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob := h.as("bob")
	r := bob.submit(newVM("dev", "sn-refused", 2))
	h.awaitJobs(t)
	c := changeFor(t, sn, r, 1)
	sn.Approve(c.SysID)
	// The move refused is the first of the closing: reading the approval moves nothing.
	sn.RefuseMoves(1)
	h.snSweep(t0.Add(6 * time.Minute))
	bob.await(r.ID, "applied")
	h.awaitJobs(t)
	c, _ = sn.Change(c.SysID)
	r = bob.read(r.ID)
	v := r.ChangeRequest
	if c.State != servicenowtest.Scheduled || v.CloseResult != "refused" || v.ClosedAt == nil || !strings.Contains(v.Problem, "ServiceNow did not take "+c.Number+" to implement") {
		t.Fatalf("refused: ServiceNow's %+v, Quai's %+v", c, v)
	}
	checkEvidence(t, sn, c, r, v)
	if !strings.Contains(h.log.String(), "level=WARN msg=\"ServiceNow refused part of closing the change request\"") {
		t.Error("the refusal is not logged")
	}
	if ev, _ := h.audited("request.change_request_closed", r.ID); ev[0].Details["result"] != "refused" || ev[0].Details["problem"] == nil {
		t.Errorf("the refusal in the audit log = %+v", ev[0])
	}
	n := len(sn.Calls())
	h.snSweep(t0.Add(30 * time.Minute))
	if len(sn.Calls()) != n {
		t.Error("a refused closing is tried again")
	}
}

// TestServiceNowSupersededNotClosed: the apply of a request's new content closes the change
// request of that content only: the one of its earlier content, superseded, is neither attached to
// nor moved.
func TestServiceNowSupersededNotClosed(t *testing.T) {
	t.Parallel()
	sn := servicenowtest.Start(t)
	h, remote := giteaHarnessEnv(t, "auto", "", changeTicket("params.env == 'prod'"), nil, giteaconn.New,
		map[string]string{"SN_SECRET": sn.ClientSecret}, withServiceNow(t, sn, nil))
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob, alice := h.as("bob"), h.as("alice")
	r := opened(t, h)
	h.awaitJobs(t)
	first := changeFor(t, sn, r, 1)
	sn.Approve(first.SysID)
	h.snSweep(t0.Add(6 * time.Minute))
	if err := remote.Write(r.Delivery.Branch, "envs/prod/vms/web01.yaml", "owner: bob\ntemplate: ubuntu-24.04\ncpu: 7\nmemory_gb: 16\ndisks: [{ size_gb: 80 }]\nnetwork: { vlan: 120 }\n"); err != nil {
		t.Fatal(err)
	}
	bob.awaitDelivery(r.ID, "modified outside Quai", func(v RequestView) bool { return hasEvent(v, "modified_outside_quai", "approvals are void") })
	h.awaitJobs(t)
	r = bob.read(r.ID)
	second := changeFor(t, sn, r, 2)
	review(t, alice, r.ID, "approve")
	sn.Approve(second.SysID)
	h.snSweep(t0.Add(12 * time.Minute))
	bob.await(r.ID, "applied")
	h.awaitJobs(t)

	second, _ = sn.Change(second.SysID)
	if second.State != servicenowtest.Closed || len(sn.Attachments(second.SysID)) != 2 {
		t.Errorf("the change request of the content applied: %+v, %d files", second, len(sn.Attachments(second.SysID)))
	}
	old, _ := sn.Change(first.SysID)
	if old.State != servicenowtest.Scheduled || old.Fields["close_code"] != "" || len(sn.Attachments(first.SysID)) != 0 ||
		strings.Join(statePatches(sn, first.SysID), ",") != "assess" || len(old.WorkNotes) != 1 {
		t.Errorf("the superseded change request was touched: %+v, %d files", old, len(sn.Attachments(first.SysID)))
	}
	if v := bob.read(r.ID).ChangeRequest; v.Number != second.Number || v.CloseResult != "closed" || v.Superseded != 1 {
		t.Errorf("the request's change request = %+v", v)
	}
}

// incidentQueries counts the queries of ServiceNow's incidents.
func incidentQueries(sn *servicenowtest.Server) int {
	n := 0
	for _, c := range sn.Calls() {
		if strings.HasPrefix(c.Path, "/api/now/table/incident") {
			n++
		}
	}
	return n
}

// TestServiceNowIncidents: with ServiceNow configured, a break-glass's incident must be the number
// of one of its incidents. One whose reference is not a ServiceNow number, injection-shaped
// included, is refused before any query; one ServiceNow has not, has two of, or closed is refused;
// a known one is verified, linked to the change request (its rfc), and named with its link in the
// work note. Nothing in ServiceNow carries the reason.
func TestServiceNowIncidents(t *testing.T) {
	t.Parallel()
	sn := servicenowtest.Start(t)
	h := snHarness(t, sn, nil, withBreakGlass)
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob, dave := h.as("bob"), h.as("dave")
	known := sn.AddIncident("INC0012345", servicenowtest.IncidentInProgress)
	sn.AddIncident("INC0020000", servicenowtest.IncidentNew)
	sn.AddIncident("INC0020000", servicenowtest.IncidentOnHold)
	sn.AddIncident("INC0030000", servicenowtest.IncidentClosed)
	sn.AddIncident("INC0040000", servicenowtest.IncidentCanceled)
	r := bob.submit(newVM("dev", "sn-incident", 2))
	h.awaitJobs(t)
	c := changeFor(t, sn, r, 1)
	r = dave.read(r.ID)

	for _, ref := range []string{"INC-3001", "inc0012345", "INC0012345ORDERBYnumber", "INC0012345^ORnumber=INC0020000", "INC0012345@evil", "INC 0012345", "number=INC0012345"} {
		status, raw, _ := dave.breakGlass(r, "DNS outage in the shop", ref)
		if status != http.StatusUnprocessableEntity || !strings.Contains(raw, "incident") {
			t.Errorf("%q = %d %s", ref, status, raw)
		}
	}
	if n := incidentQueries(sn); n != 0 {
		t.Errorf("%d queries of ServiceNow's incidents for references refused", n)
	}
	for ref, why := range map[string]string{"INC0099999": "ServiceNow has no incident INC0099999", "INC0020000": "more than one incident INC0020000",
		"INC0030000": "is closed or canceled", "INC0040000": "is closed or canceled"} {
		status, raw, _ := dave.breakGlass(r, "DNS outage in the shop", ref)
		if status != http.StatusUnprocessableEntity || !strings.Contains(raw, why) {
			t.Errorf("%s = %d %s", ref, status, raw)
		}
	}
	for _, call := range sn.Calls() {
		if call.Path == "/api/now/table/incident" && !regexp.MustCompile(`^number=INC00[0-9]{5}$`).MatchString(call.Query.Get("sysparm_query")) {
			t.Errorf("an incident query: %s", call.Query.Get("sysparm_query"))
		}
	}
	if v := dave.read(r.ID); v.State != "pending_approval" || v.BreakGlass != nil {
		t.Fatalf("refused, the request is %s, %+v", v.State, v.BreakGlass)
	}

	status, raw, _ := dave.breakGlass(r, "DNS outage in the shop", "INC0012345")
	if status != http.StatusOK {
		t.Fatalf("break-glass = %d %s", status, raw)
	}
	bob.await(r.ID, "applied")
	h.awaitJobs(t)
	inc, _ := sn.Incident(known)
	r = bob.read(r.ID)
	url := sn.URL + "/incident.do?sys_id=" + known
	if bg := r.BreakGlass; inc.Change != c.SysID || bg.IncidentCheck != "verified" || bg.IncidentURL != url || bg.IncidentLink != "linked" || bg.IncidentProblem != "" {
		t.Errorf("linked: the incident %+v, the break-glass %+v", inc, r.BreakGlass)
	}
	c, _ = sn.Change(c.SysID)
	if len(c.WorkNotes) == 0 || !strings.Contains(c.WorkNotes[0], "for incident INC0012345 ("+url+")") {
		t.Errorf("the work notes = %q", c.WorkNotes)
	}
	for _, call := range sn.Calls() {
		if strings.Contains(call.Body, "DNS outage") {
			t.Errorf("the reason went to ServiceNow: %s %s", call.Method, call.Path)
		}
	}
	ev, _ := h.audited("request.break_glass", r.ID)
	if ev[0].Details["incidentCheck"] != "verified" || ev[0].Details["incidentSysId"] != known {
		t.Errorf("the break-glass in the audit log = %+v", ev[0])
	}
	if ev, _ := h.audited("request.incident_linked", r.ID); ev[0].Details["link"] != "linked" || ev[0].Details["changeRequest"] != c.Number {
		t.Errorf("the link in the audit log = %+v", ev[0])
	}
	if !hasEvent(r, "incident_linked", c.Number) {
		t.Errorf("events = %v", h.eventKinds(r.ID))
	}
}

// TestServiceNowIncidentUnverified: when ServiceNow cannot be reached, break-glass goes ahead, the
// incident not verified, so said on the request page, in its event and the audit log; Quai asks
// again, with a growing wait. When ServiceNow then says it has no such incident, the review shows
// the problem and the audit log records ServiceNow's answer; nothing is undone. An incident that
// names another change request already is kept as it is.
func TestServiceNowIncidentUnverified(t *testing.T) {
	t.Parallel()
	sn := servicenowtest.Start(t)
	h := snHarness(t, sn, nil, withBreakGlass)
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob, dave := h.as("bob"), h.as("dave")
	r := bob.submit(newVM("dev", "sn-unverified", 2))
	h.awaitJobs(t)
	r = dave.read(r.ID)
	sn.Drop("/api/now/table/incident")
	if status, raw, _ := dave.breakGlass(r, "DNS outage in the shop", "INC0077777"); status != http.StatusOK {
		t.Fatalf("break-glass, ServiceNow unreachable = %d %s", status, raw)
	}
	bob.await(r.ID, "applied")
	h.awaitJobs(t)
	r = bob.read(r.ID)
	if bg := r.BreakGlass; bg.IncidentCheck != "unverified" || bg.IncidentURL != "" || bg.IncidentLink != "" || bg.IncidentProblem != "" {
		t.Fatalf("unverified: %+v", r.BreakGlass)
	}
	if !hasEvent(r, "break_glass", `"incidentCheck":"unverified"`) {
		t.Errorf("the break-glass event: %v", r.Events)
	}
	if ev, _ := h.audited("request.break_glass", r.ID); ev[0].Details["incidentCheck"] != "unverified" || ev[0].Details["incidentSysId"] != nil {
		t.Errorf("the break-glass in the audit log = %+v", ev[0])
	}
	c := changeFor(t, sn, r, 1)
	if c, _ = sn.Change(c.SysID); len(c.WorkNotes) == 0 || !strings.Contains(c.WorkNotes[0], "for incident INC0077777 (not verified in ServiceNow yet)") {
		t.Errorf("the work notes = %q", c.WorkNotes)
	}
	asked := incidentQueries(sn)
	h.snSweep(t0.Add(30 * time.Second)) // within the wait
	if incidentQueries(sn) != asked {
		t.Error("asked again before its wait")
	}

	// ServiceNow answers: it has no such incident.
	sn.Drop("")
	h.snSweep(t0.Add(2 * time.Minute))
	r = bob.read(r.ID)
	if bg := r.BreakGlass; r.State != "applied" || bg.IncidentCheck != "refuted" || !strings.Contains(bg.IncidentProblem, "ServiceNow has no incident INC0077777") {
		t.Fatalf("refuted: %s, %+v", r.State, r.BreakGlass)
	}
	ev, _ := h.audited("request.break_glass_incident", r.ID)
	if ev[0].Actor.ID != "servicenow" || ev[0].Details["check"] != "refuted" || ev[0].Details["problem"] == nil {
		t.Errorf("the refutation in the audit log = %+v", ev[0])
	}
	asked = incidentQueries(sn)
	h.snSweep(t0.Add(3 * time.Hour))
	if incidentQueries(sn) != asked {
		t.Error("a refuted incident is asked again")
	}

	// An incident whose rfc names another change request keeps it.
	other := bob.submit(newVM("dev", "sn-kept", 2))
	h.awaitJobs(t)
	oc := changeFor(t, sn, other, 1)
	inc := sn.AddIncident("INC0088888", servicenowtest.IncidentInProgress)
	sn.SetIncident(inc, servicenowtest.IncidentInProgress, c.SysID)
	other = dave.read(other.ID)
	if status, raw, _ := dave.breakGlass(other, "Disk full", "INC0088888"); status != http.StatusOK {
		t.Fatalf("break-glass = %d %s", status, raw)
	}
	bob.await(other.ID, "applied")
	h.awaitJobs(t)
	if i, _ := sn.Incident(inc); i.Change != c.SysID || bob.read(other.ID).BreakGlass.IncidentLink != "kept" {
		t.Errorf("kept: the incident %+v, not %s, the break-glass %+v", i, oc.SysID, bob.read(other.ID).BreakGlass)
	}
}

// TestBreakGlassWithoutServiceNow: without itsm.servicenow, a break-glass's incident is any
// ticket's identifier, checked nowhere, as before.
func TestBreakGlassWithoutServiceNow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bare, work := localRemote(t, dir)
	h := start(t, dir, `connector: generic
  url: `+bare+`
  enforcement:
    floorRules: [{ name: production-floor, when: "params.env == 'prod'", approvers: [group:platform-admins] }]
  sync: { minInterval: 1ns }`, repositories.Options{}, withBreakGlass)
	h.bare, h.work = bare, work
	r := h.as("bob").submit(newVM("prod", "glass-plain", 2))
	dave := h.as("dave")
	r = dave.read(r.ID)
	status, raw, _ := dave.breakGlass(r, "Disk full", "PagerDuty#P1A2B3C")
	if status != http.StatusOK {
		t.Fatalf("break-glass = %d %s", status, raw)
	}
	h.awaitJobs(t)
	if bg := dave.read(r.ID).BreakGlass; bg == nil || bg.Incident != "PagerDuty#P1A2B3C" || bg.IncidentCheck != "" || bg.IncidentURL != "" || bg.IncidentLink != "" {
		t.Errorf("the break-glass = %+v", bg)
	}
	if ev, _ := h.audited("request.break_glass", r.ID); ev[0].Details["incidentCheck"] != nil {
		t.Errorf("the break-glass in the audit log = %+v", ev[0])
	}
}
