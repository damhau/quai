package repositories

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// withExample copies the example GitOps repository into the remote and pushes it.
func (r *remote) withExample() {
	r.t.Helper()
	src := os.DirFS("../../../examples/gitops-repo")
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(r.work, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.push("the example repository")
}

// write changes a file (removes it when content is empty) and pushes, as author.
func (r *remote) write(path, content, author string) {
	r.t.Helper()
	p := filepath.Join(r.work, filepath.FromSlash(path))
	if content == "" {
		if err := os.Remove(p); err != nil {
			r.t.Fatal(err)
		}
	} else if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
	git(r.t, r.work, "add", "--all")
	git(r.t, r.work, "-c", "user.name="+author, "-c", "user.email="+strings.ToLower(author)+"@example.com",
		"commit", "--quiet", "--author", author+" <"+strings.ToLower(author)+"@example.com>", "-m", "edit "+path)
	git(r.t, r.work, "push", "--quiet", "origin", "HEAD:main")
}

func (r *remote) push(msg string) {
	git(r.t, r.work, "add", "--all")
	git(r.t, r.work, "commit", "--quiet", "-m", msg)
	git(r.t, r.work, "push", "--quiet", "origin", "HEAD:main")
}

func openOne(t *testing.T, r *remote, s *store.Store) *Repo {
	t.Helper()
	d := doc(t, "apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: infra }\nspec:\n  connector: generic\n  url: "+r.url+"\n  sync: { minInterval: 1h }\n")
	reg, err := Open(context.Background(), testConfig(t), Static([]Document{d}), Options{Store: s, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	repo, err := reg.Get("infra")
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// TestDefinitionsPerCommit: each commit's active definitions are stored; a broken edit keeps the
// previous version of the type active, with its problems; changes are events, by author.
func TestDefinitionsPerCommit(t *testing.T) {
	ctx := context.Background()
	s := pgtest.Store(t)
	r := newRemote(t, "infra")
	r.withExample()
	repo := openOne(t, r, s)
	first, err := repo.Snapshot(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	vm := first.Set.Types["vm"]
	if vm.Digest == "" || vm.Commit != first.Commit || vm.Broken || len(first.Set.Blueprints) == 0 {
		t.Fatalf("first set = %+v", first.Set)
	}
	original, _ := os.ReadFile(filepath.Join(r.work, ".quai/types/vm.yaml"))

	// Alice breaks the type: its previous version stays active.
	r.write(".quai/types/vm.yaml", strings.Replace(string(original), "kind: ResourceType", "kind: ResourceTyp", 1), "Alice")
	broken, err := repo.Snapshot(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if v := broken.Set.Types["vm"]; !v.Broken || v.Commit != first.Commit || v.Digest != vm.Digest {
		t.Errorf("the broken type's version = %+v", v)
	}
	if broken.Definitions.Types["vm"] == nil || len(broken.Problems) == 0 || broken.Problems[0].Path != ".quai/types/vm.yaml" {
		t.Errorf("the catalog lost the type, or its problems: %v %v", broken.Definitions.Types["vm"], broken.Problems)
	}
	// Another process, with no snapshot before, takes the stored set and loads the kept
	// version from its commit.
	other, err := openOne(t, r, s).Snapshot(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if v := other.Set.Types["vm"]; !v.Broken || v.Commit != first.Commit || other.Definitions.Types["vm"] == nil {
		t.Errorf("another process: %+v, type %v", v, other.Definitions.Types["vm"])
	}

	// Bob fixes it with a change: a new version.
	r.write(".quai/types/vm.yaml", strings.Replace(string(original), "Virtual machine (Terraform)", "Virtual machine", 1), "Bob")
	fixed, err := repo.Snapshot(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if v := fixed.Set.Types["vm"]; v.Broken || v.Commit != fixed.Commit || v.Digest == vm.Digest {
		t.Errorf("the fixed type's version = %+v (was %+v)", v, vm)
	}
	// Carol removes it.
	r.write(".quai/types/vm.yaml", "", "Carol")
	removed, err := repo.Snapshot(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := removed.Set.Types["vm"]; ok || removed.Definitions.Types["vm"] != nil {
		t.Error("a removed type stays")
	}

	// Each set is stored by commit; the changes are events.
	set, ok, err := DefinitionsAt(ctx, s, "infra", broken.Commit)
	if err != nil || !ok || !set.Types["vm"].Broken {
		t.Errorf("the stored set = %+v, %v, %v", set, ok, err)
	}
	events, err := s.ListTargetEvents(ctx, db.ListTargetEventsParams{Target: "definitions:infra/type/vm", MaxRows: 100})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i := len(events) - 1; i >= 0; i-- {
		var data map[string]any
		_ = json.Unmarshal(events[i].Data, &data)
		got = append(got, events[i].Kind+" by "+strings.Fields(events[i].Actor)[0])
		if events[i].Kind == "definition_broken" && len(data["problems"].([]any)) == 0 {
			t.Error("the broken event has no problems")
		}
	}
	if strings.Join(got, ", ") != "definition_broken by Alice, definition_changed by Bob, definition_removed by Carol" {
		t.Errorf("events = %v", got)
	}

}

// TestFallbackWithoutStore: without a database, the previous snapshot keeps a broken type.
func TestFallbackWithoutStore(t *testing.T) {
	ctx := context.Background()
	r := newRemote(t, "infra")
	r.withExample()
	repo := openOne(t, r, nil)
	first, err := repo.Snapshot(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(filepath.Join(r.work, ".quai/types/vm.yaml"))
	r.write(".quai/types/vm.yaml", string(original)+"\nunknownField: 1\n", "Alice")
	broken, err := repo.Snapshot(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if v := broken.Set.Types["vm"]; !v.Broken || v.Commit != first.Commit || broken.Definitions.Types["vm"] == nil {
		t.Errorf("without a database: %+v", v)
	}
}
