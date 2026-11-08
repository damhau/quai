package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/audit/keeper"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store/db"
)

// auditLog reads the audit log as alice, a platform admin.
func (h *harness) auditLog(query string) AuditPageView {
	h.t.Helper()
	var page AuditPageView
	if status, body := h.as("alice").do(http.MethodGet, Prefix+"/audit?"+query, nil, &page); status != http.StatusOK {
		h.t.Fatalf("audit log ?%s = %d %s", query, status, body)
	}
	return page
}

// audited returns the events of an action about an object, oldest first, and the clear values of
// their personal data; the test fails when there is none.
func (h *harness) audited(action, object string) ([]AuditEventView, map[string]string) {
	h.t.Helper()
	page := h.auditLog("limit=500&action=" + url.QueryEscape(action) + "&object=" + url.QueryEscape(object))
	if len(page.Events) == 0 {
		h.t.Fatalf("no %s event about %s in the audit log:\n%s", action, object, strings.Join(h.actions(), "\n"))
	}
	slices.Reverse(page.Events)
	return page.Events, page.Personal
}

// actions lists the audit log's actions, oldest first, with their objects.
func (h *harness) actions() []string {
	page := h.auditLog("limit=500")
	var out []string
	for i := len(page.Events) - 1; i >= 0; i-- {
		e := page.Events[i]
		out = append(out, e.Action+" "+e.Object.Type+":"+e.Object.ID)
	}
	return out
}

// byPerson checks that an event's actor is a person signed in from this test, with their
// authentication time.
func byPerson(t *testing.T, e AuditEventView, personal map[string]string, name string) {
	t.Helper()
	if e.Actor.Type != "user" || personal[e.Actor.ID] != name || personal[e.Actor.IP] != "127.0.0.1" || e.Actor.AuthTime == "" {
		t.Errorf("%s: actor %+v (%q, %q), want %s signed in", e.Action, e.Actor, personal[e.Actor.ID], personal[e.Actor.IP], name)
	}
	if at, err := time.Parse(time.RFC3339, e.Actor.AuthTime); err != nil || time.Since(at) > 10*time.Minute {
		t.Errorf("%s: auth time %q, %v", e.Action, e.Actor.AuthTime, err)
	}
}

// TestAuditLog: each security-relevant action writes its event, in its transaction, with the
// actor and their authentication time; the chain verifies; filters and pages work.
func TestAuditLog(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	// The configuration's connection, by Quai at start.
	ev, _ := h.audited("repository.created", "infra-live")
	if ev[0].Actor.Type != "system" || ev[0].Actor.ID != "quai" || ev[0].Details["managedBy"] != "config" || ev[0].Seq != 1 || ev[0].Prev != audit.Genesis {
		t.Errorf("configured connection = %+v", ev[0])
	}

	// Logins, with the grants; a change of grants between logins.
	h.as("bob")
	alice := h.as("alice")
	ev, personal := h.audited("auth.login", "bob")
	byPerson(t, ev[0], personal, "bob")
	if ev[0].Details["method"] != "dev" || !slices.Contains(toStrings(ev[0].Details["grants"]), "requester@global") || ev[0].Object.Type != "user" {
		t.Errorf("bob's login = %+v", ev[0])
	}
	if err := h.store.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		return audit.Add(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.User, ID: "bob"}, Action: "auth.login",
			Object: audit.Object{Type: "user", ID: "bob"}, Details: map[string]any{"grants": []string{"viewer@global"}}})
	}); err != nil {
		t.Fatal(err)
	}
	bob := h.as("bob")
	ev, personal = h.audited("auth.grants_changed", "bob")
	byPerson(t, ev[0], personal, "bob")
	if before, after := toStrings(ev[0].Details["before"]), toStrings(ev[0].Details["after"]); !slices.Equal(before, []string{"viewer@global"}) || !slices.Contains(after, "operator@global") {
		t.Errorf("grants changed = %v", ev[0].Details)
	}

	// Submitted, reviewed, approved, merged and applied (a direct commit).
	var r RequestView
	vm := newVM("prod", "audit01", 2)
	vm["reason"] = "capacity for the audit"
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", vm, &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve", "comment": "fine by me"}, nil); status != http.StatusOK {
		t.Fatalf("approve = %d %s", status, body)
	}
	r = bob.await(r.ID, "applied")
	if len(r.Reviews) != 1 || r.Reviews[0].AuthTime == nil {
		t.Errorf("the review keeps the reviewer's auth time: %+v", r.Reviews)
	}
	ev, personal = h.audited("request.submitted", r.ID)
	byPerson(t, ev[0], personal, "bob")
	if personal[ev[0].Details["reason"].(string)] != "capacity for the audit" || ev[0].Object.Resource != "infra-live/vm/prod/audit01" || ev[0].Details["changeSet"] == "" {
		t.Errorf("submitted = %+v", ev[0])
	}
	ev, personal = h.audited("request.reviewed", r.ID)
	byPerson(t, ev[0], personal, "alice")
	if ev[0].Details["decision"] != "approve" || personal[ev[0].Details["comment"].(string)] != "fine by me" {
		t.Errorf("reviewed = %+v", ev[0])
	}
	if at, _ := time.Parse(time.RFC3339, ev[0].Actor.AuthTime); !at.Equal(r.Reviews[0].AuthTime.Truncate(time.Second)) {
		t.Errorf("the review's auth time %v, the event's %s", r.Reviews[0].AuthTime, ev[0].Actor.AuthTime)
	}
	ev, personal = h.audited("request.approved", r.ID)
	byPerson(t, ev[0], personal, "alice")
	if a := toStrings(ev[0].Details["approvals"]); len(a) != 1 || personal[a[0]] != "alice" {
		t.Errorf("approved = %+v", ev[0])
	}
	for _, action := range []string{"request.merged", "request.applied"} {
		ev, _ = h.audited(action, r.ID)
		if ev[0].Actor.Type != "system" || ev[0].Details["event"] != "committed" || ev[0].Details["commit"] == "" {
			t.Errorf("%s = %+v", action, ev[0])
		}
	}

	// Rejected; cancelled; stale.
	bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "audit02", 2), &r)
	alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "deny", "comment": "not now"}, nil)
	ev, personal = h.audited("request.rejected", r.ID)
	byPerson(t, ev[0], personal, "alice")
	bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "audit03", 2), &r)
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/cancel", nil, nil); status != http.StatusOK {
		t.Fatalf("cancel = %d %s", status, body)
	}
	ev, personal = h.audited("request.cancelled", r.ID)
	byPerson(t, ev[0], personal, "bob")
	if ev[0].Details["from"] != "pending_approval" {
		t.Errorf("cancelled = %+v", ev[0])
	}
	bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "vm", "kind": "action", "key": "prod/web01",
		"action": "resize", "actionInputs": map[string]any{"cpu": 6, "memory_gb": 16}}, &r)
	h.push("envs/prod/vms/web01.yaml", "owner: bob\ntemplate: ubuntu-24.04\ncpu: 5\nmemory_gb: 16\ndisks: [{ size_gb: 80 }]\nnetwork: { vlan: 120 }\n")
	alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil)
	ev, _ = h.audited("request.stale", r.ID)
	if ev[0].Details["region"] == nil {
		t.Errorf("stale = %+v", ev[0])
	}

	// Failed (as the delivery's last attempt leaves it), then retried.
	bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "audit04", 2), &r)
	if _, err := h.store.TransitionRequest(ctx, db.TransitionRequestParams{ID: uuid.MustParse(r.ID), FromState: "pending_approval", ToState: "failed"}); err != nil {
		t.Fatal(err)
	}
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/retry", nil, nil); status != http.StatusOK {
		t.Fatalf("retry = %d %s", status, body)
	}
	ev, personal = h.audited("request.retried", r.ID)
	byPerson(t, ev[0], personal, "bob")

	// API tokens: created, used (no authentication time), revoked.
	var tok CreatedTokenOutput
	if status, body := bob.do(http.MethodPost, Prefix+"/tokens", map[string]any{"name": "ci", "scopes": []string{"catalog:read", "requests:write"}}, &tok.Body); status != http.StatusCreated {
		t.Fatalf("create token = %d %s", status, body)
	}
	ev, personal = h.audited("token.created", tok.Body.ID)
	byPerson(t, ev[0], personal, "bob")
	if ev[0].Details["name"] != "ci" || strings.Contains(strings.Join(slices.Collect(maps.Values(personal)), " "), tok.Body.Token) {
		t.Errorf("token created = %+v", ev[0])
	}
	ci := h.as("")
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+Prefix+"/requests/type", strings.NewReader(`{"repository":"infra-live","type":"vm","kind":"create","params":{"env":"dev","name":"audit05"},"values":{"template":"rhel-9","cpu":2,"memory_gb":8,"disks":[{"size_gb":50}],"network":{"vlan":110}}}`))
	req.Header.Set("Authorization", "Bearer "+tok.Body.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ci.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("submit with a token = %d %s", resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	bob.await(r.ID, "applied")
	ev, personal = h.audited("request.submitted", "infra-live/vm/dev/audit05")
	if a := ev[0].Actor; a.Type != "token" || personal[a.ID] != "bob" || a.Token != tok.Body.ID || a.AuthTime != "" {
		t.Errorf("submitted with a token: actor %+v", a)
	}
	if status, body := bob.do(http.MethodDelete, Prefix+"/tokens/"+tok.Body.ID, nil, nil); status != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", status, body)
	}
	ev, personal = h.audited("token.revoked", tok.Body.ID)
	byPerson(t, ev[0], personal, "bob")

	// A run token generated.
	var rt RunTokenView
	if status, body := alice.do(http.MethodPost, Prefix+"/repositories/infra-live/run-token", nil, &rt); status != http.StatusOK {
		t.Fatalf("run token = %d %s", status, body)
	}
	ev, personal = h.audited("run_token.created", "infra-live")
	byPerson(t, ev[0], personal, "alice")
	if ev[0].Details["sha256"] != rt.SHA256 {
		t.Errorf("run token = %+v", ev[0])
	}

	// A new definitions commit, with its author.
	vmType, _ := h.file(".quai/types/vm.yaml")
	h.push(".quai/types/vm.yaml", vmType+"# reviewed\n")
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Snapshot(ctx, true); err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(git(t, h.bare, "rev-parse", "main"))
	// The first definitions, then this change: the commits of requests and the hand edit of
	// web01 change no definition.
	ev, personal = h.audited("definitions.commit", "infra-live")
	last := ev[len(ev)-1]
	if len(ev) != 2 || last.Details["commit"] != head || personal[last.Details["author"].(string)] != "Alice <alice@example.com>" ||
		!slices.ContainsFunc(last.Details["changes"].([]any), func(c any) bool { return c.(map[string]any)["name"] == "vm" }) {
		t.Errorf("definitions commit = %+v", last)
	}

	// A logout.
	if status, _ := bob.do(http.MethodPost, "/auth/logout", nil, nil); status != http.StatusNoContent {
		t.Fatalf("logout = %d", status)
	}
	ev, personal = h.audited("auth.logout", "bob")
	byPerson(t, ev[0], personal, "bob")

	// Filters: by action prefix, actor, object type, time; pages.
	for _, e := range h.auditLog("limit=500&action=request").Events {
		if !strings.HasPrefix(e.Action, "request.") {
			t.Errorf("action=request: %s", e.Action)
		}
	}
	page := h.auditLog("limit=500&actor=bob")
	for _, e := range page.Events {
		if page.Personal[e.Actor.ID] != "bob" {
			t.Errorf("actor=bob: %+v", e.Actor)
		}
	}
	if len(page.Events) < 10 || !page.Protected {
		t.Errorf("bob's events: %d, protected %v", len(page.Events), page.Protected)
	}
	if p := h.auditLog("objectType=token&limit=500"); len(p.Events) != 2 {
		t.Errorf("objectType=token: %d events", len(p.Events))
	}
	if p := h.auditLog("since=" + url.QueryEscape(time.Now().Add(time.Hour).Format(time.RFC3339))); len(p.Events) != 0 {
		t.Errorf("since an hour from now: %d events", len(p.Events))
	}
	if p := h.auditLog("until=" + url.QueryEscape(time.Now().Add(-time.Hour).Format(time.RFC3339))); len(p.Events) != 0 {
		t.Errorf("until an hour ago: %d events", len(p.Events))
	}
	first := h.auditLog("limit=5")
	second := h.auditLog("limit=5&before=" + strconv.FormatInt(first.Next, 10))
	if len(first.Events) != 5 || first.Next != first.Events[4].Seq || len(second.Events) != 5 || second.Events[0].Seq != first.Next-1 {
		t.Errorf("pages: %d events next %d, then %d from %d", len(first.Events), first.Next, len(second.Events), second.Events[0].Seq)
	}

	// The chain holds.
	if res, err := audit.Verify(ctx, h.store.Pool, nil); err != nil || res.Broken != nil || res.Events < 30 {
		t.Errorf("verify = %+v, %v", res, err)
	}
}

// TestAuditReaders: only auditors and platform admins read the audit log; a token cannot.
func TestAuditReaders(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bare, _ := localRemote(t, dir)
	h := start(t, dir, "connector: generic\n  url: "+bare, repositories.Options{}, func(c *config.Config) {
		c.Auth.Dev.Users = append(c.Auth.Dev.Users, config.DevUser{Name: "dana", Groups: []string{"auditors"}})
		c.Auth.Roles = append(c.Auth.Roles, config.RoleMapping{Group: "auditors", Role: "auditor"})
	})
	for name, want := range map[string]int{"dana": http.StatusOK, "alice": http.StatusOK, "bob": http.StatusForbidden, "": http.StatusUnauthorized} {
		var page AuditPageView
		status, body := h.as(name).do(http.MethodGet, Prefix+"/audit", nil, &page)
		if status != want || (want == http.StatusOK && (len(page.Events) == 0 || !page.Protected)) {
			t.Errorf("%q reading the audit log = %d %s", name, status, body)
		}
	}
	var tok CreatedTokenOutput
	h.as("alice").do(http.MethodPost, Prefix+"/tokens", map[string]any{"name": "read", "scopes": []string{"requests:read"}}, &tok.Body)
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+Prefix+"/audit", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Body.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a token reading the audit log = %d", resp.StatusCode)
	}
}

// TestAuditExport: auditors and platform admins download the audit log, which verifies offline;
// the audit log page shows the latest checkpoint and verification.
func TestAuditExport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	bare, _ := localRemote(t, dir)
	h := start(t, dir, "connector: generic\n  url: "+bare, repositories.Options{}, func(c *config.Config) {
		c.Auth.Dev.Users = append(c.Auth.Dev.Users, config.DevUser{Name: "dana", Groups: []string{"auditors"}})
		c.Auth.Roles = append(c.Auth.Roles, config.RoleMapping{Group: "auditors", Role: "auditor"})
	})
	if page := h.auditLog("limit=1"); page.Checkpoint != nil || page.Verification != nil {
		t.Errorf("before any checkpoint or verification: %+v, %+v", page.Checkpoint, page.Verification)
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := audit.Keys{}
	keys.Add(pub)
	c, _, err := audit.SignLatest(ctx, h.store.Queries, key, time.Now())
	if err != nil || c.Seq == 0 {
		t.Fatalf("checkpoint = %+v, %v", c, err)
	}
	res, err := keeper.New(keeper.Options{Store: h.store}).Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	page := h.auditLog("limit=1")
	if cp := page.Checkpoint; cp == nil || cp.Seq != c.Seq || cp.Key != c.Key || cp.At != c.At {
		t.Errorf("checkpoint = %+v, want %+v", cp, c)
	}
	// Without the key, the keeper finds the checkpoint it cannot verify.
	if v := page.Verification; v == nil || v.Verified || v.Broken != c.Seq || !strings.Contains(v.Reason, "which was not given") || v.Seq <= c.Seq {
		t.Errorf("verification = %+v (%+v)", v, res)
	}

	for name, want := range map[string]int{"dana": http.StatusOK, "alice": http.StatusOK, "bob": http.StatusForbidden, "": http.StatusUnauthorized} {
		u := h.as(name)
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+Prefix+"/audit/export", nil)
		resp, err := u.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%q exporting the audit log = %d %s", name, resp.StatusCode, body)
			continue
		}
		if want != http.StatusOK {
			continue
		}
		if ct, cd := resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"); ct != "application/jsonl" || !strings.HasPrefix(cd, "attachment;") {
			t.Errorf("export headers: %q, %q", ct, cd)
		}
		v := audit.NewVerifier(keys)
		if err := audit.ReadExport(bytes.NewReader(body), v); err != nil {
			t.Fatal(err)
		}
		if res := v.Result(); res.Broken != nil || res.First != 1 || res.Checkpoints != 1 || res.Events < int64(c.Seq) {
			t.Errorf("the export verifies: %+v, %v", res, res.Broken)
		}
		if strings.Contains(string(body), "alice") {
			t.Error("the export holds a clear personal value")
		}
	}
	var tok CreatedTokenOutput
	h.as("alice").do(http.MethodPost, Prefix+"/tokens", map[string]any{"name": "read", "scopes": []string{"requests:read"}}, &tok.Body)
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+Prefix+"/audit/export", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Body.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a token exporting the audit log = %d", resp.StatusCode)
	}
}

func toStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, e := range l {
			s, _ := e.(string)
			out = append(out, s)
		}
	}
	return out
}
