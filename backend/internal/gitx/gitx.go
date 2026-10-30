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
	// SigningKey returns the SSH private key Quai signs its commits with (gpg.format=ssh), read
	// before each commit; nil, or no key, leaves them unsigned.
	SigningKey func(context.Context) ([]byte, error)
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
	Base string
	// Replace, when set, rebuilds Branch from Base's head and replaces it, provided Branch is
	// still at this commit (a force push with a lease): a pull request's branch rebuilt on a new
	// base, never over someone else's push.
	Replace string
	Changes []plan.Change
	Message string
	Author  Signature
	// Fingerprints are the regions' fingerprints when the request was planned, by reference
	// (locate.Region.Ref); each must still hold on the branch's head.
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
	if (errors.Is(err, ErrNotFound) || c.Replace != "") && c.Base != "" {
		head, err = m.Head(ctx, c.Base)
	}
	if err != nil {
		return "", err
	}
	// Only the regions to check and the files to change are checked out.
	var sparse []string
	for ref := range c.Fingerprints {
		sparse = append(sparse, locate.RefPath(ref))
	}
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
		now, err := locate.FingerprintRef(wt.FS(), p)
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
			// A change to entries of a shared file is made again on the file as it is now.
			after := ch.After
			if len(ch.Entries) > 0 {
				current, err := fs.ReadFile(wt.FS(), ch.Path)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					return "", err
				}
				if after, err = ch.On(current); err != nil {
					return "", fmt.Errorf("%w: %s: %v", ErrStale, ch.Path, err)
				}
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
				return "", err
			}
			if err := os.WriteFile(full, after, 0o600); err != nil {
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
	args := []string{"commit", "--quiet", "--no-verify", "--cleanup=verbatim", "-F", "-", withStdin(c.Message)}
	signing, cleanup, err := m.signing(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if _, err := m.command(ctx, wt.dir, env, m.opts.PartialClone, append(signing, args...)...); err != nil {
		return "", err
	}
	sha, err := m.git(ctx, wt.dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if m.beforePush != nil {
		m.beforePush(attempt)
	}
	push := []string{"push", "--quiet", "--no-verify", "origin", "HEAD:refs/heads/" + c.Branch}
	if c.Replace != "" {
		push = append(push[:3], "--force-with-lease=refs/heads/"+c.Branch+":"+c.Replace, "origin", "HEAD:refs/heads/"+c.Branch)
	}
	if _, err := m.command(ctx, wt.dir, nil, true, push...); err != nil {
		if strings.Contains(err.Error(), "rejected") || strings.Contains(err.Error(), "non-fast-forward") || strings.Contains(err.Error(), "fetch first") {
			return "", fmt.Errorf("%w: %v", errRejected, err)
		}
		return "", fmt.Errorf("push to %s: %w", redact(m.remote), err)
	}
	return strings.TrimSpace(sha), nil
}

// signing returns the options that sign a commit with the signing key, written to a file only
// the user can read, and what removes it; none without a key.
func (m *Mirror) signing(ctx context.Context) ([]string, func(), error) {
	none := func() {}
	if m.opts.SigningKey == nil {
		return nil, none, nil
	}
	key, err := m.opts.SigningKey(ctx)
	if err != nil {
		return nil, none, fmt.Errorf("signing key: %w", err)
	}
	if len(bytes.TrimSpace(key)) == 0 {
		return nil, none, nil
	}
	dir, err := os.MkdirTemp("", "quai-signing-")
	if err != nil {
		return nil, none, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	file := filepath.Join(dir, "key")
	// ssh-keygen refuses a key without its final newline.
	if err := os.WriteFile(file, append(bytes.TrimSpace(key), '\n'), 0o600); err != nil {
		cleanup()
		return nil, none, err
	}
	return []string{"-c", "gpg.format=ssh", "-c", "user.signingKey=" + file, "-c", "commit.gpgSign=true"}, cleanup, nil
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

// DeleteBranch deletes a branch on the remote, and in the mirror. A branch already gone is no error.
func (m *Mirror) DeleteBranch(ctx context.Context, branch string) error {
	if err := checkRef(branch); err != nil {
		return err
	}
	if _, err := m.command(ctx, "", nil, true, "push", "--quiet", "origin", "--delete", "refs/heads/"+branch); err != nil && !strings.Contains(err.Error(), "remote ref does not exist") {
		return fmt.Errorf("delete %s on %s: %w", branch, redact(m.remote), err)
	}
	_, _ = m.git(ctx, "", "update-ref", "-d", "refs/heads/"+branch)
	return nil
}

// MergeBase returns the best common ancestor of two commits.
func (m *Mirror) MergeBase(ctx context.Context, a, b string) (string, error) {
	if err := checkCommit(a); err != nil {
		return "", err
	}
	if err := checkCommit(b); err != nil {
		return "", err
	}
	out, err := m.git(ctx, "", "merge-base", a, b)
	if err != nil {
		return "", fmt.Errorf("merge base of %.12s and %.12s: %w", a, b, err)
	}
	return strings.TrimSpace(out), nil
}

// IsAncestor reports whether a is b or one of its ancestors. A commit the mirror does not have
// is no ancestor.
func (m *Mirror) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	if err := checkCommit(a); err != nil {
		return false, err
	}
	if err := checkCommit(b); err != nil {
		return false, err
	}
	out, err := m.git(ctx, "", "rev-parse", a+"^{commit}", b+"^{commit}") // checked: hex SHAs, never options
	if err != nil {
		return false, nil // one of them is unknown here
	}
	shas := strings.Fields(out)
	if len(shas) != 2 {
		return false, nil
	}
	base, err := m.git(ctx, "", "merge-base", shas[0], shas[1])
	if err != nil {
		return false, nil // unrelated histories
	}
	return strings.TrimSpace(base) == shas[0], nil
}

// CommitFiles is a commit and the files it touched.
type CommitFiles struct {
	SHA   string
	Files []string
}

// Changes returns the commits reachable from to and not from from (all of to's history when
// from is empty), newest first, each with the files it touched; merges list none, the commits
// they merge do.
func (m *Mirror) Changes(ctx context.Context, from, to string) ([]CommitFiles, error) {
	if err := checkCommit(to); err != nil {
		return nil, err
	}
	rng := to
	if from != "" {
		if err := checkCommit(from); err != nil {
			return nil, err
		}
		rng = from + ".." + to
	}
	out, err := m.git(ctx, "", "log", "--format=%x1e%H", "--name-only", "--no-renames", rng, "--")
	if err != nil {
		return nil, fmt.Errorf("changes in %s: %w", rng, err)
	}
	var commits []CommitFiles
	for _, rec := range strings.Split(out, "\x1e") {
		lines := strings.Split(strings.TrimSpace(rec), "\n")
		if len(lines) == 0 || lines[0] == "" {
			continue
		}
		c := CommitFiles{SHA: lines[0]}
		for _, l := range lines[1:] {
			if l = strings.TrimSpace(l); l != "" {
				c.Files = append(c.Files, l)
			}
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// FileChange is a file a diff touches: Status is A (added), M (modified) or D (deleted).
type FileChange struct {
	Path   string
	Status string
}

// Diff returns the files that differ between two commits, without rename detection.
func (m *Mirror) Diff(ctx context.Context, from, to string) ([]FileChange, error) {
	if err := checkCommit(from); err != nil {
		return nil, err
	}
	if err := checkCommit(to); err != nil {
		return nil, err
	}
	out, err := m.git(ctx, "", "diff", "--name-status", "--no-renames", "-z", from, to)
	if err != nil {
		return nil, err
	}
	f := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	var changes []FileChange
	for i := 0; i+1 < len(f); i += 2 {
		changes = append(changes, FileChange{Status: f[i][:1], Path: f[i+1]})
	}
	return changes, nil
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
			// Credentials in the proxy URL go with the first request, as Go's clients send them:
			// curl's default probes without them first, and a proxy that closes the connection
			// after its 407 then fails the clone.
			if proxy.User != nil {
				config = append(config, [2]string{"http.proxyAuthMethod", "basic"})
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

// subcommand names the git command args run, past the options before it.
func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c" || args[i] == "--git-dir":
			i++
		case !strings.HasPrefix(args[i], "-"):
			return args[i]
		}
	}
	return "command"
}

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
		"HOME=" + os.TempDir(), "TMPDIR=" + os.TempDir(), "PATH=" + os.Getenv("PATH"), "LC_ALL=C",
	}, env...)
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", subcommand(args), err, scrub(strings.TrimSpace(stderr.String()), secrets))
	}
	return stdout.String(), nil
}

// Files returns the files of a commit, read from the mirror's objects without a checkout: an
// fs.FS that also reads files (fs.ReadFileFS) and directories (fs.ReadDirFS), for reading
// definitions as they were at an earlier commit.
func (m *Mirror) Files(ctx context.Context, commit string) (fs.FS, error) {
	if err := checkCommit(commit); err != nil {
		return nil, err
	}
	return &commitFS{m: m, ctx: ctx, commit: commit}, nil
}

type commitFS struct {
	m      *Mirror
	ctx    context.Context
	commit string
}

func (c *commitFS) object(name string) string {
	if name == "." {
		return c.commit + "^{tree}"
	}
	return c.commit + ":" + name
}

func (c *commitFS) ReadFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) || name == "." {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrInvalid}
	}
	out, err := c.m.git(c.ctx, "", "cat-file", "blob", c.object(name))
	if err != nil {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	return []byte(out), nil
}

func (c *commitFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	out, err := c.m.git(c.ctx, "", "ls-tree", "-z", c.object(name))
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	var entries []fs.DirEntry
	for _, rec := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		meta, entryName, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			continue
		}
		var mode fs.FileMode
		switch f[1] {
		case "tree":
			mode = fs.ModeDir
		case "blob":
			if f[0] == "120000" {
				mode = fs.ModeSymlink
			}
		default:
			continue // a submodule
		}
		entries = append(entries, entry{name: entryName, mode: mode})
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}

func (c *commitFS) Open(name string) (fs.File, error) {
	if b, err := c.ReadFile(name); err == nil {
		return &memFile{entry: entry{name: path.Base(name)}, r: bytes.NewReader(b), size: int64(len(b))}, nil
	}
	if _, err := c.ReadDir(name); err == nil {
		return &memFile{entry: entry{name: path.Base(name), mode: fs.ModeDir}}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// entry is a file of a commit, as a directory lists it.
type entry struct {
	name string
	mode fs.FileMode
}

func (e entry) Name() string               { return e.name }
func (e entry) IsDir() bool                { return e.mode.IsDir() }
func (e entry) Type() fs.FileMode          { return e.mode.Type() }
func (e entry) Info() (fs.FileInfo, error) { return info{e, 0}, nil }

type info struct {
	entry
	size int64
}

func (i info) Size() int64        { return i.size }
func (i info) Mode() fs.FileMode  { return i.mode | 0o444 }
func (i info) ModTime() time.Time { return time.Time{} }
func (i info) Sys() any           { return nil }

// memFile is an opened file or directory of a commit.
type memFile struct {
	entry
	r    *bytes.Reader
	size int64
}

func (f *memFile) Stat() (fs.FileInfo, error) { return info{f.entry, f.size}, nil }
func (f *memFile) Close() error               { return nil }
func (f *memFile) Read(b []byte) (int, error) {
	if f.r == nil {
		return 0, &fs.PathError{Op: "read", Path: f.name, Err: fs.ErrInvalid}
	}
	return f.r.Read(b)
}
