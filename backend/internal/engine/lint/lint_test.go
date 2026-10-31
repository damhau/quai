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
	if len(r.Problems) > 0 || len(r.Notes) > 0 || r.Types != 1 || r.Blueprints != 2 || r.Resources != 4 || r.Tests != 2 {
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
	if len(r.Notes) != 1 || !strings.Contains(r.Notes.Error(), "the dir locator with the identity generator is not available yet") {
		t.Errorf("notes = %v", r.Notes)
	}
	if len(r.Problems) != 3 || r.Problems[0].String() != ".quai/types/map.yaml: spec.generate.read: input a cannot be read back: the template does not bind it directly and no read expression says how" ||
		!strings.HasPrefix(r.Problems[1].String(), "flags/f.yaml: round trip: visibleIf of a:") || r.Problems[2].String() != "items/one.yaml: a: must be a string (schema s.json)" {
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

// TestPlan: every Terraform resource a plan changes belongs to exactly one resource; the plans
// are tfplan's recorded ones.
func TestPlan(t *testing.T) {
	fsys := os.DirFS("../../../../examples/gitops-repo")
	read := func(name string) []byte {
		b, err := os.ReadFile("../tfplan/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if problems := Plan(fsys, read("prod.json"), "prod.json", "envs/prod"); len(problems) > 0 {
		t.Errorf("the example stack's plan: %v", problems)
	}
	problems := Plan(fsys, read("nested.json"), "nested.json", "envs/prod")
	if len(problems) != 3 || problems[0].Field != `module.app["shop"].module.vm["db"].terraform_data.vm` ||
		problems[0].Message != "created by no resource: no type's stack and address match it in envs/prod" {
		t.Errorf("a plan of resources no type claims: %v", problems)
	}
	if problems := Plan(fsys, []byte(`{}`), "plan.json", "envs/prod"); len(problems) != 1 || problems[0].Path != "plan.json" {
		t.Errorf("not a plan: %v", problems)
	}
}

// TestFormats: types whose files are JSON and HCL are checked as YAML ones are: each file against
// the schema (a required value defined in code is not missing) and round trip.
func TestFormats(t *testing.T) {
	fsys := copyExamples(t)
	typ := func(name, path string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("apiVersion: quai.dev/v1beta1\nkind: ResourceType\nmetadata: { name: " + name + ", title: VM }\n" +
			"spec:\n  locate: { strategy: file, path: \"" + path + "\" }\n  params:\n    - { name: env, title: Environment }\n    - { name: name, title: Name }\n" +
			"  schema: catalog/schemas/vm.schema.json\n  generate: { kind: identity }\n")}
	}
	fsys[".quai/types/vm-json.yaml"] = typ("vm-json", "json/{env}/{name}.tfvars.json")
	fsys[".quai/types/vm-hcl.yaml"] = typ("vm-hcl", "hcl/{env}/{name}.tfvars")
	fsys["json/dev/a.tfvars.json"] = &fstest.MapFile{Data: []byte(`{"template": "rhel-9", "cpu": 2, "memory_gb": 4, "disks": [{"size_gb": 50}], "network": {"vlan": 110}}` + "\n")}
	fsys["hcl/dev/a.tfvars"] = &fstest.MapFile{Data: []byte("template  = \"rhel-9\"\ncpu       = var.cpu # defined in code\nmemory_gb = 4\ndisks     = [{ size_gb = 50 }]\nnetwork   = { vlan = 110 }\n")}
	fsys["hcl/dev/bad.tfvars"] = &fstest.MapFile{Data: []byte("template = \"rhel-9\"\ncpu = 2\nmemory_gb = \"four\"\ndisks = [{ size_gb = 50 }]\nnetwork = { vlan = 110 }\n")}
	r := Run(fsys)
	if r.Types != 3 || r.Resources != 7 || len(r.Problems) != 1 || r.Problems[0].Path != "hcl/dev/bad.tfvars" || r.Problems[0].Field != "memory_gb" {
		t.Errorf("report = %+v", r)
	}
}

// TestEntries: the entries of a shared file are checked one by one, each round trip writing the
// entry back into the whole file; a file whose keys repeat is one problem.
func TestEntries(t *testing.T) {
	fsys := copyExamples(t)
	fsys[".quai/types/namespace.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: quai.dev/v1beta1\nkind: ResourceType\n" +
		"metadata: { name: namespace, title: Namespace }\nspec:\n" +
		"  locate: { strategy: entry, path: \"clusters/{cluster}/namespaces.yaml\", collection: namespaces, key: name, keyParam: name }\n" +
		"  params:\n    - { name: cluster, title: Cluster }\n    - { name: name, title: Name }\n" +
		"  schema: catalog/schemas/namespace.schema.json\n  generate: { kind: identity }\n")}
	fsys["catalog/schemas/namespace.schema.json"] = &fstest.MapFile{Data: []byte(`{"type": "object", "required": ["name"], "additionalProperties": false,
  "properties": {"name": {"type": "string"}, "quota": {"type": "object", "properties": {"cpu": {"type": "string"}, "memory": {"type": "string"}}}}}`)}
	fsys["clusters/prod-1/namespaces.yaml"] = &fstest.MapFile{Data: []byte("# Namespaces on prod-1.\nnamespaces:\n  - name: shop\n    quota: { cpu: \"8\", memory: 16Gi }\n  - name: orders\n    quota: { cpu: 4 }\n")}
	fsys["clusters/prod-2/namespaces.yaml"] = &fstest.MapFile{Data: []byte("namespaces:\n  - name: shop\n  - name: shop\n")}
	r := Run(fsys)
	text := r.Problems.Error()
	for _, want := range []string{
		"clusters/prod-1/namespaces.yaml (orders): quota.cpu: must be a string",
		`clusters/prod-2/namespaces.yaml: the key "shop" appears twice in namespaces`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no problem contains %q; problems:\n%s", want, text)
		}
	}
	if len(r.Problems) != 2 || r.Resources != 7 {
		t.Errorf("%d resources, problems:\n%s", r.Resources, text)
	}
}

// TestFields: the fields a field type writes are checked against its schema in each entry, and
// round-trip without touching the rest of the entry.
func TestFields(t *testing.T) {
	fsys := copyExamples(t)
	fsys[".quai/types/quota.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: quai.dev/v1beta1\nkind: ResourceType\n" +
		"metadata: { name: quota, title: Quota }\nspec:\n" +
		"  locate: { strategy: field, path: \"clusters/{cluster}/namespaces.yaml\", collection: namespaces, key: name, keyParam: name, fields: [quota] }\n" +
		"  params:\n    - { name: cluster, title: Cluster }\n    - { name: name, title: Name }\n" +
		"  schema: catalog/schemas/quota.schema.json\n  generate: { kind: identity }\n")}
	fsys["catalog/schemas/quota.schema.json"] = &fstest.MapFile{Data: []byte(`{"type": "object", "properties": {"quota": {"type": "object", "properties": {"cpu": {"type": "string"}}}}}`)}
	fsys["clusters/prod-1/namespaces.yaml"] = &fstest.MapFile{Data: []byte("namespaces:\n  - name: shop\n    labels: { team: shop }\n    quota: { cpu: \"8\" }\n  - name: orders\n    quota: { cpu: 4 }\n  - name: search\n")}
	r := Run(fsys)
	if len(r.Problems) != 1 || r.Problems[0].String() != "clusters/prod-1/namespaces.yaml (orders): quota.cpu: must be a string (schema catalog/schemas/quota.schema.json)" || r.Resources != 7 {
		t.Errorf("%d resources, problems:\n%s", r.Resources, r.Problems.Error())
	}
}

// TestMappingTypes: files of a mapping type round-trip through its read and its template, with
// every locator; one the template would rewrite is a problem.
func TestMappingTypes(t *testing.T) {
	typ := func(name, locate string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("apiVersion: quai.dev/v1beta1\nkind: ResourceType\nmetadata: { name: " + name + ", title: T }\nspec:\n" +
			"  locate: " + locate + "\n  params: [{ name: cluster }, { name: name }]\n  schema: ns.schema.json\n  inputs: size.schema.json\n" +
			"  generate:\n    kind: mapping\n    template: { quota: { cpu: \"${{ inputs.size == 'small' ? '2' : '8' }}\" }, team: \"${{ inputs.team }}\" }\n" +
			"    read: { size: \"${{ doc.quota.cpu == '2' ? 'small' : 'large' }}\" }\n")}
	}
	fsys := fstest.MapFS{
		".quai/types/app.yaml":  typ("app", "{ strategy: file, path: \"apps/{cluster}/{name}.yaml\" }"),
		".quai/types/ns.yaml":   typ("ns", "{ strategy: entry, path: \"clusters/{cluster}/ns.yaml\", collection: namespaces, key: name, keyParam: name }"),
		"ns.schema.json":        {Data: []byte(`{"type": "object", "properties": {"name": {"type": "string"}, "team": {"type": "string"}, "quota": {"type": "object"}}}`)},
		"size.schema.json":      {Data: []byte(`{"type": "object", "properties": {"size": {"enum": ["small", "large"]}, "team": {"type": "string"}}}`)},
		"apps/prod/shop.yaml":   {Data: []byte("# Shop.\nteam: shop\nquota: { cpu: \"8\" } # large\nextra: kept\n")},
		"apps/prod/odd.yaml":    {Data: []byte("team: odd\nquota: { cpu: \"4\" }\n")},
		"clusters/prod/ns.yaml": {Data: []byte("namespaces:\n  - name: orders\n    team: orders\n    quota: { cpu: \"2\" }\n")},
	}
	r := Run(fsys)
	if len(r.Notes) > 0 || r.Resources != 3 || len(r.Problems) != 1 ||
		r.Problems[0].String() != `apps/prod/odd.yaml: round trip: Quai would rewrite this file on its first edit (removes "quota: { cpu: \"4\" }", adds "quota: { cpu: \"8\" }")` {
		t.Errorf("notes %v, %d resources, problems:\n%s", r.Notes, r.Resources, r.Problems.Error())
	}
}

// TestGoldenExamples: a case passes when its request writes exactly the files under after/; any
// other byte, a missing or an extra file, or a case of no type, is a problem.
func TestGoldenExamples(t *testing.T) {
	fsys := copyExamples(t)
	r := Run(fsys)
	if len(r.Problems) > 0 || r.Tests != 2 {
		t.Fatalf("the examples' cases: %d, %v", r.Tests, r.Problems)
	}
	created := ".quai/tests/vm/create-dev-vm/after/envs/dev/vms/build-agent-02.yaml"
	fsys[created].Data = []byte(strings.Replace(string(fsys[created].Data), "cpu: 4", "cpu: 2", 1))
	fsys[".quai/tests/vm/resize-web01/after/envs/prod/vms/db01.yaml"] = &fstest.MapFile{Data: []byte("x: 1\n")}
	fsys[".quai/tests/vm/resize-web01/before/envs/prod/vms/web01.yaml"] = &fstest.MapFile{Data: []byte("owner: bob\ntemplate: rhel-9\ncpu: 4\nmemory_gb: 16\ndisks: [{ size_gb: 50 }]\nnetwork: { vlan: 120 }\n")}
	fsys[".quai/tests/nope/x/request.yaml"] = &fstest.MapFile{Data: []byte("kind: create\n")}
	fsys[".quai/tests/vm/bad/request.yaml"] = &fstest.MapFile{Data: []byte("kind: create\nparams: { env: dev, name: x }\nvalues: { cpu: 4 }\nsurprise: true\n")}
	r = Run(fsys)
	text := r.Problems.Error()
	for _, want := range []string{
		`.quai/tests/nope: is not the name of a type`,
		`.quai/tests/vm/bad/request.yaml: json: unknown field "surprise"`,
		`.quai/tests/vm/create-dev-vm: after/envs/dev/vms/build-agent-02.yaml: the request writes other bytes (removes "cpu: 2", adds "cpu: 4")`,
		`.quai/tests/vm/resize-web01: after/envs/prod/vms/db01.yaml: the request does not write this file`,
		`.quai/tests/vm/resize-web01: after/envs/prod/vms/web01.yaml: the request writes other bytes`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no problem contains %q; problems:\n%s", want, text)
		}
	}
}
