package definitions

import (
	"encoding/json"
	"io/fs"
	"os"
	"slices"
	"testing"
	"testing/fstest"
)

// copyFS copies a directory into a MapFS, which the tests then change.
func copyFS(t *testing.T, dir string) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	err := fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(os.DirFS(dir), p)
		out[p] = &fstest.MapFile{Data: b}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFilesAndDigest(t *testing.T) {
	fsys := copyFS(t, "../../../../examples/gitops-repo")
	repo, problems := Load(fsys)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	vm := repo.Types["vm"]
	if vm == nil || !slices.Equal(vm.Files, []string{".quai/types/vm.yaml", "catalog/schemas/vm.schema.json"}) {
		t.Fatalf("vm files = %v", vm.Files)
	}
	for _, b := range repo.Blueprints {
		if !slices.Contains(b.Files, b.Path) || !slices.Contains(b.Files, ".quai/types/vm.yaml") {
			t.Errorf("blueprint %s files = %v", b.Metadata.Name, b.Files)
		}
	}
	before := Digest(fsys, vm.Files)
	if Digest(fsys, vm.Files) != before {
		t.Error("the digest is not stable")
	}
	// A schema reached through a $ref is one of the type's files: changing it changes the type.
	fsys["catalog/schemas/common.schema.json"] = &fstest.MapFile{Data: []byte(`{"$defs": {"size": {"type": "integer", "minimum": 1}}}`)}
	var doc map[string]any
	if err := json.Unmarshal(fsys["catalog/schemas/vm.schema.json"].Data, &doc); err != nil {
		t.Fatal(err)
	}
	delete(doc, "$id") // relative references then resolve in the repository
	doc["properties"].(map[string]any)["spare"] = map[string]any{"$ref": "common.schema.json#/$defs/size"}
	b, _ := json.Marshal(doc)
	fsys["catalog/schemas/vm.schema.json"] = &fstest.MapFile{Data: b}
	repo, problems = Load(fsys)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	vm = repo.Types["vm"]
	if !slices.Contains(vm.Files, "catalog/schemas/common.schema.json") {
		t.Fatalf("the referenced schema is not among the files: %v", vm.Files)
	}
	withRef := Digest(fsys, vm.Files)
	fsys["catalog/schemas/common.schema.json"] = &fstest.MapFile{Data: []byte(`{"$defs": {"size": {"type": "integer", "minimum": 2}}}`)}
	if withRef == before || Digest(fsys, vm.Files) == withRef {
		t.Error("a changed schema does not change the digest")
	}
	// Files that are not the type's do not change it.
	unchanged := Digest(fsys, vm.Files)
	fsys["README.md"] = &fstest.MapFile{Data: []byte("changed")}
	if Digest(fsys, vm.Files) != unchanged {
		t.Error("another file changed the digest")
	}
}
