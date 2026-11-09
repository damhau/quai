package evidence_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"rsc.io/pdf"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/evidence"
)

// text reads a PDF's text back, page by page, a line per baseline. The reader knows no widths of
// the core fonts: it drops spaces, so the text is compared without them (squash).
func text(t *testing.T, data []byte) string {
	t.Helper()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for p := 1; p <= r.NumPage(); p++ {
		lastY := math.NaN()
		for _, g := range r.Page(p).Content().Text {
			if g.Y != lastY {
				b.WriteString("\n")
			}
			b.WriteString(g.S)
			lastY = g.Y
		}
		b.WriteString("\n")
	}
	return b.String()
}

func squash(s string) string { return strings.Join(strings.Fields(s), "") }

// hostile is what someone could type in a reason: direction overrides, controls, scripts outside
// Windows-1252, a line that does not end, many lines.
func hostile() string {
	rlo, lri, zwj := string(rune(0x202e)), string(rune(0x2066)), string(rune(0x200d))
	return "Pay " + rlo + "gnp.exe" + lri + " now\x07\x1b[31m red " + zwj + " 東京 éàü € " + strings.Repeat("A", 5000)
}

func broken() *evidence.Bundle {
	cs := plan.Hash([]plan.Change{change})
	big := plan.Change{Path: "envs/prod/vms/big.yaml", Op: plan.OpCreate, After: []byte(strings.Repeat("key: value\n", 2000))}
	return &evidence.Bundle{
		BundleVersion: evidence.Version, GeneratedAt: "2026-11-02T12:00:00.000Z", Quai: "1.2.3",
		Request: evidence.Request{ID: requestID, Number: 42, Kind: "update", Repository: "infra-live", Target: "infra-live/vm/prod/web01", Type: "vm",
			Requester: "Bob Builder", Reason: hostile(), State: "applied", Inputs: json.RawMessage(`{}`), CreatedAt: "2026-11-02T09:01:00.000Z",
			UpdatedAt: "2026-11-02T11:00:00.000Z", DefinitionsCommit: defs, Owners: []string{"team:shop"},
			Rules: []evidence.Rule{{Names: []string{"prod needs platform admins"}, Approvers: []string{"group:platform-admins"}, Min: 1, Approvals: []string{}}}},
		ChangeSets: []evidence.ChangeSet{{Repository: "infra-live", Hash: cs, Commit: commit, Changes: []evidence.Change{evidence.NewChange(change), evidence.NewChange(big),
			evidence.NewChange(plan.Change{Path: "bin/blob", Op: plan.OpCreate, After: []byte{0xff, 0xfe, 0}})}}},
		Plans: []evidence.Run{{Stack: "envs/prod", Commit: commit, Status: "succeeded", StartedAt: "2026-11-02T09:02:00.000Z",
			Summary: &evidence.Summary{Change: 1}, Policies: []evidence.Policy{{Name: "no-public-ip", Result: "pass"}},
			Resources: []evidence.PlanResource{{Resource: "infra-live/vm/prod/web01", Changes: []evidence.PlanChange{{Address: "azurerm_linux_virtual_machine.web01", Action: "update"}}}}}},
		Approvals: []evidence.Approval{},
		Delivery:  &evidence.Delivery{MergeCommit: commit, Trailers: []string{"Quai-Request: #42 0b9d5a52", "Break-glass-by: Dave Oncall", "Break-glass-incident: INC-7"}},
		Applies:   []evidence.Run{},
		BreakGlass: &evidence.BreakGlass{Breaker: "Dave Oncall", At: "2026-11-02T09:05:00.000Z", AuthTime: "2026-11-02T09:04:00Z", StepUp: true,
			Reason: "The shop is down.\n" + strings.Repeat("bgline\n", 3000), Incident: "INC-7", ChangeSet: cs, FromState: "pending_approval",
			Review: evidence.BreakGlassRev{Open: true, Needed: 2, SignOffs: []evidence.SignOff{{Signer: "Erin Auditor", Capacity: "auditor", At: "2026-11-02T10:00:00.000Z"}}}},
		Events: []audit.Record{{Seq: 7, At: "2026-11-02T09:05:00.000Z", Actor: json.RawMessage(`{"type":"user","id":"pd:` + strings.Repeat("1", 64) + `"}`),
			Action: "request.break_glass", Object: json.RawMessage(`{}`), Details: json.RawMessage(`{}`), Hash: strings.Repeat("e", 64)}},
		Personal: map[string]evidence.PersonalValue{"pd:" + strings.Repeat("1", 64): {Value: "Dave Oncall"}},
		Proof:    evidence.Proof{Segments: []evidence.Segment{}, Unproven: []evidence.Unproven{{Seq: 7, Reason: evidence.NotYet}}, Bound: evidence.MaxProofRecords},
		Notes:    []string{"The plan's values are left out."},
	}
}

// TestPDF: the summary's text reads back, hostile input drawn as plain text, cut, never a link.
func TestPDF(t *testing.T) {
	b := broken()
	data, err := evidence.PDF(b)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := evidence.PDF(b); !bytes.Equal(again, data) {
		t.Error("the same bundle makes another PDF")
	}
	got := text(t, data)
	flat := squash(got)
	for _, want := range []string{
		"Evidence bundle: request #42", "Bob Builder", "infra-live/vm/prod/web01", "applied", "-cpu: 4", "+cpu: 8",
		"the diff is cut here", "Not text: the JSON bundle has its content.", "no-public-ip", "pass", "azurerm_linux_virtual_machine.web01",
		"Merged by break-glass: its post-incident review is OPEN, 1 of 2 sign-offs.", "Break-glass-by: Dave Oncall", "Post-incident review: OPEN, 1 of 2 sign-offs", "Erin Auditor", "INC-7", "The shop is down.",
		"request.break_glass", strings.Repeat("e", 64), "Dave Oncall", "no signed checkpoint follows it yet",
		"quai audit verify --bundle", "The plan's values are left out.", "éàü €", "Pay gnp.exe now", "bgline",
	} {
		if !strings.Contains(flat, squash(want)) {
			t.Errorf("the PDF lacks %q", want)
		}
	}
	if n := strings.Count(got, "bgline"); n > 45 {
		t.Errorf("the PDF draws %d lines of a value", n)
	}
	for _, never := range []string{string(rune(0x202e)), string(rune(0x2066)), "\x1b", "東京"} {
		if strings.Contains(got, never) {
			t.Errorf("the PDF draws %q", never)
		}
	}
	if !strings.Contains(got, "??") {
		t.Error("the characters outside Windows-1252 are not shown as ?")
	}
	for _, never := range []string{"/URI", "/JavaScript", "/JS", "/Annots", "/Launch", "/OpenAction"} {
		if bytes.Contains(data, []byte(never)) {
			t.Errorf("the PDF has %s", never)
		}
	}
	if len(data) > 200<<10 {
		t.Errorf("the PDF of a hostile request is %d KiB", len(data)>>10)
	}
}

// TestPDFServiceNow: a decision of a change request in ServiceNow is ServiceNow's, not Quai's.
func TestPDFServiceNow(t *testing.T) {
	b := broken()
	b.Events = append(b.Events, audit.Record{Seq: 8, At: "2026-11-02T09:06:00.000Z", Actor: json.RawMessage(`{"type":"system","id":"servicenow"}`),
		Action: "request.rejected", Object: json.RawMessage(`{}`), Details: json.RawMessage(`{}`), Hash: strings.Repeat("f", 64)})
	data, err := evidence.PDF(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := squash(text(t, data)); !strings.Contains(got, "ServiceNow") {
		t.Error("the PDF does not name ServiceNow as the actor of its decision")
	}
}

// TestPDFBounds: a bundle of many events and a long diff still makes a PDF of bounded pages.
func TestPDFBounds(t *testing.T) {
	b := broken()
	for i := range 5000 {
		b.Events = append(b.Events, audit.Record{Seq: int64(100 + i), At: "2026-11-02T09:05:00.000Z", Actor: json.RawMessage(`{"type":"system","id":"quai"}`),
			Action: "request.reminded", Object: json.RawMessage(`{}`), Details: json.RawMessage(`{}`), Hash: fmt.Sprintf("%064x", i)})
	}
	data, err := evidence.PDF(b)
	if err != nil {
		t.Fatal(err)
	}
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if n := r.NumPage(); n > 60 {
		t.Errorf("%d pages", n)
	}
	if got := squash(text(t, data)); !strings.Contains(got, squash("and 4701 more events: the JSON bundle has them all")) {
		t.Error("the PDF does not say it leaves events out")
	}
}
