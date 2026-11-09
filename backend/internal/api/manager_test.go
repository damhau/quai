package api

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/graphtest"
	"github.com/damhau/quai/backend/internal/oidctest"
	"github.com/damhau/quai/backend/internal/repositories"
)

// managerHarness serves the example repository with sign-ins through an OpenID Connect provider,
// whose access tokens Microsoft Graph's fake takes, configured by configure; the vm type's
// production rule asks for the requester's manager instead of a platform admin.
func managerHarness(t *testing.T, configure func(c *config.Config, g *graphtest.Graph)) (*harness, *oidctest.Provider, *graphtest.Graph) {
	t.Helper()
	p, g := oidctest.New(t), graphtest.New(t)
	p.IssueAccessTokens(g.Issue)
	var bundle []byte
	for _, ca := range []string{p.CAFile(t), g.CAFile(t)} {
		b, err := os.ReadFile(ca)
		if err != nil {
			t.Fatal(err)
		}
		bundle = append(bundle, b...)
	}
	dir := t.TempDir()
	ca := filepath.Join(dir, "bundle.pem")
	if err := os.WriteFile(ca, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	bare, work := localRemote(t, dir)
	h := start(t, dir, "connector: generic\n  url: "+bare+"\n  sync: { minInterval: 1ns }", repositories.Options{}, func(c *config.Config) {
		// The development users' roles, for people who sign in with the identity provider.
		c.Auth.Mode, c.Auth.Dev = config.AuthOIDC, config.Dev{}
		c.Auth.OIDC = config.OIDC{Issuer: p.Issuer(), ClientID: oidctest.ClientID, ClientSecret: "s", Scopes: []string{"openid"},
			NameClaim: "preferred_username", GroupsClaim: "groups"}
		c.Network.CAFile = ca
		configure(c, g)
	})
	h.bare, h.work = bare, work
	vm, err := os.ReadFile(filepath.Join(h.work, ".quai/types/vm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const production = "        approvers: [group:platform-admins]\n"
	if strings.Count(string(vm), production) != 1 {
		t.Fatal("the example vm type changed its production rule")
	}
	h.push(".quai/types/vm.yaml", strings.Replace(string(vm), production, "        approvers: [manager]\n", 1))
	return h, p, g
}

// signIn signs in through the identity provider with claims, in a browser of its own.
func (h *harness) signIn(p *oidctest.Provider, claims map[string]any) *user {
	h.t.Helper()
	p.SetClaims(claims)
	jar, _ := cookiejar.New(nil)
	u := &user{h: h, c: &http.Client{Jar: jar, Transport: p.Transport()}}
	resp, err := u.c.Get(h.srv.URL + "/auth/login")
	if err != nil {
		h.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var me struct{ Name, CSRFToken string }
	if status, _ := u.do(http.MethodGet, Prefix+"/me", nil, &me); status != http.StatusOK || me.Name != claims["preferred_username"] {
		h.t.Fatalf("sign-in %v = %d %s", claims["preferred_username"], status, body)
	}
	u.csrf = me.CSRFToken
	return u
}

// tryApprove approves a request, and returns the status.
func (u *user) tryApprove(id string) int {
	u.h.t.Helper()
	status, _ := u.do(http.MethodPost, Prefix+"/requests/"+id+"/review", map[string]string{"decision": "approve"}, nil)
	return status
}

// TestManagerFromGraph: a rule whose approvers are manager waits for the requester's manager, read
// from Microsoft Graph at their sign-in (open questions 68 to 70): the manager approves, nobody
// else may, a platform admin included. The manager is the one at submission: a new manager after
// does not change who approves that request, only new ones. Without a manager (none in Graph, the
// requester their own, one who never signed in), the request says why and nobody can approve.
// A change of manager is in the audit log. Someone in more groups than the ID token lists signs
// in with the groups Graph has, and their roles.
func TestManagerFromGraph(t *testing.T) {
	t.Parallel()
	tenant, developers := uuid.NewString(), uuid.NewString()
	h, p, g := managerHarness(t, func(c *config.Config, g *graphtest.Graph) {
		c.Auth.Graph = &config.Graph{TenantID: tenant, BaseURL: g.URL(), Manager: true, Groups: true}
		c.Auth.Roles = append(c.Auth.Roles, config.RoleMapping{Group: developers, Role: "viewer"}, config.RoleMapping{Group: developers, Role: "requester"})
	})
	oid := map[string]string{}
	for _, name := range []string{"alice", "bob", "carol", "dan", "erin", "mia", "olga", "zoe"} {
		oid[name] = uuid.NewString()
	}
	for name, manager := range map[string]string{"alice": "", "bob": oid["mia"], "carol": "", "dan": oid["dan"], "erin": uuid.NewString(), "mia": "", "zoe": ""} {
		g.AddUser(graphtest.User{ID: oid[name], DisplayName: name, Manager: manager})
	}
	as := func(name string, groups ...string) *user {
		return h.signIn(p, map[string]any{"preferred_username": name, "sub": "sub-" + name, "oid": oid[name], "tid": tenant, "groups": groups})
	}
	mia, zoe, alice := as("mia", "developers"), as("zoe", "developers"), as("alice", "platform-admins")
	bob := as("bob", "developers", "shop")

	planOf := func(u *user, env, name string) PlanView {
		t.Helper()
		var plan PlanView
		if status, body := u.do(http.MethodPost, Prefix+"/plans/type", newVM(env, name, 2), &plan); status != http.StatusOK {
			t.Fatalf("%s plan = %d %s", env, status, body)
		}
		return plan
	}
	if plan := planOf(bob, "prod", "mgr-plan"); plan.Manager == nil || *plan.Manager != (ManagerView{Name: "mia"}) {
		t.Fatalf("prod plan's manager = %+v", plan.Manager)
	}
	// No rule names the manager: nothing to say.
	if plan := planOf(bob, "dev", "mgr-plan"); plan.Manager != nil {
		t.Errorf("dev plan's manager = %+v", plan.Manager)
	}
	submit := func(u *user, name string) RequestView {
		t.Helper()
		var r RequestView
		if status, body := u.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", name, 2), &r); status != http.StatusCreated {
			t.Fatalf("submit %s = %d %s", name, status, body)
		}
		return r
	}
	approved, later := submit(bob, "mgr-01"), submit(bob, "mgr-02")
	if approved.State != "pending_approval" || approved.Manager == nil || approved.Manager.Name != "mia" {
		t.Fatalf("request = %s %+v", approved.State, approved.Manager)
	}
	// Someone else, a platform admin too, may not approve; the requester never does.
	for name, u := range map[string]*user{"alice": alice, "zoe": zoe, "bob": bob} {
		r := RequestView{}
		u.do(http.MethodGet, Prefix+"/requests/"+approved.ID, nil, &r)
		if status := u.tryApprove(approved.ID); r.CanReview || status != http.StatusForbidden {
			t.Errorf("%s may approve: canReview %v, review %d", name, r.CanReview, status)
		}
	}
	if status := mia.tryApprove(approved.ID); status != http.StatusOK {
		t.Fatalf("mia's approval = %d", status)
	}
	if r := bob.await(approved.ID, "applied"); len(r.Reviews) != 1 || r.Reviews[0].Reviewer != "mia" {
		t.Errorf("approved by %+v", r.Reviews)
	}

	// Bob's manager is now zoe: the request submitted before still waits for mia.
	g.AddUser(graphtest.User{ID: oid["bob"], Manager: oid["zoe"]})
	bob = as("bob", "developers", "shop")
	get := func(u *user, id string) RequestView {
		t.Helper()
		var r RequestView
		if status, body := u.do(http.MethodGet, Prefix+"/requests/"+id, nil, &r); status != http.StatusOK {
			t.Fatalf("request %s = %d %s", id, status, body)
		}
		return r
	}
	if r := get(bob, later.ID); r.Manager == nil || r.Manager.Name != "mia" {
		t.Errorf("after the change, the request's manager = %+v", r.Manager)
	}
	if status := zoe.tryApprove(later.ID); status != http.StatusForbidden {
		t.Errorf("zoe approved a request submitted before she was bob's manager: %d", status)
	}
	if !get(mia, later.ID).CanReview {
		t.Error("mia may no longer review the request submitted when she was bob's manager")
	}
	if plan := planOf(bob, "prod", "mgr-03"); plan.Manager == nil || plan.Manager.Name != "zoe" {
		t.Errorf("new plan's manager = %+v", plan.Manager)
	}
	var page AuditPageView
	if status, body := alice.do(http.MethodGet, Prefix+"/audit?limit=50&action=auth.manager_changed&object=bob", nil, &page); status != http.StatusOK || len(page.Events) != 2 {
		t.Fatalf("manager changes = %d %s", status, body)
	}
	before, _ := page.Events[0].Details["before"].(map[string]any)
	after, _ := page.Events[0].Details["after"].(map[string]any)
	if page.Personal[before["manager"].(string)] != oid["mia"] || page.Personal[after["manager"].(string)] != oid["zoe"] || after["source"] != "graph" {
		t.Errorf("manager changed = %+v", page.Events[0].Details)
	}

	// No manager: the request says why, and nobody can approve it.
	for name, why := range map[string]string{
		"carol": "Quai knows no manager for carol: Microsoft Graph has no manager for them",
		"dan":   "dan is their own manager",
		"erin":  "erin's manager has not signed in to Quai yet",
	} {
		u := as(name, "developers")
		r := submit(u, "mgr-"+name)
		if r.State != "pending_approval" || r.Manager == nil || r.Manager.Name != "" || !strings.Contains(r.Manager.Reason, why) {
			t.Errorf("%s's request = %s %+v, want the reason %q", name, r.State, r.Manager, why)
		}
		for _, other := range []*user{alice, mia, zoe} {
			if status := other.tryApprove(r.ID); status != http.StatusForbidden {
				t.Errorf("%s's request approved without a manager: %d", name, status)
			}
		}
	}

	// A group overage: olga's groups, and so her roles, come from Graph, never from the endpoint
	// the ID token names.
	g.AddUser(graphtest.User{ID: oid["olga"], Manager: oid["mia"], Groups: []graphtest.Group{{ID: developers, SecurityEnabled: true}}})
	olga := h.signIn(p, map[string]any{"preferred_username": "olga", "sub": "sub-olga", "oid": oid["olga"], "tid": tenant,
		"_claim_names":   map[string]any{"groups": "src1"},
		"_claim_sources": map[string]any{"src1": map[string]any{"endpoint": "https://graph.windows.net/" + tenant + "/users/" + oid["olga"] + "/getMemberObjects"}}})
	var me struct{ Groups []string }
	if olga.do(http.MethodGet, Prefix+"/me", nil, &me); len(me.Groups) != 1 || me.Groups[0] != developers {
		t.Errorf("olga's groups = %v", me.Groups)
	}
	r := submit(olga, "mgr-olga")
	if r.Manager == nil || r.Manager.Name != "mia" {
		t.Errorf("olga's request's manager = %+v", r.Manager)
	}
	if status := mia.tryApprove(r.ID); status != http.StatusOK {
		t.Fatalf("mia's approval of olga's request = %d", status)
	}
	olga.await(r.ID, "applied")
}

// TestManagerFromClaim: with auth.oidc.managerClaim, the manager is the person whose sub the
// claim holds (open question 69): they approve, someone else may not.
func TestManagerFromClaim(t *testing.T) {
	t.Parallel()
	h, p, _ := managerHarness(t, func(c *config.Config, _ *graphtest.Graph) { c.Auth.OIDC.ManagerClaim = "manager_sub" })
	as := func(name, manager string, groups ...string) *user {
		return h.signIn(p, map[string]any{"preferred_username": name, "sub": "sub-" + name, "manager_sub": manager, "groups": groups})
	}
	mia, alice := as("mia", "", "developers"), as("alice", "", "platform-admins")
	bob := as("bob", "sub-mia", "developers", "shop")
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "claim-01", 2), &r); status != http.StatusCreated ||
		r.Manager == nil || *r.Manager != (ManagerView{Name: "mia"}) {
		t.Fatalf("submit = %d %s", status, body)
	}
	if status := alice.tryApprove(r.ID); status != http.StatusForbidden {
		t.Errorf("alice approved: %d", status)
	}
	if status := mia.tryApprove(r.ID); status != http.StatusOK {
		t.Fatalf("mia's approval = %d", status)
	}
	bob.await(r.ID, "applied")

	// Without the claim, no manager.
	carol := as("carol", "", "developers")
	r = RequestView{}
	if status, body := carol.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "claim-02", 2), &r); status != http.StatusCreated ||
		r.Manager == nil || r.Manager.Name != "" || !strings.Contains(r.Manager.Reason, "is not an identifier") {
		t.Errorf("carol's request = %d %s", status, body)
	}

	// Without auth.graph, a group overage refuses the sign-in.
	p.SetClaims(map[string]any{"preferred_username": "olga", "sub": "sub-olga", "manager_sub": "sub-mia", "hasgroups": true})
	jar, _ := cookiejar.New(nil)
	olga := &user{h: h, c: &http.Client{Jar: jar, Transport: p.Transport()}}
	resp, err := olga.c.Get(h.srv.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if status, _ := olga.do(http.MethodGet, Prefix+"/me", nil, nil); resp.StatusCode != http.StatusForbidden || status != http.StatusUnauthorized ||
		!strings.Contains(string(body), "more groups than the ID token can list") {
		t.Errorf("overage sign-in = %d %s, then /me %d", resp.StatusCode, body, status)
	}
}
