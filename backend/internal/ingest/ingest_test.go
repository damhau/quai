package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/connectors"
	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/giteatest"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Around(func() int { return giteatest.Main(m) })()) }

const hookSecret = "hook-secret-for-tests"

// recorder is the Handler of the tests: it keeps the pull requests it is given.
type recorder struct {
	mu  sync.Mutex
	prs []connectors.PullRequest
}

func (r *recorder) PullRequest(_ context.Context, _ *repositories.Repo, pr connectors.PullRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prs = append(r.prs, pr)
	return nil
}

func (r *recorder) seen(id string, state connectors.PullRequestState) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pr := range r.prs {
		if pr.ID == id && pr.State == state {
			return true
		}
	}
	return false
}

// delivery is a request the webhook endpoint received, to send again.
type delivery struct {
	header http.Header
	body   []byte
}

type env struct {
	t          *testing.T
	name       string // the repository's
	gitea      *giteatest.Instance
	remote     *giteatest.Repo
	store      *store.Store
	reg        *repositories.Registry
	svc        *Service
	handler    *recorder
	srv        *httptest.Server
	proxy      func(*url.URL) (*url.URL, error) // how git reaches the remote; nil: directly
	mu         sync.Mutex
	deliveries []delivery
}

// setup connects a Gitea repository through the Gitea connector.
func setup(t *testing.T) *env {
	t.Helper()
	g := giteatest.Get(t)
	r := g.Repo(t, "infra")
	vars := map[string]string{"GITEA_TOKEN": g.Token, "HOOK_SECRET": hookSecret}
	e := start(t, r.Name, `connector: gitea
  url: `+r.CloneURL+`
  credentials: { ref: env:GITEA_TOKEN }`, repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { v, ok := vars[n]; return v, ok }}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"gitea": giteaconn.New}},
	})
	e.gitea, e.remote = g, r
	return e
}

// start connects the repository spec describes (its connector, URL and credentials; the
// webhook secret is env:HOOK_SECRET), with Quai's webhook endpoint and a job worker.
func start(t *testing.T, name, spec string, opts repositories.Options) *env {
	t.Helper()
	ctx := context.Background()
	s := pgtest.Store(t)
	docs, err := repositories.Parse([]byte(`apiVersion: quai.dev/v1beta1
kind: Repository
metadata: { name: ` + name + ` }
spec:
  ` + spec + `
  webhooks: { secret: { ref: env:HOOK_SECRET } }
  sync: { minInterval: 1h, pollInterval: 1m }
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := repositories.Reconcile(ctx, s, docs); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	opts.Log = log
	reg, err := repositories.Open(ctx, &config.Config{Git: config.Git{CacheDir: t.TempDir()}}, repositories.FromStore(s), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	e := &env{t: t, name: name, store: s, reg: reg, svc: New(s, reg, log), handler: &recorder{}}
	e.svc.UseHandler(e.handler)
	worker, err := jobs.NewWorker(s.Pool, jobs.Options{Logger: log, Workers: e.svc.Workers})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.UseJobs(worker)
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = worker.StopAndCancel(ctx)
	})
	hooks := e.svc.Webhooks()
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		e.mu.Lock()
		e.deliveries = append(e.deliveries, delivery{req.Header.Clone(), body})
		e.mu.Unlock()
		req.Body = io.NopCloser(bytes.NewReader(body))
		hooks.ServeHTTP(w, req)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) repo() *repositories.Repo {
	repo, err := e.reg.Get(e.name)
	if err != nil {
		e.t.Fatal(err)
	}
	return repo
}

// head returns the commit at the tip of the remote's main.
func (e *env) head() string {
	b, _, err := e.gitea.Client.GetRepoBranch(e.remote.Owner, e.remote.Name, "main")
	if err != nil {
		e.t.Fatal(err)
	}
	return b.Commit.ID
}

// openPR pushes a quai/ branch and opens a pull request for it.
func (e *env) openPR(branch string) connectors.PullRequest {
	e.t.Helper()
	ctx := context.Background()
	repo := e.repo()
	m, err := gitx.OpenMirror(ctx, repo.Document.Spec.URL, filepath.Join(e.t.TempDir(), "m.git"), gitx.Options{Committer: gitx.Signature{Name: "Quai", Email: "quai@example.com"}, Auth: repo.Connector.GitAuth, Proxy: e.proxy})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := m.CommitAndPush(ctx, gitx.Commit{Branch: branch, Base: "main", Message: "Resize web01\n", Author: gitx.Signature{Name: "Bob", Email: "bob@example.com"},
		Changes: []plan.Change{{Op: plan.OpCreate, Path: branch + ".yaml", After: []byte("cpu: 8\n")}}}); err != nil {
		e.t.Fatal(err)
	}
	pr, err := repo.Connector.PullRequests().Open(ctx, connectors.NewPullRequest{Source: branch, Target: "main", Title: "Resize web01"})
	if err != nil {
		e.t.Fatal(err)
	}
	return pr
}

func (e *env) eventually(what string, ok func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			e.t.Fatalf("after 30s, still not: %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (e *env) send(path string, d delivery) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, bytes.NewReader(d.body))
	req.Header = d.header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestWebhooks: Gitea's deliveries are verified, recorded once, and processed by re-reading:
// a push fetches the new head, a pull request is read again and handed on.
func TestWebhooks(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	name := e.remote.Name
	if err := e.repo().Connector.Webhooks().Subscribe(ctx, e.srv.URL+"/webhooks/"+name, secrets.FromValue(hookSecret)); err != nil {
		t.Fatal(err)
	}

	if err := e.remote.Commit("main", "envs/prod/vms/web01.yaml", "cpu: 4\n"); err != nil {
		t.Fatal(err)
	}
	head := e.head()
	e.eventually("the push is fetched", func() bool {
		s, err := e.store.GetRepositorySync(ctx, name)
		return err == nil && s.LastWebhookAt.Valid && s.Head == head
	})
	if snap, err := e.repo().Snapshot(ctx, false); err != nil || snap.Commit != head {
		t.Errorf("the snapshot is at %v, %v; the head is %s", snap, err, head)
	}

	pr := e.openPR("quai/r-1-web01")
	e.eventually("the pull request is re-read", func() bool { return e.handler.seen(pr.ID, connectors.PROpen) })

	// A redelivery is taken once; a wrong signature, another repository or a GET are refused.
	e.mu.Lock()
	first := e.deliveries[0]
	e.mu.Unlock()
	if status, out := e.send("/webhooks/"+name, first); status != http.StatusAccepted || out["duplicate"] != true {
		t.Errorf("a redelivery = %d %v", status, out)
	}
	forged := delivery{first.header.Clone(), first.body}
	forged.header.Set("X-Gitea-Signature", "00")
	if status, _ := e.send("/webhooks/"+name, forged); status != http.StatusUnauthorized {
		t.Errorf("a forged delivery = %d", status)
	}
	if status, _ := e.send("/webhooks/nope", first); status != http.StatusNotFound {
		t.Errorf("an unknown repository = %d", status)
	}
	if resp, err := http.Get(e.srv.URL + "/webhooks/" + name); err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET = %v, %v", resp, err)
	}
	if status, _ := e.send("/webhooks/"+name, delivery{first.header, bytes.Repeat([]byte("x"), MaxDelivery+1)}); status != http.StatusRequestEntityTooLarge {
		t.Errorf("a large delivery = %d", status)
	}
}

// TestPolling: without webhooks, the scheduled poll finds the new head and Quai's open pull
// requests.
func TestPolling(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	name := e.remote.Name
	if err := e.remote.Commit("main", "envs/prod/vms/db01.yaml", "cpu: 12\n"); err != nil {
		t.Fatal(err)
	}
	head := e.head()
	pr := e.openPR("quai/r-2-db01")

	if _, err := e.svc.jobs.Insert(ctx, ScheduleArgs{}, nil); err != nil {
		t.Fatal(err)
	}
	e.eventually("the poll fetched the new head", func() bool {
		s, err := e.store.GetRepositorySync(ctx, name)
		return err == nil && s.LastPollAt.Valid && s.Head == head && !s.LastWebhookAt.Valid
	})
	e.eventually("the poll handed on the open pull request", func() bool { return e.handler.seen(pr.ID, connectors.PROpen) })

	// The next schedule within the poll interval queues nothing.
	before, _ := e.store.GetRepositorySync(ctx, name)
	if _, err := e.svc.jobs.Insert(ctx, ScheduleArgs{}, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	if after, _ := e.store.GetRepositorySync(ctx, name); !after.LastPollAt.Time.Equal(before.LastPollAt.Time) {
		t.Error("polled again within the poll interval")
	}
}
