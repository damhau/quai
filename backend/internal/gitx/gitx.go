// Package gitx works with Git repositories through the git command line
// (docs/design/01-repository-connections.md#sync): one bare mirror per repository, a temporary
// worktree per job, and commits pushed with the requester as author, Quai as committer and the
// Quai trailers (docs/design/05-delivery-and-approvals.md). Before committing, the regions a
// request changes are fingerprinted again on the current head: if one changed since the plan,
// the request is stale. A push that loses a race with another push is retried on the new head.
//
// Git runs without a terminal prompt, without system or global configuration and without hooks,
// so nothing from the machine or the repository runs with it. What it needs to reach the remote
// (credentials, the CA bundle, the proxy) comes through the environment, never the command line,
// and is removed from the errors it reports (docs/design/12-git-connectors.md#network-server).
package gitx

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/engine/plan"
)

var (
	// ErrStale is returned when a region changed on the branch since the request was planned.
	ErrStale = errors.New("the request is stale: its regions changed since it was planned")
	// ErrNoChange is returned when the changes leave the branch as it is.
	ErrNoChange = errors.New("the changes leave the branch as it is")
	// ErrNotFound is returned for a file that is not in the commit.
	ErrNotFound = fs.ErrNotExist
)

// Signature names a person in a commit.
type Signature struct {
	Name  string
	Email string
}

// Credentials authenticate git with the remote: an HTTP header, or an SSH key.
type Credentials struct {
	// Header is an HTTP header ("Authorization: Basic …") sent to the remote's host only.
	Header string
	// SSHKey is a private key in OpenSSH format. KnownHosts holds the remote's host keys, in
	// known_hosts format: the host key is always verified.
	SSHKey     []byte
	KnownHosts []byte
}

// Options say how a mirror reaches its remote and commits.
type Options struct {
	Committer Signature // the identity Quai commits as
	// Auth returns the credentials, before each command that may reach the remote (nil: none).
	Auth func(context.Context) (Credentials, error)
	// CAFile is the PEM bundle git verifies HTTPS servers with (http.sslCAInfo); empty: git's.
	CAFile string
	// Proxy returns the proxy to reach an HTTP(S) remote through, or nil (nil: no proxy).
	Proxy func(*url.URL) (*url.URL, error)
	// PartialClone clones without file contents (--filter=blob:none), which git then fetches
	// when a command needs them. It applies when the mirror is first cloned.
	PartialClone bool
}

// Mirror is the bare mirror of one remote repository.
type Mirror struct {
	remote    string
	dir       string
	committer Signature
	opts      Options
	mu        sync.Mutex // one commit at a time per mirror in this process

	// beforePush runs before each push; tests use it to lose a race.
	beforePush func(attempt int)
}

// OpenMirror returns the mirror of remote in dir, cloning it the first time and fetching it
// afterwards.
func OpenMirror(ctx context.Context, remote, dir string, opts Options) (*Mirror, error) {
	m := &Mirror{remote: remote, dir: dir, committer: opts.Committer, opts: opts}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
			return nil, err
		}
		args := []string{"clone", "--mirror", "--quiet"}
		if opts.PartialClone {
			args = append(args, "--filter=blob:none")
		}
		if _, err := m.command(ctx, "", nil, true, append(args, "--", remote, dir)...); err != nil {
			return nil, fmt.Errorf("mirror %s: %w", redact(remote), err)
		}
		// Fetch every ref like a mirror, but push one branch at a time, which a mirror remote
		// refuses.
		if _, err := m.git(ctx, "", "config", "remote.origin.mirror", "false"); err != nil {
			return nil, err
		}
		return m, nil
	}
	// The connection's URL may have changed since the mirror was cloned.
	if _, err := m.git(ctx, "", "remote", "set-url", "origin", remote); err != nil {
		return nil, err
	}
	return m, m.Fetch(ctx)
}

// Fetch brings the mirror up to date with the remote, pruning deleted branches.
func (m *Mirror) Fetch(ctx context.Context) error {
	if _, err := m.command(ctx, "", nil, true, "fetch", "--prune", "--quiet", "origin"); err != nil {
		return fmt.Errorf("fetch %s: %w", redact(m.remote), err)
	}
	return nil
}

// Head returns the commit at the tip of branch in the mirror.
func (m *Mirror) Head(ctx context.Context, branch string) (string, error) {
	if err := checkRef(branch); err != nil {
		return "", err
	}
	out, err := m.git(ctx, "", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("branch %s: %w", branch, ErrNotFound)
	}
	return strings.TrimSpace(out), nil
}

// ReadFile returns a file of a commit.
func (m *Mirror) ReadFile(ctx context.Context, commit, file string) ([]byte, error) {
	if err := checkCommit(commit); err != nil {
		return nil, err
	}
	if err := checkPath(file); err != nil {
		return nil, err
	}
	out, err := m.git(ctx, "", "cat-file", "blob", commit+":"+file)
	if err != nil {
		return nil, fmt.Errorf("%s at %.12s: %w", file, commit, ErrNotFound)
	}
	return []byte(out), nil
}

// Worktree is a temporary checkout of one commit, detached from any branch.
type Worktree struct {
	m   *Mirror
	dir string
}

// Worktree checks out commit in a new temporary directory. Given paths, it is sparse: only those
// files and directories, with everything below them, are checked out (and, from a partial clone,
// fetched). Close removes it.
func (m *Mirror) Worktree(ctx context.Context, commit string, paths ...string) (*Worktree, error) {
	if err := checkCommit(commit); err != nil {
		return nil, err
	}
	for _, p := range paths {
		if err := checkPath(p); err != nil {
			return nil, err
		}
	}
	dir, err := os.MkdirTemp("", "quai-worktree-")
	if err != nil {
		return nil, err
	}
	w := &Worktree{m: m, dir: dir}
	if len(paths) == 0 {
		_, err = m.git(ctx, "", "worktree", "add", "--detach", "--quiet", dir, commit)
	} else {
		err = w.sparse(ctx, commit, paths)
	}
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	return w, nil
}

// sparse checks out the paths of commit only. The patterns are anchored and escaped, so that a
// path matches itself and nothing else.
func (w *Worktree) sparse(ctx context.Context, commit string, paths []string) error {
	if _, err := w.m.git(ctx, "", "worktree", "add", "--detach", "--no-checkout", "--quiet", w.dir, commit); err != nil {
		return err
	}
	var patterns strings.Builder
	for _, p := range paths {
		patterns.WriteString("/" + globEscaper.Replace(p) + "\n")
	}
	if _, err := w.m.git(ctx, w.dir, "sparse-checkout", "set", "--no-cone", "--stdin", withStdin(patterns.String())); err != nil {
		return err
	}
	_, err := w.m.git(ctx, w.dir, "checkout", "--quiet", "--detach", commit)
	return err
}

// globEscaper escapes what sparse-checkout patterns (gitignore syntax) give a meaning to.
var globEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`, ` `, `\ `)

// Dir is the worktree's directory.
func (w *Worktree) Dir() string { return w.dir }

// FS is the worktree's files.
func (w *Worktree) FS() fs.FS { return os.DirFS(w.dir) }

// Close removes the worktree.
func (w *Worktree) Close() error {
	_, err := w.m.git(context.Background(), "", "worktree", "remove", "--force", w.dir)
	_ = os.RemoveAll(w.dir)
	_, _ = w.m.git(context.Background(), "", "worktree", "prune")
	return err
}

// Trailer is a "Key: Value" line at the end of a commit message.
type Trailer struct {
	Key   string
	Value string
}

// Message formats a commit message: subject, body and trailers, as docs/design/05 shows.
func Message(subject, body string, trailers ...Trailer) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(firstLine(subject)))
	b.WriteString("\n")
	if body = strings.TrimSpace(body); body != "" {
		b.WriteString("\n" + body + "\n")
	}
	if len(trailers) > 0 {
		b.WriteString("\n")
		for _, t := range trailers {
			b.WriteString(t.Key + ": " + firstLine(t.Value) + "\n")
		}
	}
	return b.String()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// Commit is a change to deliver straight to a branch.
type Commit struct {
	Branch string
	// Base is the branch to start from when Branch does not exist yet (a pull request's
	// source branch); empty: Branch must exist.
	Base    string
	Changes []plan.Change
	Message string
	Author  Signature
	// Fingerprints are the regions' fingerprints when the request was planned, by path; each
	// must still hold on the branch's head.
	Fingerprints map[string]string
}

// maxAttempts bounds the retries after losing a push race.
const maxAttempts = 4

// CommitAndPush applies c on the branch's current head and pushes it, and returns the new
// commit. It returns ErrStale when a region changed since the plan, and ErrNoChange when the
// changes are already on the branch.
func (m *Mirror) CommitAndPush(ctx context.Context, c Commit) (string, error) {
	if err := checkRef(c.Branch); err != nil {
		return "", err
	}
	if c.Base != "" {
		if err := checkRef(c.Base); err != nil {
			return "", err
		}
	}
	for _, ch := range c.Changes {
		if err := checkPath(ch.Path); err != nil {
			return "", err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for attempt := 1; ; attempt++ {
		sha, err := m.commitOnce(ctx, c, attempt)
		if !errors.Is(err, errRejected) || attempt == maxAttempts {
			return sha, err
		}
	}
}

var errRejected = errors.New("the push was rejected")

func (m *Mirror) commitOnce(ctx context.Context, c Commit, attempt int) (string, error) {
	if err := m.Fetch(ctx); err != nil {
		return "", err
	}
	head, err := m.Head(ctx, c.Branch)
	if errors.Is(err, ErrNotFound) && c.Base != "" {
		head, err = m.Head(ctx, c.Base)
	}
	if err != nil {
		return "", err
	}
	// Only the regions to check and the files to change are checked out.
	sparse := slices.Collect(maps.Keys(c.Fingerprints))
	for _, ch := range c.Changes {
		sparse = append(sparse, ch.Path)
	}
	slices.Sort(sparse)
	wt, err := m.Worktree(ctx, head, slices.Compact(sparse)...)
	if err != nil {
		return "", err
	}
	defer func() { _ = wt.Close() }()

	for _, p := range slices.Sorted(maps.Keys(c.Fingerprints)) {
		strategy := locate.File
		if info, err := os.Lstat(filepath.Join(wt.dir, filepath.FromSlash(p))); err == nil && info.IsDir() {
			strategy = locate.Dir
		}
		now, err := locate.Fingerprint(wt.FS(), locate.Region{Strategy: strategy, Path: p})
		if err != nil {
			return "", err
		}
		if now != c.Fingerprints[p] {
			return "", fmt.Errorf("%w: %s", ErrStale, p)
		}
	}
	var paths []string
	for _, ch := range c.Changes {
		full := filepath.Join(wt.dir, filepath.FromSlash(ch.Path))
		paths = append(paths, ch.Path)
		switch ch.Op {
		case plan.OpDelete:
			if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
		default:
			if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
				return "", err
			}
			if err := os.WriteFile(full, ch.After, 0o600); err != nil {
				return "", err
			}
		}
	}
	if _, err := m.git(ctx, wt.dir, append([]string{"add", "--all", "--"}, paths...)...); err != nil {
		return "", err
	}
	if _, err := m.git(ctx, wt.dir, "diff", "--cached", "--quiet"); err == nil {
		return head, ErrNoChange
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + c.Author.Name, "GIT_AUTHOR_EMAIL=" + c.Author.Email,
		"GIT_COMMITTER_NAME=" + m.committer.Name, "GIT_COMMITTER_EMAIL=" + m.committer.Email,
	}
	if _, err := m.command(ctx, wt.dir, env, m.opts.PartialClone, "commit", "--quiet", "--no-verify", "--cleanup=verbatim", "-F", "-", withStdin(c.Message)); err != nil {
		return "", err
	}
	sha, err := m.git(ctx, wt.dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if m.beforePush != nil {
		m.beforePush(attempt)
	}
	if _, err := m.command(ctx, wt.dir, nil, true, "push", "--quiet", "--no-verify", "origin", "HEAD:refs/heads/"+c.Branch); err != nil {
		if strings.Contains(err.Error(), "rejected") || strings.Contains(err.Error(), "non-fast-forward") || strings.Contains(err.Error(), "fetch first") {
			return "", fmt.Errorf("%w: %v", errRejected, err)
		}
		return "", fmt.Errorf("push to %s: %w", redact(m.remote), err)
	}
	return strings.TrimSpace(sha), nil
}

// LogEntry is a commit, as the Git log page shows it.
type LogEntry struct {
	SHA     string
	Author  string
	Email   string
	Date    time.Time
	Subject string
	Body    string
}

// Log returns the latest commits of branch, those touching file when it is not empty.
func (m *Mirror) Log(ctx context.Context, branch, file string, limit int) ([]LogEntry, error) {
	if err := checkRef(branch); err != nil {
		return nil, err
	}
	args := []string{"log", fmt.Sprintf("-n%d", limit), "--format=%H%x1f%an%x1f%ae%x1f%aI%x1f%s%x1f%b%x1e", "refs/heads/" + branch}
	if file != "" {
		if err := checkPath(file); err != nil {
			return nil, err
		}
		args = append(args, "--", file)
	}
	out, err := m.git(ctx, "", args...)
	if err != nil {
		return nil, err
	}
	var entries []LogEntry
	for _, rec := range strings.Split(out, "\x1e") {
		f := strings.Split(strings.TrimSpace(rec), "\x1f")
		if len(f) < 6 {
			continue
		}
		date, _ := time.Parse(time.RFC3339, f[3])
		entries = append(entries, LogEntry{SHA: f[0], Author: f[1], Email: f[2], Date: date, Subject: f[4], Body: strings.TrimSpace(f[5])})
	}
	return entries, nil
}

// Parents returns a commit's parents: one for a squash or a rebase, two for a merge commit.
func (m *Mirror) Parents(ctx context.Context, commit string) ([]string, error) {
	if err := checkCommit(commit); err != nil {
		return nil, err
	}
	out, err := m.git(ctx, "", "rev-list", "--parents", "-n1", commit)
	if err != nil {
		return nil, fmt.Errorf("commit %.12s: %w", commit, ErrNotFound)
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return nil, fmt.Errorf("commit %.12s: %w", commit, ErrNotFound)
	}
	return f[1:], nil
}

// Show returns a commit's header, message and patch.
func (m *Mirror) Show(ctx context.Context, commit string) (string, error) {
	if err := checkCommit(commit); err != nil {
		return "", err
	}
	return m.git(ctx, "", "show", "--format=%H%n%an <%ae>%n%aI%n%n%B", commit)
}

var (
	shaRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

func checkCommit(c string) error {
	if !shaRe.MatchString(c) {
		return fmt.Errorf("invalid commit %q", c)
	}
	return nil
}

func checkRef(r string) error {
	if !refRe.MatchString(r) || strings.Contains(r, "..") || strings.HasSuffix(r, "/") || strings.HasSuffix(r, ".lock") {
		return fmt.Errorf("invalid branch %q", r)
	}
	return nil
}

func checkPath(p string) error {
	if p == "" || p != path.Clean(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../") || p == ".." ||
		strings.HasPrefix(p, ".git/") || p == ".git" || strings.HasPrefix(p, "-") {
		return fmt.Errorf("invalid path %q", p)
	}
	return nil
}

// redact hides credentials in a remote URL.
func redact(remote string) string {
	if i := strings.Index(remote, "://"); i >= 0 {
		if at := strings.LastIndex(remote, "@"); at > i {
			return remote[:i+3] + "***@" + remote[at+1:]
		}
	}
	return remote
}

// userinfoRe finds the credentials of URLs in a text.
var userinfoRe = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]+@`)

// scrub hides the credentials of URLs, and the secrets given, in a text.
func scrub(text string, secrets []string) string {
	text = userinfoRe.ReplaceAllString(text, "${1}***@")
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	return text
}

// git runs a command in the mirror (dir empty) or in one of its worktrees. From a partial clone,
// any command may fetch missing contents, so each can reach the remote.
func (m *Mirror) git(ctx context.Context, dir string, args ...string) (string, error) {
	return m.command(ctx, dir, nil, m.opts.PartialClone, args...)
}

// command runs git with env; when remote, with what it needs to reach the remote.
func (m *Mirror) command(ctx context.Context, dir string, env []string, remote bool, args ...string) (string, error) {
	if dir == "" && args[0] != "clone" {
		args = append([]string{"--git-dir", m.dir}, args...)
	}
	if !remote {
		return run(ctx, dir, env, args...)
	}
	t, err := m.transport(ctx)
	if err != nil {
		return "", err
	}
	defer t.close()
	return runScrubbed(ctx, dir, append(env, t.env...), t.secrets, args...)
}

// transport is what git needs to reach the remote: configuration passed in the environment
// (GIT_CONFIG_COUNT), the secrets to remove from its errors, and temporary files.
type transport struct {
	env     []string
	secrets []string
	tmp     string
}

func (t *transport) close() {
	if t.tmp != "" {
		_ = os.RemoveAll(t.tmp)
	}
}

func (m *Mirror) transport(ctx context.Context) (*transport, error) {
	t := &transport{}
	var config [][2]string
	u, err := url.Parse(m.remote)
	web := err == nil && (u.Scheme == "https" || u.Scheme == "http")
	if m.opts.CAFile != "" {
		config = append(config, [2]string{"http.sslCAInfo", m.opts.CAFile})
	}
	if web && m.opts.Proxy != nil {
		proxy, err := m.opts.Proxy(u)
		if err != nil {
			return nil, fmt.Errorf("proxy for %s: %w", redact(m.remote), err)
		}
		if proxy != nil {
			config = append(config, [2]string{"http.proxy", proxy.String()})
			if pw, ok := proxy.User.Password(); ok {
				t.secrets = append(t.secrets, pw)
			}
		}
	}
	if m.opts.Auth != nil {
		c, err := m.opts.Auth(ctx)
		if err != nil {
			return nil, fmt.Errorf("credentials for %s: %w", redact(m.remote), err)
		}
		if c.Header != "" {
			name, value, ok := strings.Cut(c.Header, ":")
			if !web || !ok || strings.TrimSpace(name) == "" || strings.ContainsAny(c.Header, "\r\n") {
				return nil, fmt.Errorf("credentials for %s: an HTTP header \"Name: value\" for an HTTP(S) remote", redact(m.remote))
			}
			// Scoped to the remote's host: the header goes nowhere else.
			config = append(config, [2]string{"http." + u.Scheme + "://" + u.Host + "/.extraHeader", c.Header})
			value = strings.TrimSpace(value)
			t.secrets = append(t.secrets, value)
			if scheme, cred, ok := strings.Cut(value, " "); ok {
				t.secrets = append(t.secrets, cred)
				// A Basic header carries the password readable once decoded.
				if plain, err := base64.StdEncoding.DecodeString(cred); err == nil && strings.EqualFold(scheme, "basic") {
					_, password, _ := strings.Cut(string(plain), ":")
					t.secrets = append(t.secrets, string(plain), password)
				}
			}
		}
		if len(c.SSHKey) > 0 {
			cmd, err := t.ssh(c)
			if err != nil {
				t.close()
				return nil, err
			}
			config = append(config, [2]string{"core.sshCommand", cmd})
		}
	}
	t.env = append(t.env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(config)))
	for i, kv := range config {
		t.env = append(t.env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return t, nil
}

// ssh writes the key and the host keys to files only the user can read, and returns the ssh
// command that uses them and nothing else: no configuration file, no agent, no prompt.
func (t *transport) ssh(c Credentials) (string, error) {
	if len(c.KnownHosts) == 0 {
		return "", errors.New("an SSH key needs the remote's host keys (known_hosts): they are always verified")
	}
	dir, err := os.MkdirTemp("", "quai-ssh-")
	if err != nil {
		return "", err
	}
	t.tmp = dir
	key, hosts := filepath.Join(dir, "key"), filepath.Join(dir, "known_hosts")
	pem := append(bytes.TrimSpace(c.SSHKey), '\n') // ssh refuses a key without its final newline
	if err := os.WriteFile(key, pem, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(hosts, c.KnownHosts, 0o600); err != nil {
		return "", err
	}
	return "ssh -F /dev/null -i " + shellQuote(key) + " -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes" +
		" -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + shellQuote(hosts) + " -o GlobalKnownHostsFile=/dev/null", nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// withStdin marks the last argument of run as standard input.
func withStdin(s string) string { return "\x00stdin:" + s }

// run runs git with a clean environment: no prompts, no system or global configuration, no
// hooks; only PATH is kept, for git to find its helpers. An argument made by withStdin is sent on
// standard input instead.
func run(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	return runScrubbed(ctx, dir, env, nil, args...)
}

// runScrubbed is run, with the credentials of URLs and the secrets removed from its errors.
func runScrubbed(ctx context.Context, dir string, env, secrets []string, args ...string) (string, error) {
	var input string
	if n := len(args); n > 0 && strings.HasPrefix(args[n-1], "\x00stdin:") {
		input, args = strings.TrimPrefix(args[n-1], "\x00stdin:"), args[:n-1]
	}
	full := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "init.defaultBranch=main"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...) // #nosec G204 -- fixed binary, arguments checked above
	cmd.Dir = dir
	cmd.Env = append([]string{
		"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"HOME=" + os.TempDir(), "PATH=" + os.Getenv("PATH"), "LC_ALL=C",
	}, env...)
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, scrub(strings.TrimSpace(stderr.String()), secrets))
	}
	return stdout.String(), nil
}
