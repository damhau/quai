package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damhau/quai/backend/internal/engine/generate/plugin/plugintest"
)

const examples = "../../../../examples/gitops-repo"

func copyExamples(t *testing.T) fstest.MapFS {
	t.Helper()
	return copyTree(t, examples)
}

// copyTree reads the files under root, as a checkout of them has them: without empty directories.
func copyTree(t *testing.T, root string) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fsys[filepath.ToSlash(rel)] = &fstest.MapFile{Data: b}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

// The Kubernetes example: a type per component, its directories and golden examples.
func TestKubernetesExamplePass(t *testing.T) {
	r := Run(copyTree(t, "../../../../examples/kubernetes-repo"))
	if len(r.Problems) > 0 || r.Types != 5 || r.Resources != 5 || r.Tests != 8 {
		t.Errorf("report = %+v", r)
	}
}

// The pilot's types (P3-19): entries and a field of shared YAML, HCL and JSON files.
func TestPilotExamplePass(t *testing.T) {
	r := Run(os.DirFS("../../../../examples/pilot-infra"))
	if len(r.Problems) > 0 || r.Types != 4 || r.Resources != 25 {
		t.Errorf("report = %+v", r)
	}
}

func TestExamplesPass(t *testing.T) {
	r := Run(os.DirFS(examples))
	if len(r.Problems) > 0 || r.Types != 5 || r.Blueprints != 2 || r.Resources != 20 || r.Tests != 4 {
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
	// Entitlements by environment, on a blueprint whose environment input is not named env.
	vm := string(fsys["blueprints/linux-vm.yaml"].Data)
	set("blueprints/linux-vm.yaml", strings.Replace(vm, "\nspec:\n", "\nspec:\n  entitlements: { environments: { prod: [group:sre] } }\n", 1))

	r := Run(fsys)
	text := r.Problems.Error()
	for _, want := range []string{
		"blueprints/bad.yaml: apiVersion: quai.dev/v1alpha1 is the prototype's format",
		"envs/dev/vms/broken.yaml: the file is not valid YAML",
		"envs/dev/vms/list.yaml: must hold a mapping (type vm)",
		"envs/dev/vms/anchored.yaml: round trip: Quai cannot edit this file: yamledit: refused",
		`envs/prod/vms/db01.yaml: round trip: Quai would rewrite this file on its first edit (removes "backup_policy: hourly")`,
		"envs/prod/vms/web01.yaml: cpu: must be at most 32 (schema catalog/schemas/vm.schema.json)",
		"blueprints/linux-vm.yaml: spec.entitlements.environments: are keyed by the value of the env input, and this blueprint's inputs have none",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no problem contains %q; problems:\n%s", want, text)
		}
	}
	if r.Resources != 23 {
		t.Errorf("resources = %d, want 23", r.Resources)
	}
}

func TestTypeProblems(t *testing.T) {
	typ := func(name, locate, generate string) string {
		return "apiVersion: quai.dev/v1beta1\nkind: ResourceType\nmetadata: { name: " + name + ", title: T }\nspec:\n  schema: s.json\n  params: [{ name: name }]\n  locate: " + locate + "\n" + generate
	}
	fsys := fstest.MapFS{
		"s.json":                {Data: []byte(`{"type": "object", "properties": {"a": {"type": "string"}}}`)},
		".quai/types/apps.yaml": {Data: []byte(typ("apps", `{ strategy: file, path: "apps/{name}.ini" }`, "  generate: { kind: plugin, module: p.wasm, digest: \"sha256:"+strings.Repeat("0", 64)+"\" }\n"))},
		".quai/types/map.yaml":  {Data: []byte(typ("map", `{ strategy: file, path: "m/{name}.yaml" }`, "  generate: { kind: mapping, template: { a: x } }\n"))},
		".quai/types/item.yaml": {Data: []byte(typ("item", `{ strategy: file, path: "items/{name}.yaml" }`, ""))},
		".quai/types/flag.yaml": {Data: []byte(typ("flag", `{ strategy: file, path: "flags/{name}.yaml" }`, "  ui: { fields: { a: { visibleIf: inputs.flag } } }\n"))},
		"flags/f.yaml":          {Data: []byte("a: x\n")},
		"items/one.yaml":        {Data: []byte("a: 1\n")},
		"items/two.yaml":        {Data: []byte("a: x\nzz: kept\n")},
	}
	r := Run(fsys)
	if len(r.Problems) != 4 || r.Problems[0].String() != ".quai/types/apps.yaml: spec.generate.module: p.wasm does not exist" ||
		r.Problems[1].String() != ".quai/types/map.yaml: spec.generate.read: input a cannot be read back: the template does not bind it directly and no read expression says how" ||
		!strings.HasPrefix(r.Problems[2].String(), "flags/f.yaml: round trip: visibleIf of a:") || r.Problems[3].String() != "items/one.yaml: a: must be a string (schema s.json)" {
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
	if r.Types != 7 || r.Resources != 23 || len(r.Problems) != 1 || r.Problems[0].Path != "hcl/dev/bad.tfvars" || r.Problems[0].Field != "memory_gb" {
		t.Errorf("report = %+v", r)
	}
}

// TestEntries: the entries of a shared file are checked one by one, each round trip writing the
// entry back into the whole file; a file whose keys repeat is one problem.
func TestEntries(t *testing.T) {
	fsys := fstest.MapFS{} // its own namespaces, not the example's
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
	if len(r.Problems) != 2 || r.Resources != 3 {
		t.Errorf("%d resources, problems:\n%s", r.Resources, text)
	}
}

// TestFields: the fields a field type writes are checked against its schema in each entry, and
// round-trip without touching the rest of the entry.
func TestFields(t *testing.T) {
	fsys := fstest.MapFS{} // its own namespaces, not the example's
	fsys[".quai/types/quota.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: quai.dev/v1beta1\nkind: ResourceType\n" +
		"metadata: { name: quota, title: Quota }\nspec:\n" +
		"  locate: { strategy: field, path: \"clusters/{cluster}/namespaces.yaml\", collection: namespaces, key: name, keyParam: name, fields: [quota] }\n" +
		"  params:\n    - { name: cluster, title: Cluster }\n    - { name: name, title: Name }\n" +
		"  schema: catalog/schemas/quota.schema.json\n  generate: { kind: identity }\n")}
	fsys["catalog/schemas/quota.schema.json"] = &fstest.MapFile{Data: []byte(`{"type": "object", "properties": {"quota": {"type": "object", "properties": {"cpu": {"type": "string"}}}}}`)}
	fsys["clusters/prod-1/namespaces.yaml"] = &fstest.MapFile{Data: []byte("namespaces:\n  - name: shop\n    labels: { team: shop }\n    quota: { cpu: \"8\" }\n  - name: orders\n    quota: { cpu: 4 }\n  - name: search\n")}
	r := Run(fsys)
	if len(r.Problems) != 1 || r.Problems[0].String() != "clusters/prod-1/namespaces.yaml (orders): quota.cpu: must be a string (schema catalog/schemas/quota.schema.json)" || r.Resources != 3 {
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
	if r.Resources != 3 || len(r.Problems) != 1 ||
		r.Problems[0].String() != `apps/prod/odd.yaml: round trip: Quai would rewrite this file on its first edit (removes "quota: { cpu: \"4\" }", adds "quota: { cpu: \"8\" }")` {
		t.Errorf("%d resources, problems:\n%s", r.Resources, r.Problems.Error())
	}
}

// TestGoldenExamples: a case passes when its request writes exactly the files under after/; any
// other byte, a missing or an extra file, or a case of no type, is a problem.
func TestGoldenExamples(t *testing.T) {
	fsys := copyExamples(t)
	r := Run(fsys)
	if len(r.Problems) > 0 || r.Tests != 4 {
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

// TestComponents runs the golden examples of each built component, and the round trip of the
// directories they wrote, in testdata/components.
func TestComponents(t *testing.T) {
	r := Run(copyTree(t, "testdata/components"))
	if len(r.Problems) > 0 {
		t.Errorf("problems:\n%v", r.Problems)
	}
	if r.Tests != 5 || r.Resources != 2 {
		t.Errorf("tests = %d, resources = %d, want 5 and 2", r.Tests, r.Resources)
	}
}

// A directory without its record, or whose manifests are not what Quai would write, fails.
func TestComponentsBroken(t *testing.T) {
	fsys := copyTree(t, "testdata/components")
	delete(fsys, "configs/greeting/quai.yaml")
	m := string(fsys["teams/shop/manifests.yaml"].Data)
	fsys["teams/shop/manifests.yaml"] = &fstest.MapFile{Data: []byte(strings.Replace(m, "team: shop", "team: web", 1))}
	fsys["teams/shop/quai.yaml"] = &fstest.MapFile{Data: []byte("name: shop\nowner: alice\nteam: shop\ncpu: lots\n")}
	r := Run(fsys)
	text := r.Problems.Error()
	for _, want := range []string{
		"configs/greeting: has no quai.yaml, the record of its values (type config)",
		"teams/shop/quai.yaml: cpu: must match the pattern",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no problem contains %q; problems:\n%s", want, text)
		}
	}
	fsys = copyTree(t, "testdata/components")
	fsys["teams/shop/manifests.yaml"] = &fstest.MapFile{Data: []byte(strings.Replace(m, "team: shop", "team: web", 1))}
	if text := Run(fsys).Problems.Error(); !strings.Contains(text, `teams/shop/manifests.yaml: round trip: Quai would rewrite this file on its first edit`) {
		t.Errorf("problems:\n%s", text)
	}
}

// pluginRepo is a repository with the settings type, whose files the guest plugin writes, its
// resources and golden examples.
func pluginRepo(t *testing.T) fstest.MapFS {
	t.Helper()
	module := plugintest.Build(t, "guest")
	files := map[string]string{
		".quai/plugins/settings.wasm":                         string(module),
		".quai/types/settings.yaml":                           plugintest.Type(".quai/plugins/settings.wasm", plugintest.Digest(module)),
		"catalog/schemas/settings.schema.json":                plugintest.Schema,
		"settings/web.conf":                                   "# The web team's settings.\ncolour = blue\nowner = alice\n",
		"settings/shop.conf":                                  "colour = red\n",
		".quai/tests/settings/create/request.yaml":            "kind: create\nparams: { name: new }\nowner: bob\nvalues: { colour: green, size: small }\n",
		".quai/tests/settings/create/after/settings/new.conf": "colour = green\nowner = bob\nsize = small\n",
		".quai/tests/settings/paint/request.yaml":             "kind: action\nkey: web\naction: paint\nactionInputs: { colour: red }\n",
		".quai/tests/settings/paint/after/settings/web.conf":  "# The web team's settings.\ncolour = red\nowner = alice\n",
	}
	fsys := fstest.MapFS{}
	for p, s := range files {
		fsys[p] = &fstest.MapFile{Data: []byte(s)}
	}
	return fsys
}

// TestPlugins: the files of a plugin type are read by its plugin, checked against the schema
// and round trip, and its golden examples run through it; its module must be the one the type
// pins and import nothing a plugin may not.
func TestPlugins(t *testing.T) {
	if r := Run(pluginRepo(t)); len(r.Problems) > 0 || r.Types != 1 || r.Resources != 2 || r.Tests != 2 {
		t.Fatalf("%d types, %d resources, %d tests, problems:\n%v", r.Types, r.Resources, r.Tests, r.Problems)
	}

	fsys := pluginRepo(t)
	fsys["settings/pink.conf"] = &fstest.MapFile{Data: []byte("colour = pink\n")}
	fsys["settings/odd.conf"] = &fstest.MapFile{Data: []byte("colour: blue\n")}
	fsys["settings/tight.conf"] = &fstest.MapFile{Data: []byte("colour=blue\n")}
	forbidden := plugintest.Build(t, "forbidden")
	fsys[".quai/plugins/forbidden.wasm"] = &fstest.MapFile{Data: forbidden}
	fsys[".quai/types/other.yaml"] = &fstest.MapFile{Data: []byte(strings.NewReplacer("name: settings", "name: other", "settings/{name}", "other/{name}", "settings.wasm", "forbidden.wasm").
		Replace(plugintest.Type(".quai/plugins/settings.wasm", plugintest.Digest(forbidden))))}
	fsys[".quai/types/pinned.yaml"] = &fstest.MapFile{Data: []byte(strings.NewReplacer("name: settings", "name: pinned", "settings/{name}", "pinned/{name}").
		Replace(plugintest.Type(".quai/plugins/settings.wasm", plugintest.Digest(forbidden))))}
	r := Run(fsys)
	want := []string{
		".quai/types/other.yaml: spec.generate.module: plugin .quai/plugins/forbidden.wasm: the module imports a host function plugins may not use: env.fetch (a plugin may import only wasi_snapshot_preview1)",
		".quai/types/pinned.yaml: spec.generate.digest: is not the digest of .quai/plugins/settings.wasm",
		"settings/odd.conf: plugin .quai/plugins/settings.wasm: refused: line 1: expected key = value",
		"settings/pink.conf: colour: must be one of blue, red, green (schema catalog/schemas/settings.schema.json)",
		`settings/tight.conf: round trip: Quai would rewrite this file on its first edit (removes "colour=blue", adds "colour = blue")`,
	}
	if len(r.Problems) != len(want) {
		t.Fatalf("problems:\n%v", r.Problems)
	}
	for i, p := range r.Problems {
		if !strings.HasPrefix(p.String(), want[i]) {
			t.Errorf("problem %d = %s\nwant %s…", i, p, want[i])
		}
	}
}
