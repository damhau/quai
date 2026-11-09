package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/repositories"
)

// teamsHarness serves the example repository with auth.teams mapping acme-shop-ops to the team
// shop and every other acme-<name> group to the team <name>. sam (acme-shop-ops) operates the
// team shop's resources; wes (acme-web) is an operator everywhere, in the team web; dana is an
// auditor. The groups of the default users (shop, developers…) map to no entry: each is the team
// of its name, as before auth.teams.
func teamsHarness(t *testing.T, configure ...func(*config.Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	bare, work := localRemote(t, dir)
	h := start(t, dir, "connector: generic\n  url: "+bare+"\n  sync: { minInterval: 1ns }", repositories.Options{}, append([]func(*config.Config){func(c *config.Config) {
		c.Auth.Teams = []config.TeamMapping{{Group: "acme-shop-ops", Team: "shop"}, {Group: "acme-*", Team: "*"}}
		c.Auth.Dev.Users = append(c.Auth.Dev.Users,
			config.DevUser{Name: "sam", Groups: []string{"acme-shop-ops"}},
			config.DevUser{Name: "wes", Groups: []string{"acme-web"}},
			config.DevUser{Name: "dana", Groups: []string{"auditors"}})
		c.Auth.Roles = append(c.Auth.Roles,
			config.RoleMapping{Group: "acme-shop-ops", Role: "viewer"},
			config.RoleMapping{Group: "acme-shop-ops", Role: "operator", Scope: "team:shop"},
			config.RoleMapping{Group: "acme-web", Role: "viewer"},
			config.RoleMapping{Group: "acme-web", Role: "operator"},
			config.RoleMapping{Group: "auditors", Role: "auditor"})
	}}, configure...)...)
	h.bare, h.work = bare, work
	return h
}

// TestTeams: ownership by team, team-scoped roles and owners approvers go through auth.teams.
func TestTeams(t *testing.T) {
	t.Parallel()
	h := teamsHarness(t)
	// The VMs are the team shop's.
	vm, err := os.ReadFile(filepath.Join(h.work, ".quai/types/vm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(vm, []byte("ownership: { field: owner }")) {
		t.Fatal("the example vm type changed its ownership")
	}
	h.push(".quai/types/vm.yaml", strings.Replace(string(vm), "ownership: { field: owner }", "ownership: { team: shop }", 1))
	bob, sam, wes := h.as("bob"), h.as("sam"), h.as("wes")

	var me MeOutput
	if sam.do(http.MethodGet, Prefix+"/me", nil, &me.Body); !slices.Equal(me.Body.Teams, []string{"shop"}) || !slices.Equal(me.Body.Groups, []string{"acme-shop-ops"}) {
		t.Errorf("sam = %+v", me.Body)
	}
	if wes.do(http.MethodGet, Prefix+"/me", nil, &me.Body); !slices.Equal(me.Body.Teams, []string{"web"}) {
		t.Errorf("wes = %+v", me.Body)
	}
	// Without an entry, a group is the team of its name.
	if bob.do(http.MethodGet, Prefix+"/me", nil, &me.Body); !slices.Equal(me.Body.Teams, []string{"developers", "shop"}) {
		t.Errorf("bob = %+v", me.Body)
	}

	// sam's operator role is the team shop's, and sam is in it through acme-shop-ops; wes is an
	// operator, but not in the team.
	const id = "infra-live/vm/dev/build-agent-01"
	var d ResourceDetail
	if status, body := sam.do(http.MethodGet, Prefix+"/resource?id="+id, nil, &d); status != http.StatusOK || d.Owner != "team:shop" || !d.CanOperate {
		t.Fatalf("sam sees %d %s", status, body)
	}
	if wes.do(http.MethodGet, Prefix+"/resource?id="+id, nil, &d); d.CanOperate {
		t.Error("wes may operate the team shop's VM")
	}
	resize := map[string]any{"repository": "infra-live", "type": "vm", "kind": "action", "key": "dev/build-agent-01", "action": "resize", "actionInputs": map[string]any{"cpu": "4", "memory_gb": 16}}
	if status, body := sam.do(http.MethodPost, Prefix+"/plans/type", resize, nil); status != http.StatusOK {
		t.Errorf("sam resizing = %d %s", status, body)
	}
	if status, _ := wes.do(http.MethodPost, Prefix+"/plans/type", resize, nil); status != http.StatusForbidden {
		t.Errorf("wes resizing = %d", status)
	}

	// Deleting needs the owners' approval: sam's counts, through the team; wes's is refused.
	var r RequestView
	del := map[string]any{"repository": "infra-live", "type": "vm", "kind": "delete", "key": "dev/build-agent-01", "reason": "decommissioned"}
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", del, &r); status != http.StatusCreated || r.State != "pending_approval" ||
		!slices.Contains(r.Rules[0].Approvers, "owners") {
		t.Fatalf("delete = %d %s", status, body)
	}
	if status, _ := wes.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusForbidden {
		t.Errorf("wes approving = %d", status)
	}
	if sam.do(http.MethodGet, Prefix+"/requests/"+r.ID, nil, &r); !r.CanReview {
		t.Error("sam is not offered to review")
	}
	sam.approve(r.ID)
	bob.await(r.ID, "applied")
	var teams []string
	if err := h.store.Pool.QueryRow(context.Background(), "SELECT teams FROM reviews WHERE reviewer = 'sam'").Scan(&teams); err != nil || !slices.Equal(teams, []string{"shop"}) {
		t.Errorf("sam's review keeps the teams %v, %v", teams, err)
	}
}

// TestAccessReport: auditors and platform admins read who can do what, and download it as CSV
// with every cell a spreadsheet would compute neutralized.
func TestAccessReport(t *testing.T) {
	t.Parallel()
	const hostile = `=HYPERLINK("https://evil.example.com","x")`
	h := teamsHarness(t, func(c *config.Config) {
		c.Auth.Dev.Users = append(c.Auth.Dev.Users, config.DevUser{Name: hostile, Email: "+1@example.com", Groups: []string{"@SUM(A1)", "-x\tz"}})
		c.Auth.Roles = append(c.Auth.Roles, config.RoleMapping{Group: "@SUM(A1)", Role: "viewer"}, config.RoleMapping{Group: "-x\tz", Role: "viewer"})
	})
	alice, dana := h.as("alice"), h.as("dana")
	h.as("bob")
	h.as("sam")
	h.as(hostile)
	h.as("carol")
	if _, err := h.store.Pool.Exec(context.Background(), "UPDATE users SET deactivated_at = now() WHERE name = 'carol'"); err != nil {
		t.Fatal(err)
	}

	// Tokens that still work are in the report, with the groups they act with: alice's, and zoe's
	// from before the directory, zoe not having signed in since; not a revoked or an expired one.
	var tok, old CreatedTokenOutput
	alice.do(http.MethodPost, Prefix+"/tokens", map[string]any{"name": "read", "scopes": []string{"requests:read"}}, &tok.Body)
	alice.do(http.MethodPost, Prefix+"/tokens", map[string]any{"name": "old", "scopes": []string{"requests:read"}}, &old.Body)
	if status, body := alice.do(http.MethodDelete, Prefix+"/tokens/"+old.Body.ID, nil, nil); status != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", status, body)
	}
	if _, err := h.store.Pool.Exec(context.Background(), `INSERT INTO api_tokens (id, owner, groups, name, hash, scopes, expires_at) VALUES
		(gen_random_uuid(), 'zoe', '{platform-admins}', 'ci', '\x01', '{requests:write}', now() + interval '1 day'),
		(gen_random_uuid(), 'bob', '{platform-admins}', 'gone', '\x02', '{requests:write}', now() - interval '1 second')`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/access-report", "/access-report/export"} {
		for name, want := range map[string]int{"dana": http.StatusOK, "alice": http.StatusOK, "bob": http.StatusForbidden, "wes": http.StatusForbidden, "": http.StatusUnauthorized} {
			if status, body := h.as(name).do(http.MethodGet, Prefix+path, nil, nil); status != want {
				t.Errorf("%q reading %s = %d %s", name, path, status, body)
			}
		}
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+Prefix+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok.Body.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("a token reading %s = %d", path, resp.StatusCode)
		}
	}

	var report AccessReportView
	if status, body := dana.do(http.MethodGet, Prefix+"/access-report", nil, &report); status != http.StatusOK {
		t.Fatalf("report = %d %s", status, body)
	}
	users := map[string]AccessUserView{}
	var names []string
	for _, u := range report.Users {
		users[u.Name] = u
		names = append(names, u.Name)
	}
	// Everyone who signed in, by name (the reads above signed wes in too), and zoe.
	if want := []string{hostile, "alice", "bob", "carol", "dana", "sam", "wes", "zoe"}; !slices.Equal(names, want) {
		t.Errorf("users = %q, want %q", names, want)
	}
	bob := users["bob"]
	if !slices.Equal(bob.Groups, []string{"developers", "shop"}) || !slices.Equal(bob.Teams, []string{"developers", "shop"}) || bob.State != "active" ||
		bob.LastLogin == nil || bob.AuthTime == nil || len(bob.Roles) != 3 || bob.Roles[0].Role != "operator" || bob.Roles[0].Scope != "global" || !slices.Equal(bob.Roles[0].Groups, []string{"developers"}) ||
		bob.Roles[1].Role != "requester" || bob.Roles[2].Role != "viewer" || len(bob.Tokens) != 0 {
		t.Errorf("bob = %+v", bob)
	}
	if a := users["alice"]; len(a.Tokens) != 1 || a.Tokens[0].Name != "read" || !slices.Equal(a.Tokens[0].Scopes, []string{"requests:read"}) ||
		!slices.Equal(a.Tokens[0].Groups, []string{"platform-admins"}) || len(a.Tokens[0].Roles) != 1 || a.Tokens[0].Roles[0].Role != "platform-admin" ||
		a.Tokens[0].ExpiresAt.IsZero() || a.Tokens[0].LastUsedAt == nil {
		t.Errorf("alice = %+v", a)
	}
	if z := users["zoe"]; z.LastLogin != nil || len(z.Roles) != 0 || len(z.Tokens) != 1 || z.Tokens[0].Roles[0].Role != "platform-admin" || z.State != "active" {
		t.Errorf("zoe = %+v", z)
	}
	sam := users["sam"]
	if !slices.Equal(sam.Teams, []string{"shop"}) || len(sam.Roles) != 2 || sam.Roles[0].Role != "operator" || sam.Roles[0].Scope != "team:shop" ||
		!slices.Equal(sam.Roles[0].Groups, []string{"acme-shop-ops"}) {
		t.Errorf("sam = %+v", sam)
	}
	if carol := users["carol"]; carol.State != "deactivated" || carol.DeactivatedAt == nil {
		t.Errorf("carol = %+v", carol)
	}
	if bad := users[hostile]; len(bad.Roles) != 1 || !slices.Equal(bad.Roles[0].Groups, []string{"-x\tz", "@SUM(A1)"}) {
		t.Errorf("the hostile user = %+v", bad)
	}

	// The CSV: one row per person or token, role, scope and granting group.
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+Prefix+"/access-report/export", nil)
	resp, err := alice.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/csv; charset=utf-8" || resp.Header.Get("Cache-Control") != "no-store" ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="quai-access-report-`+time.Now().UTC().Format(time.DateOnly)+`.csv"` {
		t.Errorf("headers = %v", resp.Header)
	}
	br := bufio.NewReader(resp.Body)
	if bom, _, _ := br.ReadRune(); bom != '\uFEFF' {
		t.Errorf("no byte order mark: %q", bom)
	}
	rows, err := csv.NewReader(br).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rows[0], accessColumns) {
		t.Errorf("header = %q", rows[0])
	}
	byName := map[string][][]string{}
	for _, row := range rows[1:] {
		byName[row[0]] = append(byName[row[0]], row)
	}
	if b := byName["bob"]; len(b) != 3 || b[0][4] != "active" || b[0][7] != "developers; shop" || b[0][9] != "sign-in" || b[0][12] != "operator" ||
		b[0][13] != "global" || b[0][14] != "developers" {
		t.Errorf("bob's rows = %q", b)
	}
	if d := byName["dana"]; len(d) != 1 || d[0][12] != "auditor" || d[0][14] != "auditors" {
		t.Errorf("dana's rows = %q", d)
	}
	if a := byName["alice"]; len(a) != 2 || a[0][9] != "sign-in" || a[1][9] != "token:read" || a[1][10] != "requests:read" || a[1][11] == "" ||
		a[1][12] != "platform-admin" || a[1][14] != "platform-admins" {
		t.Errorf("alice's rows = %q", a)
	}
	if z := byName["zoe"]; len(z) != 2 || z[0][5] != "" || z[0][9] != "sign-in" || z[0][12] != "" || z[1][9] != "token:ci" || z[1][10] != "requests:write" || z[1][12] != "platform-admin" {
		t.Errorf("zoe's rows = %q", z)
	}
	// Every cell that would start a formula starts with an apostrophe.
	bad := byName["'"+hostile]
	if len(bad) != 2 {
		t.Fatalf("the hostile user's rows = %q (all: %q)", bad, rows)
	}
	for _, row := range bad {
		if row[1] != "'+1@example.com" || row[7] != "'-x\tz; @SUM(A1)" || row[12] != "viewer" {
			t.Errorf("hostile row = %q", row)
		}
	}
	if bad[0][14] != "'-x\tz" || bad[1][14] != "'@SUM(A1)" {
		t.Errorf("granted by = %q, %q", bad[0][14], bad[1][14])
	}
}

func TestCSVSafe(t *testing.T) {
	for in, want := range map[string]string{
		"":          "",
		"alice":     "alice",
		"=1+1":      "'=1+1",
		"+1":        "'+1",
		"-1":        "'-1",
		"@SUM(A1)":  "'@SUM(A1)",
		"\tx":       "'\tx",
		"\rx":       "'\rx",
		"a=b":       "a=b",
		"'=already": "'=already",
	} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	writeCSVRow(w, []string{`say "hi"`, "a;b,c", "=x"})
	_ = w.Flush()
	if got := b.String(); got != `"say ""hi""","a;b,c","'=x"`+"\r\n" {
		t.Errorf("row = %q", got)
	}
}
