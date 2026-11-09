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
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"rsc.io/pdf"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/evidence"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store/db"
)

// download sends a request and returns the status, the headers and the body, whatever its type.
func (u *user) download(method, path string, body any) (int, http.Header, []byte) {
	u.h.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u.h.srv.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	if u.csrf != "" {
		req.Header.Set("X-CSRF-Token", u.csrf)
	}
	resp, err := u.c.Do(req)
	if err != nil {
		u.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}

// bundleOf downloads a request's JSON bundle as u, failing the test unless it is one.
func (u *user) bundleOf(id string) *evidence.Bundle {
	u.h.t.Helper()
	status, header, raw := u.download(http.MethodGet, Prefix+"/requests/"+id+"/evidence", nil)
	if status != http.StatusOK || header.Get("Content-Type") != "application/json" || header.Get("Cache-Control") != "no-store" {
		u.h.t.Fatalf("evidence of %s = %d %v %s", id, status, header, raw)
	}
	b, err := evidence.Parse(bytes.NewReader(raw))
	if err != nil {
		u.h.t.Fatalf("evidence of %s: %v", id, err)
	}
	return b
}

// pdfText reads a PDF's text back: the reader knows no widths of the core fonts and drops spaces,
// so the text comes without them.
func pdfText(t *testing.T, data []byte) string {
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
	}
	return strings.Join(strings.Fields(b.String()), "")
}

// evidenceHarness serves the example repository delivered by direct commits with a runner, its
// floor rule asking platform-admins for production, with the break-glass users (dave breaks glass,
// erin audits everything), rita an auditor of infra-live and otto one of another repository.
func evidenceHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	bare, work := localRemote(t, dir)
	h := startRepo(t, dir, "{ name: infra-live, title: Infrastructure }", `connector: generic
  url: `+bare+`
  enforcement:
    floorRules: [{ name: production-floor, when: "params.env == 'prod'", approvers: [group:platform-admins] }]
  sync: { minInterval: 1ns }
  runner: { reportToken: { ref: env:INFRA_LIVE_RUN_TOKEN } }`, repositories.Options{Secrets: secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(name string) (string, bool) {
		sum := sha256.Sum256([]byte(runToken))
		return hex.EncodeToString(sum[:]), name == "INFRA_LIVE_RUN_TOKEN"
	}}})}, withBreakGlass, func(c *config.Config) {
		c.Auth.Dev.Users = append(c.Auth.Dev.Users, config.DevUser{Name: "rita", Groups: []string{"live-audit"}}, config.DevUser{Name: "otto", Groups: []string{"dev-audit"}})
		c.Auth.Roles = append(c.Auth.Roles, config.RoleMapping{Group: "live-audit", Role: "auditor", Scope: "repository:infra-live"},
			config.RoleMapping{Group: "dev-audit", Role: "auditor", Scope: "repository:infra-dev"})
	})
	h.bare, h.work = bare, work
	return h
}

// TestEvidenceBundles: the bundles of requests applied, failed, rejected, approved on someone's
// behalf and broken by break-glass verify offline with the checkpoints' key, their summaries
// matching their events, and fail once edited; the PDF summary reads back. Auditors of everything
// and platform admins get the proof; a requester, an auditor of the repository and an API token get
// none; others nothing. Each download is audited.
func TestEvidenceBundles(t *testing.T) {
	t.Parallel()
	h := evidenceHarness(t)
	ctx := context.Background()
	bob, alice, carol, dave, erin := h.as("bob"), h.as("alice"), h.as("carol"), h.as("dave"), h.as("erin")
	head := func() string { return strings.TrimSpace(git(t, h.bare, "rev-parse", "main")) }
	runs := 0
	apply := func(stack, status string) {
		t.Helper()
		runs++
		if code, _, body := h.reportRun(runToken, runReport(map[string]any{"stack": stack, "status": status, "commit": head(), "url": fmt.Sprintf("https://ci.example.com/%d", runs)})); code != http.StatusAccepted {
			t.Fatalf("apply report = %d %s", code, body)
		}
	}

	// Applied: approved by alice, applied by the runner, with a plan of its own: its delivery made
	// a pull request merged at the commit, as one would be, for the plan to find it.
	applied := bob.submit(newVM("prod", "ev01", 2))
	alice.approve(applied.ID)
	bob.await(applied.ID, "applying")
	apply("envs/prod", "succeeded")
	bob.await(applied.ID, "applied")
	if _, err := h.store.PutDelivery(ctx, db.PutDeliveryParams{RequestID: uuid.MustParse(applied.ID), Repository: "infra-live", Branch: "quai/r-ev01", PullRequest: "41"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetDeliveryMerged(ctx, db.SetDeliveryMergedParams{RequestID: uuid.MustParse(applied.ID), MergeCommit: head()}); err != nil {
		t.Fatal(err)
	}
	h.reportRun(runToken, runReport(map[string]any{"kind": "plan", "stack": "envs/prod", "commit": head(), "pullRequest": "41",
		"summary": map[string]any{"add": 1, "change": 0, "destroy": 0}, "policies": []map[string]any{{"name": "no-public-ip", "result": "passed"}}}))

	// Failed: approved by no rule, its apply failed.
	failed := bob.submit(newVM("dev", "ev02", 2))
	bob.await(failed.ID, "applying")
	apply("envs/dev", "failed")
	bob.await(failed.ID, "failed")

	// Rejected by alice.
	rejected := bob.submit(newVM("prod", "ev03", 2))
	if status, raw, _ := alice.reviewFor(rejected.ID, "", "deny"); status != http.StatusOK {
		t.Fatalf("deny = %d %s", status, raw)
	}

	// Approved by carol on alice's behalf.
	delegation := alice.mustDelegate("carol", time.Now(), time.Now().Add(7*24*time.Hour))
	delegated := bob.submit(newVM("prod", "ev04", 2))
	if status, raw, _ := carol.reviewFor(delegated.ID, "alice", "approve"); status != http.StatusOK {
		t.Fatalf("approve on alice's behalf = %d %s", status, raw)
	}
	bob.await(delegated.ID, "applying")
	apply("envs/prod", "succeeded")
	bob.await(delegated.ID, "applied")

	// Broken by dave, its review open.
	broken := bob.submit(newVM("prod", "ev05", 2))
	if status, raw, _ := dave.breakGlass(dave.read(broken.ID), "The shop is down: café "+string([]rune{0x6771, 0x4eac}), "INC-4242"); status != http.StatusOK {
		t.Fatalf("break-glass = %d %s", status, raw)
	}
	bob.await(broken.ID, "applying")
	apply("envs/prod", "succeeded")
	bob.await(broken.ID, "applied")

	// Nothing proves an event before a checkpoint follows it.
	if b := erin.bundleOf(applied.ID); len(b.Proof.Segments) != 0 || len(b.Proof.Unproven) != len(b.Events) || b.Proof.Unproven[0].Reason != evidence.NoCheckpoint {
		t.Errorf("the proof without a checkpoint = %+v", b.Proof)
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

	verified := func(name string, b *evidence.Bundle) evidence.Report {
		t.Helper()
		r := evidence.Verify(b, keys)
		if !r.OK() || r.Proven != r.Events || r.Events == 0 {
			t.Errorf("the %s bundle: %+v", name, r)
		}
		return r
	}
	for _, c := range []struct {
		name, id, state string
		check           func(b *evidence.Bundle) string
	}{
		{"applied", applied.ID, "applied", func(b *evidence.Bundle) string {
			switch {
			case len(b.Approvals) != 1 || b.Approvals[0].Reviewer != "alice" || b.Approvals[0].AuthTime == "":
				return fmt.Sprintf("approvals %+v", b.Approvals)
			case len(b.Plans) != 1 || b.Plans[0].Summary == nil || b.Plans[0].Summary.Add != 1 || len(b.Plans[0].Policies) != 1:
				return fmt.Sprintf("plans %+v", b.Plans)
			case len(b.Applies) == 0 || b.Applies[0].Status != "succeeded":
				return fmt.Sprintf("applies %+v", b.Applies)
			case b.Delivery == nil || b.Delivery.MergeCommit == "" || !strings.Contains(strings.Join(b.Delivery.Trailers, "\n"), "Approved-by: alice"):
				return fmt.Sprintf("delivery %+v", b.Delivery)
			case b.Request.Reason != "" && b.Personal == nil:
				return "no personal values"
			}
			return ""
		}},
		{"failed", failed.ID, "failed", func(b *evidence.Bundle) string {
			if len(b.Approvals) != 0 || b.Delivery == nil || b.Delivery.MergeCommit == "" || len(b.Applies) == 0 || b.Applies[0].Status != "failed" {
				return fmt.Sprintf("approvals %+v, delivery %+v, applies %+v", b.Approvals, b.Delivery, b.Applies)
			}
			return ""
		}},
		{"rejected", rejected.ID, "rejected", func(b *evidence.Bundle) string {
			if len(b.Approvals) != 1 || b.Approvals[0].Decision != "deny" || b.Approvals[0].Comment != "not this week" || b.Delivery != nil {
				return fmt.Sprintf("approvals %+v, delivery %+v", b.Approvals, b.Delivery)
			}
			return ""
		}},
		{"delegated", delegated.ID, "applied", func(b *evidence.Bundle) string {
			a := b.Approvals
			if len(a) != 1 || a[0].Reviewer != "carol" || a[0].OnBehalfOf != "alice" || a[0].Delegation == nil || a[0].Delegation.ID != delegation.ID ||
				a[0].Delegation.Delegator != "alice" || a[0].Delegation.Delegate != "carol" {
				return fmt.Sprintf("approvals %+v", a)
			}
			return ""
		}},
		{"broken", broken.ID, "applied", func(b *evidence.Bundle) string {
			bg := b.BreakGlass
			switch {
			case bg == nil || bg.Breaker != "dave" || bg.Incident != "INC-4242" || bg.FromState != "pending_approval" || !bg.Review.Open || bg.Review.Needed != 2:
				return fmt.Sprintf("break-glass %+v", bg)
			case !strings.Contains(strings.Join(b.Notes, "\n"), "post-incident review is open: 0 of 2 sign-offs"):
				return fmt.Sprintf("notes %q", b.Notes)
			case !strings.Contains(strings.Join(b.Delivery.Trailers, "\n"), "Break-glass-by: dave\nBreak-glass-incident: INC-4242"):
				return fmt.Sprintf("trailers %q", b.Delivery.Trailers)
			}
			return ""
		}},
	} {
		b := erin.bundleOf(c.id)
		if b.Request.State != c.state {
			t.Errorf("the %s bundle's state = %s", c.name, b.Request.State)
		}
		if why := c.check(b); why != "" {
			t.Errorf("the %s bundle: %s", c.name, why)
		}
		r := verified(c.name, b)
		if len(r.Checkpoints) == 0 || r.Records == 0 {
			t.Errorf("the %s bundle proves nothing with other records: %+v", c.name, r)
		}
		// One byte of an event, and the bundle fails.
		e := &b.Events[len(b.Events)-1]
		e.Details = bytes.Replace(e.Details, []byte(`"`), []byte(`"x`), 2)
		if r := evidence.Verify(b, keys); r.OK() {
			t.Errorf("the %s bundle verifies once edited", c.name)
		}
	}
	// The bundle discloses none of the other requests' personal values, nor anyone's address.
	_, _, raw := erin.download(http.MethodGet, Prefix+"/requests/"+broken.ID+"/evidence", nil)
	for _, leak := range []string{"127.0.0.1", "not this week", "@example.com"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Errorf("the broken request's bundle discloses %q", leak)
		}
	}

	// The review closed by two sign-offs, the bundle says so, its events proven after the next
	// checkpoint.
	for _, signer := range []*user{erin, alice} {
		if status, raw, _ := signer.signOff(broken.ID, "Reviewed."); status != http.StatusOK {
			t.Fatalf("sign-off = %d %s", status, raw)
		}
	}
	b := erin.bundleOf(broken.ID)
	if r := evidence.Verify(b, keys); !r.OK() || len(r.Unproven) != 3 || !strings.Contains(r.Unproven[0].Reason, "no signed checkpoint follows it yet") {
		t.Errorf("the closed review before a checkpoint: %+v", r)
	}
	sign()
	b = erin.bundleOf(broken.ID)
	if b.BreakGlass.Review.Open || len(b.BreakGlass.Review.SignOffs) != 2 || strings.Contains(strings.Join(b.Notes, "\n"), "review is open") {
		t.Errorf("the closed review = %+v, notes %q", b.BreakGlass.Review, b.Notes)
	}
	verified("broken and reviewed", b)
	b.BreakGlass.Review.SignOffs = b.BreakGlass.Review.SignOffs[:1]
	if r := evidence.Verify(b, keys); r.OK() || !strings.Contains(strings.Join(r.Failures, "\n"), "sign-offs") {
		t.Errorf("a sign-off removed from the summary: %+v", r.Failures)
	}

	// The PDF summary, read back.
	status, header, data := erin.download(http.MethodGet, Prefix+"/requests/"+broken.ID+"/evidence?format=pdf", nil)
	if status != http.StatusOK || header.Get("Content-Type") != "application/pdf" ||
		header.Get("Content-Disposition") != fmt.Sprintf(`attachment; filename="quai-request-%d-evidence.pdf"`, broken.Number) {
		t.Fatalf("PDF = %d %v", status, header)
	}
	text := pdfText(t, data)
	for _, want := range []string{fmt.Sprintf("Evidencebundle:request#%d", broken.Number), "Brokenbydave", "IncidentINC-4242", "Theshopisdown:café??",
		"Post-incidentreview:closed", "Signedoffbyerin", "Break-glass-by:dave", "request.break_glass", "request.submitted", "quaiauditverify--bundle"} {
		if !strings.Contains(text, want) {
			t.Errorf("the PDF lacks %q", want)
		}
	}
	if strings.Contains(text, string([]rune{0x6771, 0x4eac})) {
		t.Error("the PDF draws characters its fonts do not have")
	}

	// Who downloads, and what they get.
	if b := bob.bundleOf(applied.ID); len(b.Proof.Segments) != 0 || b.Proof.Omitted != proofForAuditors {
		t.Errorf("the requester's bundle carries a proof: %+v", b.Proof)
	} else if r := evidence.Verify(b, nil); !r.OK() || r.Proven != 0 {
		t.Errorf("the requester's bundle: %+v", r)
	}
	if b := h.as("rita").bundleOf(applied.ID); b.Proof.Omitted != proofForAuditors {
		t.Errorf("a repository auditor's bundle carries a proof: %+v", b.Proof)
	}
	if b := alice.bundleOf(applied.ID); len(b.Proof.Segments) == 0 {
		t.Errorf("a platform admin's bundle carries no proof: %+v", b.Proof)
	}
	for _, who := range []string{"fiona", "otto", ""} {
		want := http.StatusForbidden
		if who == "" {
			want = http.StatusUnauthorized
		}
		if status, _, raw := h.as(who).download(http.MethodGet, Prefix+"/requests/"+applied.ID+"/evidence", nil); status != want {
			t.Errorf("%q downloads = %d %s", who, status, raw)
		}
	}
	if status, _, _ := erin.download(http.MethodGet, Prefix+"/requests/"+uuid.NewString()+"/evidence", nil); status != http.StatusNotFound {
		t.Errorf("an unknown request's evidence = %d", status)
	}
	if status, _, _ := erin.download(http.MethodGet, Prefix+"/requests/"+applied.ID+"/evidence?format=docx", nil); status != http.StatusUnprocessableEntity {
		t.Errorf("an unknown format = %d", status)
	}
	if v := bob.read(applied.ID); !v.CanDownloadEvidence {
		t.Error("the requester is not offered the download")
	}
	if v := h.as("fiona").read(applied.ID); v.CanDownloadEvidence {
		t.Error("fiona is offered the download")
	}
	token := erin.token("evidence", "evidence:read")
	status, raw2 := bearer(t, h, token, http.MethodGet, Prefix+"/requests/"+applied.ID+"/evidence", nil)
	if tb, err := evidence.Parse(strings.NewReader(raw2)); status != http.StatusOK || err != nil || tb.Proof.Omitted != proofForToken {
		t.Errorf("a token's download = %d, %v", status, err)
	}
	if status, _ := bearer(t, h, bob.token("requests", "requests:read"), http.MethodGet, Prefix+"/requests/"+applied.ID+"/evidence", nil); status != http.StatusForbidden {
		t.Errorf("a requests:read token's download = %d", status)
	}
	// The downloads are in the audit log: by whom, of what, with the proof or not.
	ev, personal := h.audited("evidence.downloaded", applied.ID)
	byWho := map[string]bool{}
	for _, e := range ev {
		byWho[personal[e.Actor.ID]+fmt.Sprint(e.Details["proof"], e.Details["format"])] = true
	}
	for _, want := range []string{"erintruejson", "bobfalsejson", "alicetruejson", "erinfalsejson"} {
		if !byWho[want] {
			t.Errorf("no download %s in the audit log: %v", want, byWho)
		}
	}
}

// TestEvidenceSample: an auditor lists a period's requests by repository, state and break-glass,
// and downloads their bundles as one archive, of generated names, a manifest with their hashes; the
// filters and the archive are bounded, two archives made at once.
func TestEvidenceSample(t *testing.T) {
	t.Parallel()
	h := evidenceHarness(t)
	bob, alice, dave, erin := h.as("bob"), h.as("alice"), h.as("dave"), h.as("erin")
	applied := bob.submit(newVM("dev", "s01", 2))
	bob.await(applied.ID, "applying")
	rejected := bob.submit(newVM("prod", "s02", 2))
	if status, raw, _ := alice.reviewFor(rejected.ID, "", "deny"); status != http.StatusOK {
		t.Fatalf("deny = %d %s", status, raw)
	}
	broken := bob.submit(newVM("prod", "s03", 2))
	if status, raw, _ := dave.breakGlass(dave.read(broken.ID), "The shop is down.", "INC-7"); status != http.StatusOK {
		t.Fatalf("break-glass = %d %s", status, raw)
	}
	since, until := time.Now().Add(-time.Hour).UTC(), time.Now().Add(time.Hour).UTC()
	list := func(u *user, query string) (int, []EvidenceRequestView) {
		t.Helper()
		var out struct {
			Requests  []EvidenceRequestView
			Truncated bool
		}
		status, _ := u.do(http.MethodGet, Prefix+"/evidence/requests?"+query, nil, &out)
		return status, out.Requests
	}
	period := "since=" + url.QueryEscape(since.Format(time.RFC3339)) + "&until=" + url.QueryEscape(until.Format(time.RFC3339))
	ids := func(rs []EvidenceRequestView) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return out
	}
	if status, rs := list(erin, period); status != http.StatusOK || strings.Join(ids(rs), " ") != strings.Join([]string{applied.ID, rejected.ID, broken.ID}, " ") ||
		rs[0].Repository != "infra-live" || !rs[2].BreakGlass {
		t.Errorf("the period's requests = %d %+v", status, rs)
	}
	if status, rs := list(erin, period+"&state=rejected&repository=infra-live"); status != http.StatusOK || len(rs) != 1 || rs[0].ID != rejected.ID {
		t.Errorf("the rejected ones = %d %+v", status, rs)
	}
	if status, rs := list(erin, period+"&breakGlass=true"); status != http.StatusOK || len(rs) != 1 || rs[0].ID != broken.ID {
		t.Errorf("the broken ones = %d %+v", status, rs)
	}
	if status, rs := list(h.as("rita"), period+"&repository=infra-live"); status != http.StatusOK || len(rs) != 3 {
		t.Errorf("a repository auditor's sample = %d %+v", status, rs)
	}
	for _, c := range []struct {
		who, query string
		status     int
	}{
		{"rita", period, http.StatusForbidden},
		{"bob", period, http.StatusForbidden},
		{"erin", period + "&repository=elsewhere", http.StatusUnprocessableEntity},
		{"erin", period + "&state=lost", http.StatusUnprocessableEntity},
		{"erin", "since=" + url.QueryEscape(until.Format(time.RFC3339)) + "&until=" + url.QueryEscape(since.Format(time.RFC3339)), http.StatusUnprocessableEntity},
		{"erin", "since=2020-01-01T00:00:00Z&until=2026-01-01T00:00:00Z", http.StatusUnprocessableEntity},
		{"erin", "until=2026-01-01T00:00:00Z", http.StatusUnprocessableEntity},
		{"erin", period + "&limit=501", http.StatusUnprocessableEntity},
	} {
		if status, _ := list(h.as(c.who), c.query); status != c.status {
			t.Errorf("%s lists ?%s = %d, want %d", c.who, c.query, status, c.status)
		}
	}
	if _, rs := list(erin, period+"&limit=2"); len(rs) != 2 {
		t.Errorf("a limit of 2 = %d requests", len(rs))
	}

	// The archive: each request's bundle and summary, named by its number, and the manifest.
	sample := map[string]any{"requests": []string{applied.ID, rejected.ID, broken.ID}}
	status, header, data := erin.download(http.MethodPost, Prefix+"/evidence/archive", sample)
	if status != http.StatusOK || header.Get("Content-Type") != "application/zip" || !strings.HasPrefix(header.Get("Content-Disposition"), `attachment; filename="quai-evidence-`) {
		t.Fatalf("archive = %d %v %s", status, header, data)
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	files := map[string][]byte{}
	for _, f := range z.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		files[f.Name] = b
	}
	if err := json.Unmarshal(files["manifest.json"], &m); err != nil || len(m.Files) != 6 || len(m.LeftOut) != 0 || !m.Proof {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	for _, f := range m.Files {
		sum := sha256.Sum256(files[f.Name])
		if hex.EncodeToString(sum[:]) != f.SHA256 || f.Name != fmt.Sprintf("request-%d.%s", f.Number, f.Name[strings.LastIndex(f.Name, ".")+1:]) {
			t.Errorf("file %+v", f)
		}
		if strings.HasSuffix(f.Name, ".json") {
			b, err := evidence.Parse(bytes.NewReader(files[f.Name]))
			if err != nil || b.Request.ID != f.Request {
				t.Errorf("%s: %v", f.Name, err)
			} else if r := evidence.Verify(b, audit.Keys{}); !r.OK() {
				t.Errorf("%s: %v", f.Name, r.Failures)
			}
		}
	}
	if len(z.File) != 7 {
		t.Errorf("%d files in the archive", len(z.File))
	}
	if ev, _ := h.audited("evidence.downloaded", "archive"); len(ev) != 1 || ev[0].Details["count"] != float64(3) || ev[0].Details["format"] != "zip" {
		t.Errorf("the archive's audit event = %+v", ev)
	}

	// Bounded: 50 requests at most, an auditor's, known ones, two archives at once.
	many := make([]string, 51)
	for i := range many {
		many[i] = uuid.NewString()
	}
	for _, c := range []struct {
		who    string
		body   any
		status int
	}{
		{"erin", map[string]any{"requests": many}, http.StatusUnprocessableEntity},
		{"erin", map[string]any{"requests": []string{}}, http.StatusUnprocessableEntity},
		{"erin", map[string]any{"requests": []string{uuid.NewString()}}, http.StatusNotFound},
		{"erin", map[string]any{"requests": []string{"../../etc/passwd"}}, http.StatusUnprocessableEntity},
		{"bob", sample, http.StatusForbidden},
		{"otto", sample, http.StatusForbidden},
	} {
		if status, _, raw := h.as(c.who).download(http.MethodPost, Prefix+"/evidence/archive", c.body); status != c.status {
			t.Errorf("%s's archive of %v = %d, want %d: %s", c.who, c.body, status, c.status, raw)
		}
	}
	archiveSlots <- struct{}{}
	archiveSlots <- struct{}{}
	status, header, _ = erin.download(http.MethodPost, Prefix+"/evidence/archive", sample)
	<-archiveSlots
	<-archiveSlots
	if status != http.StatusTooManyRequests || header.Get("Retry-After") == "" {
		t.Errorf("a third archive at once = %d %v", status, header)
	}
}

func TestEvidenceTrailersWithoutAddresses(t *testing.T) {
	got := withoutAddresses([]string{
		"Quai-Request: #16 bfd43e4f-fdfa-4ebe-a56b-60ecad26bb37",
		"Requested-by: bob <bob@example.com>",
		"Approved-by: Alice Admin <alice@example.com> (on behalf of carol <carol@corp.example>)",
		"Break-glass-by: dave",
		"Break-glass-incident: <INC-4242>",
	})
	want := []string{
		"Quai-Request: #16 bfd43e4f-fdfa-4ebe-a56b-60ecad26bb37",
		"Requested-by: bob <address left out>",
		"Approved-by: Alice Admin <address left out> (on behalf of carol <address left out>)",
		"Break-glass-by: dave",
		"Break-glass-incident: <INC-4242>",
	}
	if !slices.Equal(got, want) {
		t.Errorf("withoutAddresses = %q, want %q", got, want)
	}
}
