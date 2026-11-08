package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/secrets"
)

const appID = 4242

// testKey is the app's usual key in these tests: making one takes a while.
var testKey = sync.OnceValues(newKey)

// newKey returns a private key, as the PEM a secret holds, and its public part.
func newKey() (secrets.Secret, *rsa.PublicKey) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return secrets.FromValue(string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))), &key.PublicKey
}

// fakeAPI is GitHub's App authentication and just enough of its API, over TLS, verifying each JWT's
// signature against the keys it was told are the app's. Handlers answer the rest, by "METHOD path".
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	keys         []*rsa.PublicKey
	installation int64
	expires      time.Duration
	refuseToken  int // the status of the next token exchange, once
	lookups      int
	tokens       []string
	scopes       [][]string // the repositories each token was limited to
	calls        []string   // "METHOD path" of the other calls
	bodies       map[string][]byte
	handlers     map[string]http.HandlerFunc
}

func newFakeAPI(t *testing.T) *fakeAPI {
	_, public := testKey()
	f := &fakeAPI{t: t, keys: []*rsa.PublicKey{public}, installation: 77, expires: time.Hour, handlers: map[string]http.HandlerFunc{}, bodies: map[string][]byte{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// jwtFrom verifies r's JWT.
func (f *fakeAPI) jwtFrom(r *http.Request) error {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return errors.New("no JWT")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return err
	}
	if claims.Iss != strconv.Itoa(appID) {
		return fmt.Errorf("issued by %q", claims.Iss)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	for _, key := range f.keys {
		if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature) == nil {
			return nil
		}
	}
	return errors.New("signed with an unknown key")
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	token := fmt.Sprintf("/app/installations/%d/access_tokens", f.installation)
	switch {
	case r.URL.Path == "/repos/o/r/installation":
		if err := f.jwtFrom(r); err != nil {
			f.t.Errorf("installation lookup: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.lookups++
		_, _ = fmt.Fprintf(w, `{"id":%d}`, f.installation)
	case strings.HasPrefix(r.URL.Path, "/app/installations/"):
		if err := f.jwtFrom(r); err != nil {
			f.t.Errorf("token exchange: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != token {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		if f.refuseToken != 0 {
			w.WriteHeader(f.refuseToken)
			_, _ = io.WriteString(w, `{"message":"refused"}`)
			f.refuseToken = 0
			return
		}
		var opts struct {
			Repositories []string `json:"repositories"`
		}
		_ = json.Unmarshal(body, &opts)
		f.scopes = append(f.scopes, opts.Repositories)
		f.tokens = append(f.tokens, "ghs_"+strconv.Itoa(len(f.tokens)+1))
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, f.tokens[len(f.tokens)-1], time.Now().Add(f.expires).UTC().Format(time.RFC3339))
	default:
		if len(f.tokens) == 0 || r.Header.Get("Authorization") != "token "+f.tokens[len(f.tokens)-1] {
			f.t.Errorf("%s %s with Authorization %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		key := r.Method + " " + r.URL.Path
		f.calls = append(f.calls, key)
		f.bodies[key] = body
		if h, ok := f.handlers[key]; ok {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	}
}

func (f *fakeAPI) handle(key string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[key] = h
}

// answer answers key with status and body.
func (f *fakeAPI) answer(key string, status int, body string) {
	f.handle(key, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

// sequence answers key with each answer in turn, staying on the last.
func (f *fakeAPI) sequence(key string, answers ...answer) {
	n := 0
	f.handle(key, func(w http.ResponseWriter, _ *http.Request) {
		a := answers[min(n, len(answers)-1)]
		n++
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	})
}

type answer struct {
	status int
	body   string
}

func (f *fakeAPI) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == key {
			n++
		}
	}
	return n
}

func (f *fakeAPI) body(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out map[string]any
	if err := json.Unmarshal(f.bodies[key], &out); err != nil {
		f.t.Fatalf("%s: %v", key, err)
	}
	return out
}

// set changes the fake's behaviour under its lock.
func (f *fakeAPI) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

// exchanges returns how many installations were looked up and how many tokens were issued.
func (f *fakeAPI) exchanges() (lookups, tokens int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups, len(f.tokens)
}

// connector returns a connector to o/r on the fake, with the app's usual key.
func (f *fakeAPI) connector() *Connector {
	f.t.Helper()
	key, _ := testKey()
	return f.connectorOver(f.srv.Client().Transport, nil, func(context.Context) (secrets.Secret, bool, error) { return key, true, nil })
}

func (f *fakeAPI) connectorOver(base http.RoundTripper, log *slog.Logger, key func(context.Context) (secrets.Secret, bool, error)) *Connector {
	f.t.Helper()
	c, err := newConnector(connectors.Config{URL: "https://github.com/o/r", AppID: appID, Log: log, HTTP: &http.Client{Transport: base, Timeout: time.Minute}, Credentials: key}, f.srv.URL+"/")
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func TestURLs(t *testing.T) {
	for raw, want := range map[string][2]string{
		"https://github.com/o/r":      {"o", "r"},
		"https://github.com/o/r.git":  {"o", "r"},
		"https://github.com/o/r/":     {"o", "r"},
		"https://GitHub.com/O/R.git/": {"O", "R"},
		"https://github.com/o/r.x":    {"o", "r.x"},
	} {
		owner, repo, err := parseURL(raw)
		if err != nil || owner != want[0] || repo != want[1] {
			t.Errorf("%s: %s %s, %v", raw, owner, repo, err)
		}
	}
	for _, raw := range []string{
		"http://github.com/o/r", // the app's token would travel in the clear
		"https://github.example.com/o/r",
		"https://evil.example/github.com/o/r",
		"git@github.com:o/r.git",
		"ssh://git@github.com/o/r",
		"https://github.com/o",
		"https://github.com/o/r/s",
		"https://github.com/o/.git",
		"https://github.com//r",
		"not a url\x7f",
	} {
		if _, _, err := parseURL(raw); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
	if _, _, err := parseURL("https://x:hunter2@evil.example/o/r"); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the URL's password in the error: %v", err)
	}
}

func TestConfig(t *testing.T) {
	if _, err := New(connectors.Config{URL: "https://github.com/o/r"}); err == nil || !strings.Contains(err.Error(), "appId") {
		t.Errorf("no app ID: %v", err)
	}
	c, err := New(connectors.Config{URL: "https://github.com/o/r", AppID: appID})
	if err != nil {
		t.Fatal(err)
	}
	info := c.Info()
	if info.Platform != "github" || !info.Has(connectors.CapPullRequests) || info.Has(connectors.CapAutoComplete) || len(info.MergeMethods) != 3 {
		t.Errorf("info = %+v", info)
	}
	// What a connector lacks is a true nil, which callers compare: P3-17 and P3-18 fill these.
	if c.Statuses() != nil || c.Policies() != nil || c.Owners() != nil || c.Webhooks() != nil || c.Pipelines() != nil || c.Identities() != nil {
		t.Error("a capability the connector lacks is not nil")
	}
	if _, err := c.GitAuth(t.Context()); err == nil || !strings.Contains(err.Error(), "spec.credentials.ref") {
		t.Errorf("no credentials: %v", err)
	}
	if !errors.Is(c.PullRequests().AutoComplete(t.Context(), "1", connectors.MergeOptions{}), connectors.ErrUnsupported) {
		t.Error("auto-complete is not built")
	}
}

// TestInstallationToken: the JWT finds the installation once, and its token, limited to the
// repository, is kept until a minute before it expires.
func TestInstallationToken(t *testing.T) {
	f := newFakeAPI(t)
	c := f.connector()
	ctx := t.Context()
	f.answer("GET /repos/o/r/pulls/1", http.StatusOK, `{"number":1}`)
	for range 3 {
		if _, err := c.PullRequests().Get(ctx, "1"); err != nil {
			t.Fatal(err)
		}
	}
	if lookups, tokens := f.exchanges(); lookups != 1 || tokens != 1 {
		t.Errorf("%d lookups and %d tokens for three calls", lookups, tokens)
	}
	if len(f.scopes) != 1 || len(f.scopes[0]) != 1 || f.scopes[0][0] != "r" {
		t.Errorf("the token was limited to %v", f.scopes)
	}

	f = newFakeAPI(t)
	f.set(func() { f.expires = 30 * time.Second }) // inside the minute before its expiry
	c = f.connector()
	for range 2 {
		if _, err := c.GitAuth(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if lookups, tokens := f.exchanges(); lookups != 1 || tokens != 2 {
		t.Errorf("a token about to expire: %d lookups, %d tokens", lookups, tokens)
	}
}

func TestGitAuth(t *testing.T) {
	f := newFakeAPI(t)
	c := f.connector()
	creds, err := c.GitAuth(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, ok := strings.CutPrefix(creds.Header, "Authorization: Basic ")
	if !ok {
		t.Fatalf("header %q", creds.Header)
	}
	plain, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || string(plain) != "x-access-token:"+f.tokens[0] {
		t.Errorf("credentials %q, %v", plain, err)
	}
}

// TestKeyRotation: the key is read on each call, so a rotated one is used from the next call on.
func TestKeyRotation(t *testing.T) {
	f := newFakeAPI(t)
	key, _ := testKey()
	second, public := newKey()
	var mu sync.Mutex
	c := f.connectorOver(f.srv.Client().Transport, nil, func(context.Context) (secrets.Secret, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return key, true, nil
	})
	if _, err := c.GitAuth(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.set(func() { f.keys = []*rsa.PublicKey{public} }) // GitHub no longer trusts the first key
	mu.Lock()
	key = second
	mu.Unlock()
	if _, err := c.GitAuth(t.Context()); err != nil {
		t.Fatal(err)
	}
	if lookups, tokens := f.exchanges(); lookups != 2 || tokens != 2 {
		t.Errorf("after a rotation: %d lookups, %d tokens", lookups, tokens)
	}
}

// TestReinstalled: the app removed and installed again has a new installation ID. The refused token
// makes the next call look it up.
func TestReinstalled(t *testing.T) {
	f := newFakeAPI(t)
	c := f.connector()
	f.set(func() { f.expires = 30 * time.Second }) // so the next call exchanges again
	if _, err := c.GitAuth(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.set(func() { f.installation, f.expires = 78, time.Hour })
	if _, err := c.GitAuth(t.Context()); err == nil {
		t.Fatal("the old installation's token was issued")
	}
	if _, err := c.GitAuth(t.Context()); err != nil {
		t.Fatalf("after the lookup: %v", err)
	}
	if lookups, _ := f.exchanges(); lookups != 2 {
		t.Errorf("%d lookups", lookups)
	}
}

func TestNotInstalled(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer api.Close()
	key, _ := testKey()
	c, err := newConnector(connectors.Config{URL: "https://github.com/o/r", AppID: appID, HTTP: &http.Client{Transport: api.Client().Transport},
		Credentials: func(context.Context) (secrets.Secret, bool, error) { return key, true, nil }}, api.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GitAuth(t.Context()); !errors.Is(err, connectors.ErrNotFound) || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("not installed: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("%d calls", calls.Load())
	}
}

// TestTokenStaysOnTheAPI: neither a call to another host nor a redirect to one carries the token.
func TestTokenStaysOnTheAPI(t *testing.T) {
	f := newFakeAPI(t)
	c := f.connector()
	var mu sync.Mutex
	var seen []string
	other := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.Header.Get("Authorization"))
	}))
	defer other.Close()
	f.handle("GET /redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/there", http.StatusFound)
	})
	if _, err := c.GitAuth(t.Context()); err != nil {
		t.Fatal(err)
	}
	client := c.client.Client()
	for _, u := range []string{other.URL + "/direct", f.srv.URL + "/redirect"} {
		resp, err := client.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "" || seen[1] != "" {
		t.Errorf("the other host saw Authorization %q", seen)
	}
	if f.count("GET /redirect") != 1 {
		t.Error("the redirect was not asked with the token")
	}
}

// TestSecretsStayOut: neither the key nor a token shows in the connector's errors, logs or printing.
func TestSecretsStayOut(t *testing.T) {
	f := newFakeAPI(t)
	key, _ := testKey()
	var mu sync.Mutex
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(lockedWriter{&mu, &logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	resolve := func(context.Context) (secrets.Secret, bool, error) { return key, true, nil }
	c := f.connectorOver(connectors.NewTransport(f.srv.Client().Transport, log), log, resolve)
	f.answer("GET /repos/o/r/pulls/1", http.StatusInternalServerError, `{"message":"down"}`)
	_, failed := c.PullRequests().Get(t.Context(), "1")
	if failed == nil {
		t.Fatal("no error")
	}
	f.set(func() { f.refuseToken, f.expires = http.StatusForbidden, time.Hour })
	refusing := f.connectorOver(connectors.NewTransport(f.srv.Client().Transport, log), log, resolve)
	_, refused := refusing.GitAuth(t.Context())
	if refused == nil {
		t.Fatal("no error from the refused token")
	}
	garbage := "-----BEGIN RSA PRIVATE KEY-----\nnot-the-real-key-material\n-----END RSA PRIVATE KEY-----\n"
	bad := f.connectorOver(f.srv.Client().Transport, log, func(context.Context) (secrets.Secret, bool, error) { return secrets.FromValue(garbage), true, nil })
	_, badKey := bad.GitAuth(t.Context())
	if badKey == nil {
		t.Fatal("no error from the bad key")
	}
	mu.Lock()
	defer mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	places := map[string]string{"the logs": logs.String(), "an API error": failed.Error(), "the refusal": refused.Error(), "the bad key's error": badKey.Error(),
		"the connector, printed": fmt.Sprintf("%+v %#v", c, c)}
	line := strings.Split(strings.TrimSpace(key.Reveal()), "\n")[1]
	for place, text := range places {
		for _, secret := range append([]string{line, "not-the-real-key-material"}, f.tokens...) {
			if strings.Contains(text, secret) {
				t.Errorf("a secret shows in %s", place) // not which: it would print
			}
		}
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// TestRateLimitedToken: the shared transport's retry covers the token exchange, with its body.
func TestRateLimitedToken(t *testing.T) {
	var mu sync.Mutex
	var attempts int
	var bodies []string
	key, _ := testKey()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/o/r/installation":
			_, _ = io.WriteString(w, `{"id":7}`)
		case attempts == 0:
			attempts++
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			attempts++
			b, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(b))
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		}
	}))
	defer srv.Close()
	c, err := newConnector(connectors.Config{URL: "https://github.com/o/r", AppID: appID, HTTP: &http.Client{Transport: connectors.NewTransport(srv.Client().Transport, nil)},
		Credentials: func(context.Context) (secrets.Secret, bool, error) { return key, true, nil }}, srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GitAuth(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 || len(bodies) != 1 || !strings.Contains(bodies[0], `"repositories":["r"]`) {
		t.Errorf("%d attempts, bodies %q", attempts, bodies)
	}
}

func TestErrors(t *testing.T) {
	f := newFakeAPI(t)
	c := f.connector()
	prs := c.PullRequests()
	ctx := t.Context()
	if _, err := prs.Get(ctx, "1"); !errors.Is(err, connectors.ErrNotFound) {
		t.Errorf("404: %v", err)
	}
	if _, err := prs.Get(ctx, "not-a-number"); !errors.Is(err, connectors.ErrNotFound) {
		t.Errorf("a bad ID: %v", err)
	}
	f.handle("GET /repos/o/r/pulls/2", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	})
	if _, err := prs.Get(ctx, "2"); !errors.Is(err, connectors.ErrRateLimited) {
		t.Errorf("the primary limit: %v", err)
	}
	f.handle("GET /repos/o/r/pulls/3", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`)
	})
	if _, err := prs.Get(ctx, "3"); !errors.Is(err, connectors.ErrRateLimited) {
		t.Errorf("a secondary limit: %v", err)
	}
	f.answer("GET /repos/o/r/pulls/4", http.StatusInternalServerError, `{"message":"down"}`)
	_, err := prs.Get(ctx, "4")
	if err == nil || errors.Is(err, connectors.ErrNotFound) || errors.Is(err, connectors.ErrRateLimited) || errors.Is(err, connectors.ErrBlocked) {
		t.Errorf("a server error: %v", err)
	}
}

func prJSON(state string, extra string) string {
	return `{"number":5,"html_url":"https://github.com/o/r/pull/5","title":"t","body":"d","state":"` + state + `",
		"head":{"ref":"quai/x","sha":"approved","repo":{"id":1}},"base":{"ref":"main","sha":"b","repo":{"id":1}}` + extra + `}`
}

func TestConvert(t *testing.T) {
	f := newFakeAPI(t)
	c := f.connector()
	f.answer("GET /repos/o/r/pulls/5", http.StatusOK, prJSON("open", `,"draft":true,"labels":[{"name":"quai"}],"auto_merge":{"merge_method":"squash"}`))
	pr, err := c.PullRequests().Get(t.Context(), "5")
	if err != nil {
		t.Fatal(err)
	}
	if pr.ID != "5" || pr.URL != "https://github.com/o/r/pull/5" || pr.Title != "t" || pr.Description != "d" || pr.Source != "quai/x" || pr.Target != "main" ||
		pr.Head != "approved" || pr.State != connectors.PROpen || !pr.Draft || len(pr.Labels) != 1 || pr.Labels[0] != "quai" || !pr.AutoComplete {
		t.Errorf("open: %+v", pr)
	}
	f.answer("GET /repos/o/r/pulls/5", http.StatusOK, prJSON("closed", `,"merged":true,"merge_commit_sha":"m"`))
	if pr, _ := c.PullRequests().Get(t.Context(), "5"); pr.State != connectors.PRMerged || pr.MergeCommit != "m" {
		t.Errorf("merged: %+v", pr)
	}
	// A list leaves out merged, but not merged_at.
	f.answer("GET /repos/o/r/pulls/5", http.StatusOK, prJSON("closed", `,"merged_at":"2026-01-02T03:04:05Z","merge_commit_sha":"m"`))
	if pr, _ := c.PullRequests().Get(t.Context(), "5"); pr.State != connectors.PRMerged {
		t.Errorf("merged, by its date: %+v", pr)
	}
	// GitHub's merge_commit_sha on a pull request not merged is a test merge, not a merge.
	f.answer("GET /repos/o/r/pulls/5", http.StatusOK, prJSON("closed", `,"merge_commit_sha":"test-merge"`))
	if pr, _ := c.PullRequests().Get(t.Context(), "5"); pr.State != connectors.PRAbandoned || pr.MergeCommit != "" {
		t.Errorf("abandoned: %+v", pr)
	}
	f.answer("GET /repos/o/r/pulls/5", http.StatusOK, prJSON("open", `,"merge_commit_sha":"test-merge"`))
	if pr, _ := c.PullRequests().Get(t.Context(), "5"); pr.State != connectors.PROpen || pr.MergeCommit != "" {
		t.Errorf("open, with a test merge: %+v", pr)
	}
}

func TestOpen(t *testing.T) {
	f := newFakeAPI(t)
	c := f.connector()
	f.answer("POST /repos/o/r/pulls", http.StatusCreated, prJSON("open", ""))
	f.answer("POST /repos/o/r/issues/5/labels", http.StatusOK, `[{"name":"quai"},{"name":"other"}]`)
	marker := connectors.Marker("req-1")
	long := strings.Repeat("é", 60)
	pr, err := c.PullRequests().Open(t.Context(), connectors.NewPullRequest{Source: "quai/x", Target: "main", Title: strings.Repeat("é", 300),
		Description: strings.Repeat("d", maxDescription+1000) + marker, Labels: []string{"quai", long}, Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Labels) != 2 || pr.Labels[1] != "other" {
		t.Errorf("labels = %q", pr.Labels)
	}
	body := f.body("POST /repos/o/r/pulls")
	if body["head"] != "quai/x" || body["base"] != "main" || body["draft"] != true {
		t.Errorf("opened with %v", body)
	}
	if title := body["title"].(string); utf8.RuneCountInString(title) != maxTitle {
		t.Errorf("a title of %d characters", utf8.RuneCountInString(title))
	}
	description := body["body"].(string)
	if len(description) > maxDescription || !strings.Contains(description, marker) {
		t.Errorf("a description of %d bytes, marker kept: %v", len(description), strings.Contains(description, marker))
	}
	var labels []string
	f.mu.Lock()
	err = json.Unmarshal(f.bodies["POST /repos/o/r/issues/5/labels"], &labels)
	f.mu.Unlock()
	if err != nil || len(labels) != 2 || labels[0] != "quai" || utf8.RuneCountInString(labels[1]) != maxLabel {
		t.Errorf("labelled %q, %v", labels, err)
	}

	// No labels: no second call.
	f.set(func() { f.calls = nil })
	if _, err := c.PullRequests().Open(t.Context(), connectors.NewPullRequest{Source: "quai/x", Target: "main", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if n := f.count("POST /repos/o/r/issues/5/labels"); n != 0 {
		t.Errorf("%d label calls", n)
	}

	// The two calls are apart: the error says which pull request lacks its labels.
	f.answer("POST /repos/o/r/issues/5/labels", http.StatusInternalServerError, `{"message":"down"}`)
	_, err = c.PullRequests().Open(t.Context(), connectors.NewPullRequest{Source: "quai/x", Target: "main", Title: "t", Labels: []string{"quai"}})
	if err == nil || !strings.Contains(err.Error(), "pull request 5") {
		t.Errorf("labels refused: %v", err)
	}
}

// TestListForks: a fork can name a branch quai/<anything>, and core takes List's answer to be its
// own pull request.
func TestListForks(t *testing.T) {
	f := newFakeAPI(t)
	c := f.connector()
	item := func(n int, ref string, head string) string {
		return fmt.Sprintf(`{"number":%d,"state":"open","head":{"ref":%q,"sha":"s"%s},"base":{"ref":"main","repo":{"id":1}}}`, n, ref, head)
	}
	f.handle("GET /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", "<"+f.srv.URL+`/repos/o/r/pulls?page=2>; rel="next"`)
			_, _ = io.WriteString(w, "["+item(1, "quai/a", `,"repo":{"id":1}`)+","+item(2, "other", `,"repo":{"id":1}`)+","+item(3, "quai/fork", `,"repo":{"id":2}`)+"]")
			return
		}
		_, _ = io.WriteString(w, "["+item(4, "quai/b", `,"repo":{"id":1}`)+","+item(5, "quai/gone", ``)+"]")
	})
	got, err := c.PullRequests().List(t.Context(), "quai/")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "4" {
		t.Errorf("listed %+v", got)
	}
}

// TestMerge pins what each of GitHub's answers means, as measured on the sandbox.
func TestMerge(t *testing.T) {
	timeout, retry := mergeTimeout, mergeRetry
	mergeTimeout, mergeRetry = 200*time.Millisecond, time.Millisecond
	t.Cleanup(func() { mergeTimeout, mergeRetry = timeout, retry })

	const (
		get   = "GET /repos/o/r/pulls/5"
		put   = "PUT /repos/o/r/pulls/5/merge"
		del   = "DELETE /repos/o/r/git/refs/heads/quai/x"
		ok    = `{"sha":"m","merged":true,"message":"Pull Request successfully merged"}`
		fresh = `,"mergeable":true`
	)
	refused := func(status int, message string) answer {
		return answer{status, fmt.Sprintf(`{"message":%q}`, message)}
	}
	open := answer{http.StatusOK, prJSON("open", fresh)}
	unknown := answer{http.StatusOK, prJSON("open", "")}
	dirty := answer{http.StatusOK, prJSON("open", `,"mergeable":false`)}
	draft := answer{http.StatusOK, prJSON("open", `,"draft":true`+fresh)}
	moved := answer{http.StatusOK, strings.Replace(prJSON("open", fresh), `"sha":"approved"`, `"sha":"newer"`, 1)}
	mergedPR := answer{http.StatusOK, prJSON("closed", `,"merged":true,"merge_commit_sha":"m2"`)}
	closed := answer{http.StatusOK, prJSON("closed", "")}

	for name, tc := range map[string]struct {
		gets      []answer
		puts      []answer
		err       error // nil: merged
		plain     bool  // an error that is none of the connectors' own
		commit    string
		merges    int
		deletions int
	}{
		"merged at once":      {gets: []answer{open}, puts: []answer{{http.StatusOK, ok}}, commit: "m", merges: 1, deletions: 1},
		"already merged":      {gets: []answer{mergedPR}, commit: "m2", deletions: 1},
		"closed":              {gets: []answer{closed}, plain: true},
		"mergeability pends":  {gets: []answer{unknown}, puts: []answer{refused(405, "Pull Request is not mergeable"), {http.StatusOK, ok}}, commit: "m", merges: 2, deletions: 1},
		"mergeability hangs":  {gets: []answer{unknown}, puts: []answer{refused(405, "Pull Request is not mergeable")}, err: connectors.ErrBlocked},
		"the base moved":      {gets: []answer{open}, puts: []answer{refused(405, "Base branch was modified. Review and try the merge again."), {http.StatusOK, ok}}, commit: "m", merges: 2, deletions: 1},
		"the head lags":       {gets: []answer{open}, puts: []answer{refused(409, "Head branch was modified. Review and try the merge again."), {http.StatusOK, ok}}, commit: "m", merges: 2, deletions: 1},
		"the head moved":      {gets: []answer{open, moved}, puts: []answer{refused(409, "Head branch was modified. Review and try the merge again.")}, err: connectors.ErrBlocked, merges: 1},
		"a draft":             {gets: []answer{draft}, puts: []answer{refused(405, "Pull Request is still a draft")}, err: connectors.ErrBlocked, merges: 1},
		"a conflict":          {gets: []answer{dirty}, puts: []answer{refused(405, "Pull Request has merge conflicts")}, err: connectors.ErrBlocked, merges: 1},
		"a required check":    {gets: []answer{open}, puts: []answer{refused(405, "Required status check is expected.")}, err: connectors.ErrBlocked, merges: 1},
		"a forbidden method":  {gets: []answer{open}, puts: []answer{refused(405, "Rebase merges are not allowed on this repository.")}, err: connectors.ErrUnsupported, merges: 1},
		"merged by someone":   {gets: []answer{open, mergedPR}, puts: []answer{refused(405, "Pull Request is not mergeable")}, commit: "m2", merges: 1, deletions: 1},
		"closed by someone":   {gets: []answer{open, closed}, puts: []answer{refused(405, "Pull Request is not mergeable")}, plain: true, merges: 1},
		"no merge, no reason": {gets: []answer{open}, puts: []answer{{http.StatusOK, `{"merged":false,"message":"no"}`}}, err: connectors.ErrBlocked, merges: 1},
		"a server error":      {gets: []answer{open}, puts: []answer{refused(500, "down")}, plain: true, merges: 1},
		"not found":           {gets: []answer{{404, `{"message":"Not Found"}`}}, err: connectors.ErrNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAPI(t)
			c := f.connector()
			f.sequence(get, tc.gets...)
			if tc.puts != nil {
				f.sequence(put, tc.puts...)
			}
			f.answer(del, http.StatusNoContent, "")
			pr, err := c.PullRequests().Merge(t.Context(), "5", connectors.MergeOptions{Head: "approved", Method: connectors.Squash, DeleteBranch: true})
			switch {
			case tc.err != nil:
				if !errors.Is(err, tc.err) {
					t.Errorf("err = %v, want %v", err, tc.err)
				}
			case tc.plain:
				if err == nil || errors.Is(err, connectors.ErrBlocked) || errors.Is(err, connectors.ErrUnsupported) || errors.Is(err, connectors.ErrNotFound) {
					t.Errorf("err = %v, want a failure of its own", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if pr.State != connectors.PRMerged || pr.MergeCommit != tc.commit {
					t.Errorf("merged %+v, want the commit %s", pr, tc.commit)
				}
			}
			if tc.merges != 0 && f.count(put) != tc.merges {
				t.Errorf("%d merges asked, want %d", f.count(put), tc.merges)
			}
			if f.count(del) != tc.deletions {
				t.Errorf("%d deletions, want %d", f.count(del), tc.deletions)
			}
		})
	}
}

func TestMergeRequest(t *testing.T) {
	f := newFakeAPI(t)
	c := f.connector()
	const put = "PUT /repos/o/r/pulls/5/merge"
	f.answer("GET /repos/o/r/pulls/5", http.StatusOK, prJSON("open", ""))
	f.answer(put, http.StatusOK, `{"sha":"m","merged":true}`)
	f.answer("DELETE /repos/o/r/git/refs/heads/quai/x", http.StatusNoContent, "")
	for method, name := range methods {
		if _, err := c.PullRequests().Merge(t.Context(), "5", connectors.MergeOptions{Head: "approved", Method: method, Message: "Title\n\nBody.\nApproved-by: Bob\n"}); err != nil {
			t.Fatal(err)
		}
		body := f.body(put)
		if body["merge_method"] != name || body["sha"] != "approved" || body["commit_title"] != "Title" || body["commit_message"] != "Body.\nApproved-by: Bob" {
			t.Errorf("%s: %v", method, body)
		}
	}
	// A one-line message: an empty body, not GitHub's default one.
	if _, err := c.PullRequests().Merge(t.Context(), "5", connectors.MergeOptions{Head: "approved", Method: connectors.Squash, Message: "Title\n"}); err != nil {
		t.Fatal(err)
	}
	if body := f.body(put); body["commit_title"] != "Title" || body["commit_message"] != "" {
		t.Errorf("one line: %v", body)
	}
	// No message: GitHub's defaults, and no field for it to read as blank.
	if _, err := c.PullRequests().Merge(t.Context(), "5", connectors.MergeOptions{Head: "approved", Method: connectors.Squash}); err != nil {
		t.Fatal(err)
	}
	if body := f.body(put); body["commit_title"] != nil || body["commit_message"] != nil {
		t.Errorf("no message: %v", body)
	}
	if n := f.count("DELETE /repos/o/r/git/refs/heads/quai/x"); n != 0 {
		t.Errorf("DeleteBranch false deleted %d times", n)
	}
	if _, err := c.PullRequests().Merge(t.Context(), "5", connectors.MergeOptions{Method: "fast-forward"}); !errors.Is(err, connectors.ErrUnsupported) {
		t.Errorf("an unknown method: %v", err)
	}
}

// TestDeleteBranch: a merge stands whatever happens to the branch, and a fork's branch is not the
// repository's, even named as one of its own.
func TestDeleteBranch(t *testing.T) {
	const del = "DELETE /repos/o/r/git/refs/heads/quai/x"
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	for name, tc := range map[string]struct {
		pr        string
		status    int
		deletions int
		warned    bool
	}{
		"deleted":            {pr: prJSON("open", ""), status: http.StatusNoContent, deletions: 1},
		"already gone":       {pr: prJSON("open", ""), status: http.StatusUnprocessableEntity, deletions: 1},
		"refused":            {pr: prJSON("open", ""), status: http.StatusInternalServerError, deletions: 1, warned: true},
		"a fork":             {pr: strings.Replace(prJSON("open", ""), `"sha":"approved","repo":{"id":1}`, `"sha":"approved","repo":{"id":2}`, 1)},
		"a fork that's gone": {pr: strings.Replace(prJSON("open", ""), `"sha":"approved","repo":{"id":1}`, `"sha":"approved"`, 1)},
	} {
		t.Run(name, func(t *testing.T) {
			logs.Reset()
			f := newFakeAPI(t)
			key, _ := testKey()
			c := f.connectorOver(f.srv.Client().Transport, log, func(context.Context) (secrets.Secret, bool, error) { return key, true, nil })
			f.answer("GET /repos/o/r/pulls/5", http.StatusOK, tc.pr)
			f.answer("PUT /repos/o/r/pulls/5/merge", http.StatusOK, `{"sha":"m","merged":true}`)
			if tc.status != 0 {
				f.answer(del, tc.status, `{"message":"x"}`)
			}
			pr, err := c.PullRequests().Merge(t.Context(), "5", connectors.MergeOptions{Head: "approved", Method: connectors.Squash, DeleteBranch: true})
			if err != nil || pr.State != connectors.PRMerged {
				t.Fatalf("merged %+v, %v", pr, err)
			}
			if f.count(del) != tc.deletions {
				t.Errorf("%d deletions, want %d", f.count(del), tc.deletions)
			}
			if got := strings.Contains(logs.String(), "was not deleted"); got != tc.warned {
				t.Errorf("warned %v, want %v: %s", got, tc.warned, logs.String())
			}
		})
	}
}

func TestAbandonAndComment(t *testing.T) {
	f := newFakeAPI(t)
	c := f.connector()
	f.answer("PATCH /repos/o/r/pulls/5", http.StatusOK, prJSON("closed", ""))
	f.answer("POST /repos/o/r/issues/5/comments", http.StatusCreated, `{"id":1}`)
	if err := c.PullRequests().Abandon(t.Context(), "5"); err != nil {
		t.Fatal(err)
	}
	if body := f.body("PATCH /repos/o/r/pulls/5"); body["state"] != "closed" {
		t.Errorf("abandoned with %v", body)
	}
	if err := c.PullRequests().Comment(t.Context(), "5", strings.Repeat("x", maxDescription+10)); err != nil {
		t.Fatal(err)
	}
	if body := f.body("POST /repos/o/r/issues/5/comments"); len(body["body"].(string)) > maxDescription {
		t.Errorf("a comment of %d bytes", len(body["body"].(string)))
	}
	title, text := strings.Repeat("é", 300), strings.Repeat("d", maxDescription+10)+connectors.Marker("r")
	if _, err := c.PullRequests().Update(t.Context(), "5", connectors.PullRequestUpdate{Title: &title, Description: &text}); err != nil {
		t.Fatal(err)
	}
	body := f.body("PATCH /repos/o/r/pulls/5")
	if utf8.RuneCountInString(body["title"].(string)) != maxTitle || len(body["body"].(string)) > maxDescription || !strings.Contains(body["body"].(string), connectors.Marker("r")) {
		t.Errorf("updated with %d characters and %d bytes", utf8.RuneCountInString(body["title"].(string)), len(body["body"].(string)))
	}
}
