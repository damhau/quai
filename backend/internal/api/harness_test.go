package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/damhau/quai/backend/internal/giteatest"
	"github.com/damhau/quai/backend/internal/ingest"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/notify"
	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/scim"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/servicenow"
	"github.com/damhau/quai/backend/internal/store"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Around(func() int { return giteatest.Main(m) })()) }

// harness runs the API on a repository seeded with examples/gitops-repo, a real database, and
// the dev users: alice (platform admin), bob and carol (developers in team shop), fiona
// (finops).
type harness struct {
	t      *testing.T
	srv    *httptest.Server
	bare   string // the repository's bare remote
	work   string // a clone people push to
	store  *store.Store
	reg    *repositories.Registry
	ingest *ingest.Service
	auth   *auth.Authenticator
	log    *bytes.Buffer
	jobs   *jobs.Client
	notify *notify.Service // with notifications.email configured
	// clock is the notifications' and the delegations' clock: the time it is set to, the real
	// time until it is.
	clock *testClock
	// requests is the requests' service: its sweep takes reminders, escalation and expiry by clock.
	requests *requests.Service
	// scim is the identity provider's provisioning, with auth.scim configured.
	scim *scim.Handler
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

// runToken is the token runners report to infra-live with (spec.runner.reportToken).
const runToken = "run-token-for-infra-live"

func newHarness(t *testing.T) *harness {
	t.Helper()
	return harnessWith(t, "")
}

// newHarnessRunner is newHarness with a runner reporting with runToken, and more runner settings
// such as "planValues: true": merged requests then wait for their apply.
func newHarnessRunner(t *testing.T, runner string) *harness {
	t.Helper()
	if runner != "" {
		runner = ", " + runner
	}
	return harnessWith(t, "\n  runner: { reportToken: { ref: env:INFRA_LIVE_RUN_TOKEN }"+runner+" }")
}

func harnessWith(t *testing.T, runner string) *harness {
	t.Helper()
	dir := t.TempDir()
	bare, work := localRemote(t, dir)

	h := start(t, dir, `connector: generic
  url: `+bare+`
  enforcement:
    floorRules: [{ name: production-floor, when: "params.env == 'prod'", approvers: [group:platform-admins] }]
  sync: { minInterval: 1ns }`+runner, repositories.Options{Secrets: secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(name string) (string, bool) {
		sum := sha256.Sum256([]byte(runToken))
		return hex.EncodeToString(sum[:]), name == "INFRA_LIVE_RUN_TOKEN"
	}}})})
	h.bare, h.work = bare, work
	return h
}

// localRemote makes a bare repository in dir holding the example GitOps repository on main, and
// a clone of it people push to.
func localRemote(t *testing.T, dir string) (bare, work string) {
	t.Helper()
	bare, work = filepath.Join(dir, "infra-live.git"), filepath.Join(dir, "work")
	git(t, "", "init", "--quiet", "--bare", "--initial-branch=main", bare)
	git(t, "", "clone", "--quiet", bare, work)
	if err := os.CopyFS(work, os.DirFS("../../../examples/gitops-repo")); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "--all")
	git(t, work, "commit", "--quiet", "-m", "the existing infrastructure")
	git(t, work, "push", "--quiet", "origin", "HEAD:main")
	return bare, work
}

// start serves Quai with one repository, infra-live, whose spec is given, and a job worker.
// Webhooks and polls are processed by h.ingest. configure changes Quai's configuration.
func start(t *testing.T, dir, spec string, opts repositories.Options, configure ...func(*config.Config)) *harness {
	t.Helper()
	return startRepo(t, dir, "{ name: infra-live, title: Infrastructure }", spec, opts, configure...)
}

// notifyBackoff is how long the harness's notifications wait before trying again.
func notifyBackoff(int) time.Duration { return 200 * time.Millisecond }

// startRepo is start with the repository's metadata given.
func startRepo(t *testing.T, dir, metadata, spec string, opts repositories.Options, configure ...func(*config.Config)) *harness {
	t.Helper()
	cfg, err := config.Load("", func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range configure {
		c(cfg)
	}
	cfg.Git.CacheDir = filepath.Join(dir, "mirrors")
	docs, err := repositories.Parse([]byte("apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: " + metadata + "\nspec:\n  " + spec + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	s := pgtest.Store(t)
	if err := repositories.Reconcile(context.Background(), s, docs); err != nil {
		t.Fatal(err)
	}
	opts.Store, opts.Log = s, log
	reg, err := repositories.Open(context.Background(), cfg, repositories.FromStore(s), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	public := cfg.Server.PublicURL
	if public == "" {
		public = "https://quai.example.com"
	}
	svc := requests.New(s, reg, nil, log)
	svc.UsePublicURL(public)
	svc.UseDirectory(auth.NewDirectory(cfg.Auth))
	svc.UseApprovals(cfg.Approvals)
	windows, err := cfg.Windows()
	if err != nil {
		t.Fatal(err)
	}
	svc.UseChangeWindows(windows)
	srv := httptest.NewUnstartedServer(nil)
	var idp *http.Client // reaches the identity provider, and Microsoft Graph, through network.caFile
	if cfg.Auth.Mode == config.AuthOIDC {
		if cfg.Auth.OIDC.RedirectURL == "" {
			cfg.Auth.OIDC.RedirectURL = "http://" + srv.Listener.Addr().String() + "/auth/callback"
		}
		network, err := netx.New(cfg.Network.CAFile)
		if err != nil {
			t.Fatal(err)
		}
		idp = network.Client()
	}
	authn := auth.New(cfg, s, idp, log)
	svc.UseStepUp(authn.StepUp())
	hooks := ingest.New(s, reg, log)
	hooks.UseHandler(svc)
	var n *notify.Service
	clk := &testClock{}
	svc.UseClock(clk.now)
	if sn := cfg.ITSM.ServiceNow; sn != nil {
		network, err := netx.New(cfg.Network.CAFile)
		if err != nil {
			t.Fatal(err)
		}
		c, err := servicenow.New(*sn, network, opts.Secrets)
		if err != nil {
			t.Fatal(err)
		}
		c.UseClock(clk.now)
		svc.UseServiceNow(sn, c)
		svc.UseEvidence(EvidenceForITSM(svc, s, reg, "test", log))
	}
	if cfg.Notifications.Email != nil || cfg.Notifications.Teams != nil || cfg.Notifications.Slack != nil {
		network, err := netx.New(cfg.Network.CAFile)
		if err != nil {
			t.Fatal(err)
		}
		var channels []notify.Channel
		if e := cfg.Notifications.Email; e != nil {
			email, err := notify.NewEmail(*e, network, opts.Secrets, public)
			if err != nil {
				t.Fatal(err)
			}
			channels = append(channels, email)
		}
		if tc := cfg.Notifications.Teams; tc != nil {
			teams, err := notify.NewTeams(*tc, network, opts.Secrets)
			if err != nil {
				t.Fatal(err)
			}
			teams.UseClock(clk.now)
			channels = append(channels, teams)
		}
		if sc := cfg.Notifications.Slack; sc != nil {
			slack, err := notify.NewSlack(*sc, network, opts.Secrets)
			if err != nil {
				t.Fatal(err)
			}
			slack.UseClock(clk.now)
			channels = append(channels, slack)
		}
		links := notify.Links(cfg.Notifications.Links, opts.Secrets)
		if links != nil {
			links.UseClock(clk.now)
		}
		n = notify.New(notify.Options{Store: s, Requests: svc, Repositories: reg, Directory: auth.NewDirectory(cfg.Auth),
			Channels: channels, Alerts: cfg.Notifications.Alerts, PublicURL: public, Log: log, Backoff: notifyBackoff,
			Settings: notify.NewSettings(cfg.Notifications), Links: links})
		n.UseClock(clk.now)
		s.UseEvents(n.EventRecorded)
		svc.UseReach(n.Reach)
		svc.UseLinks(links)
	}
	worker, err := jobs.NewWorker(s.Pool, jobs.Options{Logger: log, Workers: func(w *river.Workers) {
		svc.Workers(w)
		hooks.Workers(w)
		if n != nil {
			n.Workers(w)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	svc.UseJobs(worker)
	hooks.UseJobs(worker)
	if n != nil {
		n.UseJobs(worker)
	}
	if err := worker.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = worker.StopAndCancel(ctx)
	})
	apiOpts := Options{Version: "test", Env: cfg.Env, Logger: log, Store: s, Auth: authn, Repositories: reg, Requests: svc, PublicURL: public,
		Webhooks: hooks.Webhooks(), Notifications: notify.NewSettings(cfg.Notifications)}
	if si := notify.NewSlackInteractions(cfg.Notifications.Slack, opts.Secrets, log); si != nil {
		si.UseClock(clk.now)
		apiOpts.SlackInteractions = si
	}
	sh := scim.New(cfg.Auth, public, s, opts.Secrets, svc, log)
	if sh != nil {
		sh.UseClock(clk.now)
		apiOpts.SCIM = sh
	}
	srv.Config.Handler = New(apiOpts)
	srv.Start()
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, store: s, reg: reg, ingest: hooks, auth: authn, log: &logs, jobs: worker, notify: n, clock: clk, requests: svc, scim: sh}
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
