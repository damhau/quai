package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/audit"
	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/evidence"
	"github.com/damhau/quai/backend/internal/mailtest"
)

// sampleChange is one of the ten changes of the M2 audit sample (P4-22): who requests it, what,
// and who decides it: its approvers, in order, the one who denies it, or dave breaking glass.
type sampleChange struct {
	by         string
	body       map[string]any
	approvers  []string
	deny       string
	breakGlass bool
}

// sampleType builds a resource type's request bodies for the sample: a VM of a size (1 small, 2
// medium, 3 large: large enough for the type's large-vm rule), resized, or deleted.
type sampleType struct {
	create func(env, name string, size int) map[string]any
	resize func(key string, size int) map[string]any
}

// The break-glass of the sample: its reason and incident.
const sampleReason, sampleIncident = "The shop is down: the front-end needs more CPU", "INC-2204"

// sampleChanges are the sample's ten changes on four VMs, web and db in prod, ci in dev and tmp:
// three created and resized, then deleted; tmp denied; web's last resize broken by dave. Each
// needs an approval: the production rule (alice or pat, the platform admins), the large-vm rule
// (fiona, FinOps), both, or the owner of a VM someone else deletes (bob).
func sampleChanges(vm sampleType, web, db, ci, tmp string) []sampleChange {
	with := func(body map[string]any, reason string) map[string]any {
		body["reason"] = reason
		return body
	}
	remove := func(key, reason string) map[string]any {
		return map[string]any{"repository": "infra-live", "type": "vm", "kind": "delete", "key": key, "reason": reason}
	}
	return []sampleChange{
		{by: "bob", body: with(vm.create("prod", web, 1), "A front-end for the shop"), approvers: []string{"alice"}},
		{by: "carol", body: with(vm.create("prod", db, 2), "The shop's database"), approvers: []string{"pat"}},
		{by: "bob", body: with(vm.create("dev", ci, 3), "A build agent for the shop"), approvers: []string{"fiona"}},
		{by: "bob", body: with(vm.resize("prod/"+web, 2), "More traffic"), approvers: []string{"alice"}},
		{by: "carol", body: with(vm.resize("prod/"+db, 3), "Black Friday"), approvers: []string{"pat", "fiona"}},
		{by: "bob", body: with(vm.create("prod", tmp, 1), "A test VM in production"), deny: "alice"},
		{by: "bob", body: with(vm.resize("prod/"+web, 3), "Checkout is slow"), breakGlass: true},
		{by: "bob", body: remove("prod/"+web, "The front-end moves to the cluster"), approvers: []string{"alice"}},
		{by: "carol", body: remove("prod/"+db, "The database moves to the managed service"), approvers: []string{"pat"}},
		{by: "alice", body: remove("dev/"+ci, "Build agents are on the cluster now"), approvers: []string{"bob"}},
	}
}

// sampleRuns stand in for a repository's pipelines, or read them: plan returns a request whose
// pull request is open once its plan is there, applied the request once merged and applied.
type sampleRuns struct {
	plan    func(r RequestView) RequestView
	applied func(r RequestView) RequestView
}

// sampled is one change of the sample: the request once decided, and its bundle as the auditor
// downloaded it, with what verifying it found.
type sampled struct {
	change  sampleChange
	request RequestView
	bundle  *evidence.Bundle
	report  evidence.Report
}

// auditSample makes the sample's changes one after the other, each planned, decided, merged and
// applied before the next is submitted, so that each apply runs at its own request's merge commit;
// then two people sign off the break-glass review, a checkpoint is signed, and erin, an auditor of
// everything, lists the period's requests and downloads their bundles as one archive. Each bundle
// holds the reason, the diff, the plan, the approvers with their authentication times (the
// breaker's for the break-glass), the pull request's merge commit with its trailers and the apply,
// and verifies with the checkpoint's key, every event of its request proven; so does the audit
// log's export after a second checkpoint. The break-glass alert is in the alert address's inbox,
// with the incident and without the reason. With out, the key, the export and the archive's files
// are written there, for quai audit verify.
func auditSample(t *testing.T, h *harness, mail *mailtest.Server, changes []sampleChange, runs sampleRuns, out string) []sampled {
	t.Helper()
	ctx := context.Background()
	alice, dave, erin := h.as("alice"), h.as("dave"), h.as("erin")
	since := time.Now().Add(-time.Minute).UTC()
	var done []sampled
	broken := -1
	for i, c := range changes {
		u := h.as(c.by)
		r := u.submit(c.body)
		r = u.awaitDelivery(r.ID, "opened", func(v RequestView) bool {
			return v.Delivery != nil && v.Delivery.PullRequest != "" && v.Delivery.Head != ""
		})
		r = runs.plan(r)
		switch {
		case c.deny != "":
			if status, raw, _ := h.as(c.deny).reviewFor(r.ID, "", "deny"); status != http.StatusOK {
				t.Fatalf("change %d: deny = %d %s", i+1, status, raw)
			}
			r = u.read(r.ID)
		case c.breakGlass:
			if status, raw, _ := dave.breakGlass(dave.read(r.ID), sampleReason, sampleIncident); status != http.StatusOK {
				t.Fatalf("change %d: break-glass = %d %s", i+1, status, raw)
			}
			broken = i
			r = runs.applied(r)
		default:
			for _, a := range c.approvers {
				h.as(a).approve(r.ID)
			}
			r = runs.applied(r)
		}
		want := "applied"
		if c.deny != "" {
			want = "rejected"
		}
		if r.State != want {
			t.Fatalf("change %d (%s): %s, want %s: %+v", i+1, r.Target, r.State, want, r.Events)
		}
		done = append(done, sampled{change: c, request: r})
	}

	// The post-incident review: two sign-offs, neither the breaker's nor the requester's.
	if broken < 0 {
		t.Fatal("no change of the sample breaks glass")
	}
	for _, signer := range []*user{erin, alice} {
		if status, raw, _ := signer.signOff(done[broken].request.ID, "Reviewed: the incident needed it."); status != http.StatusOK {
			t.Fatalf("sign-off = %d %s", status, raw)
		}
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := audit.Keys{}
	keys.Add(pub)
	sign := func() {
		t.Helper()
		if _, _, err := audit.SignLatest(ctx, h.store.Queries, key, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	sign()

	// The auditor's sample: the period's requests, the broken and the rejected ones among them.
	until := time.Now().Add(time.Minute).UTC()
	period := "since=" + url.QueryEscape(since.Format(time.RFC3339)) + "&until=" + url.QueryEscape(until.Format(time.RFC3339))
	list := func(query string) []string {
		t.Helper()
		var page struct {
			Requests  []EvidenceRequestView
			Truncated bool
		}
		if status, raw := erin.do(http.MethodGet, Prefix+"/evidence/requests?"+query, nil, &page); status != http.StatusOK || page.Truncated {
			t.Fatalf("the sample ?%s = %d %s", query, status, raw)
		}
		var ids []string
		for _, r := range page.Requests {
			ids = append(ids, r.ID)
		}
		return ids
	}
	var ids, rejected []string
	for _, s := range done {
		ids = append(ids, s.request.ID)
		if s.change.deny != "" {
			rejected = append(rejected, s.request.ID)
		}
	}
	if got := list(period + "&repository=infra-live"); !slices.Equal(got, ids) {
		t.Errorf("the period's requests = %v, want %v", got, ids)
	}
	if got := list(period + "&breakGlass=true"); !slices.Equal(got, []string{done[broken].request.ID}) {
		t.Errorf("the broken ones = %v", got)
	}
	if got := list(period + "&state=rejected"); !slices.Equal(got, rejected) {
		t.Errorf("the rejected ones = %v", got)
	}

	// Their bundles, in one archive: each request's JSON bundle and PDF summary, and the manifest
	// with their hashes.
	status, header, data := erin.download(http.MethodPost, Prefix+"/evidence/archive", map[string]any{"requests": ids})
	if status != http.StatusOK || header.Get("Content-Type") != "application/zip" {
		t.Fatalf("archive = %d %v %s", status, header, data)
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, f := range z.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[f.Name] = b
	}
	var m manifest
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil || len(m.Files) != 2*len(ids) || len(m.LeftOut) != 0 || !m.Proof || len(files) != 2*len(ids)+1 {
		t.Fatalf("manifest = %+v (%d files), %v", m, len(files), err)
	}
	for _, f := range m.Files {
		if sum := sha256.Sum256(files[f.Name]); hex.EncodeToString(sum[:]) != f.SHA256 {
			t.Errorf("%s: its hash is not the manifest's", f.Name)
		}
	}
	for i := range done {
		s := &done[i]
		name := fmt.Sprintf("request-%d.json", s.request.Number)
		b, err := evidence.Parse(bytes.NewReader(files[name]))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s.bundle = b
		if why := sampleBundle(s.change, s.request, b); why != "" {
			t.Errorf("change %d, request #%d (%s): %s", i+1, s.request.Number, s.request.Target, why)
		}
		s.report = evidence.Verify(b, keys)
		if !s.report.OK() || s.report.Events == 0 || s.report.Proven != s.report.Events || len(s.report.Checkpoints) == 0 {
			t.Errorf("request #%d's bundle does not verify: %+v", s.request.Number, s.report)
		}
		if pdf := files[fmt.Sprintf("request-%d.pdf", s.request.Number)]; !bytes.HasPrefix(pdf, []byte("%PDF-")) {
			t.Errorf("request #%d has no PDF summary", s.request.Number)
		}
	}

	// The audit log's export verifies with the key, the chain whole and both checkpoints holding.
	sign()
	status, header, export := erin.download(http.MethodGet, Prefix+"/audit/export", nil)
	if status != http.StatusOK || header.Get("Content-Type") != "application/jsonl" {
		t.Fatalf("export = %d %v %s", status, header, export)
	}
	v := audit.NewVerifier(keys)
	if err := audit.ReadExport(bytes.NewReader(export), v); err != nil {
		t.Fatal(err)
	}
	if res := v.Result(); res.Broken != nil || res.First != 1 || res.Checkpoints != 2 || res.Events == 0 {
		t.Errorf("the export: %+v, %v", res, res.Broken)
	}

	// The alert, at once, with the incident and the request's link, without the reason.
	h.quiet(t)
	r := done[broken].request
	subject := "[Quai] Alert: break-glass on request #" + strconv.FormatInt(r.Number, 10) + " in infra-live"
	if got := inbox(t, mail, "security@example.com"); !slices.Contains(got, subject) {
		t.Errorf("the alert address's messages = %q", got)
	} else {
		for _, msg := range mail.To(t, "security@example.com") {
			if msg.Subject != subject {
				continue
			}
			full := mail.Message(t, msg.ID)
			if !strings.Contains(full.Text, sampleIncident) || !strings.Contains(full.Text, "/requests/"+r.ID) || strings.Contains(full.Text+full.HTML, sampleReason) {
				t.Errorf("the alert says %q", full.Text)
			}
		}
	}

	if out != "" {
		pem, err := audit.MarshalPublicKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		files["audit.pub"], files["export.jsonl"] = pem, export
		for name, b := range files {
			if err := os.WriteFile(filepath.Join(out, name), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return done
}

// sampleBundle says what a sampled request's bundle lacks, or "".
func sampleBundle(c sampleChange, r RequestView, b *evidence.Bundle) string {
	denied := c.deny != ""
	switch {
	case b.Request.ID != r.ID || b.Request.State != r.State:
		return fmt.Sprintf("the request %s in state %s", b.Request.ID, b.Request.State)
	case b.Request.Reason != c.body["reason"]:
		return fmt.Sprintf("the reason %q", b.Request.Reason)
	case len(b.ChangeSets) == 0 || len(b.ChangeSets[len(b.ChangeSets)-1].Changes) != 1 || len(b.ChangeSets[len(b.ChangeSets)-1].Changes[0].Diff) == 0:
		return fmt.Sprintf("the diff: %+v", b.ChangeSets)
	}
	plan := slices.IndexFunc(b.Plans, func(p evidence.Run) bool { return p.Stack == r.Stack })
	if plan < 0 || b.Plans[plan].Status != "succeeded" || b.Plans[plan].Summary == nil || b.Plans[plan].URL == "" {
		return fmt.Sprintf("the plan of %s: %+v", r.Stack, b.Plans)
	}
	switch {
	case c.breakGlass:
		bg := b.BreakGlass
		if len(b.Approvals) != 0 || bg == nil || bg.Breaker != "dave" || bg.AuthTime == "" || bg.Incident != sampleIncident || bg.Reason != sampleReason ||
			bg.Review.Open || len(bg.Review.SignOffs) != 2 {
			return fmt.Sprintf("the break-glass %+v, approvals %+v", bg, b.Approvals)
		}
	case denied:
		if len(b.Approvals) != 1 || b.Approvals[0].Reviewer != c.deny || b.Approvals[0].Decision != "deny" || b.Approvals[0].AuthTime == "" || b.BreakGlass != nil {
			return fmt.Sprintf("the denial %+v", b.Approvals)
		}
	default:
		var who []string
		for _, a := range b.Approvals {
			if a.Decision != "approve" || a.AuthTime == "" || a.Voided {
				return fmt.Sprintf("the approval %+v", a)
			}
			who = append(who, a.Reviewer)
		}
		if !slices.Equal(who, c.approvers) {
			return fmt.Sprintf("approved by %v, want %v", who, c.approvers)
		}
	}
	d := b.Delivery
	if d == nil || d.PullRequest != r.Delivery.PullRequest {
		return fmt.Sprintf("the delivery %+v", d)
	}
	if denied {
		if d.MergeCommit != "" || len(b.Applies) != 0 {
			return fmt.Sprintf("a denied request merged at %q, applied %+v", d.MergeCommit, b.Applies)
		}
		return ""
	}
	trailers := strings.Join(d.Trailers, "\n")
	if d.MergeCommit == "" || d.MergeCommit != r.Delivery.MergeCommit {
		return fmt.Sprintf("the merge commit %q, the request's %q", d.MergeCommit, r.Delivery.MergeCommit)
	}
	if c.breakGlass && !strings.Contains(trailers, "Break-glass-by: dave\nBreak-glass-incident: "+sampleIncident) {
		return fmt.Sprintf("the trailers %q", d.Trailers)
	}
	for _, a := range c.approvers {
		if !strings.Contains(trailers, "Approved-by: "+a) {
			return fmt.Sprintf("the trailers %q", d.Trailers)
		}
	}
	apply := slices.IndexFunc(b.Applies, func(p evidence.Run) bool { return p.Stack == r.Stack })
	if apply < 0 || b.Applies[apply].Status != "succeeded" || b.Applies[apply].Commit != d.MergeCommit || b.Applies[apply].URL == "" {
		return fmt.Sprintf("the apply of %s at %s: %+v", r.Stack, d.MergeCommit, b.Applies)
	}
	return ""
}

// TestAuditSample is the M2 exit on Gitea (P4-22), as the rehearsal on pilot-infra runs it
// (TestPilotInfraRehearsal): ten VM changes of the example repository, created, resized and deleted,
// one denied, one broken glass, each through its pull request, its plan and its apply reported as
// a runner would; an auditor samples the ten and downloads their bundles, which hold the reason,
// the diff, the plan, the approvers with their authentication times, the merge commit and the
// apply, and verify, as the audit log's export does; the break-glass alert reaches Mailpit.
func TestAuditSample(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h, _ := giteaHarnessOver(t, "auto", runner, nil, nil, giteaconn.New, withMail(mail, "security@example.com"), withBreakGlass)
	cpu := map[int]int{1: 2, 2: 4, 3: 12} // the example's large-vm rule: more than 8
	vm := sampleType{
		create: func(env, name string, size int) map[string]any { return newVM(env, name, cpu[size]) },
		resize: func(key string, size int) map[string]any {
			return map[string]any{"repository": "infra-live", "type": "vm", "kind": "action", "key": key, "action": "resize",
				"actionInputs": map[string]any{"cpu": cpu[size], "memory_gb": 8 * size}}
		},
	}
	reports := 0
	report := func(change map[string]any) {
		t.Helper()
		reports++
		change["url"] = fmt.Sprintf("https://ci.example.com/runs/%d", reports)
		if status, _, body := h.reportRun(runToken, runReport(change)); status != http.StatusAccepted {
			t.Fatalf("the %s report = %d %s", change["kind"], status, body)
		}
	}
	alice := h.as("alice")
	runs := sampleRuns{
		// The production stack's plan is the recorded one; the others carry their summary.
		plan: func(r RequestView) RequestView {
			plan := map[string]any{"kind": "plan", "stack": r.Stack, "pullRequest": r.Delivery.PullRequest, "commit": r.Delivery.Head}
			switch {
			case r.Stack == "envs/prod":
				plan["plan"] = map[string]any{"format": "terraform-json", "content": recordedPlan(t, "prod.json")}
			case r.Kind == "create":
				plan["summary"] = map[string]any{"add": 1, "change": 0, "destroy": 0}
			case r.Kind == "delete":
				plan["summary"] = map[string]any{"add": 0, "change": 0, "destroy": 1}
			default:
				plan["summary"] = map[string]any{"add": 0, "change": 1, "destroy": 0}
			}
			report(plan)
			return alice.read(r.ID)
		},
		applied: func(r RequestView) RequestView {
			r = alice.awaitDelivery(r.ID, "merged", func(v RequestView) bool { return v.Delivery.MergeCommit != "" && v.State == "applying" })
			report(map[string]any{"kind": "apply", "stack": r.Stack, "commit": r.Delivery.MergeCommit})
			return alice.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
		},
	}
	auditSample(t, h, mail, sampleChanges(vm, "sample-web", "sample-db", "sample-ci", "sample-tmp"), runs, "")
}
