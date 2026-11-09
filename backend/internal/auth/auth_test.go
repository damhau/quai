package auth_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/api"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/oidctest"
	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/store"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// quai starts the API with OIDC login against p, or the dev switcher when p is nil.
func quai(t *testing.T, p *oidctest.Provider) (*httptest.Server, *http.Client) {
	t.Helper()
	srv, c, _ := quaiWith(t, p, nil)
	return srv, c
}

// quaiWith is quai with a change to its configuration, when change is not nil; it returns the
// store too.
func quaiWith(t *testing.T, p *oidctest.Provider, change func(*config.Config)) (*httptest.Server, *http.Client, *store.Store) {
	t.Helper()
	e := quaiEnv(t, p, change)
	return e.srv, e.c, e.s
}

// env is Quai's test server, a browser signed in to nobody, the store and the authenticator.
type env struct {
	srv *httptest.Server
	c   *http.Client
	s   *store.Store
	a   *auth.Authenticator
}

func quaiEnv(t *testing.T, p *oidctest.Provider, change func(*config.Config)) env {
	t.Helper()
	cfg := &config.Config{Env: config.Development}
	srv := httptest.NewUnstartedServer(nil)
	if p != nil {
		cfg.Auth = config.Auth{Mode: config.AuthOIDC, OIDC: config.OIDC{
			Issuer: p.Issuer(), ClientID: oidctest.ClientID, ClientSecret: "s", RedirectURL: "http://" + srv.Listener.Addr().String() + "/auth/callback",
			Scopes: []string{"openid"}, NameClaim: "preferred_username", GroupsClaim: "groups",
		}}
	} else {
		cfg.Auth = config.Auth{Mode: config.AuthDev, Dev: config.Dev{Users: []config.DevUser{{Name: "alice", Groups: []string{"platform-admins"}}}}}
	}
	cfg.Auth.Roles = []config.RoleMapping{{Group: "developers", Role: "requester"}, {Group: "shop", Role: "operator", Scope: "team:shop"}, {Group: "platform-admins", Role: "platform-admin"}}
	cfg.Auth.Session = config.Session{Lifetime: time.Hour, IdleTimeout: time.Hour}
	if change != nil {
		change(cfg)
	}
	s := pgtest.Store(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var client *http.Client
	browser := http.DefaultTransport
	if p != nil {
		// Over HTTPS with a CA of its own, which Quai trusts through network.caFile, as an
		// internal identity provider would be; a test that reaches other fakes names a bundle.
		ca := cfg.Network.CAFile
		if ca == "" {
			ca = p.CAFile(t)
		}
		network, err := netx.New(ca)
		if err != nil {
			t.Fatal(err)
		}
		client, browser = network.Client(), p.Transport()
	}
	a := auth.New(cfg, s, client, log)
	srv.Config.Handler = api.New(api.Options{Version: "test", Env: cfg.Env, Logger: log, Store: s, Auth: a})
	srv.Start()
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return env{srv: srv, c: &http.Client{Jar: jar, Transport: browser}, s: s, a: a}
}

type me struct {
	Name      string
	Email     string
	Groups    []string
	Teams     []string
	Roles     []struct{ Role, Scope string }
	Scopes    []string
	CSRFToken string
	StepUp    struct {
		Satisfied bool
		Reason    string
		Until     *time.Time
		MaxAge    int
	}
}

func getMe(t *testing.T, c *http.Client, base string, header ...string) (me, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/me", nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m me
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return m, resp.StatusCode
}

func send(t *testing.T, c *http.Client, method, u string, body any, header ...string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u, r)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestOIDCLogin(t *testing.T) {
	p := oidctest.New(t)
	claims := map[string]any{"preferred_username": "bob", "email": "bob@example.com", "email_verified": true, "groups": []string{"developers", "shop"}}
	p.SetClaims(claims)
	srv, c := quai(t, p)

	if _, status := getMe(t, c, srv.URL); status != http.StatusUnauthorized {
		t.Fatalf("before login: %d", status)
	}
	resp, err := c.Get(srv.URL + "/auth/login?next=/requests/42")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/requests/42" {
		t.Errorf("after login the browser is at %s, want /requests/42", resp.Request.URL)
	}
	m, status := getMe(t, c, srv.URL)
	if status != http.StatusOK || m.Name != "bob" || m.Email != "bob@example.com" || len(m.Groups) != 2 || m.CSRFToken == "" {
		t.Fatalf("me = %d %+v", status, m)
	}
	if len(m.Roles) != 2 || m.Roles[0].Role != "requester" || m.Roles[0].Scope != "global" || m.Roles[1].Scope != "team:shop" {
		t.Errorf("roles = %+v", m.Roles)
	}
	cookie := c.Jar.Cookies(mustURL(srv.URL))[0]
	if cookie.Name != "quai_session" {
		t.Errorf("cookie = %+v", cookie)
	}

	// Changing state needs the CSRF token.
	if resp := send(t, c, http.MethodPost, srv.URL+"/auth/logout", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("logout without CSRF token = %d", resp.StatusCode)
	}
	if resp := send(t, c, http.MethodPost, srv.URL+"/auth/logout", nil, "X-CSRF-Token", m.CSRFToken); resp.StatusCode != http.StatusNoContent {
		t.Errorf("logout = %d", resp.StatusCode)
	}
	if _, status := getMe(t, c, srv.URL); status != http.StatusUnauthorized {
		t.Errorf("after logout: %d", status)
	}
	// An open redirect is refused: the browser lands on /.
	resp, _ = c.Get(srv.URL + "/auth/login?next=//evil.example.com/x")
	resp.Body.Close()
	if resp.Request.URL.Host != mustURL(srv.URL).Host || resp.Request.URL.Path != "/" {
		t.Errorf("open redirect to %s", resp.Request.URL)
	}

	// An email the IdP says it has not verified is not kept: owners named by email would match it.
	claims["email_verified"] = false
	p.SetClaims(claims)
	resp, _ = c.Get(srv.URL + "/auth/login")
	resp.Body.Close()
	if m, status := getMe(t, c, srv.URL); status != http.StatusOK || m.Name != "bob" || m.Email != "" {
		t.Errorf("unverified email: me = %d %+v", status, m)
	}
}

// TestOIDCEmail: a login's email counts only when the identity provider vouches for it, with
// email_verified or Entra ID's xms_edov the JSON boolean true; any other email is dropped, since
// owners named by email would match it. Without a name claim, a vouched-for email names the
// person, and anything less refuses the login, saying which claims are missing.
func TestOIDCEmail(t *testing.T) {
	p := oidctest.New(t)
	srv, c := quai(t, p)
	const email = "erin@example.com"
	for _, tt := range []struct {
		name   string
		claims map[string]any
		want   string // the session's name and email, or the refusal's words
		status int
	}{
		{"email_verified true", map[string]any{"email_verified": true}, "erin " + email, http.StatusOK},
		{"email_verified false", map[string]any{"email_verified": false}, "erin ", http.StatusOK},
		{"email_verified a string", map[string]any{"email_verified": "true"}, "erin ", http.StatusOK},
		{"email_verified a number", map[string]any{"email_verified": 1}, "erin ", http.StatusOK},
		{"xms_edov true, email_verified absent", map[string]any{"xms_edov": true}, "erin " + email, http.StatusOK},
		{"xms_edov false", map[string]any{"xms_edov": false}, "erin ", http.StatusOK},
		{"xms_edov a string", map[string]any{"xms_edov": "1"}, "erin ", http.StatusOK},
		{"both absent", map[string]any{}, "erin ", http.StatusOK},
		{"no name, a verified email", map[string]any{"preferred_username": nil, "email_verified": true}, email + " " + email, http.StatusOK},
		{"no name, xms_edov", map[string]any{"preferred_username": nil, "xms_edov": true}, email + " " + email, http.StatusOK},
		{"no name, an unverified email", map[string]any{"preferred_username": nil, "email_verified": false},
			"no preferred_username claim to name you by, and its email claim is not verified (neither email_verified nor xms_edov is true)", http.StatusForbidden},
		{"no name, an email without email_verified", map[string]any{"preferred_username": nil},
			"no preferred_username claim to name you by, and its email claim is not verified", http.StatusForbidden},
		{"no name, no email", map[string]any{"preferred_username": nil, "email": nil},
			"no preferred_username claim to name you by, nor an email claim", http.StatusForbidden}, // the sub claim names no one
	} {
		t.Run(tt.name, func(t *testing.T) {
			claims := map[string]any{"preferred_username": "erin", "email": email, "groups": []string{"developers"}}
			for k, v := range tt.claims {
				if v == nil {
					delete(claims, k)
				} else {
					claims[k] = v
				}
			}
			p.SetClaims(claims)
			jar, _ := cookiejar.New(nil)
			browser := &http.Client{Jar: jar, Transport: c.Transport}
			resp, err := browser.Get(srv.URL + "/auth/login")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			m, status := getMe(t, browser, srv.URL)
			if tt.status == http.StatusOK {
				if status != http.StatusOK || m.Name+" "+m.Email != tt.want {
					t.Errorf("me = %d %q %q, want %q", status, m.Name, m.Email, tt.want)
				}
				return
			}
			if resp.StatusCode != tt.status || !strings.Contains(string(body), tt.want) {
				t.Errorf("login = %d %s, want %d and %q", resp.StatusCode, body, tt.status, tt.want)
			}
			if status != http.StatusUnauthorized {
				t.Errorf("signed in anyway: me = %d %+v", status, m)
			}
		})
	}
}

func mustURL(s string) *url.URL {
	u, _ := url.Parse(s)
	return u
}

func TestOIDCRefusals(t *testing.T) {
	p := oidctest.New(t)
	p.SetClaims(map[string]any{"preferred_username": "bob"})
	srv, c := quai(t, p)
	noRedirect := &http.Client{Jar: c.Jar, Transport: c.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// A callback without a login started here.
	if resp := send(t, c, http.MethodGet, srv.URL+"/auth/callback?code=x&state=forged", nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("forged state = %d", resp.StatusCode)
	}
	// The provider's error is reported.
	resp, _ := noRedirect.Get(srv.URL + "/auth/login")
	resp.Body.Close()
	if resp := send(t, c, http.MethodGet, srv.URL+"/auth/callback?error=access_denied", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("provider error = %d", resp.StatusCode)
	}

	login := func() int {
		t.Helper()
		resp, err := c.Get(srv.URL + "/auth/login")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for name, tamper := range map[string]func(map[string]any){
		"wrong nonce":    func(c map[string]any) { c["nonce"] = "other" },
		"wrong audience": func(c map[string]any) { c["aud"] = "someone-else" },
		"expired":        func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		"group overage":  func(c map[string]any) { c["_claim_names"] = map[string]any{"groups": "src1"} },
	} {
		p.Tamper(tamper)
		if status := login(); status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Errorf("%s: login = %d", name, status)
		}
		if _, status := getMe(t, c, srv.URL); status != http.StatusUnauthorized {
			t.Errorf("%s: signed in anyway", name)
		}
	}
	p.Tamper(nil)

	// A code exchanged without the right PKCE verifier is refused by the provider.
	resp, _ = noRedirect.Get(srv.URL + "/auth/login")
	resp.Body.Close()
	authorizeURL := resp.Header.Get("Location")
	u := mustURL(authorizeURL)
	q := u.Query()
	q.Set("code_challenge", "AAAA")
	u.RawQuery = q.Encode()
	resp, _ = noRedirect.Get(u.String())
	resp.Body.Close()
	callback := resp.Header.Get("Location")
	if resp := send(t, c, http.MethodGet, callback, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong PKCE challenge = %d", resp.StatusCode)
	}
}

func TestAPITokens(t *testing.T) {
	p := oidctest.New(t)
	p.SetClaims(map[string]any{"preferred_username": "bob", "groups": []string{"developers"}})
	srv, c := quai(t, p)
	resp, _ := c.Get(srv.URL + "/auth/login")
	resp.Body.Close()
	m, _ := getMe(t, c, srv.URL)
	csrf := []string{"X-CSRF-Token", m.CSRFToken}

	create := func(body map[string]any) (*http.Response, map[string]any) {
		resp := send(t, c, http.MethodPost, srv.URL+"/api/v1/tokens", body, csrf...)
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}
	resp, tok := create(map[string]any{"name": "ci", "scopes": []string{"requests:write", "catalog:read"}, "expiresInDays": 30})
	if resp.StatusCode != http.StatusCreated || !strings.HasPrefix(tok["token"].(string), auth.TokenPrefix) {
		t.Fatalf("create = %d %v", resp.StatusCode, tok)
	}
	if resp, _ := create(map[string]any{"name": "ci", "scopes": []string{"catalog:read"}}); resp.StatusCode != http.StatusConflict {
		t.Errorf("duplicate name = %d", resp.StatusCode)
	}
	if resp, _ := create(map[string]any{"name": "x", "scopes": []string{"everything"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("unknown scope = %d", resp.StatusCode)
	}

	// The token identifies its owner, with the owner's roles and its scopes, without a cookie
	// or a CSRF token.
	bearer := []string{"Authorization", "Bearer " + tok["token"].(string)}
	plain := &http.Client{}
	tm, status := getMe(t, plain, srv.URL, bearer...)
	if status != http.StatusOK || tm.Name != "bob" || len(tm.Scopes) != 2 || len(tm.Roles) != 1 || tm.CSRFToken != "" {
		t.Errorf("me with the token = %d %+v", status, tm)
	}
	// Tokens cannot make tokens.
	if resp := send(t, plain, http.MethodPost, srv.URL+"/api/v1/tokens", map[string]any{"name": "y", "scopes": []string{"catalog:read"}}, bearer...); resp.StatusCode != http.StatusForbidden {
		t.Errorf("token creating a token = %d", resp.StatusCode)
	}
	if _, status := getMe(t, plain, srv.URL, "Authorization", "Bearer quai_unknown"); status != http.StatusUnauthorized {
		t.Errorf("unknown token = %d", status)
	}
	if _, status := getMe(t, plain, srv.URL, "Authorization", "Basic abc"); status != http.StatusUnauthorized {
		t.Errorf("basic auth = %d", status)
	}

	resp = send(t, c, http.MethodGet, srv.URL+"/api/v1/tokens", nil)
	var list struct{ Tokens []map[string]any }
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Tokens) != 1 || list.Tokens[0]["lastUsedAt"] == nil || list.Tokens[0]["token"] != nil {
		t.Errorf("list = %+v", list)
	}
	id := tok["id"].(string)
	if resp := send(t, c, http.MethodDelete, srv.URL+"/api/v1/tokens/"+id, nil, csrf...); resp.StatusCode != http.StatusNoContent {
		t.Errorf("revoke = %d", resp.StatusCode)
	}
	if resp := send(t, c, http.MethodDelete, srv.URL+"/api/v1/tokens/"+id, nil, csrf...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("revoke twice = %d", resp.StatusCode)
	}
	if _, status := getMe(t, plain, srv.URL, bearer...); status != http.StatusUnauthorized {
		t.Errorf("revoked token = %d", status)
	}
	// Nobody signed in.
	if resp := send(t, plain, http.MethodGet, srv.URL+"/api/v1/tokens", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous list = %d", resp.StatusCode)
	}
}

func TestDevSwitcher(t *testing.T) {
	srv, c := quai(t, nil)
	resp := send(t, c, http.MethodGet, srv.URL+"/auth/dev/users", nil)
	var users []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&users)
	if len(users) != 1 || users[0]["Name"] != "alice" {
		t.Errorf("users = %v", users)
	}
	if resp := send(t, c, http.MethodPost, srv.URL+"/auth/dev/login", map[string]any{"name": "mallory"}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown dev user = %d", resp.StatusCode)
	}
	if resp := send(t, c, http.MethodPost, srv.URL+"/auth/dev/login", "not an object"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad body = %d", resp.StatusCode)
	}
	if resp := send(t, c, http.MethodPost, srv.URL+"/auth/dev/login", map[string]any{"name": "alice"}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("dev login = %d", resp.StatusCode)
	}
	m, status := getMe(t, c, srv.URL)
	if status != http.StatusOK || m.Name != "alice" || m.Roles[0].Role != "platform-admin" || !slices.Equal(m.Teams, []string{"platform-admins"}) {
		t.Errorf("me = %d %+v", status, m)
	}
	// The OIDC endpoints do not exist in dev mode.
	if resp := send(t, c, http.MethodGet, srv.URL+"/auth/login", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/auth/login in dev mode = %d", resp.StatusCode)
	}
}

func TestIdentityHelpers(t *testing.T) {
	if _, ok := auth.FromContext(context.Background()); ok {
		t.Error("an identity without a context")
	}
	tok, hash := auth.NewToken()
	if len(tok) < 40 || !bytes.Equal(hash, auth.HashToken(tok)) {
		t.Errorf("token %q", tok)
	}
}
