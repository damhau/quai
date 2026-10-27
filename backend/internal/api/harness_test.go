package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/store"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

// harness runs the API on a repository seeded with examples/gitops-repo, a real database, and
// the dev users: alice (platform admin), bob and carol (developers in team shop), fiona
// (finops).
type harness struct {
	t     *testing.T
	srv   *httptest.Server
	bare  string // the repository's bare remote
	work  string // a clone people push to
	store *store.Store
	reg   *repositories.Registry
	log   *bytes.Buffer
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...) // #nosec G204 -- test helper
	cmd.Dir = dir
	cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + os.TempDir(), "PATH=" + os.Getenv("PATH"),
		"GIT_AUTHOR_NAME=Alice", "GIT_AUTHOR_EMAIL=alice@example.com", "GIT_COMMITTER_NAME=Alice", "GIT_COMMITTER_EMAIL=alice@example.com"}
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, b)
	}
	return string(b)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	bare, work := filepath.Join(dir, "infra-live.git"), filepath.Join(dir, "work")
	git(t, "", "init", "--quiet", "--bare", "--initial-branch=main", bare)
	git(t, "", "clone", "--quiet", bare, work)
	if err := os.CopyFS(work, os.DirFS("../../../examples/gitops-repo")); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "--all")
	git(t, work, "commit", "--quiet", "-m", "the existing infrastructure")
	git(t, work, "push", "--quiet", "origin", "HEAD:main")

	cfg, err := config.Load("", func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	cfg.Git.CacheDir = filepath.Join(dir, "mirrors")
	cfg.Repositories = []config.Repository{{Name: "infra-live", Title: "Infrastructure", URL: bare, DefaultBranch: "main",
		Delivery: config.Delivery{Mode: "direct"}, Sync: config.SyncSettings{MinInterval: time.Nanosecond},
		FloorRules: []config.FloorRule{{Name: "production-floor", When: "params.env == 'prod'", Approvers: []string{"group:platform-admins"}}}}}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	reg, err := repositories.Open(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	s := pgtest.Store(t)
	svc := requests.New(s, reg, nil, log)
	worker, err := jobs.NewWorker(s.Pool, jobs.Options{Logger: log, Workers: func(w *river.Workers) { river.AddWorker(w, requests.NewWorker(svc)) }})
	if err != nil {
		t.Fatal(err)
	}
	svc.UseJobs(worker)
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = worker.StopAndCancel(ctx)
	})
	srv := httptest.NewServer(New(Options{Version: "test", Env: cfg.Env, Logger: log, Store: s, Auth: auth.New(cfg, s, log), Repositories: reg, Requests: svc}))
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, bare: bare, work: work, store: s, reg: reg, log: &logs}
}

// push commits a change to the repository as a person would.
func (h *harness) push(file, content string) {
	h.t.Helper()
	git(h.t, h.work, "pull", "--quiet", "--rebase", "origin", "main")
	p := filepath.Join(h.work, filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	git(h.t, h.work, "add", "--all")
	git(h.t, h.work, "commit", "--quiet", "-m", "hand edit of "+file)
	git(h.t, h.work, "push", "--quiet", "origin", "HEAD:main")
}

// user is a signed-in client.
type user struct {
	h    *harness
	c    *http.Client
	csrf string
}

func (h *harness) as(name string) *user {
	h.t.Helper()
	jar, _ := cookiejar.New(nil)
	u := &user{h: h, c: &http.Client{Jar: jar}}
	if name == "" {
		return u
	}
	if status, _ := u.do(http.MethodPost, "/auth/dev/login", map[string]string{"name": name}, nil); status != http.StatusNoContent {
		h.t.Fatalf("login %s: %d", name, status)
	}
	var me struct{ CSRFToken string }
	u.do(http.MethodGet, Prefix+"/me", nil, &me)
	u.csrf = me.CSRFToken
	return u
}

// do sends a request and decodes the JSON answer into out, returning the status and the raw body.
func (u *user) do(method, path string, body, out any) (int, string) {
	u.h.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u.h.srv.URL+path, r)
	req.Header.Set("Content-Type", "application/json")
	if u.csrf != "" {
		req.Header.Set("X-CSRF-Token", u.csrf)
	}
	resp, err := u.c.Do(req)
	if err != nil {
		u.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, out); err != nil {
			u.h.t.Fatalf("%s %s: %v\n%s", method, path, err, raw)
		}
	}
	return resp.StatusCode, string(raw)
}

// file returns a file of the repository's main branch, as Git has it.
func (h *harness) file(p string) (string, bool) {
	h.t.Helper()
	cmd := exec.Command("git", "--git-dir", h.bare, "show", "main:"+p) // #nosec G204 -- test helper
	b, err := cmd.Output()
	return string(b), err == nil
}

// await waits until a request reaches state.
func (u *user) await(id, state string) RequestView {
	u.h.t.Helper()
	var r RequestView
	deadline := time.Now().Add(30 * time.Second)
	for {
		u.do(http.MethodGet, Prefix+"/requests/"+id, nil, &r)
		if r.State == state {
			return r
		}
		if time.Now().After(deadline) {
			u.h.t.Fatalf("request %s is %s, not %s; events %+v\nlog:\n%s", id, r.State, state, r.Events, u.h.log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
