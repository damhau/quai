package auth_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/graphtest"
	"github.com/damhau/quai/backend/internal/oidctest"
)

// graphFixture is Quai signing in with an Entra ID-like provider and reading Microsoft Graph's
// fake, with the waits between retries it would have made.
type graphFixture struct {
	env
	p *oidctest.Provider
	g *graphtest.Graph

	mu    sync.Mutex
	waits []time.Duration
}

// newGraphFixture starts it with auth.graph (nil: none) and a change to the configuration.
func newGraphFixture(t *testing.T, graph func(base string) *config.Graph, change func(*config.Config)) *graphFixture {
	t.Helper()
	f := &graphFixture{p: oidctest.New(t), g: graphtest.New(t)}
	f.p.IssueAccessTokens(f.g.Issue)
	// One file trusts both: Quai has one network.caFile.
	var bundle []byte
	for _, ca := range []string{f.p.CAFile(t), f.g.CAFile(t)} {
		b, err := os.ReadFile(ca)
		if err != nil {
			t.Fatal(err)
		}
		bundle = append(bundle, b...)
	}
	ca := filepath.Join(t.TempDir(), "bundle.pem")
	if err := os.WriteFile(ca, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	f.env = quaiEnv(t, f.p, func(c *config.Config) {
		c.Network.CAFile = ca
		if graph != nil {
			c.Auth.Graph = graph(f.g.URL())
		}
		if change != nil {
			change(c)
		}
	})
	f.a.UseSleep(func(_ context.Context, d time.Duration) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.waits = append(f.waits, d)
		return nil
	})
	return f
}

// signIn signs in, in a new browser, with claims: the session's identity, or the status and body
// of the refusal.
func (f *graphFixture) signIn(t *testing.T, claims map[string]any) (me, int, string) {
	t.Helper()
	f.p.SetClaims(claims)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Transport: f.c.Transport}
	resp, err := c.Get(f.srv.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m, status := getMe(t, c, f.srv.URL)
	if status != http.StatusOK {
		return me{}, resp.StatusCode, string(body)
	}
	return m, http.StatusOK, ""
}

// waited returns the waits since the last call, and forgets them.
func (f *graphFixture) waited() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.waits
	f.waits = nil
	return w
}

// lastScope is the scope the last authorization asked for.
func (f *graphFixture) lastScope(t *testing.T) []string {
	t.Helper()
	asked := f.p.Authorizations()
	if len(asked) == 0 {
		t.Fatal("no authorization")
	}
	return strings.Fields(asked[len(asked)-1].Scope)
}

// TestGraphGroups: a group overage (open question 68). The groups are read from Microsoft Graph's
// /me/getMemberObjects, security groups only, with the sign-in's own access token, at the
// configured root and never at the endpoint the ID token names; throttling is waited out within
// bounds. Anything less than the whole list refuses the sign-in: an error, a token Graph refuses,
// a missing permission, a partial answer (@odata.nextLink, never followed), too many groups,
// another tenant. Without auth.graph.groups, an overage refuses the sign-in, as before.
func TestGraphGroups(t *testing.T) {
	tenant, erin := uuid.NewString(), uuid.NewString()
	admins, shop, m365 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	f := newGraphFixture(t, func(base string) *config.Graph {
		return &config.Graph{TenantID: strings.ToUpper(tenant), BaseURL: base, Groups: true}
	}, func(c *config.Config) {
		c.Auth.Roles = append(c.Auth.Roles, config.RoleMapping{Group: admins, Role: "platform-admin"})
	})
	f.g.AddUser(graphtest.User{ID: erin, Groups: []graphtest.Group{{ID: admins, SecurityEnabled: true}, {ID: strings.ToUpper(shop), SecurityEnabled: true}, {ID: m365}}})
	var elsewhere atomic.Int32 // calls to the endpoint the ID token names
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write([]byte(`{"value": ["` + uuid.NewString() + `"]}`))
	}))
	t.Cleanup(other.Close)
	overage := map[string]any{"preferred_username": "erin", "oid": erin, "tid": tenant,
		"_claim_names":   map[string]any{"groups": "src1"},
		"_claim_sources": map[string]any{"src1": map[string]any{"endpoint": other.URL + "/v1.0/users/" + erin + "/getMemberObjects"}}}
	want := []string{admins, shop}
	slices.Sort(want)

	m, status, body := f.signIn(t, overage)
	if status != http.StatusOK || !slices.Equal(m.Groups, want) || !slices.ContainsFunc(m.Roles, func(r struct{ Role, Scope string }) bool { return r.Role == "platform-admin" }) {
		t.Fatalf("overage sign-in = %d %s %+v", status, body, m)
	}
	if calls := f.g.Calls(); len(calls) != 1 || calls[0] != (graphtest.Call{Method: http.MethodPost, Path: "/v1.0/me/getMemberObjects", Status: http.StatusOK, User: erin}) {
		t.Errorf("graph calls = %+v", calls)
	}
	// User.Read only: groups, not the manager.
	if scope := f.lastScope(t); !slices.Contains(scope, f.g.URL()+"/User.Read") || slices.Contains(scope, f.g.URL()+"/User.Read.All") || !slices.Contains(scope, "openid") {
		t.Errorf("scope = %v", scope)
	}
	// Entra ID's implicit flow says hasgroups instead.
	implicit := map[string]any{"preferred_username": "erin", "oid": erin, "tid": tenant, "hasgroups": true}
	if m, status, body := f.signIn(t, implicit); status != http.StatusOK || !slices.Equal(m.Groups, want) {
		t.Errorf("hasgroups sign-in = %d %s %+v", status, body, m)
	}
	// Throttled, then served: Retry-After is waited for, or one second, then two, without it.
	f.waited()
	f.g.FailNext(1, http.StatusTooManyRequests, "3")
	if m, status, body := f.signIn(t, overage); status != http.StatusOK || !slices.Equal(m.Groups, want) || !slices.Equal(f.waited(), []time.Duration{3 * time.Second}) {
		t.Errorf("throttled once = %d %s %+v", status, body, m)
	}
	f.g.FailNext(2, http.StatusServiceUnavailable, "")
	if _, status, body := f.signIn(t, overage); status != http.StatusOK || !slices.Equal(f.waited(), []time.Duration{time.Second, 2 * time.Second}) {
		t.Errorf("unavailable twice = %d %s", status, body)
	}

	refused := func(what string, claims map[string]any, reason string) {
		t.Helper()
		m, status, body := f.signIn(t, claims)
		if status != http.StatusForbidden || !strings.Contains(body, reason) || m.Name != "" {
			t.Errorf("%s: sign-in = %d %s %+v, want refused with %q", what, status, body, m.Name, reason)
		}
		if strings.Contains(body, "Bearer") {
			t.Errorf("%s: the refusal shows a token: %s", what, body)
		}
	}
	const couldNot = "Quai could not read them from Microsoft Graph"
	f.g.FailNext(3, http.StatusTooManyRequests, "1")
	refused("throttled thrice", overage, couldNot)
	if w := f.waited(); len(w) != 2 {
		t.Errorf("throttled thrice: waits %v, want two retries", w)
	}
	f.g.FailNext(1, http.StatusTooManyRequests, "30")
	refused("a Retry-After too long", overage, couldNot)
	if w := f.waited(); len(w) != 0 {
		t.Errorf("a Retry-After too long was waited for: %v", w)
	}
	f.g.FailNext(1, http.StatusInternalServerError, "")
	refused("an error", overage, couldNot)

	// A token Graph does not take (expired), or without the permission.
	f.p.IssueAccessTokens(func(claims map[string]any, scope string) string {
		v := f.g.Issue(claims, scope)
		f.g.ExpireTokens()
		return v
	})
	refused("an expired token", overage, couldNot)
	if calls := f.g.Calls(); calls[len(calls)-1].Status != http.StatusUnauthorized {
		t.Errorf("expired token: %+v", calls[len(calls)-1])
	}
	f.p.IssueAccessTokens(f.g.Issue)
	f.g.Consent("User.Read.All")
	refused("User.Read not consented", overage, couldNot)
	if calls := f.g.Calls(); calls[len(calls)-1].Status != http.StatusForbidden {
		t.Errorf("no consent: %+v", calls[len(calls)-1])
	}
	f.g.Consent("User.Read", "User.Read.All")

	// A partial answer: the next page is never asked for, there or elsewhere.
	for _, link := range []string{other.URL + "/v1.0/me/getMemberObjects?$skiptoken=x", f.g.URL() + "/v1.0/me/getMemberObjects?$skiptoken=x"} {
		f.g.WithNextLink(link)
		before := len(f.g.Calls())
		refused("a next page", overage, couldNot)
		if calls := f.g.Calls(); len(calls) != before+1 {
			t.Errorf("next page %s: calls %+v", link, calls[before:])
		}
	}
	f.g.WithNextLink("")
	// Too many groups.
	many := make([]graphtest.Group, 10001)
	for i := range many {
		many[i] = graphtest.Group{ID: uuid.NewString(), SecurityEnabled: true}
	}
	f.g.AddUser(graphtest.User{ID: erin, Groups: many})
	refused("10,001 groups", overage, couldNot)
	f.g.AddUser(graphtest.User{ID: erin, Groups: many[:10000]})
	if m, status, body := f.signIn(t, overage); status != http.StatusOK || len(m.Groups) != 10000 {
		t.Errorf("10,000 groups = %d %s %d", status, body, len(m.Groups))
	}

	// Another tenant, or none: Graph is not called.
	before := len(f.g.Calls())
	for _, tid := range []any{uuid.NewString(), nil} {
		claims := map[string]any{}
		for k, v := range overage {
			claims[k] = v
		}
		claims["tid"] = tid
		refused(fmt.Sprintf("tid %v", tid), claims, "your directory is not the one Quai reads groups from")
	}
	if calls := f.g.Calls(); len(calls) != before {
		t.Errorf("graph called for another tenant: %+v", calls[before:])
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("the endpoint the ID token names was called %d times", n)
	}

	// Without Graph's groups, an overage refuses the sign-in.
	for name, graph := range map[string]func(string) *config.Graph{
		"no graph":     nil,
		"manager only": func(base string) *config.Graph { return &config.Graph{TenantID: tenant, BaseURL: base, Manager: true} },
	} {
		f := newGraphFixture(t, graph, nil)
		f.g.AddUser(graphtest.User{ID: erin, Groups: []graphtest.Group{{ID: admins, SecurityEnabled: true}}})
		for _, claims := range []map[string]any{overage, implicit} {
			if m, status, body := f.signIn(t, claims); status != http.StatusForbidden || !strings.Contains(body, "more groups than the ID token can list") || m.Name != "" {
				t.Errorf("%s: overage sign-in = %d %s", name, status, body)
			}
		}
		if calls := f.g.Calls(); slices.ContainsFunc(calls, func(c graphtest.Call) bool { return c.Path == "/v1.0/me/getMemberObjects" }) {
			t.Errorf("%s: groups read from Graph: %+v", name, calls)
		}
	}
}

// TestGraphManager: auth.graph.manager reads the manager at each sign-in from /me/manager, with
// the sign-in's token, into the directory (open question 69). No manager in Graph, a contact as
// manager, a failed lookup, another tenant: the person signs in, with no manager and the reason,
// never the manager found before.
func TestGraphManager(t *testing.T) {
	ctx := context.Background()
	tenant, erin, mia := uuid.NewString(), uuid.NewString(), uuid.NewString()
	f := newGraphFixture(t, func(base string) *config.Graph {
		return &config.Graph{TenantID: tenant, BaseURL: base, Manager: true}
	}, nil)
	f.g.AddUser(graphtest.User{ID: mia, DisplayName: "Mia"})
	f.g.AddUser(graphtest.User{ID: erin, Manager: strings.ToUpper(mia)})
	claims := map[string]any{"preferred_username": "erin", "oid": erin, "tid": tenant, "groups": []string{"developers"}}
	recorded := func(what, id, source, reason string) {
		t.Helper()
		u, err := f.s.GetUser(ctx, "erin")
		if err != nil || u.ManagerID != id || u.ManagerSource != source || !strings.Contains(u.ManagerReason, reason) || (reason == "" && u.ManagerReason != "") {
			t.Errorf("%s: directory manager = %q %q %q, %v; want %q %q %q", what, u.ManagerID, u.ManagerSource, u.ManagerReason, err, id, source, reason)
		}
	}
	signedIn := func(what string) {
		t.Helper()
		if m, status, body := f.signIn(t, claims); status != http.StatusOK || m.Name != "erin" || !slices.Equal(m.Groups, []string{"developers"}) {
			t.Fatalf("%s: sign-in = %d %s %+v", what, status, body, m)
		}
	}

	signedIn("a manager")
	recorded("a manager", mia, auth.ManagerFromGraph, "")
	if calls := f.g.Calls(); len(calls) != 1 || calls[0] != (graphtest.Call{Method: http.MethodGet, Path: "/v1.0/me/manager", Status: http.StatusOK, User: erin}) {
		t.Errorf("graph calls = %+v", calls)
	}
	if scope := f.lastScope(t); !slices.Contains(scope, f.g.URL()+"/User.Read.All") || slices.Contains(scope, f.g.URL()+"/User.Read") {
		t.Errorf("scope = %v", scope)
	}

	// A failed lookup leaves no manager, not the last one: whoever Graph names now may differ.
	f.g.FailNext(1, http.StatusInternalServerError, "")
	signedIn("an error")
	recorded("an error", "", auth.ManagerFromGraph, "Quai could not read their manager from Microsoft Graph")
	signedIn("again")
	recorded("again", mia, auth.ManagerFromGraph, "")
	f.g.FailNext(3, http.StatusTooManyRequests, "1")
	signedIn("throttled")
	recorded("throttled", "", auth.ManagerFromGraph, "could not read")
	f.g.Consent("User.Read")
	signedIn("User.Read.All not consented")
	recorded("User.Read.All not consented", "", auth.ManagerFromGraph, "could not read")
	f.g.Consent("User.Read", "User.Read.All")

	f.g.AddUser(graphtest.User{ID: erin})
	signedIn("no manager")
	recorded("no manager", "", auth.ManagerFromGraph, "Microsoft Graph has no manager for them")
	f.g.AddUser(graphtest.User{ID: erin, Manager: mia, ManagerIsContact: true})
	signedIn("a contact")
	recorded("a contact", "", auth.ManagerFromGraph, "not a user who can sign in")

	f.g.AddUser(graphtest.User{ID: erin, Manager: mia})
	before := len(f.g.Calls())
	claims["tid"] = uuid.NewString()
	signedIn("another tenant")
	recorded("another tenant", "", auth.ManagerFromGraph, "is not the one Quai reads managers from")
	if calls := f.g.Calls(); len(calls) != before {
		t.Errorf("graph called for another tenant: %+v", calls[before:])
	}
}

// TestManagerClaim: auth.oidc.managerClaim takes the manager's immutable identifier from the ID
// token; a sign-in without it, or with anything but one identifier, records no manager and why.
func TestManagerClaim(t *testing.T) {
	ctx := context.Background()
	p := oidctest.New(t)
	e := quaiEnv(t, p, func(c *config.Config) { c.Auth.OIDC.ManagerClaim = "manager_id" })
	for _, tt := range []struct {
		value      any
		id, reason string
	}{
		{"sub-mia", "sub-mia", ""},
		{nil, "", "their ID token has no manager_id claim"},
		{[]string{"sub-mia", "sub-max"}, "", "is not a single identifier"},
		{"sub mia", "", "is not an identifier"},
		{"", "", "is not an identifier"},
	} {
		p.SetClaims(map[string]any{"preferred_username": "erin", "sub": "sub-erin", "manager_id": tt.value})
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar, Transport: e.c.Transport}
		resp, err := c.Get(e.srv.URL + "/auth/login")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if m, status := getMe(t, c, e.srv.URL); status != http.StatusOK || m.Name != "erin" {
			t.Fatalf("%v: sign-in = %d", tt.value, status)
		}
		u, err := e.s.GetUser(ctx, "erin")
		if err != nil || u.ManagerID != tt.id || u.ManagerSource != auth.ManagerFromClaim || !strings.Contains(u.ManagerReason, tt.reason) {
			t.Errorf("%v: directory manager = %q %q %q, %v", tt.value, u.ManagerID, u.ManagerSource, u.ManagerReason, err)
		}
	}
}
