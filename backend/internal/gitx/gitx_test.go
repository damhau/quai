package gitx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/engine/plan"
)

var (
	ctx   = context.Background()
	quai  = Signature{Name: "Quai", Email: "quai@example.com"}
	bob   = Signature{Name: "Bob Builder", Email: "bob@example.com"}
	files = map[string]string{
		"envs/prod/vms/web01.yaml": "cpu: 4\n",
		"envs/prod/vms/db01.yaml":  "cpu: 12\n",
		"README.md":                "# infra\n",
	}
)

// remote makes a bare repository with one commit of files on main, and returns its path and a
// function that pushes another commit to it, as a person would.
func remote(t *testing.T) (string, func(file, content string)) {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "remote.git")
	work := filepath.Join(dir, "work")
	mustRun(t, "", "init", "--quiet", "--bare", "--initial-branch=main", bare)
	mustRun(t, "", "clone", "--quiet", bare, work)
	for p, c := range files {
		write(t, filepath.Join(work, p), c)
	}
	commitAll(t, work, "initial")
	return bare, func(file, content string) {
		mustRun(t, work, "pull", "--quiet", "--rebase", "origin", "main")
		write(t, filepath.Join(work, file), content)
		commitAll(t, work, "a person's change to "+file)
	}
}

func commitAll(t *testing.T, work, msg string) {
	t.Helper()
	mustRun(t, work, "add", "--all")
	if _, err := run(ctx, work, []string{"GIT_AUTHOR_NAME=Alice", "GIT_AUTHOR_EMAIL=alice@example.com", "GIT_COMMITTER_NAME=Alice", "GIT_COMMITTER_EMAIL=alice@example.com"}, "commit", "--quiet", "-m", msg); err != nil {
		t.Fatal(err)
	}
	mustRun(t, work, "push", "--quiet", "origin", "HEAD:main")
}

func mustRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := run(ctx, dir, nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mirror(t *testing.T, bare string) *Mirror {
	t.Helper()
	m, err := OpenMirror(ctx, bare, filepath.Join(t.TempDir(), "mirrors", "infra-live.git"), Options{Committer: quai})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func fingerprint(t *testing.T, m *Mirror, file string) string {
	t.Helper()
	head, err := m.Head(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	wt, err := m.Worktree(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Close()
	fp, err := locate.Fingerprint(wt.FS(), locate.Region{Strategy: locate.File, Path: file})
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestMirrorReads(t *testing.T) {
	bare, push := remote(t)
	m := mirror(t, bare)
	head, err := m.Head(ctx, "main")
	if err != nil || len(head) != 40 {
		t.Fatalf("Head = %q, %v", head, err)
	}
	b, err := m.ReadFile(ctx, head, "envs/prod/vms/web01.yaml")
	if err != nil || string(b) != "cpu: 4\n" {
		t.Errorf("ReadFile = %q, %v", b, err)
	}
	if _, err := m.ReadFile(ctx, head, "nope.yaml"); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadFile of a missing file: %v", err)
	}
	wt, err := m.Worktree(ctx, head)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := fs.ReadFile(wt.FS(), "README.md"); err != nil || string(b) != "# infra\n" {
		t.Errorf("worktree read = %q, %v", b, err)
	}
	dir := wt.Dir()
	if err := wt.Close(); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Error("the worktree was not removed")
	}

	// A second open fetches what was pushed since.
	push("envs/dev/vms/new.yaml", "cpu: 2\n")
	m2, err := OpenMirror(ctx, bare, m.dir, Options{Committer: quai})
	if err != nil {
		t.Fatal(err)
	}
	head2, _ := m2.Head(ctx, "main")
	if head2 == head {
		t.Error("the mirror did not fetch the new commit")
	}
	if _, err := m.Head(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Head of a missing branch: %v", err)
	}
}

func TestCommitAndPush(t *testing.T) {
	bare, _ := remote(t)
	m := mirror(t, bare)
	web01 := "envs/prod/vms/web01.yaml"
	msg := Message("quai: resize infra-live/vm/prod/web01", "Black Friday traffic.",
		Trailer{"Quai-Request", "#1042 3b6f2a8c"}, Trailer{"Quai-Change-Set", "sha256:9f2c"}, Trailer{"Quai-Stack", "envs/prod"},
		Trailer{"Requested-by", "Bob Builder <bob@example.com>"})
	sha, err := m.CommitAndPush(ctx, Commit{
		Branch: "main", Author: bob, Message: msg,
		Changes: []plan.Change{
			{Path: web01, Op: plan.OpUpdate, After: []byte("cpu: 8\n")},
			{Path: "envs/prod/vms/db01.yaml", Op: plan.OpDelete},
			{Path: "envs/test/vms/new.yaml", Op: plan.OpCreate, After: []byte("cpu: 2\n")},
		},
		Fingerprints: map[string]string{web01: fingerprint(t, m, web01), "envs/test/vms/new.yaml": locate.Absent},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The remote has the commit, with the author, committer, message and trailers.
	show := mustRun(t, "", "--git-dir", bare, "show", "--no-patch", "--format=%H%n%an <%ae>%n%cn <%ce>%n%B", "main")
	for _, want := range []string{sha, "Bob Builder <bob@example.com>", "Quai <quai@example.com>", "quai: resize infra-live/vm/prod/web01\n\nBlack Friday traffic.\n\nQuai-Request: #1042 3b6f2a8c\nQuai-Change-Set: sha256:9f2c\nQuai-Stack: envs/prod\nRequested-by: Bob Builder <bob@example.com>"} {
		if !strings.Contains(show, want) {
			t.Errorf("the pushed commit lacks %q:\n%s", want, show)
		}
	}
	if trailer := strings.TrimSpace(mustRun(t, "", "--git-dir", bare, "log", "-1", "--format=%(trailers:key=Quai-Request,valueonly)", "main")); trailer != "#1042 3b6f2a8c" {
		t.Errorf("Quai-Request trailer = %q", trailer)
	}
	if b, err := m.ReadFile(ctx, sha, web01); err != nil || string(b) != "cpu: 8\n" {
		t.Errorf("web01 = %q, %v", b, err)
	}
	if _, err := m.ReadFile(ctx, sha, "envs/prod/vms/db01.yaml"); !errors.Is(err, ErrNotFound) {
		t.Errorf("db01 was not deleted: %v", err)
	}

	// The same change again changes nothing.
	again, err := m.CommitAndPush(ctx, Commit{Branch: "main", Author: bob, Message: "again\n", Changes: []plan.Change{{Path: web01, Op: plan.OpUpdate, After: []byte("cpu: 8\n")}}})
	if !errors.Is(err, ErrNoChange) || again != sha {
		t.Errorf("no-op commit = %s, %v", again, err)
	}

	entries, err := m.Log(ctx, "main", web01, 10)
	if err != nil || len(entries) != 2 || entries[0].SHA != sha || entries[0].Author != "Bob Builder" || entries[0].Subject != "quai: resize infra-live/vm/prod/web01" ||
		!strings.HasPrefix(entries[0].Body, "Black Friday traffic.") || entries[1].Subject != "initial" || entries[0].Date.IsZero() {
		t.Errorf("log = %+v, %v", entries, err)
	}
	if out, err := m.Show(ctx, sha); err != nil || !strings.Contains(out, "+cpu: 8") {
		t.Errorf("show = %q, %v", out, err)
	}
}

func TestStale(t *testing.T) {
	bare, push := remote(t)
	m := mirror(t, bare)
	web01 := "envs/prod/vms/web01.yaml"
	planned := fingerprint(t, m, web01)
	// Someone edits web01 after the plan.
	push(web01, "cpu: 6 # by hand\n")
	_, err := m.CommitAndPush(ctx, Commit{Branch: "main", Author: bob, Message: "m\n",
		Changes: []plan.Change{{Path: web01, Op: plan.OpUpdate, After: []byte("cpu: 8\n")}}, Fingerprints: map[string]string{web01: planned}})
	if !errors.Is(err, ErrStale) || !strings.Contains(err.Error(), web01) {
		t.Errorf("stale request: %v", err)
	}
	// A file to create that someone created meanwhile is stale too.
	push("envs/prod/vms/new.yaml", "cpu: 1\n")
	_, err = m.CommitAndPush(ctx, Commit{Branch: "main", Author: bob, Message: "m\n",
		Changes: []plan.Change{{Path: "envs/prod/vms/new.yaml", Op: plan.OpCreate, After: []byte("cpu: 2\n")}}, Fingerprints: map[string]string{"envs/prod/vms/new.yaml": locate.Absent}})
	if !errors.Is(err, ErrStale) {
		t.Errorf("created meanwhile: %v", err)
	}
	// A change elsewhere in the repository does not make it stale.
	push("README.md", "# infra, edited\n")
	if _, err := m.CommitAndPush(ctx, Commit{Branch: "main", Author: bob, Message: "m\n",
		Changes: []plan.Change{{Path: "envs/prod/vms/db01.yaml", Op: plan.OpUpdate, After: []byte("cpu: 16\n")}}, Fingerprints: map[string]string{"envs/prod/vms/db01.yaml": fingerprintOf("cpu: 12\n")}}); err != nil {
		t.Errorf("an unrelated edit made the request stale: %v", err)
	}
}

// fingerprintOf is a file region's fingerprint for content (locate.Fingerprint).
func fingerprintOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestPushRaceIsRetried(t *testing.T) {
	bare, push := remote(t)
	m := mirror(t, bare)
	web01 := "envs/prod/vms/web01.yaml"
	fp := fingerprint(t, m, web01)
	// Someone pushes between Quai's fetch and push, on the first two attempts.
	m.beforePush = func(attempt int) {
		if attempt <= 2 {
			push("README.md", strings.Repeat("x", attempt)+"\n")
		}
	}
	sha, err := m.CommitAndPush(ctx, Commit{Branch: "main", Author: bob, Message: "quai: resize\n",
		Changes: []plan.Change{{Path: web01, Op: plan.OpUpdate, After: []byte("cpu: 8\n")}}, Fingerprints: map[string]string{web01: fp}})
	if err != nil {
		t.Fatal(err)
	}
	parent := strings.TrimSpace(mustRun(t, "", "--git-dir", bare, "log", "-1", "--format=%s", sha+"^"))
	if parent != "a person's change to README.md" {
		t.Errorf("the commit is on %q, want the latest change", parent)
	}
	// Always losing the race fails after a few attempts.
	m.beforePush = func(attempt int) { push("README.md", strings.Repeat("y", attempt)+"\n") }
	if _, err := m.CommitAndPush(ctx, Commit{Branch: "main", Author: bob, Message: "m\n",
		Changes: []plan.Change{{Path: web01, Op: plan.OpUpdate, After: []byte("cpu: 10\n")}}}); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("a push that always loses: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	bare, _ := remote(t)
	m := mirror(t, bare)
	for _, p := range []string{"", "/etc/passwd", "../x", "a/../../x", ".git/config", "-rf", "a//b"} {
		if _, err := m.CommitAndPush(ctx, Commit{Branch: "main", Changes: []plan.Change{{Path: p, Op: plan.OpCreate}}}); err == nil || !strings.Contains(err.Error(), "invalid path") {
			t.Errorf("path %q: %v", p, err)
		}
	}
	for _, b := range []string{"", "-x", "a..b", "a/", "x.lock", "a b"} {
		if _, err := m.CommitAndPush(ctx, Commit{Branch: b}); err == nil || !strings.Contains(err.Error(), "invalid branch") {
			t.Errorf("branch %q: %v", b, err)
		}
	}
	for _, c := range []string{"HEAD", "main", "zzzzzzz", "--all"} {
		if _, err := m.Worktree(ctx, c); err == nil {
			t.Errorf("Worktree(%q) succeeded", c)
		}
		if _, err := m.Show(ctx, c); err == nil {
			t.Errorf("Show(%q) succeeded", c)
		}
	}
	if _, err := m.CommitAndPush(ctx, Commit{Branch: "missing", Message: "m\n"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing branch: %v", err)
	}
	if _, err := OpenMirror(ctx, filepath.Join(t.TempDir(), "nothing.git"), filepath.Join(t.TempDir(), "m.git"), Options{Committer: quai}); err == nil {
		t.Error("a mirror of nothing")
	}
	if got := redact("https://user:secret@dev.azure.com/org/p/_git/r"); got != "https://***@dev.azure.com/org/p/_git/r" {
		t.Errorf("redact = %q", got)
	}
	if got := Message("subject\nignored", ""); got != "subject\n" {
		t.Errorf("Message = %q", got)
	}
}
