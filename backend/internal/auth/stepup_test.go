package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/oidctest"
)

// clock is a clock the test moves, shared by Quai and the identity provider.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Now().Truncate(time.Second)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestStepUpCheck: only what the ID token says counts, and anything missing or out of range fails
// closed: no auth_time, one in the future beyond the clock skew, one older than maxAge, an acr
// not among acrValues, acrs without the authentication context; an API token never steps up.
func TestStepUpCheck(t *testing.T) {
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	su := auth.NewStepUp(config.Auth{StepUp: &config.StepUp{MaxAge: 5 * time.Minute, ACRValues: []string{"mfa", "phr"}, AuthContext: "c1"}}, nil)
	su.UseClock(func() time.Time { return now })
	alice := authz.Principal{Name: "alice"}
	fresh := auth.Identity{Principal: alice, AuthTime: now.Add(-time.Minute), ACR: "mfa", ACRS: []string{"c3", "c1"}}
	for _, tt := range []struct {
		name   string
		change func(*auth.Identity)
		want   string // a word of the reason; "" when satisfied
	}{
		{"fresh, with the acr and the context", func(*auth.Identity) {}, ""},
		{"another of the acr values", func(id *auth.Identity) { id.ACR = "phr" }, ""},
		{"exactly maxAge old", func(id *auth.Identity) { id.AuthTime = now.Add(-5 * time.Minute) }, ""},
		{"a second older than maxAge", func(id *auth.Identity) { id.AuthTime = now.Add(-5*time.Minute - time.Second) }, "more than 5m0s ago"},
		{"no auth_time", func(id *auth.Identity) { id.AuthTime = time.Time{} }, "auth_time"},
		{"in the future, within the skew", func(id *auth.Identity) { id.AuthTime = now.Add(auth.ClockSkew) }, ""},
		{"in the future, beyond the skew", func(id *auth.Identity) { id.AuthTime = now.Add(auth.ClockSkew + time.Second) }, "in the future"},
		{"no acr", func(id *auth.Identity) { id.ACR = "" }, "acr none"},
		{"another acr", func(id *auth.Identity) { id.ACR = "pwd" }, `acr "pwd"`},
		{"an acr differing in case", func(id *auth.Identity) { id.ACR = "MFA" }, "acr"},
		{"no acrs", func(id *auth.Identity) { id.ACRS = nil }, "c1"},
		{"acrs without the context", func(id *auth.Identity) { id.ACRS = []string{"c10"} }, "c1"},
		{"an API token, however fresh", func(id *auth.Identity) { id.TokenID = "t1" }, "API token"},
		{"nobody", func(id *auth.Identity) { id.Principal = authz.Principal{} }, "nobody"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := fresh
			id.ACRS = slices.Clone(fresh.ACRS)
			tt.change(&id)
			got := su.Check(id)
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Errorf("Check = %q, want %q", got, tt.want)
			}
			if until := su.Until(id); (got == "") != !until.IsZero() || (got == "" && !until.Equal(id.AuthTime.Add(5*time.Minute))) {
				t.Errorf("Until = %v", until)
			}
			err := su.Require(context.Background(), id, "test", audit.Object{Type: "test", ID: "x"})
			var se *auth.StepUpError
			if got == "" && err != nil || got != "" && (!asStepUp(err, &se) || se.Reason != got) {
				t.Errorf("Require = %v", err)
			}
		})
	}

	// Without auth.stepUp, approval rules still ask for a step-up: an authentication of the last
	// five minutes, of any class.
	def := auth.NewStepUp(config.Auth{}, nil)
	def.UseClock(func() time.Time { return now })
	if def.Configured() || def.Admin() || def.MaxAge() != config.DefaultStepUpMaxAge {
		t.Errorf("default: configured %v, admin %v, maxAge %s", def.Configured(), def.Admin(), def.MaxAge())
	}
	if got := def.Check(auth.Identity{Principal: alice, AuthTime: now.Add(-4 * time.Minute)}); got != "" {
		t.Errorf("default, fresh: %q", got)
	}
	if got := def.Check(auth.Identity{Principal: alice, AuthTime: now.Add(-6 * time.Minute)}); got == "" {
		t.Error("default, six minutes old: satisfied")
	}
	admin := auth.NewStepUp(config.Auth{StepUp: &config.StepUp{Admin: true}}, nil)
	if !admin.Configured() || !admin.Admin() || admin.MaxAge() != config.DefaultStepUpMaxAge {
		t.Errorf("admin: configured %v, admin %v, maxAge %s", admin.Configured(), admin.Admin(), admin.MaxAge())
	}
}

func asStepUp(err error, target **auth.StepUpError) bool {
	se, ok := err.(*auth.StepUpError)
	if ok {
		*target = se
	}
	return ok
}

// TestStepUpLogin: the re-authentication round trip with the fake identity provider. A login with
// stepUp=1 asks the provider to authenticate the person again (prompt=login, max_age), for the
// configured acr and Entra ID authentication context, with the state, nonce and PKCE of a normal
// login, and comes back to the decision; the session then satisfies the step-up until maxAge has
// passed. What the provider did not do does not count.
func TestStepUpLogin(t *testing.T) {
	p := oidctest.New(t)
	p.SetClaims(map[string]any{"preferred_username": "alice", "groups": []string{"platform-admins"}})
	p.SupportACRs("mfa")
	p.SupportAuthContexts("c1")
	e := quaiEnv(t, p, func(c *config.Config) {
		c.Auth.StepUp = &config.StepUp{MaxAge: 5 * time.Minute, ACRValues: []string{"mfa"}, AuthContext: "c1"}
	})
	clk := newClock()
	e.a.UseClock(clk.now)
	p.SetClock(clk.now)
	srv, c := e.srv, e.c
	noRedirect := &http.Client{Jar: c.Jar, Transport: c.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	visit := func(path string) *http.Response {
		t.Helper()
		resp, err := c.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	stepUp := func(want string) {
		t.Helper()
		m, status := getMe(t, c, srv.URL)
		if status != http.StatusOK || m.Name != "alice" || m.StepUp.MaxAge != 300 {
			t.Fatalf("me = %d %+v", status, m)
		}
		switch {
		case want == "" && (!m.StepUp.Satisfied || m.StepUp.Until == nil || m.StepUp.Reason != ""):
			t.Errorf("step-up not satisfied: %+v", m.StepUp)
		case want != "" && (m.StepUp.Satisfied || m.StepUp.Until != nil || !strings.Contains(m.StepUp.Reason, want)):
			t.Errorf("step-up %+v, want refused with %q", m.StepUp, want)
		}
	}

	// A normal login asks for none of it, and does not satisfy the step-up: no acr, no acrs.
	if resp := visit("/auth/login?next=/requests/42"); resp.Request.URL.Path != "/requests/42" {
		t.Fatalf("login lands on %s", resp.Request.URL)
	}
	if a := p.Authorizations()[0]; a.Prompt != "" || a.MaxAge != "" || a.ACRValues != "" || a.Claims != "" {
		t.Errorf("a normal login asked for %+v", a)
	}
	stepUp("acr none")

	// The step-up login: the provider authenticates again, as asked, and the browser comes back
	// to the request.
	resp, err := noRedirect.Get(srv.URL + "/auth/login?stepUp=1&next=" + url.QueryEscape("/requests/42?review=approve"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	authorize := mustURL(resp.Header.Get("Location"))
	q := authorize.Query()
	if q.Get("prompt") != "login" || q.Get("max_age") != "300" || q.Get("acr_values") != "mfa" || q.Get("code_challenge_method") != "S256" ||
		q.Get("state") == "" || q.Get("nonce") == "" || q.Get("claims") != `{"id_token":{"acrs":{"essential":true,"value":"c1"}}}` {
		t.Errorf("the step-up's authorization request: %s", authorize.RawQuery)
	}
	resp, err = c.Get(authorize.String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/requests/42" || resp.Request.URL.RawQuery != "review=approve" {
		t.Errorf("after the step-up the browser is at %s, want /requests/42?review=approve", resp.Request.URL)
	}
	asked := p.Authorizations()
	if last := asked[len(asked)-1]; !last.Authenticated {
		t.Errorf("the provider did not authenticate again: %+v", last)
	}
	stepUp("")
	m, _ := getMe(t, c, srv.URL)
	if !m.StepUp.Until.Equal(clk.now().Add(5 * time.Minute)) {
		t.Errorf("until %v, want %v", m.StepUp.Until, clk.now().Add(5*time.Minute))
	}
	var details map[string]any
	if err := e.s.Pool.QueryRow(context.Background(), "SELECT details FROM audit_events WHERE action = 'auth.login' ORDER BY seq DESC LIMIT 1").Scan(&details); err != nil {
		t.Fatal(err)
	}
	if details["stepUp"] != true || details["acr"] != "mfa" || !slices.Equal(toStrings(details["acrs"]), []string{"c1"}) {
		t.Errorf("auth.login of the step-up = %v", details)
	}

	// Until maxAge has passed.
	clk.add(5 * time.Minute)
	stepUp("")
	clk.add(time.Second)
	stepUp("more than 5m0s ago")
	visit("/auth/login?stepUp=1&next=/requests/42")
	stepUp("")

	// What the provider does not do does not count, though the person is signed in: a context
	// the person cannot satisfy, an acr it ignores, an auth_time missing or in the future.
	for _, tt := range []struct {
		name   string
		setup  func()
		reason string
	}{
		{"the context not satisfied", func() { p.SupportAuthContexts() }, "authentication context c1"},
		{"the acr ignored", func() { p.SupportACRs() }, "acr none"},
		{"no auth_time", func() { p.Tamper(func(c map[string]any) { delete(c, "auth_time") }) }, "auth_time"},
		{"auth_time in the future", func() {
			p.Tamper(func(c map[string]any) { c["auth_time"] = clk.now().Add(10 * time.Minute).Unix() })
		}, "in the future"},
		{"an old auth_time", func() {
			p.Tamper(func(c map[string]any) { c["auth_time"] = clk.now().Add(-time.Hour).Unix() })
		}, "more than 5m0s ago"},
		{"acrs a string", func() { p.Tamper(func(c map[string]any) { c["acrs"] = "c2" }) }, "authentication context c1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup()
			defer func() { p.Tamper(nil); p.SupportACRs("mfa"); p.SupportAuthContexts("c1") }()
			clk.add(time.Minute)
			if resp := visit("/auth/login?stepUp=1&next=/requests/42"); resp.Request.URL.Path != "/requests/42" {
				t.Errorf("the login lands on %s", resp.Request.URL)
			}
			stepUp(tt.reason)
		})
	}

	// The step-up's state, nonce and PKCE verifier are this session's: another browser cannot
	// finish it, and an ID token for another nonce is refused.
	resp, _ = noRedirect.Get(srv.URL + "/auth/login?stepUp=1&next=/requests/42")
	resp.Body.Close()
	resp, _ = noRedirect.Get(resp.Header.Get("Location"))
	resp.Body.Close()
	callback := resp.Header.Get("Location")
	jar, _ := cookiejar.New(nil)
	other := &http.Client{Jar: jar, Transport: c.Transport}
	if resp := send(t, other, http.MethodGet, callback, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("another browser finishing the step-up = %d", resp.StatusCode)
	}
	p.Tamper(func(c map[string]any) { c["nonce"] = "other" })
	if resp := visit("/auth/login?stepUp=1&next=/requests/42"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a step-up with another nonce = %d", resp.StatusCode)
	}
	p.Tamper(nil)

	// An API token never satisfies it.
	m, _ = getMe(t, c, srv.URL)
	resp = send(t, c, http.MethodPost, srv.URL+"/api/v1/tokens", map[string]any{"name": "ci", "scopes": []string{"requests:write"}}, "X-CSRF-Token", m.CSRFToken)
	var tok struct{ Token string }
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	tm, status := getMe(t, &http.Client{}, srv.URL, "Authorization", "Bearer "+tok.Token)
	if status != http.StatusOK || tm.StepUp.Satisfied || !strings.Contains(tm.StepUp.Reason, "API token") {
		t.Errorf("me with a token = %d %+v", status, tm.StepUp)
	}
}

// TestLoginRedirects: where a login comes back to is a path on this server, validated when the
// login starts; anything else, however it is encoded, is /.
func TestLoginRedirects(t *testing.T) {
	p := oidctest.New(t)
	p.SetClaims(map[string]any{"preferred_username": "bob"})
	srv, c := quai(t, p)
	noRedirect := &http.Client{Jar: c.Jar, Transport: c.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// back returns where the callback sends the browser.
	back := func(next string, stepUp bool) string {
		t.Helper()
		u := srv.URL + "/auth/login?next=" + url.QueryEscape(next)
		if stepUp {
			u += "&stepUp=1"
		}
		resp, err := noRedirect.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp, err = noRedirect.Get(resp.Header.Get("Location")); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp, err = noRedirect.Get(resp.Header.Get("Location")); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("callback = %d", resp.StatusCode)
		}
		return resp.Header.Get("Location")
	}
	for _, next := range []string{"/requests/42", "/requests/42?review=approve", "/catalog/vm#prod", "/resources/infra-live%2Fvm%2Fprod%2Fweb-01"} {
		if got := back(next, false); got != next {
			t.Errorf("next %q: back to %q", next, got)
		}
	}
	for _, next := range []string{
		"", "requests/42", "https://evil.example.com/x", "//evil.example.com/x", "///evil.example.com", "/\\evil.example.com",
		"/\\/evil.example.com", "/x\\y", "\\\\evil.example.com", "/\t/evil.example.com", "/\n/evil.example.com", "/x\r\nSet-Cookie: a=b",
		"/%2F/evil.example.com", "/%2f%2fevil.example.com", "/%5Cevil.example.com", "/%5cevil.example.com", "/%09/evil.example.com",
		"/%252F/evil.example.com", "/%25252F%25252Fevil.example.com", "/%0d%0aSet-Cookie:a=b", "/x\x7f", "/%7F", "javascript:alert(1)",
		"/%zz", " /requests/42", "/" + strings.Repeat("a", 2048),
	} {
		for _, stepUp := range []bool{false, true} {
			if got := back(next, stepUp); got != "/" {
				t.Errorf("next %q (step-up %v): back to %q, want /", next, stepUp, got)
			}
		}
	}
}

// TestStepUpDevSwitcher: in development, signing in again with the switcher steps up.
func TestStepUpDevSwitcher(t *testing.T) {
	e := quaiEnv(t, nil, func(c *config.Config) { c.Auth.StepUp = &config.StepUp{MaxAge: 10 * time.Minute} })
	clk := newClock()
	e.a.UseClock(clk.now)
	login := func() {
		t.Helper()
		m, _ := getMe(t, e.c, e.srv.URL)
		if resp := send(t, e.c, http.MethodPost, e.srv.URL+"/auth/dev/login", map[string]any{"name": "alice"}, "X-CSRF-Token", m.CSRFToken); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("dev login = %d", resp.StatusCode)
		}
	}
	login()
	if m, _ := getMe(t, e.c, e.srv.URL); !m.StepUp.Satisfied || m.StepUp.MaxAge != 600 {
		t.Errorf("after the login: %+v", m.StepUp)
	}
	clk.add(11 * time.Minute)
	if m, _ := getMe(t, e.c, e.srv.URL); m.StepUp.Satisfied || !strings.Contains(m.StepUp.Reason, "more than 10m0s ago") {
		t.Errorf("eleven minutes later: %+v", m.StepUp)
	}
	login()
	if m, _ := getMe(t, e.c, e.srv.URL); !m.StepUp.Satisfied {
		t.Errorf("after signing in again: %+v", m.StepUp)
	}
}

func toStrings(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			s, _ := x.(string)
			out = append(out, s)
		}
	}
	return out
}
