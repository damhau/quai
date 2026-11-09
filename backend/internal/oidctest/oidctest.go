// Package oidctest is a fake OpenID Connect provider for tests: an httptest server over HTTPS with
// a CA of its own, as an internal identity provider would be, serving discovery, keys, the
// authorization endpoint (the code flow with PKCE, S256 only) and the token endpoint, which issues
// RS256 ID tokens. It checks what a real provider checks (the PKCE verifier, single-use codes) and
// keeps a session per subject, as a browser signed in to the provider has, so that it can answer
// re-authentication requests as a provider does.
//
// It follows OpenID Connect Core 1.0 (https://openid.net/specs/openid-connect-core-1_0.html):
// prompt=login and max_age, which make the person authenticate again, and the auth_time claim,
// which is then required (section 3.1.2.1); acr_values, voluntary, answered with the acr claim of
// the class the person authenticated with (sections 2 and 3.1.2.1); the claims parameter (section
// 5.5). And Microsoft Entra ID's Conditional Access authentication contexts, from these pages,
// read on 2026-11-09:
//
//   - https://learn.microsoft.com/en-us/entra/identity-platform/developer-guide-conditional-access-authentication-context
//     (contexts c1 to c99; a context requested and satisfied is added to the acrs claim, one not
//     satisfied is not);
//   - https://learn.microsoft.com/en-us/entra/identity-platform/claims-challenge (the claims
//     request parameter, {"id_token":{"acrs":{"essential":true,"value":"c1"}}} for an ID token, as
//     Microsoft's web app sample asks);
//   - https://learn.microsoft.com/en-us/entra/identity-platform/optional-claims-reference (acrs,
//     the authentication context IDs, and auth_time).
//
// Anything else a login's ID token holds (name, email, groups, Entra ID's oid and tid, a manager
// claim) is a claim the test sets with SetClaims, Entra ID's group overage included: without the
// groups claim, _claim_names {"groups": "src1"} and _claim_sources {"src1": {"endpoint": ...}}, or
// hasgroups true in the implicit flow, as the ID token claims reference describes
// (https://learn.microsoft.com/en-us/entra/identity-platform/id-token-claims-reference, read on
// 2026-11-09). The access token is "at" unless the test issues its own (IssueAccessTokens), as
// internal/graphtest does for Microsoft Graph.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// ClientID is the client the provider issues ID tokens to.
const ClientID = "quai"

// Subject is the sub of a login whose claims do not set one.
const Subject = "user-1"

// Provider is a fake OpenID Connect provider.
type Provider struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu       sync.Mutex
	now      func() time.Time
	codes    map[string]grant
	claims   map[string]any
	tamper   func(map[string]any)
	acrs     []string // the acr values a person can authenticate with, when asked for
	contexts []string // the authentication contexts a person can satisfy, when asked for
	sessions map[string]*session
	asked    []Authorization
	issue    func(claims map[string]any, scope string) string
}

// Authorization is what Quai sent to the authorization endpoint.
type Authorization struct {
	Scope     string
	Prompt    string
	MaxAge    string
	ACRValues string
	Claims    string
	State     string
	Nonce     string
	// Authenticated says whether the person authenticated again, rather than the provider's
	// session answering.
	Authenticated bool
}

// session is the provider's session of a person: when, and how, they last authenticated.
type session struct {
	at   time.Time
	acr  string
	acrs []string
}

type grant struct {
	nonce, challenge, scope string
	claims                  map[string]any
}

// New starts a provider, stopped at the end of the test.
func New(t testing.TB) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{key: key, now: time.Now, codes: map[string]grant{}, sessions: map[string]*session{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/authorize", p.authorize)
	mux.HandleFunc("/token", p.token)
	p.srv = httptest.NewTLSServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

// Issuer is the provider's issuer URL.
func (p *Provider) Issuer() string { return p.srv.URL }

// Transport is how a test's browser reaches the provider: it trusts the provider's CA.
func (p *Provider) Transport() http.RoundTripper { return p.srv.Client().Transport }

// CAFile writes the provider's CA certificate to a PEM file, for network.caFile.
func (p *Provider) CAFile(t testing.TB) string {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return ca
}

// SetClaims sets the claims of the next logins, over iss, aud, sub, nonce, iat, exp and the
// authentication's claims (auth_time, acr, acrs). A nil value removes a claim.
func (p *Provider) SetClaims(claims map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.claims = maps.Clone(claims)
}

// Tamper changes the ID tokens the token endpoint returns, after everything else, for failure
// tests; nil stops.
func (p *Provider) Tamper(f func(claims map[string]any)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tamper = f
}

// SetClock sets the clock of authentications (auth_time), which a test shares with Quai's; the ID
// tokens' iat and exp stay on the real clock, which the verifier checks them against.
func (p *Provider) SetClock(now func() time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.now = now
}

// SupportACRs sets the authentication classes a person can authenticate with when acr_values asks
// for one: the first one asked for that is among them is the login's acr. Others are ignored, as
// acr_values is voluntary.
func (p *Provider) SupportACRs(values ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.acrs = values
}

// SupportAuthContexts sets the Entra ID authentication contexts a person can satisfy when the
// claims parameter asks for them; those asked for and not among them are left out of acrs, as
// Entra ID does when the Conditional Access policy is not satisfied.
func (p *Provider) SupportAuthContexts(ids ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.contexts = ids
}

// IssueAccessTokens makes the token endpoint answer, as the access token, what issue returns for
// the ID token's claims and the scope the authorization asked for; nil: "at".
func (p *Provider) IssueAccessTokens(issue func(claims map[string]any, scope string) string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.issue = issue
}

// Authorizations returns what the authorization endpoint received, oldest first.
func (p *Provider) Authorizations() []Authorization {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.asked)
}

// ForgetSessions ends the provider's sessions: the next login authenticates again.
func (p *Provider) ForgetSessions() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sessions = map[string]*session{}
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token",
		"jwks_uri": p.srv.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
		"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		"claims_parameter_supported": true, "claims_supported": []string{"sub", "auth_time", "acr", "acrs"},
	})
}

// authorize answers at once, as a provider whose person is signed in, or authenticates again, and
// sends the browser back with a code.
func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "PKCE required", http.StatusBadRequest)
		return
	}
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || redirect.Host == "" {
		http.Error(w, "redirect_uri required", http.StatusBadRequest)
		return
	}
	reply := func(v url.Values) {
		v.Set("state", q.Get("state"))
		redirect.RawQuery = v.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusFound)
	}
	maxAge := -1
	if s := q.Get("max_age"); s != "" {
		if maxAge, err = strconv.Atoi(s); err != nil || maxAge < 0 {
			reply(url.Values{"error": {"invalid_request"}, "error_description": {"max_age must be a number of seconds"}})
			return
		}
	}
	var wanted []string // the authentication contexts the claims parameter asks for in the ID token
	if s := q.Get("claims"); s != "" {
		if wanted, err = authContexts(s); err != nil {
			reply(url.Values{"error": {"invalid_request"}, "error_description": {"claims: " + err.Error()}})
			return
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	claims := maps.Clone(p.claims)
	sub := Subject
	if s, ok := claims["sub"].(string); ok && s != "" {
		sub = s
	}
	acr := ""
	for _, v := range strings.Fields(q.Get("acr_values")) {
		if slices.Contains(p.acrs, v) {
			acr = v
			break
		}
	}
	var granted []string
	for _, c := range wanted {
		if slices.Contains(p.contexts, c) {
			granted = append(granted, c)
		}
	}
	s := p.sessions[sub]
	// max_age=0 is prompt=login (OpenID Connect Core, 3.1.2.1).
	again := s == nil || q.Get("prompt") == "login" || maxAge == 0 || (maxAge > 0 && now.Sub(s.at) > time.Duration(maxAge)*time.Second) ||
		(acr != "" && s.acr != acr) || slices.ContainsFunc(granted, func(c string) bool { return !slices.Contains(s.acrs, c) })
	if again {
		s = &session{at: now.Truncate(time.Second), acr: acr, acrs: granted}
		p.sessions[sub] = s
	}
	p.asked = append(p.asked, Authorization{Scope: q.Get("scope"), Prompt: q.Get("prompt"), MaxAge: q.Get("max_age"), ACRValues: q.Get("acr_values"),
		Claims: q.Get("claims"), State: q.Get("state"), Nonce: q.Get("nonce"), Authenticated: again})

	// The authentication's claims, under the test's: a test can remove them, or set its own.
	auth := map[string]any{"sub": sub, "auth_time": s.at.Unix()}
	if s.acr != "" {
		auth["acr"] = s.acr
	}
	if wanted != nil {
		// Entra ID lists the contexts satisfied among those asked for; none, no claim.
		var acrs []string
		for _, c := range wanted {
			if slices.Contains(s.acrs, c) {
				acrs = append(acrs, c)
			}
		}
		if len(acrs) > 0 {
			auth["acrs"] = acrs
		}
	}
	for k, v := range claims {
		auth[k] = v
	}
	code := random()
	p.codes[code] = grant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), scope: q.Get("scope"), claims: auth}
	reply(url.Values{"code": {code}})
}

// authContexts returns the authentication contexts a claims request parameter asks for in the ID
// token: {"id_token": {"acrs": {"essential": true, "value": "c1"}}}, or "values": [...].
func authContexts(s string) ([]string, error) {
	var req struct {
		IDToken map[string]*struct {
			Essential bool     `json:"essential"`
			Value     string   `json:"value"`
			Values    []string `json:"values"`
		} `json:"id_token"`
	}
	if err := json.Unmarshal([]byte(s), &req); err != nil {
		return nil, err
	}
	c := req.IDToken["acrs"]
	if c == nil {
		return []string{}, nil
	}
	out := slices.Clone(c.Values)
	if c.Value != "" {
		out = append(out, c.Value)
	}
	return out, nil
}

// token exchanges a code, once, for an ID token, when the PKCE verifier matches its challenge.
func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	p.mu.Lock()
	g, ok := p.codes[r.Form.Get("code")]
	delete(p.codes, r.Form.Get("code"))
	tamper, issue := p.tamper, p.issue
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": "invalid_grant"}`))
		return
	}
	now := time.Now()
	claims := map[string]any{"iss": p.srv.URL, "aud": ClientID, "nonce": g.nonce, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	for k, v := range g.claims {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	if tamper != nil {
		tamper(claims)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	idToken, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	access := "at"
	if issue != nil {
		access = issue(claims, g.scope)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600, "id_token": idToken})
}

func random() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
