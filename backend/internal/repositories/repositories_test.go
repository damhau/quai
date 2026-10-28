package repositories

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/generic"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store/db"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@localhost", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@localhost")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// remote is a bare repository with one commit; commit pushes another.
type remote struct {
	t         *testing.T
	url, work string
}

func newRemote(t *testing.T, name string) *remote {
	root := t.TempDir()
	r := &remote{t: t, url: filepath.Join(root, name+".git"), work: filepath.Join(root, "work")}
	git(t, root, "init", "--quiet", "--bare", "--initial-branch=main", r.url)
	git(t, root, "clone", "--quiet", r.url, r.work)
	r.commit(name)
	return r
}

func (r *remote) commit(msg string) {
	if err := os.WriteFile(filepath.Join(r.work, "README.md"), []byte(msg), 0o600); err != nil {
		r.t.Fatal(err)
	}
	git(r.t, r.work, "add", "README.md")
	git(r.t, r.work, "commit", "--quiet", "-m", msg)
	git(r.t, r.work, "push", "--quiet", "origin", "HEAD:main")
}

func doc(t *testing.T, src string) Document {
	t.Helper()
	docs, err := Parse([]byte(src))
	if err != nil || len(docs) != 1 {
		t.Fatalf("Parse = %v, %v", docs, err)
	}
	return docs[0]
}

func testConfig(t *testing.T) *config.Config {
	return &config.Config{Git: config.Git{CacheDir: filepath.Join(t.TempDir(), "cache")}}
}

func TestKnownHosts(t *testing.T) {
	d := doc(t, `apiVersion: quai.dev/v1beta1
kind: Repository
metadata: { name: infra }
spec:
  connector: generic
  url: git@git.example.com:infra.git
  credentials:
    ref: k8s:quai/infra-deploy-key
    knownHosts: |
      # git.example.com
      git.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl
`)
	if !strings.Contains(d.Spec.Credentials.KnownHosts, "ssh-ed25519") {
		t.Errorf("known hosts = %q", d.Spec.Credentials.KnownHosts)
	}
}

func TestParse(t *testing.T) {
	d := doc(t, `apiVersion: quai.dev/v1beta1
kind: Repository
metadata: { name: infra-live }
spec:
  connector: gitea
  url: https://git.example.com/quai/infra-live.git
  credentials: { ref: "k8s:quai/infra-live#token" }
  enforcement:
    floorRules: [{ name: production-floor, when: "params.env == 'prod'", approvers: [group:platform-admins] }]
  sync: { pollInterval: 1m }
`)
	s := d.Spec
	if d.Metadata.Title != "infra-live" || s.DefaultBranch != "main" || s.Definitions != (Definitions{Path: ".quai", Branch: "main"}) ||
		s.Delivery.Mode != "pull-request" || s.Delivery.BranchPrefix != "quai/" || s.Delivery.Merge != "auto" || s.Delivery.MergeMethod != "squash" ||
		!*s.Delivery.DeleteBranch || s.Enforcement.RequiredCheck != "quai/approval" || s.Sync.PollInterval != Duration(time.Minute) ||
		s.Sync.MinInterval != Duration(10*time.Second) {
		t.Errorf("defaults = %+v", d)
	}
	// Stored as JSON, durations as text, and read back as they were.
	b, _ := json.Marshal(d)
	var back Document
	if err := json.Unmarshal(b, &back); err != nil || !strings.Contains(string(b), `"pollInterval":"1m0s"`) || back.Spec.Sync != s.Sync {
		t.Errorf("JSON = %s, %v", b, err)
	}

	for name, tt := range map[string]struct{ src, want string }{
		"known hosts":   {"apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: a }\nspec: { connector: generic, url: 'git@h:r', credentials: { ref: env:KEY, knownHosts: 'h not-a-key' } }\n", "spec.credentials.knownHosts"},
		"unknown field": {"apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: a }\nspec: { connector: gitea, url: x, branch: main }\n", "field branch not found"},
		"kind":          {"apiVersion: quai.dev/v1beta1\nkind: Repo\nmetadata: { name: a }\nspec: { connector: gitea, url: x }\n", "kind must be Repository"},
		"name":          {"apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: Infra }\nspec: { connector: gitea, url: x }\n", "metadata.name must be a lowercase DNS label"},
		"connector":     {"apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: a }\nspec: { connector: svn, url: x }\n", "spec.connector must be one of"},
		"url":           {"apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: a }\nspec: { connector: gitea }\n", "spec.url is required"},
		"secret":        {"apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: a }\nspec: { connector: gitea, url: x, credentials: { ref: hunter2 } }\n", "spec.credentials.ref must be a secret reference"},
		"delivery":      {"apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: a }\nspec: { connector: generic, url: x, delivery: { mode: pull-request } }\n", "generic Git has no pull requests"},
		"floor":         {"apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: a }\nspec: { connector: gitea, url: x, enforcement: { floorRules: [{ name: f, when: \"params.env ==\", approvers: [owners] }] } }\n", "floorRules[0].when"},
		"run token":     {"apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: a }\nspec: { connector: gitea, url: x, runner: { reportToken: { ref: token123 } } }\n", "spec.runner.reportToken.ref must be a secret reference"},
		"duration":      {"apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: a }\nspec: { connector: gitea, url: x, sync: { pollInterval: often } }\n", "is not a duration"},
	} {
		if _, err := Parse([]byte(tt.src)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
}

func TestLoadFiles(t *testing.T) {
	dir := t.TempDir()
	one := "apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: %s }\nspec: { connector: generic, url: /srv/%s.git }\n"
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("b.yaml", strings.ReplaceAll(one, "%s", "beta")+"---\n"+strings.ReplaceAll(one, "%s", "gamma"))
	write("a.yml", strings.ReplaceAll(one, "%s", "alpha"))
	write("notes.txt", "not read")
	docs, err := LoadFiles(dir)
	if err != nil || len(docs) != 3 || docs[0].Metadata.Name != "alpha" || docs[2].Metadata.Name != "gamma" {
		t.Fatalf("LoadFiles = %+v, %v", docs, err)
	}
	if docs, err := LoadFiles(filepath.Join(dir, "a.yml")); err != nil || len(docs) != 1 {
		t.Errorf("one file = %+v, %v", docs, err)
	}
	if docs, err := LoadFiles(""); err != nil || docs != nil {
		t.Errorf("no path = %+v, %v", docs, err)
	}
	write("c.yaml", strings.ReplaceAll(one, "%s", "alpha"))
	if _, err := LoadFiles(dir); err == nil || !strings.Contains(err.Error(), `"alpha" is declared in`) {
		t.Errorf("a name declared twice: %v", err)
	}
}

func TestRefresh(t *testing.T) {
	r := newRemote(t, "infra")
	d := doc(t, "apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: infra }\nspec: { connector: generic, url: "+r.url+", sync: { minInterval: 1h } }\n")
	ctx := context.Background()
	reg, err := Open(ctx, testConfig(t), Static([]Document{d}), Options{Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	repo, _ := reg.Get("infra")
	before, err := repo.Snapshot(ctx, false)
	if err != nil {
		t.Fatal(err)
	}

	// Another process commits: reads within the sync interval keep the snapshot they have.
	r.commit("second")
	if snap, _ := repo.Snapshot(ctx, false); snap.Commit != before.Commit {
		t.Fatal("a read within the sync interval fetched")
	}
	if err := repo.Refresh(ctx, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if snap, _ := repo.Snapshot(ctx, false); snap.Commit != before.Commit {
		t.Error("a refresh for a time before the last fetch fetched")
	}
	if err := repo.Refresh(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if snap, _ := repo.Snapshot(ctx, false); snap.Commit == before.Commit {
		t.Error("a refresh for a time after the last fetch did not fetch")
	}
}

func TestReconcileAndReload(t *testing.T) {
	ctx := context.Background()
	s := pgtest.Store(t)
	alpha, beta, other := newRemote(t, "alpha"), newRemote(t, "beta"), newRemote(t, "other")
	repoDoc := func(name, url, title string) Document {
		return doc(t, "apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: "+name+", title: "+title+" }\nspec: { connector: generic, url: "+url+" }\n")
	}
	generations := func() map[string]int64 {
		rows, err := s.ListRepositories(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, r := range rows {
			out[r.Name+"/"+r.ManagedBy] = r.Generation
		}
		return out
	}

	if err := Reconcile(ctx, s, []Document{repoDoc("alpha", alpha.url, "Alpha"), repoDoc("beta", beta.url, "Beta")}); err != nil {
		t.Fatal(err)
	}
	reg, err := Open(ctx, testConfig(t), FromStore(s), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if names := reg.List(); len(names) != 2 || names[0].Name != "alpha" || names[0].ManagedBy != ManagedByConfig {
		t.Fatalf("list = %+v", names)
	}
	a, _ := reg.Get("alpha")
	snap, err := a.Snapshot(ctx, false)
	if err != nil {
		t.Fatal(err)
	}

	// Reconciling the same documents changes nothing; changing one moves its generation only.
	if err := Reconcile(ctx, s, []Document{repoDoc("alpha", alpha.url, "Alpha"), repoDoc("beta", beta.url, "Beta")}); err != nil {
		t.Fatal(err)
	}
	if g := generations(); g["alpha/config"] != 1 || g["beta/config"] != 1 {
		t.Errorf("generations after the same documents = %v", g)
	}
	// alpha gets a new title, beta leaves the configuration, and the API adds gamma.
	if err := Reconcile(ctx, s, []Document{repoDoc("alpha", alpha.url, "Alpha (renamed)")}); err != nil {
		t.Fatal(err)
	}
	gamma, _ := json.Marshal(repoDoc("gamma", other.url, "Gamma"))
	if err := s.CreateRepository(ctx, db.CreateRepositoryParams{Name: "gamma", Document: gamma}); err != nil {
		t.Fatal(err)
	}
	if g := generations(); g["alpha/config"] != 2 || g["beta/config"] != 0 || g["gamma/api"] != 1 {
		t.Errorf("generations = %v", g)
	}
	if err := reg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	a2, _ := reg.Get("alpha")
	if a2 == a || a2.Title != "Alpha (renamed)" || a2.Mirror != a.Mirror {
		t.Errorf("alpha after reload = %+v (the mirror is kept when the URL stays)", a2)
	}
	if s2, _ := a2.Snapshot(ctx, false); s2 != snap {
		t.Error("the snapshot was not kept across the reload")
	}
	if _, err := reg.Get("beta"); err == nil {
		t.Error("beta is still connected")
	}
	if g, err := reg.Get("gamma"); err != nil || g.ManagedBy != ManagedByAPI {
		t.Errorf("gamma = %+v, %v", g, err)
	}

	// A new URL opens the mirror again, at the new remote.
	if err := Reconcile(ctx, s, []Document{repoDoc("alpha", other.url, "Alpha (renamed)")}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	a3, _ := reg.Get("alpha")
	if s3, err := a3.Snapshot(ctx, true); err != nil || s3.Commit != strings.TrimSpace(git(t, other.work, "rev-parse", "HEAD")) {
		t.Errorf("alpha at its new URL = %+v, %v", s3, err)
	}

	// The configuration cannot take over a repository the API manages.
	if err := Reconcile(ctx, s, []Document{repoDoc("gamma", other.url, "Gamma")}); err == nil || !strings.Contains(err.Error(), "managed through the API") {
		t.Errorf("a configuration document named like an API repository: %v", err)
	}

	// A repository that cannot be opened is left out, and connected once it can be.
	if err := Reconcile(ctx, s, []Document{repoDoc("alpha", other.url, "Alpha (renamed)"), repoDoc("delta", filepath.Join(t.TempDir(), "nope.git"), "Delta")}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Get("delta"); err == nil {
		t.Error("an unreachable repository was connected")
	}
}

// apiOnly is a connector whose credentials are not for git.
type apiOnly struct{ connectors.Connector }

func (apiOnly) GitAuth(context.Context) (gitx.Credentials, error) { return gitx.Credentials{}, nil }

func TestSecretReferences(t *testing.T) {
	ctx := context.Background()
	r := newRemote(t, "infra")
	d := doc(t, "apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: infra }\nspec:\n  connector: generic\n  url: "+r.url+
		"\n  credentials: { ref: env:INFRA_PAT }\n  runner: { reportToken: { ref: env:INFRA_RUN_TOKEN } }\n")
	env := map[string]string{"INFRA_PAT": "pat-value"}
	store := secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { v, ok := env[n]; return v, ok }}})
	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, nil))

	// The generic connector authenticates git with an SSH deploy key only: credentials on any
	// other URL keep the connection from opening.
	reg, err := Open(ctx, testConfig(t), Static([]Document{d}), Options{Secrets: store, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Get("infra"); err == nil || !strings.Contains(logs.String(), "SSH deploy key") {
		t.Errorf("credentials on a local path: %v\n%s", err, logs.String())
	}
	reg.Close()

	// A platform's connector, whose credentials are for its API; git reaches a local remote.
	conns := &connectors.Registry{Factories: map[string]connectors.Factory{"generic": func(cfg connectors.Config) (connectors.Connector, error) {
		c, err := generic.New(cfg)
		return apiOnly{c}, err
	}}}
	reg, err = Open(ctx, testConfig(t), Static([]Document{d}), Options{Secrets: store, Connectors: conns, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	repo, err := reg.Get("infra")
	if err != nil {
		t.Fatal(err)
	}
	if s, ok, err := repo.Credentials(ctx); err != nil || !ok || s.Reveal() != "pat-value" {
		t.Errorf("credentials = %v %v %v", s, ok, err)
	}
	if _, ok, err := repo.WebhookSecret(ctx); ok || err != nil {
		t.Errorf("no webhook secret: %v %v", ok, err)
	}
	// A reference that does not resolve is an error when it is used, not when the repository opens.
	if _, _, err := repo.RunToken(ctx); err == nil || !strings.Contains(err.Error(), "env:INFRA_RUN_TOKEN") {
		t.Errorf("a missing run token: %v", err)
	}
	if strings.Contains(logs.String(), "pat-value") {
		t.Errorf("the logs show the secret: %s", logs.String())
	}
}

// TestNetwork: git gets the CA bundle in the cache directory, and a partial clone when the
// connection asks for one.
func TestNetwork(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	srv.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	file := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(file, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	network, err := netx.New(file)
	if err != nil {
		t.Fatal(err)
	}
	r := newRemote(t, "infra")
	git(t, filepath.Dir(r.url), "--git-dir", r.url, "config", "uploadpack.allowFilter", "true")
	d := doc(t, "apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: infra }\nspec:\n  connector: generic\n  url: file://"+r.url+
		"\n  sync: { partialClone: true }\n")
	cfg := testConfig(t)
	reg, err := Open(ctx, cfg, Static([]Document{d}), Options{Network: network})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if repo, err := reg.Get("infra"); err != nil || repo.Connector.Info().Platform != "generic" {
		t.Errorf("the connector: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(cfg.Git.CacheDir, "ca-bundle.pem")); err != nil || !bytes.HasSuffix(b, ca) {
		t.Errorf("git's CA bundle: %v", err)
	}
	if got := git(t, cfg.Git.CacheDir, "--git-dir", filepath.Join(cfg.Git.CacheDir, "infra.git"), "config", "remote.origin.partialclonefilter"); strings.TrimSpace(got) != "blob:none" {
		t.Errorf("partial clone filter = %q", got)
	}
}
