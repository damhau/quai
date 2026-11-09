// Package graphtest fakes the Microsoft Graph endpoints Quai calls at an Entra ID sign-in, for
// tests (docs/plan/phase-4-backlog.md#services-with-no-account): there is no Entra ID tenant to
// test with. It is an HTTPS server with a certificate of its own (CAFile), as Graph is behind
// netx, that implements only what Quai calls, for the signed-in person (/me), with the access
// tokens it issues (Issue, which internal/oidctest's token endpoint calls), and keeps the calls it
// received for the test to read:
//
//   - POST /v1.0/me/getMemberObjects, a JSON body {"securityEnabledOnly": true|false}: the IDs of
//     the person's groups (only the security groups when true), as {"@odata.context": ...,
//     "value": [...]}. It needs the delegated permission User.Read (or a higher one: User.Read.All,
//     Directory.Read.All). A person's groups are given as they are, transitive ones included.
//   - GET /v1.0/me/manager: the person's manager, a user {"@odata.context", "@odata.type":
//     "#microsoft.graph.user", "id", "displayName", "mail", "userPrincipalName"} or an organizational
//     contact (#microsoft.graph.orgContact); 404 when they have none. It needs User.Read.All (or
//     Directory.Read.All).
//
// Every call needs Authorization: Bearer and a token Issue made that has not expired (401
// otherwise); a token without the permission the call needs is refused (403). Errors are
// {"error": {"code", "message", "innerError": {"date", "request-id", "client-request-id"}}}.
// FailNext makes the next calls answer an error instead, a 429 with Retry-After included.
//
// It follows Microsoft's documentation, read on 2026-11-09:
//   - directoryObject: getMemberObjects
//     (https://learn.microsoft.com/en-us/graph/api/directoryobject-getmemberobjects?view=graph-rest-1.0):
//     /me/getMemberObjects, its body, the String collection it answers, User.Read for the signed-in
//     person; no paging is described.
//   - List manager (https://learn.microsoft.com/en-us/graph/api/user-list-manager?view=graph-rest-1.0):
//     /me/manager, User.Read.All (application permissions are not supported), a user or an
//     organizational contact, 404 Not Found when no manager is assigned.
//   - ID token claims reference
//     (https://learn.microsoft.com/en-us/entra/identity-platform/id-token-claims-reference): the
//     group overage (more than 200 groups in a JWT), and getMemberObjects to read the groups.
//   - Microsoft Graph error responses and resource types (https://learn.microsoft.com/en-us/graph/errors):
//     the status codes and the error body.
//   - Microsoft Graph throttling guidance (https://learn.microsoft.com/en-us/graph/throttling):
//     429 Too Many Requests with Retry-After in seconds, and its sample body.
//   - Paging Microsoft Graph data in your app (https://learn.microsoft.com/en-us/graph/paging):
//     @odata.nextLink, an absolute URL to use as it is; WithNextLink adds one to getMemberObjects'
//     answer, which its page does not describe, to test that Quai refuses it.
//   - Overview of Microsoft Graph permissions (https://learn.microsoft.com/en-us/graph/permissions-overview)
//     and Microsoft Graph national cloud deployments (https://learn.microsoft.com/en-us/graph/deployments):
//     delegated permissions, and Graph's root per cloud, under which scopes are written
//     (https://graph.microsoft.com/User.Read).
//
// What the pages do not say, the fake assumes, and it is not verified against Microsoft Graph:
// the error codes of an unknown or expired token (InvalidAuthenticationToken), of a missing
// permission (Authorization_RequestDenied) and of no manager (Request_ResourceNotFound); that a
// token's permissions are those its sign-in asked for among those consented (Consent); and that
// getMemberObjects answers every ID at once.
package graphtest

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// User is a person of the fake's directory.
type User struct {
	ID          string // the object ID: the ID token's oid
	DisplayName string
	Mail        string
	// Groups are the person's groups, transitive ones included.
	Groups []Group
	// Manager is the object ID of the person's manager, a user of the fake or not; empty: none.
	Manager string
	// ManagerIsContact makes the manager an organizational contact.
	ManagerIsContact bool
}

// Group is a group a person is a member of.
type Group struct {
	ID              string
	SecurityEnabled bool
}

// Call is a request the fake received.
type Call struct {
	Method, Path string
	Status       int
	// User is the object ID the token was issued for; empty when there was no valid token.
	User string
}

type token struct {
	user    string
	scopes  []string
	expired bool
}

type failure struct {
	status     int
	retryAfter string
}

// Graph is the fake Microsoft Graph.
type Graph struct {
	srv *httptest.Server

	mu        sync.Mutex
	users     map[string]User
	tokens    map[string]*token
	consented []string
	failures  []failure
	nextLink  string
	calls     []Call
}

// New starts the fake, stopped at the end of the test. User.Read and User.Read.All are consented.
func New(t testing.TB) *Graph {
	t.Helper()
	g := &Graph{users: map[string]User{}, tokens: map[string]*token{}, consented: []string{"User.Read", "User.Read.All"}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1.0/me/getMemberObjects", g.memberObjects)
	mux.HandleFunc("GET /v1.0/me/manager", g.manager)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if g.authorize(w, r, "") == nil {
			return
		}
		g.fail(w, r, http.StatusNotFound, "Request_ResourceNotFound", "Resource not found for the segment.")
	})
	g.srv = httptest.NewTLSServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

// URL is the fake's root, for auth.graph.baseURL.
func (g *Graph) URL() string { return g.srv.URL }

// CAFile writes the fake's certificate to a PEM file, for network.caFile.
func (g *Graph) CAFile(t testing.TB) string {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "graph-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: g.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return ca
}

// AddUser adds a person to the directory, or replaces them.
func (g *Graph) AddUser(u User) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.users[strings.ToLower(u.ID)] = u
}

// Consent sets the delegated permissions an administrator consented to: a token gets those its
// sign-in asked for among them.
func (g *Graph) Consent(permissions ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.consented = permissions
}

// Issue returns an access token for Graph for the person an ID token's claims name (oid), with
// the permissions the sign-in's scope asked for (<URL>/User.Read) among those consented. It is
// what oidctest.Provider.IssueAccessTokens takes.
func (g *Graph) Issue(claims map[string]any, scope string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	oid, _ := claims["oid"].(string)
	t := &token{user: strings.ToLower(oid)}
	for _, s := range strings.Fields(scope) {
		if p, ok := strings.CutPrefix(s, g.srv.URL+"/"); ok && slices.Contains(g.consented, p) {
			t.scopes = append(t.scopes, p)
		}
	}
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	v := base64.RawURLEncoding.EncodeToString(b)
	g.tokens[v] = t
	return v
}

// ExpireTokens makes every token issued so far expired: Graph answers 401.
func (g *Graph) ExpireTokens() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, t := range g.tokens {
		t.expired = true
	}
}

// FailNext makes the next n calls answer status, with a Retry-After header when retryAfter is not
// empty: 429 with the throttling page's body, or another error.
func (g *Graph) FailNext(n, status int, retryAfter string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for range n {
		g.failures = append(g.failures, failure{status: status, retryAfter: retryAfter})
	}
}

// WithNextLink makes getMemberObjects' answers carry @odata.nextLink link; empty: none.
func (g *Graph) WithNextLink(link string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nextLink = link
}

// Calls returns the calls the fake received, oldest first.
func (g *Graph) Calls() []Call {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// authorize returns the person the request's token names when it may make the call, which needs
// one of permissions (none: any token); otherwise it answers the error, and returns nil.
func (g *Graph) authorize(w http.ResponseWriter, r *http.Request, permissions ...string) *User {
	g.mu.Lock()
	if len(g.failures) > 0 {
		f := g.failures[0]
		g.failures = g.failures[1:]
		g.mu.Unlock()
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		if f.status == http.StatusTooManyRequests {
			g.fail(w, r, f.status, "TooManyRequests", "Please retry again later.")
		} else {
			g.fail(w, r, f.status, "serviceNotAvailable", "The service is temporarily unavailable.")
		}
		return nil
	}
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	t := g.tokens[raw]
	var u User
	if t != nil {
		u, ok = g.users[t.user]
		ok = ok && raw != ""
	}
	g.mu.Unlock()
	switch {
	case !ok || t == nil || t.expired:
		g.fail(w, r, http.StatusUnauthorized, "InvalidAuthenticationToken", "Access token has expired or is not yet valid.")
		return nil
	case len(permissions) > 0 && permissions[0] != "" && !slices.ContainsFunc(t.scopes, func(s string) bool { return slices.Contains(permissions, s) }):
		g.record(r, http.StatusForbidden, t.user)
		g.write(w, http.StatusForbidden, errorBody("Authorization_RequestDenied", "Insufficient privileges to complete the operation."))
		return nil
	}
	return &u
}

func (g *Graph) memberObjects(w http.ResponseWriter, r *http.Request) {
	u := g.authorize(w, r, "User.Read", "User.Read.All", "Directory.Read.All")
	if u == nil {
		return
	}
	var body struct {
		SecurityEnabledOnly *bool `json:"securityEnabledOnly"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SecurityEnabledOnly == nil {
		g.record(r, http.StatusBadRequest, u.ID)
		g.write(w, http.StatusBadRequest, errorBody("Request_BadRequest", "securityEnabledOnly is required."))
		return
	}
	ids := []string{}
	for _, gr := range u.Groups {
		if gr.SecurityEnabled || !*body.SecurityEnabledOnly {
			ids = append(ids, gr.ID)
		}
	}
	out := map[string]any{"@odata.context": g.srv.URL + "/v1.0/$metadata#Collection(Edm.String)", "value": ids}
	g.mu.Lock()
	if g.nextLink != "" {
		out["@odata.nextLink"] = g.nextLink
	}
	g.mu.Unlock()
	g.record(r, http.StatusOK, u.ID)
	g.write(w, http.StatusOK, out)
}

func (g *Graph) manager(w http.ResponseWriter, r *http.Request) {
	u := g.authorize(w, r, "User.Read.All", "Directory.Read.All")
	if u == nil {
		return
	}
	if u.Manager == "" {
		g.record(r, http.StatusNotFound, u.ID)
		g.write(w, http.StatusNotFound, errorBody("Request_ResourceNotFound",
			"Resource 'manager' does not exist or one of its queried reference-property objects are not present."))
		return
	}
	g.mu.Lock()
	m, known := g.users[strings.ToLower(u.Manager)]
	g.mu.Unlock()
	if !known {
		m = User{ID: u.Manager}
	}
	out := map[string]any{"@odata.context": g.srv.URL + "/v1.0/$metadata#directoryObjects/$entity", "@odata.type": "#microsoft.graph.user",
		"id": m.ID, "displayName": m.DisplayName, "mail": m.Mail, "userPrincipalName": m.Mail}
	if u.ManagerIsContact {
		out = map[string]any{"@odata.context": g.srv.URL + "/v1.0/$metadata#directoryObjects/$entity", "@odata.type": "#microsoft.graph.orgContact",
			"id": m.ID, "displayName": m.DisplayName, "mail": m.Mail}
	}
	g.record(r, http.StatusOK, u.ID)
	g.write(w, http.StatusOK, out)
}

func (g *Graph) fail(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	g.record(r, status, "")
	g.write(w, status, errorBody(code, message))
}

func (g *Graph) record(r *http.Request, status int, user string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, Call{Method: r.Method, Path: r.URL.Path, Status: status, User: user})
}

func (g *Graph) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func errorBody(code, message string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message, "innerError": map[string]any{
		"date": time.Now().UTC().Format("2006-01-02T15:04:05"), "request-id": random(), "client-request-id": random()}}}
}

func random() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
