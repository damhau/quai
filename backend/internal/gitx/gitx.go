// Package gitx works with Git repositories through the git command line
// (docs/design/01-repository-connections.md#sync): one bare mirror per repository, a temporary
// worktree per job, and commits pushed with the requester as author, Quai as committer and the
// Quai trailers (docs/design/05-delivery-and-approvals.md). Before committing, the regions a
// request changes are fingerprinted again on the current head: if one changed since the plan,
// the request is stale. A push that loses a race with another push is retried on the new head.
//
// Git runs without a terminal prompt, without system or global configuration and without hooks,
// so nothing from the machine or the repository runs with it.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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

// Mirror is the bare mirror of one remote repository.
type Mirror struct {
	remote    string
	dir       string
	committer Signature
	mu        sync.Mutex // one commit at a time per mirror in this process

	// beforePush runs before each push; tests use it to lose a race.
	beforePush func(attempt int)
}

// OpenMirror returns the mirror of remote in dir, cloning it the first time and fetching it
// afterwards. committer is the identity Quai commits as.
func OpenMirror(ctx context.Context, remote, dir string, committer Signature) (*Mirror, error) {
	m := &Mirror{remote: remote, dir: dir, committer: committer}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
			return nil, err
		}
		if _, err := run(ctx, "", nil, "clone", "--mirror", "--quiet", remote, dir); err != nil {
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
	if _, err := m.git(ctx, "", "fetch", "--prune", "--quiet", "origin"); err != nil {
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

// Worktree checks out commit in a new temporary directory. Close removes it.
func (m *Mirror) Worktree(ctx context.Context, commit string) (*Worktree, error) {
	if err := checkCommit(commit); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "quai-worktree-")
	if err != nil {
		return nil, err
	}
	if _, err := m.git(ctx, "", "worktree", "add", "--detach", "--quiet", dir, commit); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return &Worktree{m: m, dir: dir}, nil
}

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
	Branch  string
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
	if err != nil {
		return "", err
	}
	wt, err := m.Worktree(ctx, head)
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
	if _, err := run(ctx, wt.dir, env, "commit", "--quiet", "--no-verify", "--cleanup=verbatim", "-F", "-", withStdin(c.Message)); err != nil {
		return "", err
	}
	sha, err := m.git(ctx, wt.dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if m.beforePush != nil {
		m.beforePush(attempt)
	}
	if _, err := m.git(ctx, wt.dir, "push", "--quiet", "--no-verify", "origin", "HEAD:refs/heads/"+c.Branch); err != nil {
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

func (m *Mirror) git(ctx context.Context, dir string, args ...string) (string, error) {
	if dir == "" {
		args = append([]string{"--git-dir", m.dir}, args...)
	}
	return run(ctx, dir, nil, args...)
}

// withStdin marks the last argument of run as standard input.
func withStdin(s string) string { return "\x00stdin:" + s }

// run runs git with a clean environment: no prompts, no system or global configuration, no
// hooks; only PATH is kept, for git to find its helpers. An argument made by withStdin is sent on
// standard input instead.
func run(ctx context.Context, dir string, env []string, args ...string) (string, error) {
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
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
