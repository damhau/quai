package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

const examples = "../../../../examples/gitops-repo"

func copyExamples(t *testing.T) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	err := filepath.WalkDir(examples, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(examples, p)
		fsys[filepath.ToSlash(rel)] = &fstest.MapFile{Data: b}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

func TestExamplesPass(t *testing.T) {
	r := Run(os.DirFS(examples))
	if len(r.Problems) > 0 || len(r.Notes) > 0 || r.Types != 1 || r.Blueprints != 2 || r.Resources != 4 {
		t.Errorf("report = %+v", r)
	}
}

// Seeded broken copies of the examples fail with messages that say what to fix.
func TestBrokenCopies(t *testing.T) {
	fsys := copyExamples(t)
	set := func(p, src string) { fsys[p] = &fstest.MapFile{Data: []byte(src)} }
	web01 := string(fsys["envs/prod/vms/web01.yaml"].Data)
	set("envs/prod/vms/web01.yaml", strings.Replace(web01, "cpu: 4 ", "cpu: 99", 1))
	set("envs/dev/vms/broken.yaml", "cpu: [4\n")
	set("envs/dev/vms/list.yaml", "- a\n")
	// backup_policy is hidden when backups are off, so Quai would drop it on the first edit.
	db01 := string(fsys["envs/prod/vms/db01.yaml"].Data)
	set("envs/prod/vms/db01.yaml", strings.Replace(db01, "backup: true", "backup: false", 1))
	// The hidden value has an anchor that another value uses: Quai cannot remove it safely.
	set("envs/dev/vms/anchored.yaml", "owner: x\ntemplate: rhel-9\ncpu: 2\nmemory_gb: 4\ndisks: [{ size_gb: 50 }]\nnetwork: { vlan: 110 }\nbackup: false\nbackup_policy: &p hourly\ntags: { policy: *p }\n")
	set("blueprints/bad.yaml", "apiVersion: quai.dev/v1alpha1\nkind: Blueprint\n")

	r := Run(fsys)
	text := r.Problems.Error()
	for _, want := range []string{
		"blueprints/bad.yaml: apiVersion: quai.dev/v1alpha1 is the prototype's format",
		"envs/dev/vms/broken.yaml: the file is not valid YAML",
		"envs/dev/vms/list.yaml: must hold a mapping (type vm)",
		"envs/dev/vms/anchored.yaml: round trip: Quai cannot edit this file: yamledit: refused",
		`envs/prod/vms/db01.yaml: round trip: Quai would rewrite this file on its first edit (removes "backup_policy: hourly")`,
		"envs/prod/vms/web01.yaml: cpu: must be at most 32 (schema catalog/schemas/vm.schema.json)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no problem contains %q; problems:\n%s", want, text)
		}
	}
	if r.Resources != 7 {
		t.Errorf("resources = %d, want 7", r.Resources)
	}
}

func TestNotesAndTypeProblems(t *testing.T) {
	typ := func(name, locate, generate string) string {
		return "apiVersion: quai.dev/v1beta1\nkind: ResourceType\nmetadata: { name: " + name + ", title: T }\nspec:\n  schema: s.json\n  params: [{ name: name }]\n  locate: " + locate + "\n" + generate
	}
	fsys := fstest.MapFS{
		"s.json":                {Data: []byte(`{"type": "object", "properties": {"a": {"type": "string"}}}`)},
		".quai/types/apps.yaml": {Data: []byte(typ("apps", `{ strategy: dir, path: "apps/{name}" }`, ""))},
		".quai/types/map.yaml":  {Data: []byte(typ("map", `{ strategy: file, path: "m/{name}.yaml" }`, "  generate: { kind: mapping, template: { a: x } }\n"))},
		".quai/types/item.yaml": {Data: []byte(typ("item", `{ strategy: file, path: "items/{name}.yaml" }`, ""))},
		".quai/types/flag.yaml": {Data: []byte(typ("flag", `{ strategy: file, path: "flags/{name}.yaml" }`, "  ui: { fields: { a: { visibleIf: inputs.flag } } }\n"))},
		"flags/f.yaml":          {Data: []byte("a: x\n")},
		"items/one.yaml":        {Data: []byte("a: 1\n")},
		"items/two.yaml":        {Data: []byte("a: x\nzz: kept\n")},
	}
	r := Run(fsys)
	if len(r.Notes) != 2 || !strings.Contains(r.Notes.Error(), "the dir locator with the identity generator is not available yet") {
		t.Errorf("notes = %v", r.Notes)
	}
	if len(r.Problems) != 2 || !strings.HasPrefix(r.Problems[0].String(), "flags/f.yaml: round trip: visibleIf of a:") || r.Problems[1].String() != "items/one.yaml: a: must be a string (schema s.json)" {
		t.Errorf("problems = %v", r.Problems)
	}
}

func TestChangedLines(t *testing.T) {
	if got := changedLines([]byte("a\nb\n"), []byte("b\na\n")); got != "reorders lines" {
		t.Errorf("changedLines = %q", got)
	}
	if got := changedLines([]byte("a: 1\n"), []byte("a: 2\n")); got != `removes "a: 1", adds "a: 2"` {
		t.Errorf("changedLines = %q", got)
	}
}
