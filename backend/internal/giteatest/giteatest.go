// Package giteatest gives tests a real Gitea (docs/engineering/testing.md): its binary, pinned
// and checked against its SHA-256, downloaded once into the user cache directory, started with
// SQLite on a free local port with an admin user and an access token. It needs no Docker, so it
// runs inside the agents' sandbox and on CI. Helpers create throwaway repositories, protect
// branches, add users and webhooks.
//
// A package that uses it calls Main from TestMain.
package giteatest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"code.gitea.io/sdk/gitea"
)

// Version is the Gitea release, the one the compose stack runs.
const Version = "1.27.3"

// sha256s are the binaries' checksums, from the release's .sha256 files.
var sha256s = map[string]string{
	"linux-amd64":        "4da93c2c10b6980c359bcb86d5573ebfd7770e2e151756534edee24c8c12d971",
	"linux-arm64":        "04c086d36dba793546e331484a9da34571763efdfa77dc526cc98e0f10917e7b",
	"darwin-10.12-amd64": "23964155add4490ed73733fa90ea63154b724ab6fe389b7a979db4ef3d7ed8ce",
	"darwin-10.12-arm64": "fd83383e05a4185e8f852563a7599f5d8f3e18ede00d412c0f9f044771aef162",
}

// Admin is the administrator's user name.
const Admin = "quai-admin"

// Instance is a running Gitea.
type Instance struct {
	URL    string // http://127.0.0.1:port, without a trailing slash
	Token  string // the admin's access token, with every scope
	Client *gitea.Client
	bin    string
	dir    string
}

var (
	instance *Instance
	names    atomic.Int64
)

// Main starts Gitea, runs the tests and stops it. Use it as
// `func TestMain(m *testing.M) { os.Exit(giteatest.Main(m)) }`.
func Main(m *testing.M) int {
	g, stop, err := start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "giteatest:", err)
		return 1
	}
	instance = g
	code := m.Run()
	stop()
	return code
}

// Get returns the running Gitea.
func Get(t testing.TB) *Instance {
	t.Helper()
	if instance == nil {
		t.Fatal("giteatest: call giteatest.Main from TestMain")
	}
	return instance
}

func start() (*Instance, func(), error) {
	bin, err := binary()
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp("", "quai-gitea-")
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*Instance, func(), error) {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	u, err := user.Current()
	if err != nil {
		return fail(err)
	}
	g := &Instance{URL: fmt.Sprintf("http://127.0.0.1:%d", port), bin: bin, dir: dir}
	conf := filepath.Join(dir, "custom", "conf", "app.ini")
	if err := os.MkdirAll(filepath.Dir(conf), 0o750); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(conf, []byte(appIni(dir, u.Username, port)), 0o600); err != nil {
		return fail(err)
	}
	if _, err := g.cli("migrate"); err != nil {
		return fail(err)
	}
	password := random()
	if _, err := g.cli("admin", "user", "create", "--admin", "--username", Admin, "--password", password,
		"--email", Admin+"@example.com", "--must-change-password=false"); err != nil {
		return fail(err)
	}
	token, err := g.cli("admin", "user", "generate-access-token", "--username", Admin, "--token-name", "tests", "--scopes", "all", "--raw")
	if err != nil {
		return fail(err)
	}
	g.Token = strings.TrimSpace(token)

	var log bytes.Buffer
	cmd := exec.Command(bin, "web", "--work-path", dir, "--config", conf) // #nosec G204 -- the checksummed binary
	cmd.Env, cmd.Dir = env(dir), dir
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	stop := func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = os.RemoveAll(dir)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		resp, err := http.Get(g.URL + "/api/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			stop()
			return nil, nil, fmt.Errorf("gitea did not start: %v\n%s", err, log.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if g.Client, err = gitea.NewClient(g.URL, gitea.SetToken(g.Token)); err != nil {
		stop()
		return nil, nil, err
	}
	return g, stop, nil
}

// appIni configures Gitea for tests: SQLite, local only, no mail, no SSH, webhooks allowed to
// reach this machine.
func appIni(dir, runUser string, port int) string {
	return fmt.Sprintf(`APP_NAME = Quai tests
RUN_MODE = prod
RUN_USER = %[2]s
WORK_PATH = %[1]s

[server]
PROTOCOL = http
HTTP_ADDR = 127.0.0.1
HTTP_PORT = %[3]d
ROOT_URL = http://127.0.0.1:%[3]d/
DISABLE_SSH = true
OFFLINE_MODE = true
LFS_START_SERVER = false
APP_DATA_PATH = %[1]s/data

[database]
DB_TYPE = sqlite3
PATH = %[1]s/data/gitea.db
SQLITE_JOURNAL_MODE = WAL

[repository]
ROOT = %[1]s/repos
DEFAULT_BRANCH = main

[security]
INSTALL_LOCK = true
SECRET_KEY = %[4]s
INTERNAL_TOKEN = %[5]s
PASSWORD_HASH_ALGO = pbkdf2_hi

[oauth2]
JWT_SECRET = %[6]s

[service]
DISABLE_REGISTRATION = true
REQUIRE_SIGNIN_VIEW = false
DEFAULT_KEEP_EMAIL_PRIVATE = false

[webhook]
ALLOWED_HOST_LIST = loopback
DELIVER_TIMEOUT = 10

[mailer]
ENABLED = false

[actions]
ENABLED = false

[cron]
ENABLED = false

[log]
MODE = console
LEVEL = Warn
ROOT_PATH = %[1]s/log
`, dir, runUser, port, random(), random()+random(), base64.RawURLEncoding.EncodeToString([]byte(random())))
}

// cli runs a gitea command against the instance's configuration and returns its output.
func (g *Instance) cli(args ...string) (string, error) {
	args = append(args, "--work-path", g.dir, "--config", filepath.Join(g.dir, "custom", "conf", "app.ini"))
	cmd := exec.Command(g.bin, args...) // #nosec G204 -- the checksummed binary
	cmd.Env, cmd.Dir = env(g.dir), g.dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gitea %s: %w: %s", strings.Join(args[:2], " "), err, stderr.String())
	}
	return stdout.String(), nil
}

// env is a clean environment for Gitea: nothing of the user's Git configuration or proxies.
func env(dir string) []string {
	return []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH"), "USER=" + os.Getenv("USER"), "GITEA_WORK_DIR=" + dir,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(dir, "gitconfig")}
}

// binary returns the path of the Gitea binary, downloading it first if the cache does not have
// it.
func binary() (string, error) {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "darwin" {
		platform = "darwin-10.12-" + runtime.GOARCH
	}
	want, ok := sha256s[platform]
	if !ok {
		return "", fmt.Errorf("no Gitea binary for %s", platform)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "quai", "gitea")
	path := filepath.Join(dir, "gitea-"+Version+"-"+platform)
	if sum, err := fileSHA256(path); err == nil && sum == want {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://dl.gitea.com/gitea/%s/gitea-%s-%s", Version, Version, platform)
	// A download can break off (a reset connection on CI): it is tried three times.
	for attempt := 1; ; attempt++ {
		err = download(url, want, dir, path)
		if err == nil || attempt == 3 {
			return path, err
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
}

// download saves url as path, through a temporary file in dir, once its SHA-256 is want.
func download(url, want, dir, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	f, err := os.CreateTemp(dir, "partial-")
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want {
		err = fmt.Errorf("download %s: the SHA-256 does not match", url)
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o700) // #nosec G302 -- an executable, for its owner only
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- the cached binary
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func random() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// unique returns a name no other test of the run uses.
func unique(prefix string) string {
	return fmt.Sprintf("%s-%d-%s", prefix, names.Add(1), random()[:6])
}

// Repo is a repository on the instance.
type Repo struct {
	Owner, Name   string
	CloneURL      string // over HTTP; authenticate with GitHeader
	HTMLURL       string
	DefaultBranch string
	g             *Instance
}

// Repo creates a repository of the admin, with a first commit (a README) on main, and deletes
// it when the test ends.
func (g *Instance) Repo(t testing.TB, prefix string) *Repo {
	t.Helper()
	r, _, err := g.Client.CreateRepo(gitea.CreateRepoOption{Name: unique(prefix), AutoInit: true, DefaultBranch: "main", Readme: "Default"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := g.Client.DeleteRepo(r.Owner.UserName, r.Name); err != nil {
			t.Errorf("delete %s: %v", r.FullName, err)
		}
	})
	return &Repo{Owner: r.Owner.UserName, Name: r.Name, CloneURL: r.CloneURL, HTMLURL: r.HTMLURL, DefaultBranch: r.DefaultBranch, g: g}
}

// GitHeader is the HTTP header git authenticates to the instance with, as the admin.
func (g *Instance) GitHeader() string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(Admin+":"+g.Token))
}

// User is a user of the instance with an access token.
type User struct {
	Name   string
	Token  string
	Client *gitea.Client
}

// User creates a user with an access token, deleted when the test ends.
func (g *Instance) User(t testing.TB, prefix string) *User {
	t.Helper()
	name := unique(prefix)
	visibility := gitea.VisibleTypePublic
	mustChange := false
	if _, _, err := g.Client.AdminCreateUser(gitea.CreateUserOption{Username: name, Email: name + "@example.com", Password: random(),
		MustChangePassword: &mustChange, Visibility: &visibility}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := g.Client.AdminDeleteUser(name); err != nil {
			t.Errorf("delete user %s: %v", name, err)
		}
	})
	token, err := g.cli("admin", "user", "generate-access-token", "--username", name, "--token-name", "tests", "--scopes", "all", "--raw")
	if err != nil {
		t.Fatal(err)
	}
	u := &User{Name: name, Token: strings.TrimSpace(token)}
	if u.Client, err = gitea.NewClient(g.URL, gitea.SetToken(u.Token)); err != nil {
		t.Fatal(err)
	}
	return u
}

// AddCollaborator gives user write access to the repository.
func (r *Repo) AddCollaborator(t testing.TB, u *User) {
	t.Helper()
	write := gitea.AccessModeWrite
	if _, err := r.g.Client.AddCollaborator(r.Owner, r.Name, u.Name, gitea.AddCollaboratorOption{Permission: &write}); err != nil {
		t.Fatal(err)
	}
}

// Protect protects branch: changes go through pull requests whose checks succeeded. The admin
// may still push to it, to set a test up.
func (r *Repo) Protect(branch string, checks ...string) error {
	opt := gitea.CreateBranchProtectionOption{RuleName: branch, EnableStatusCheck: len(checks) > 0, StatusCheckContexts: checks,
		EnablePush: true, EnablePushWhitelist: true, PushWhitelistUsernames: []string{Admin}}
	_, _, err := r.g.Client.CreateBranchProtection(r.Owner, r.Name, opt)
	return err
}

// Unprotect removes branch's protection.
func (r *Repo) Unprotect(branch string) error {
	_, err := r.g.Client.DeleteBranchProtection(r.Owner, r.Name, branch)
	return err
}

// Write changes a file on branch through the API, creating it if needed, as a person editing it
// in the web UI would.
func (r *Repo) Write(branch, path, content string) error {
	existing, _, err := r.g.Client.GetContents(r.Owner, r.Name, branch, path)
	if err != nil || existing == nil {
		return r.Commit(branch, path, content)
	}
	_, _, err = r.g.Client.UpdateFile(r.Owner, r.Name, path, gitea.UpdateFileOptions{
		FileOptions: gitea.FileOptions{BranchName: branch, Message: "Edit " + path},
		SHA:         existing.SHA,
		Content:     base64.StdEncoding.EncodeToString([]byte(content)),
	})
	return err
}

// Commit writes a file on branch through the API, as a person editing it in the web UI would.
func (r *Repo) Commit(branch, path, content string) error {
	_, _, err := r.g.Client.CreateFile(r.Owner, r.Name, path, gitea.CreateFileOptions{
		FileOptions: gitea.FileOptions{BranchName: branch, Message: "Add " + path},
		Content:     base64.StdEncoding.EncodeToString([]byte(content)),
	})
	return err
}

// Webhook makes the repository deliver its events to url, signed with secret.
func (r *Repo) Webhook(t testing.TB, url, secret string) {
	t.Helper()
	opt := gitea.CreateHookOption{Type: gitea.HookTypeGitea, Active: true, Events: []string{"push", "pull_request", "status"},
		Config: map[string]string{"url": url, "content_type": "json", "secret": secret}}
	if _, _, err := r.g.Client.CreateRepoHook(r.Owner, r.Name, opt); err != nil {
		t.Fatal(err)
	}
}
