package gitx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"

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
	if name, email, err := m.Author(ctx, sha); err != nil || name != "Bob Builder" || email != "bob@example.com" {
		t.Errorf("author = %q %q, %v", name, email, err)
	}
	if _, _, err := m.Author(ctx, "--all"); err == nil {
		t.Error("the author of an option")
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

// TestTrailerInjection: a trailer's value never adds a line, whatever a name holds: CR, LF, NEL,
// Unicode's line and paragraph separators and other control characters are dropped, in the commit
// message and in a pull request's comment alike.
func TestTrailerInjection(t *testing.T) {
	ls, ps, nel := string(rune(0x2028)), string(rune(0x2029)), string(rune(0x85))
	for _, name := range []string{
		"fiona\rApproved-by: mallory",
		"fiona" + ls + "Approved-by: mallory",
		"fiona" + ps + "Approved-by: mallory",
		"fiona" + nel + "Approved-by: mallory",
		"fiona\x00\x1b[2J\tApproved-by: mallory",
	} {
		tr := Trailer{Key: "Approved-by", Value: name + " (on behalf of alice)"}
		msg := Message("subject", "", tr)
		lines := strings.FieldsFunc(msg, func(r rune) bool {
			return r == '\n' || r == '\r' || r == rune(0x2028) || r == rune(0x2029) || r == rune(0x85)
		})
		if len(lines) != 2 || !strings.HasPrefix(lines[1], "Approved-by: fiona") || strings.ContainsFunc(lines[1], unicode.IsControl) {
			t.Errorf("%q makes %q", name, msg)
		}
		if s := tr.String(); strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == rune(0x2028) || r == rune(0x2029) }) {
			t.Errorf("%q makes the line %q", name, s)
		}
	}
	// A line feed ends the value, as it always did.
	if got := TrailerValue("fiona\nApproved-by: mallory"); got != "fiona" {
		t.Errorf("TrailerValue = %q", got)
	}
}

func TestAncestryAndChanges(t *testing.T) {
	bare, push := remote(t)
	m := mirror(t, bare)
	first, _ := m.Head(ctx, "main")
	push("envs/prod/vms/web01.yaml", "cpu: 8\n")
	push("envs/dev/vms/app01.yaml", "cpu: 2\n")
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	last, _ := m.Head(ctx, "main")
	for _, c := range []struct {
		a, b string
		want bool
	}{{first, last, true}, {last, first, false}, {last, last, true}, {first, strings.Repeat("0", 40), false}} {
		if got, err := m.IsAncestor(ctx, c.a, c.b); err != nil || got != c.want {
			t.Errorf("IsAncestor(%.7s, %.7s) = %v, %v", c.a, c.b, got, err)
		}
	}
	if _, err := m.IsAncestor(ctx, "HEAD", last); err == nil {
		t.Error("a ref was taken for a commit")
	}

	all, err := m.Changes(ctx, "", last)
	if err != nil || len(all) != 3 || all[0].SHA != last || !slices.Equal(all[0].Files, []string{"envs/dev/vms/app01.yaml"}) ||
		!slices.Equal(all[1].Files, []string{"envs/prod/vms/web01.yaml"}) || all[2].SHA != first {
		t.Errorf("all changes = %+v, %v", all, err)
	}
	since, err := m.Changes(ctx, first, last)
	if err != nil || len(since) != 2 {
		t.Errorf("changes since the first commit = %+v, %v", since, err)
	}
}

// TestAuthors: the commits of a range that change some paths, with every identity they carry:
// author, committer, Co-authored-by trailers, and the committer of a merge that brings a change.
func TestAuthors(t *testing.T) {
	bare, _ := remote(t)
	work := filepath.Join(t.TempDir(), "work")
	mustRun(t, "", "clone", "--quiet", bare, work)
	commit := func(env []string, args ...string) {
		t.Helper()
		if _, err := run(ctx, work, env, args...); err != nil {
			t.Fatal(err)
		}
	}
	as := func(author, committer string) []string {
		a, ae := splitAddress(author)
		c, ce := splitAddress(committer)
		return []string{"GIT_AUTHOR_NAME=" + a, "GIT_AUTHOR_EMAIL=" + ae, "GIT_COMMITTER_NAME=" + c, "GIT_COMMITTER_EMAIL=" + ce}
	}
	m := mirror(t, bare)
	first, _ := m.Head(ctx, "main")

	// A change to the type on a side branch, with co-authors; one to another file on main; the
	// side branch merged by Mia.
	mustRun(t, work, "checkout", "--quiet", "-b", "side")
	write(t, filepath.Join(work, ".quai/types/vm.yaml"), "kind: ResourceType\n")
	mustRun(t, work, "add", "--all")
	commit(as("Dora <dora@example.com>", "Cy <cy@example.com>"), "commit", "--quiet", "-m",
		"Change the type\n\nCo-authored-by: Carol <Carol@Example.com>\nCo-authored-by: eve@example.com")
	mustRun(t, work, "checkout", "--quiet", "main")
	write(t, filepath.Join(work, "README.md"), "# other\n")
	mustRun(t, work, "add", "--all")
	commit(as("Ann <ann@example.com>", "Ann <ann@example.com>"), "commit", "--quiet", "-m", "Not the type")
	commit(as("Ann <ann@example.com>", "Mia <mia@example.com>"), "merge", "--quiet", "--no-ff", "side", "-m", "Merge side")
	mustRun(t, work, "push", "--quiet", "origin", "HEAD:main")
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	last, _ := m.Head(ctx, "main")

	got, err := m.Authors(ctx, first, last, []string{".quai/types/vm.yaml", ".quai/schemas/vm.json"})
	if err != nil || len(got) != 2 || got[0].SHA != last {
		t.Fatalf("authors = %+v, %v", got, err)
	}
	if want := []Identity{{RoleAuthor, "Ann", "ann@example.com"}, {RoleCommitter, "Mia", "mia@example.com"}}; !slices.Equal(got[0].Identities, want) {
		t.Errorf("the merge = %+v", got[0].Identities)
	}
	if want := []Identity{{RoleAuthor, "Dora", "dora@example.com"}, {RoleCommitter, "Cy", "cy@example.com"},
		{RoleCoAuthor, "Carol", "Carol@Example.com"}, {RoleCoAuthor, "", "eve@example.com"}}; !slices.Equal(got[1].Identities, want) {
		t.Errorf("the change = %+v", got[1].Identities)
	}
	// The whole history: the first commit did not touch the type either.
	if all, err := m.Authors(ctx, "", last, []string{".quai/types/vm.yaml"}); err != nil || len(all) != 2 {
		t.Errorf("all authors = %+v, %v", all, err)
	}
	// None in an empty range, nor without paths; a pattern is a literal path.
	if none, err := m.Authors(ctx, last, last, []string{".quai/types/vm.yaml"}); err != nil || len(none) != 0 {
		t.Errorf("an empty range = %+v, %v", none, err)
	}
	if none, err := m.Authors(ctx, first, last, nil); err != nil || len(none) != 0 {
		t.Errorf("no paths = %+v, %v", none, err)
	}
	if none, err := m.Authors(ctx, first, last, []string{".quai/types/*"}); err != nil || len(none) != 0 {
		t.Errorf("a pattern = %+v, %v", none, err)
	}
	for _, bad := range [][]string{{"../x"}, {"-p"}, {".git/config"}} {
		if _, err := m.Authors(ctx, first, last, bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if _, err := m.Authors(ctx, strings.Repeat("0", 40), last, []string{".quai/types/vm.yaml"}); err == nil {
		t.Error("a commit the mirror does not have was accepted")
	}
}

// Merges: a branch merges onto a base that changed other lines, not onto one that changed the
// same lines.
func TestMerges(t *testing.T) {
	bare, push := remote(t)
	m := mirror(t, bare)
	branch, err := m.CommitAndPush(ctx, Commit{Branch: "quai/web01", Base: "main", Message: "web01\n", Author: bob,
		Changes: []plan.Change{{Op: plan.OpUpdate, Path: "envs/prod/vms/web01.yaml", Before: []byte("cpu: 4\n"), After: []byte("cpu: 4\nmemory: 8\n")}}})
	if err != nil {
		t.Fatal(err)
	}
	push("envs/prod/vms/db01.yaml", "cpu: 16\n")
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	other, _ := m.Head(ctx, "main")
	push("envs/prod/vms/web01.yaml", "cpu: 4\ndisk: 100\n")
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	same, _ := m.Head(ctx, "main")
	for _, c := range []struct {
		base string
		want bool
	}{{other, true}, {same, false}} {
		if got, err := m.Merges(ctx, c.base, branch); err != nil || got != c.want {
			t.Errorf("Merges(%.7s, %.7s) = %v, %v", c.base, branch, got, err)
		}
	}
	if _, err := m.Merges(ctx, "main", branch); err == nil {
		t.Error("a ref was taken for a commit")
	}
}
