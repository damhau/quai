package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/damhau/quai/backend/internal/api"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// idp is a mock OpenID Connect provider: discovery, keys, authorize and token endpoints, with
// PKCE (S256) and nonces checked as a real provider does.
type idp struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	codes  map[string]grant
	claims map[string]any // claims of the next login
	// tamper changes what the token endpoint returns, for failure tests.
	tamper func(claims map[string]any)
}

type grant struct {
	nonce, challenge string
	claims           map[string]any
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &idp{key: key, codes: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token",
			"jwks_uri": p.srv.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "PKCE required", http.StatusBadRequest)
			return
		}
		code := base64.RawURLEncoding.EncodeToString([]byte(time.Now().String()))
		p.mu.Lock()
		p.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), claims: p.claims}
		p.mu.Unlock()
		u, _ := url.Parse(q.Get("redirect_uri"))
		v := u.Query()
		v.Set("code", code)
		v.Set("state", q.Get("state"))
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		p.mu.Lock()
		g, ok := p.codes[r.Form.Get("code")]
		delete(p.codes, r.Form.Get("code"))
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error": "invalid_grant"}`))
			return
		}
		claims := map[string]any{
			"iss": p.srv.URL, "aud": "quai", "sub": "user-1", "nonce": g.nonce,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "auth_time": time.Now().Unix(),
		}
		for k, v := range g.claims {
			claims[k] = v
		}
		if p.tamper != nil {
			p.tamper(claims)
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
		idToken, _ := jwt.Signed(signer).Claims(claims).Serialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// quai starts the API with OIDC login against p, or the dev switcher when p is nil.
func quai(t *testing.T, p *idp) (*httptest.Server, *http.Client) {
	t.Helper()
	cfg := &config.Config{Env: config.Development}
	srv := httptest.NewUnstartedServer(nil)
	if p != nil {
		cfg.Auth = config.Auth{Mode: config.AuthOIDC, OIDC: config.OIDC{
			Issuer: p.srv.URL, ClientID: "quai", ClientSecret: "s", RedirectURL: "http://" + srv.Listener.Addr().String() + "/auth/callback",
			Scopes: []string{"openid"}, NameClaim: "preferred_username", GroupsClaim: "groups",
		}}
	} else {
		cfg.Auth = config.Auth{Mode: config.AuthDev, Dev: config.Dev{Users: []config.DevUser{{Name: "alice", Groups: []string{"platform-admins"}}}}}
	}
	cfg.Auth.Roles = []config.RoleMapping{{Group: "developers", Role: "requester"}, {Group: "shop", Role: "operator", Scope: "team:shop"}, {Group: "platform-admins", Role: "platform-admin"}}
	cfg.Auth.Session = config.Session{Lifetime: time.Hour, IdleTimeout: time.Hour}
	s := pgtest.Store(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv.Config.Handler = api.New(api.Options{Version: "test", Env: cfg.Env, Logger: log, Store: s, Auth: auth.New(cfg, s, log)})
	srv.Start()
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return srv, &http.Client{Jar: jar}
}

type me struct {
	Name      string
	Email     string
	Groups    []string
	Roles     []struct{ Role, Scope string }
	Scopes    []string
	CSRFToken string
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
	p := newIDP(t)
	p.claims = map[string]any{"preferred_username": "bob", "email": "bob@example.com", "groups": []string{"developers", "shop"}}
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
}

func mustURL(s string) *url.URL {
	u, _ := url.Parse(s)
	return u
}

func TestOIDCRefusals(t *testing.T) {
	p := newIDP(t)
	p.claims = map[string]any{"preferred_username": "bob"}
	srv, c := quai(t, p)
	noRedirect := &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

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
		p.tamper = tamper
		if status := login(); status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Errorf("%s: login = %d", name, status)
		}
		if _, status := getMe(t, c, srv.URL); status != http.StatusUnauthorized {
			t.Errorf("%s: signed in anyway", name)
		}
	}
	p.tamper = nil

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
	p := newIDP(t)
	p.claims = map[string]any{"preferred_username": "bob", "groups": []string{"developers"}}
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
	if status != http.StatusOK || m.Name != "alice" || m.Roles[0].Role != "platform-admin" {
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
