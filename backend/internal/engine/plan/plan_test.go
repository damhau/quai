package plan

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/schema"
)

var update = flag.Bool("update", false, "rewrite the golden files of this package's own output")

const examples = "../../../../examples/gitops-repo"

func loadExamples(t *testing.T) (fstest.MapFS, *definitions.Repository) {
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
	repo, problems := definitions.Load(fsys)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	return fsys, repo
}

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/golden", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The prototype wrote these files for the same intents (testdata/golden/prototype/README.md).
func TestVMEditsMatchThePrototype(t *testing.T) {
	fsys, repo := loadExamples(t)
	vm := repo.Types["vm"]
	tests := []struct {
		name   string
		req    generate.Request
		golden string
	}{
		{"create", generate.Request{
			Kind:   generate.Create,
			Params: map[string]string{"env": "prod", "name": "shop-app01"},
			Values: map[string]any{
				"network":       map[string]any{"ip": "10.0.20.15", "vlan": 120},
				"tags":          map[string]any{"app": "shop", "cost_center": "4711"},
				"backup_policy": "daily",
				"backup":        true,
				"disks":         []any{map[string]any{"tier": "ssd", "size_gb": 80}, map[string]any{"size_gb": 500, "tier": "standard"}},
				"memory_gb":     16,
				"cpu":           4,
				"template":      "rhel-9",
				"description":   "Shop: application server",
			},
			Owner: "alice",
		}, "prototype/create-shop-app01.yaml"},
		{"resize", generate.Request{
			Kind: generate.Action, Params: map[string]string{"env": "prod", "name": "web01"},
			ActionName: "resize", ActionInputs: map[string]any{"cpu": 8, "memory_gb": 32},
		}, "prototype/resize-web01.yaml"},
		{"extend-disk", generate.Request{
			Kind: generate.Action, Params: map[string]string{"env": "prod", "name": "db01"},
			ActionName: "extend-disk", ActionInputs: map[string]any{"size_gb": 250},
		}, "prototype/extend-disk-db01.yaml"},
	}
	for _, tt := range tests {
		cs, err := Type(fsys, vm, tt.req)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if len(cs.Changes) != 1 {
			t.Fatalf("%s: %d changes", tt.name, len(cs.Changes))
		}
		if got, want := string(cs.Changes[0].After), golden(t, tt.golden); got != want {
			t.Errorf("%s:\n%s\nthe prototype wrote:\n%s", tt.name, got, want)
		}
	}
}

func TestBlueprintGoldens(t *testing.T) {
	fsys, repo := loadExamples(t)
	tests := []struct {
		blueprint string
		req       BlueprintRequest
	}{
		{"linux-vm", BlueprintRequest{Name: "web02", Owner: "alice", Repository: "infra-live",
			Inputs: map[string]any{"environment": "prod", "name": "web02", "os": "rhel-9", "size": "large"}}},
		{"app-servers", BlueprintRequest{Name: "shop-dev", Owner: "bob", Repository: "infra-live",
			Inputs: map[string]any{"environment": "dev", "app": "shop", "webCpu": 4, "dbDiskGb": 500}}},
		{"app-servers", BlueprintRequest{Name: "blog-test", Owner: "bob", Repository: "infra-live",
			Inputs: map[string]any{"environment": "test", "app": "blog", "withDatabase": false}}},
	}
	for _, tt := range tests {
		cs, err := Blueprint(fsys, repo, repo.Blueprints[tt.blueprint], tt.req)
		if err != nil {
			t.Fatalf("%s/%s: %v", tt.blueprint, tt.req.Name, err)
		}
		dir := filepath.Join("testdata/golden/blueprints", tt.blueprint+"-"+tt.req.Name)
		if *update {
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		}
		for _, c := range cs.Changes {
			if c.Op != OpCreate {
				t.Errorf("%s: %s %s, want a create", tt.blueprint, c.Op, c.Path)
			}
			file := filepath.Join(dir, filepath.FromSlash(c.Path))
			if *update {
				if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, c.After, 0o600); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Errorf("%s: no golden file for %s (run with -update after reviewing): %v", tt.blueprint, c.Path, err)
				continue
			}
			if string(want) != string(c.After) {
				t.Errorf("%s: %s =\n%s\nwant\n%s", tt.blueprint, c.Path, c.After, want)
			}
		}
		if !*update {
			var count int
			_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, _ error) error {
				if d != nil && !d.IsDir() {
					count++
				}
				return nil
			})
			if count != len(cs.Changes) {
				t.Errorf("%s/%s: %d changes, %d golden files", tt.blueprint, tt.req.Name, len(cs.Changes), count)
			}
		}
	}
}

func TestTypeRequests(t *testing.T) {
	fsys, repo := loadExamples(t)
	vm := repo.Types["vm"]
	web01 := map[string]string{"env": "prod", "name": "web01"}

	_, err := Type(fsys, vm, generate.Request{Kind: generate.Create, Params: web01, Values: map[string]any{}})
	if !errors.Is(err, ErrExists) {
		t.Errorf("create over an existing file: %v", err)
	}
	ghost := map[string]string{"env": "prod", "name": "ghost"}
	if _, err := Type(fsys, vm, generate.Request{Kind: generate.Delete, Params: ghost}); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete of a missing file: %v", err)
	}
	if _, err := Type(fsys, vm, generate.Request{Kind: generate.Action, Params: web01, ActionName: "resize", ActionInputs: map[string]any{"cpu": 4, "memory_gb": 16}}); !errors.Is(err, ErrNoChange) {
		t.Errorf("a resize to the current size: %v", err)
	}
	var ve *schema.ValidationError
	if _, err := Type(fsys, vm, generate.Request{Kind: generate.Action, Params: web01, ActionName: "resize", ActionInputs: map[string]any{"cpu": 99, "memory_gb": 16}}); !errors.As(err, &ve) || ve.Fields["cpu"] == "" {
		t.Errorf("a resize beyond the schema: %v", err)
	}
	if _, err := Type(fsys, vm, generate.Request{Kind: generate.Create, Params: map[string]string{"env": "..", "name": "x"}}); err == nil {
		t.Error("a create with an unsafe param succeeded")
	}

	cs, err := Type(fsys, vm, generate.Request{Kind: generate.Delete, Params: web01})
	if err != nil || len(cs.Changes) != 1 || cs.Changes[0].Op != OpDelete || cs.Changes[0].After != nil || len(cs.Changes[0].Before) == 0 {
		t.Fatalf("delete = %+v, %v", cs, err)
	}
	if !strings.HasPrefix(cs.Fingerprints["envs/prod/vms/web01.yaml"], "sha256:") {
		t.Errorf("fingerprints = %v", cs.Fingerprints)
	}

	resize := func(cpu int) *ChangeSet {
		cs, err := Type(fsys, vm, generate.Request{Kind: generate.Action, Params: web01, ActionName: "resize", ActionInputs: map[string]any{"cpu": cpu, "memory_gb": 32}})
		if err != nil {
			t.Fatal(err)
		}
		return cs
	}
	a, b, c := resize(8), resize(8), resize(12)
	if a.Hash != b.Hash || a.Hash == c.Hash || !strings.HasPrefix(a.Hash, "sha256:") {
		t.Errorf("hashes %s %s %s: the same change must hash the same, another differently", a.Hash, b.Hash, c.Hash)
	}
	var removed, added []string
	for _, l := range a.Changes[0].Diff() {
		switch l.Op {
		case '-':
			removed = append(removed, l.Text)
		case '+':
			added = append(added, l.Text)
		}
	}
	if strings.Join(removed, "|") != "cpu: 4          # bumped in INC-4411|memory_gb: 16" || strings.Join(added, "|") != "cpu: 8          # bumped in INC-4411|memory_gb: 32" {
		t.Errorf("diff: removed %q, added %q", removed, added)
	}
}

func TestLineDiff(t *testing.T) {
	got := lineDiff("a\nb\nc\n", "a\nc\nd\n")
	want := []Line{{' ', "a"}, {'-', "b"}, {' ', "c"}, {'+', "d"}}
	if len(got) != len(want) {
		t.Fatalf("lineDiff = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("lineDiff[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if got := lineDiff("", "x\n"); len(got) != 1 || got[0] != (Line{'+', "x"}) {
		t.Errorf("lineDiff of a new file = %v", got)
	}
	if got := lineDiff("x\ny\n", ""); len(got) != 2 || got[1] != (Line{'-', "y"}) {
		t.Errorf("lineDiff of a deleted file = %v", got)
	}
}

func TestBlueprintRequests(t *testing.T) {
	fsys, repo := loadExamples(t)
	b := repo.Blueprints["linux-vm"]
	req := BlueprintRequest{Name: "web01", Owner: "alice", Repository: "infra-live", Inputs: map[string]any{"environment": "prod", "name": "web01"}}
	if _, err := Blueprint(fsys, repo, b, req); !errors.Is(err, ErrExists) {
		t.Errorf("a blueprint over an existing VM: %v", err)
	}
	req.Inputs = map[string]any{"environment": "moon", "name": "x1"}
	var ve *schema.ValidationError
	if _, err := Blueprint(fsys, repo, b, req); !errors.As(err, &ve) || ve.Fields["inputs.environment"] == "" {
		t.Errorf("invalid inputs: %v", err)
	}
	// A resource its type rejects is the resource's error, not a field of the form.
	fsys["blueprints/loose.yaml"] = &fstest.MapFile{Data: []byte(`apiVersion: quai.dev/v1beta1
kind: Blueprint
metadata: { name: loose, title: Loose }
spec:
  inputs: { type: object, properties: { cpu: { type: integer } } }
  resources:
    - id: vm
      type: { name: vm }
      params: { env: dev, name: loose01 }
      values: { template: rhel-9, cpu: "${{ inputs.cpu }}", memory_gb: 4, disks: [{ size_gb: 50 }], network: { vlan: 110 } }
`)}
	loose, problems := definitions.LoadBlueprint(fsys, "blueprints/loose.yaml", repo.Types)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	var re *ResourceError
	_, err := Blueprint(fsys, repo, loose, BlueprintRequest{Name: "loose01", Owner: "alice", Inputs: map[string]any{"cpu": 64}})
	if !errors.As(err, &re) || re.ID != "vm" || !errors.As(err, &ve) || ve.Fields["cpu"] == "" {
		t.Errorf("a resource its type rejects: %v", err)
	}
	req.Inputs = map[string]any{"environment": "moon", "name": "x1"}
	req.Name = "../x"
	if _, err := Blueprint(fsys, repo, b, req); err == nil {
		t.Error("an unsafe instance name was accepted")
	}

	fsys[".quai/instances/linux-vm/web09.yaml"] = &fstest.MapFile{Data: []byte("kind: Instance\n")}
	req = BlueprintRequest{Name: "web09", Inputs: map[string]any{"environment": "dev", "name": "web09"}}
	if _, err := Blueprint(fsys, repo, b, req); !errors.Is(err, ErrExists) {
		t.Errorf("an existing instance record: %v", err)
	}
}

func TestPlanRefusals(t *testing.T) {
	typ := func(name, locate string) string {
		return "apiVersion: quai.dev/v1beta1\nkind: ResourceType\nmetadata: { name: " + name + ", title: T }\nspec:\n  schema: s.json\n  params: [{ name: name }]\n  locate: " + locate + "\n  actions: [{ name: on, set: { on: true } }]\n"
	}
	bp := func(name, resources string) string {
		return "apiVersion: quai.dev/v1beta1\nkind: Blueprint\nmetadata: { name: " + name + ", title: B }\nspec:\n  inputs: { type: object }\n  resources:\n" + resources
	}
	fsys := fstest.MapFS{
		"s.json":                 {Data: []byte(`{"type": "object"}`)},
		".quai/types/dir.yaml":   {Data: []byte(typ("dir", "{ strategy: dir, path: \"apps/{name}\" }"))},
		".quai/types/json.yaml":  {Data: []byte(typ("json", "{ strategy: file, path: \"j/{name}.json\" }"))},
		".quai/types/item.yaml":  {Data: []byte(typ("item", "{ strategy: file, path: \"items/{name}.yaml\" }"))},
		"items/broken.yaml":      {Data: []byte("a: [1\n")},
		"items/list.yaml":        {Data: []byte("- a\n")},
		"blueprints/other.yaml":  {Data: []byte(bp("other", "    - { id: a, type: { repository: elsewhere, name: x } }\n"))},
		"blueprints/twice.yaml":  {Data: []byte(bp("twice", "    - { id: a, type: { name: item }, params: { name: x } }\n    - { id: b, type: { name: item }, params: { name: x } }\n"))},
		"blueprints/flag.yaml":   {Data: []byte(bp("flag", "    - { id: a, type: { name: item }, params: { name: \"${{ true }}\" } }\n"))},
		"blueprints/when.yaml":   {Data: []byte(bp("when", "    - { id: a, type: { name: item }, when: \"inputs.missing\" }\n"))},
		"blueprints/values.yaml": {Data: []byte(bp("values", "    - { id: a, type: { name: item }, params: { name: \"${{ inputs.missing }}\" } }\n"))},
		"blueprints/vals.yaml":   {Data: []byte(bp("vals", "    - { id: a, type: { name: item }, params: { name: v }, values: { x: \"${{ inputs.missing }}\" } }\n"))},
		"blueprints/number.yaml": {Data: []byte(bp("number", "    - { id: a, type: { name: item }, params: { name: 7 } }\n"))},
	}
	repo, problems := definitions.Load(fsys)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	name := map[string]string{"name": "x"}
	for typeName, want := range map[string]string{"dir": "dir locator is not available yet", "json": "only YAML files"} {
		if _, err := Type(fsys, repo.Types[typeName], generate.Request{Kind: generate.Create, Params: name}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", typeName, err, want)
		}
	}
	for key, want := range map[string]string{"broken": "cannot be read", "list": "does not hold a mapping"} {
		_, err := Type(fsys, repo.Types["item"], generate.Request{Kind: generate.Action, ActionName: "on", Params: map[string]string{"name": key}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", key, err, want)
		}
	}
	for blueprint, want := range map[string]string{
		"other":  "types in other repositories",
		"twice":  "written twice",
		"flag":   "must be text",
		"when":   "no such key",
		"values": "params:",
		"vals":   "values:",
	} {
		_, err := Blueprint(fsys, repo, repo.Blueprints[blueprint], BlueprintRequest{Name: "i1", Repository: "r"})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", blueprint, err, want)
		}
	}
	cs, err := Blueprint(fsys, repo, repo.Blueprints["number"], BlueprintRequest{Name: "i1", Repository: "r"})
	if err != nil || cs.Changes[1].Path != "items/7.yaml" {
		t.Errorf("a numeric param: %+v, %v", cs, err)
	}
}
