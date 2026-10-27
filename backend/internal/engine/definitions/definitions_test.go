package definitions

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damhau/quai/backend/internal/engine/expr"
)

const examples = "../../../../examples/gitops-repo"

func TestLoadExamples(t *testing.T) {
	repo, problems := Load(os.DirFS(examples))
	if len(problems) > 0 {
		t.Fatalf("problems:\n%v", problems)
	}
	if len(repo.Types) != 1 || len(repo.Blueprints) != 2 {
		t.Fatalf("loaded %d types and %d blueprints, want 1 and 2", len(repo.Types), len(repo.Blueprints))
	}
	vm := repo.Types["vm"]
	if vm.Path != ".quai/types/vm.yaml" || vm.Spec.Locate.Path != "envs/{env}/vms/{name}.yaml" || vm.Spec.Generate.Kind != GenerateIdentity {
		t.Errorf("vm type = %+v", vm)
	}
	if vm.FileSchema == nil || vm.InputSchema != vm.FileSchema {
		t.Error("the vm type's schema is not compiled, or its input schema differs from it")
	}
	if got := vm.Spec.Actions[1].Set; !reflect.DeepEqual(got, map[string]any{"disks.0.size_gb": "${{ action.size_gb }}"}) {
		t.Errorf("extend-disk set = %#v", got)
	}
	for _, name := range []string{"linux-vm", "app-servers"} {
		b := repo.Blueprints[name]
		if b == nil || b.InputSchema == nil || b.Spec.Resources[0].ValuesValue == nil {
			t.Errorf("blueprint %s is not loaded and compiled: %+v", name, b)
		}
	}
}

// The vm type's expressions, rewritten in CEL by hand, decide as the prototype's did.
func TestExampleExpressionsMatchThePrototype(t *testing.T) {
	repo, problems := Load(os.DirFS(examples))
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	vm := repo.Types["vm"]
	rule := func(name string) *Rule {
		for i := range vm.Spec.Approval.Rules {
			if vm.Spec.Approval.Rules[i].Name == name {
				return &vm.Spec.Approval.Rules[i]
			}
		}
		t.Fatalf("no rule %s", name)
		return nil
	}
	tests := []struct {
		what string
		cond *expr.Program
		vars expr.Vars
		want bool // what the prototype's expression gave
	}{
		{"production in prod", rule("production").Condition, expr.Vars{Params: map[string]any{"env": "prod"}}, true},
		{"production in dev", rule("production").Condition, expr.Vars{Params: map[string]any{"env": "dev"}}, false},
		{"large-vm on create", rule("large-vm").Condition, expr.Vars{Inputs: map[string]any{"cpu": 16}}, true},
		{"large-vm, small on create", rule("large-vm").Condition, expr.Vars{Inputs: map[string]any{"cpu": 4}}, false},
		{"large-vm, becoming large", rule("large-vm").Condition, expr.Vars{Inputs: map[string]any{"cpu": 16}, Previous: map[string]any{"cpu": 4}}, true},
		{"large-vm, already large", rule("large-vm").Condition, expr.Vars{Inputs: map[string]any{"cpu": 16}, Previous: map[string]any{"cpu": 12}}, false},
		{"backup_policy with backups", vm.Spec.UI.Fields["backup_policy"].Visible, expr.Vars{Inputs: map[string]any{"backup": true}}, true},
		{"backup_policy without backups", vm.Spec.UI.Fields["backup_policy"].Visible, expr.Vars{Inputs: map[string]any{"backup": false}}, false},
		{"backup_policy, backup unset", vm.Spec.UI.Fields["backup_policy"].Visible, expr.Vars{Inputs: map[string]any{}}, false},
	}
	for _, tt := range tests {
		got, err := tt.cond.Test(tt.vars)
		if err != nil || got != tt.want {
			t.Errorf("%s: %v, %v; the prototype gave %v", tt.what, got, err, tt.want)
		}
	}
}

// The blueprints' values, evaluated on sample inputs, are valid VM files.
func TestExampleBlueprintsProduceValidFiles(t *testing.T) {
	repo, problems := Load(os.DirFS(examples))
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	vm := repo.Types["vm"]
	samples := map[string]map[string]any{
		"linux-vm":    {"environment": "prod", "name": "web02", "os": "rhel-9", "size": "large", "diskGb": 80, "vlan": 120},
		"app-servers": {"environment": "dev", "app": "shop", "webCpu": 4, "withDatabase": true, "dbDiskGb": 500},
	}
	for name, inputs := range samples {
		b := repo.Blueprints[name]
		if err := b.InputSchema.Validate(inputs); err != nil {
			t.Errorf("%s: sample inputs: %v", name, err)
		}
		for _, r := range b.Spec.Resources {
			vars := expr.Vars{Inputs: inputs}
			if r.Condition != nil {
				if ok, err := r.Condition.Test(vars); err != nil || !ok {
					t.Fatalf("%s/%s: when = %v, %v", name, r.ID, ok, err)
				}
			}
			values, err := r.ValuesValue.Eval(vars)
			if err != nil {
				t.Fatalf("%s/%s: %v", name, r.ID, err)
			}
			if err := vm.FileSchema.Validate(values); err != nil {
				t.Errorf("%s/%s: %v\n%#v", name, r.ID, err, values)
			}
			params, err := r.ParamsValue.Eval(vars)
			if err != nil || params.(map[string]any)["env"] != inputs["environment"] {
				t.Errorf("%s/%s: params = %#v, %v", name, r.ID, params, err)
			}
		}
	}
}

func load(t *testing.T, files map[string]string) (*Repository, Problems) {
	t.Helper()
	fsys := fstest.MapFS{"catalog/vm.schema.json": {Data: []byte(`{"type": "object", "properties": {"cpu": {"type": "integer"}}}`)}}
	for p, src := range files {
		fsys[p] = &fstest.MapFile{Data: []byte(src)}
	}
	return Load(fsys)
}

func wantProblems(t *testing.T, problems Problems, fragments ...string) {
	t.Helper()
	text := problems.Error()
	for _, f := range fragments {
		if !strings.Contains(text, f) {
			t.Errorf("no problem contains %q; problems:\n%s", f, text)
		}
	}
}

const goodType = `apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: vm, title: VM }
spec:
  locate: { strategy: file, path: "envs/{env}/vms/{name}.yaml" }
  params: [{ name: env }, { name: name }]
  schema: catalog/vm.schema.json
  actions:
    - name: resize
      inputs: [{ name: cpu, type: integer, required: true }]
      set: { cpu: "${{ action.cpu }}" }
`

func TestGoodTypeAndBlueprint(t *testing.T) {
	repo, problems := load(t, map[string]string{
		".quai/types/vm.yaml":   goodType,
		".quai/types/README.md": "not a definition",
		"blueprints/pair.yml": `apiVersion: quai.dev/v1beta1
kind: Blueprint
metadata: { name: pair, title: Pair }
spec:
  inputs: { type: object, properties: { cpu: { type: integer, default: 2 } } }
  resources:
    - { id: a, type: { name: vm }, params: { env: dev, name: a }, values: { cpu: "${{ inputs.cpu }}" } }
    - { id: b, type: { repository: other-repo, name: db }, dependsOn: [a] }
  actions:
    - name: resize-a
      inputs: [{ name: cpu, type: integer, min: 1, max: 8 }]
      apply: { a: { action: resize, inputs: { cpu: "${{ action.cpu }}" } } }
`,
	})
	if len(problems) > 0 {
		t.Fatalf("problems:\n%v", problems)
	}
	if repo.Types["vm"] == nil || repo.Blueprints["pair"] == nil {
		t.Fatalf("not loaded: %+v", repo)
	}
	min := repo.Blueprints["pair"].Spec.Actions[0].Inputs[0].Min
	if min == nil || *min != 1 {
		t.Errorf("input min = %v", min)
	}
}

// Ported from the prototype's "resource type validation".
func TestTypeProblems(t *testing.T) {
	_, problems := load(t, map[string]string{
		".quai/types/a.yaml": `apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: a, title: A }
spec:
  locate: { strategy: file, path: "x/{a}{b}.txt" }
  params: [{ name: a }, { name: b }]
  schema: catalog/vm.schema.json
  stack: "envs/{nope}"
  approval:
    rules: [{ name: r, approvers: [], when: "((" }]
`,
		".quai/types/b.yaml": `apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: b, title: B }
spec:
  locate: { strategy: file, path: "../{name}.yaml" }
  params: [{ name: name }]
  schema: catalog/vm.schema.json
`,
	})
	wantProblems(t, problems,
		"a.yaml: spec.locate.path: must end in .yaml, .yml or .json",
		"a.yaml: spec.locate.path: params must be separated by a literal",
		"a.yaml: spec.stack: {nope} is not a param",
		"a.yaml: spec.approval.rules[0]: approvers are required",
		"a.yaml: spec.approval.rules[0].when: expression",
		"b.yaml: spec.locate.path: must be a clean relative path",
	)
}

// Ported from the prototype's "blueprint validation reports useful errors".
func TestBlueprintProblems(t *testing.T) {
	_, problems := load(t, map[string]string{
		".quai/types/vm.yaml": goodType,
		"blueprints/bad.yaml": `apiVersion: quai.dev/v1beta1
kind: Blueprint
metadata: { name: Bad Name }
spec:
  resources:
    - { id: a, type: { name: nope }, when: "((" }
    - { id: a, type: { name: vm }, dependsOn: [ghost] }
  actions: [{ name: delete, apply: { a: { values: {} } } }]
`,
	})
	wantProblems(t, problems,
		"metadata.name: must be a lowercase DNS label",
		"metadata.title: is required",
		"spec.inputs: is required",
		"spec.resources[0].type: type nope is not defined in this repository",
		"spec.resources[0].when: expression",
		"spec.resources[1].id: a is used twice",
		"spec.resources[1].dependsOn: ghost is not another resource",
		"spec.actions[0].name: delete is a built-in action",
	)
}

func TestFileProblems(t *testing.T) {
	_, problems := load(t, map[string]string{
		".quai/types/old.yaml":     "apiVersion: quai.dev/v1alpha1\nkind: ResourceType\n",
		".quai/types/other.yaml":   "apiVersion: example.com/v1\n",
		".quai/types/none.yaml":    "kind: ResourceType\n",
		".quai/types/kind.yaml":    "apiVersion: quai.dev/v1beta1\nkind: Blueprint\n",
		".quai/types/list.yaml":    "- a\n",
		".quai/types/yaml.yaml":    "a: [1\n",
		".quai/types/unknown.yaml": strings.Replace(goodType, "metadata: { name: vm, title: VM }", "metadata: { name: unknown, title: U, colour: red }", 1),
		".quai/types/shape.yaml":   strings.Replace(goodType, "metadata: { name: vm, title: VM }", "metadata: { name: shape, title: [x] }", 1),
		".quai/types/shapes.yaml":  strings.NewReplacer("name: vm,", "name: shapes,", "  schema: catalog/vm.schema.json", "  schema: catalog/vm.schema.json\n  ownership: x").Replace(goodType),
		".quai/types/flags.yaml":   strings.NewReplacer("name: vm,", "name: flags,", "required: true", "required: yes-please").Replace(goodType),
		".quai/types/nums.yaml":    strings.NewReplacer("name: vm,", "name: nums,", "required: true", "min: low").Replace(goodType),
		".quai/types/dup.yaml":     goodType,
		".quai/types/vm.yaml":      goodType,
		"blueprints/type.yaml":     "apiVersion: quai.dev/v1beta1\nkind: ResourceType\n",
	})
	wantProblems(t, problems,
		"old.yaml: apiVersion: quai.dev/v1alpha1 is the prototype's format and is not supported",
		`other.yaml: apiVersion: must be quai.dev/v1beta1, not "example.com/v1"`,
		"none.yaml: apiVersion: must be quai.dev/v1beta1, not nothing",
		`kind.yaml: kind: must be ResourceType in .quai/types/, not "Blueprint"`,
		"list.yaml: a definition must be a mapping",
		"yaml.yaml: the file is not valid YAML",
		`unknown.yaml: unknown field "colour"`,
		"shape.yaml: metadata.title: must be a string",
		"shapes.yaml: spec.ownership: must be a mapping",
		"flags.yaml: spec.actions.0.inputs.0.required: must be true or false",
		"nums.yaml: spec.actions.0.inputs.0.min: must be a number",
		"vm.yaml: metadata.name: type vm is already defined in .quai/types/dup.yaml",
		`blueprints/type.yaml: kind: must be Blueprint in blueprints/, not "ResourceType"`,
	)
	if problems[0].Path > problems[len(problems)-1].Path {
		t.Error("problems are not sorted by file")
	}
}

func TestLocatorsAndGenerators(t *testing.T) {
	typ := func(name, spec string) string {
		return "apiVersion: quai.dev/v1beta1\nkind: ResourceType\nmetadata: { name: " + name + ", title: T, version: x }\nspec:\n  schema: catalog/vm.schema.json\n" + spec
	}
	repo, problems := load(t, map[string]string{
		".quai/types/entry.yaml": typ("entry", `  locate: { strategy: entry, path: "clusters/{cluster}/namespaces.yaml", collection: namespaces, key: name, keyParam: name, order: key }
  params: [{ name: cluster, type: enum, options: [prod-1] }, { name: name, pattern: "[a-z]+" }]
  generate:
    kind: mapping
    template: { quota: { cpu: "${{ inputs.size == 'large' ? '16' : '4' }}" } }
    read: { size: "${{ doc.quota.cpu == '16' ? 'large' : 'small' }}" }
  ownership: { team: platform }
  paramFields: { name: name }
`),
		".quai/types/bad-entry.yaml": typ("bad-entry", `  locate: { strategy: field, path: "clusters/{cluster}/ns", keyParam: nope, order: key }
  params: [{ name: cluster, type: enum }, { name: x, pattern: "(" }, { name: cluster }]
  generate: { kind: identity, template: { a: 1 }, module: x.wasm }
  ownership: { field: owner, team: platform }
  paramFields: { ghost: "bad path!" }
  deletion: sometimes
  catalog: hidden
  approval: { rules: [{ name: r, approvers: [group:a, everyone], min: -1 }, { name: r, external: servicenow }], expiry: 1w, stepUp: "((" }
  ui: { fields: { "bad path!": { visibleIf: "1 + 1" } } }
`),
		".quai/types/gens.yaml": typ("gens", `  locate: { strategy: dir, path: "apps/{name}" }
  params: [{ name: name }]
  generate: { kind: plugin, module: .quai/plugins/x.wasm, digest: "sha256:abc" }
  actions:
    - { name: restart, kind: imperative, set: { a: 1 } }
    - { name: noop }
    - { name: bad-set, set: { "a b": 1 } }
    - { name: restart }
    - { name: Upper, kind: sometimes, availableIf: "((", approval: { rules: [{ name: x }] } }
    - name: in
      set: { a: 1 }
      inputs: [{ name: a, type: enum }, { name: a, type: float, min: 2, max: 1, pattern: "(" }, { name: Bad, type: string, visibleIf: "((" }]
`),
		".quai/types/comp.yaml": typ("comp", `  locate: { strategy: dir, path: "x" }
  generate: { kind: component, component: helm.chart }
`),
		".quai/types/map.yaml": typ("map", `  locate: { strategy: file, path: ".quai/{name}.yaml" }
  params: [{ name: name }, { name: unused }]
  generate: { kind: mapping, read: { a: "${{ (( }}" } }
  inputs: catalog/missing.json
`),
	})
	if repo.Types["entry"] != nil {
		t.Error("a type with an invalid version loaded")
	}
	wantProblems(t, problems,
		"entry.yaml: metadata.version: must be a semantic version",
		"bad-entry.yaml: spec.params[1].pattern:",
		"bad-entry.yaml: spec.params[2].name: cluster is declared twice",
		"bad-entry.yaml: spec.params[0]: an enum param needs options or discover: true",
		"bad-entry.yaml: spec.locate.path: must end in .yaml, .yml or .json",
		"bad-entry.yaml: spec.locate.collection: must be a dotted field path",
		"bad-entry.yaml: spec.locate.keyParam: must name a param",
		"bad-entry.yaml: spec.locate.fields: is required for the field strategy",
		"bad-entry.yaml: spec.locate.order: applies only to the entry strategy",
		"bad-entry.yaml: spec.generate.template: does not apply to the identity generator",
		"bad-entry.yaml: spec.generate.module: does not apply to the identity generator",
		"bad-entry.yaml: spec.ownership: must set exactly one of field, codeowners or team",
		"bad-entry.yaml: spec.paramFields.ghost: is not a param",
		"bad-entry.yaml: spec.paramFields.ghost: must be a dotted field path",
		`bad-entry.yaml: spec.deletion: must be one of allowed, approval, forbidden, not "sometimes"`,
		`bad-entry.yaml: spec.approval.rules[0].approvers: "everyone" is not an approver`,
		"bad-entry.yaml: spec.approval.rules[0].min: must be at least 0",
		"bad-entry.yaml: spec.approval.rules[1].name: r is used twice",
		"bad-entry.yaml: spec.approval.expiry: must be a duration",
		"bad-entry.yaml: spec.approval.stepUp: expression",
		`bad-entry.yaml: spec.ui.fields: invalid field path "bad path!"`,
		"bad-entry.yaml: spec.ui.fields.bad path!.visibleIf: expression",
		"gens.yaml: spec.generate.digest: must be sha256:",
		"gens.yaml: spec.actions[0]: an imperative action needs an executor and an operation",
		"gens.yaml: spec.actions[0].set: an imperative action does not set fields",
		"gens.yaml: spec.actions[1].set: is required for a declarative action",
		`gens.yaml: spec.actions[2].set: invalid field path "a b"`,
		"gens.yaml: spec.actions[3].name: restart is defined twice",
		"gens.yaml: spec.actions[4].name: must be a lowercase DNS label",
		`gens.yaml: spec.actions[4].kind: must be one of declarative, imperative, not "sometimes"`,
		"gens.yaml: spec.actions[4].availableIf: expression",
		"gens.yaml: spec.actions[4].approval.rules[0]: approvers are required",
		"gens.yaml: spec.actions[5].inputs[0].options: an enum needs options",
		"gens.yaml: spec.actions[5].inputs[1].name: a is declared twice",
		`gens.yaml: spec.actions[5].inputs[1].type: must be one of string, integer, number, boolean, enum, not "float"`,
		"gens.yaml: spec.actions[5].inputs[1]: min is greater than max",
		"gens.yaml: spec.actions[5].inputs[1].pattern:",
		"gens.yaml: spec.actions[5].inputs[2].name: must start with a lowercase letter",
		"gens.yaml: spec.actions[5].inputs[2].visibleIf: expression",
		`comp.yaml: spec.generate.component: must be one of kubevirt.vm, crossplane.claim, argocd.application, k8s.namespace, flux.kustomization, raw, not "helm.chart"`,
		"comp.yaml: spec.locate.path: needs at least one {param}",
		"map.yaml: spec.locate.path: cannot be under .quai/",
		"map.yaml: spec.params: unused is not used by spec.locate",
		"map.yaml: spec.generate.template: is required for the mapping generator",
		"map.yaml: spec.generate.read.a: expression",
		"map.yaml: spec.inputs: schema catalog/missing.json is not in the repository",
	)
}

func TestBlueprintStructureProblems(t *testing.T) {
	bp := func(name, spec string) string {
		return "apiVersion: quai.dev/v1beta1\nkind: Blueprint\nmetadata: { name: " + name + ", title: B }\nspec:\n" + spec
	}
	_, problems := load(t, map[string]string{
		".quai/types/vm.yaml": goodType,
		"blueprints/cycle.yaml": bp("cycle", `  inputs: { type: object }
  resources:
    - { id: a, type: { name: vm }, dependsOn: [b] }
    - { id: b, type: { name: vm }, dependsOn: [c] }
    - { id: c, type: { name: vm }, dependsOn: [a, c] }
    - { id: Bad, type: { name: Bad }, params: { a: "${{ nope }}" }, values: { a: "${{ (( }}" } }
  actions:
    - { name: none }
    - { name: both, apply: { a: { values: { x: 1 }, action: resize } } }
    - { name: ghost, apply: { zz: { values: { x: 1 } } } }
    - { name: missing-action, apply: { a: { action: reboot } }, approval: { rules: [{ name: r }] } }
    - { name: neither, apply: { a: {} } }
  ui: { fields: { x: { visibleIf: "((" } } }
  approval: { rules: [{ name: r, approvers: [manager], when: "((" }] }
`),
		"blueprints/empty.yaml": bp("empty", `  inputs: { type: 12 }
  resources: []
`),
		"blueprints/dup-a.yaml": bp("dup", "  inputs: {}\n  resources: [{ id: a, type: { name: vm } }]\n"),
		"blueprints/dup-b.yaml": bp("dup", "  inputs: {}\n  resources: [{ id: a, type: { name: vm } }]\n"),
	})
	wantProblems(t, problems,
		"cycle.yaml: spec.resources: dependsOn has a cycle: a → b → c → a",
		"cycle.yaml: spec.resources[2].dependsOn: c is not another resource",
		"cycle.yaml: spec.resources[3].id: must be a lowercase DNS label",
		"cycle.yaml: spec.resources[3].type.name: must name a resource type",
		"cycle.yaml: spec.resources[3].params: a: expression",
		"cycle.yaml: spec.resources[3].values: a: expression",
		"cycle.yaml: spec.actions[0].apply: is required",
		"cycle.yaml: spec.actions[1].apply.a: must set either values or action",
		"cycle.yaml: spec.actions[2].apply.zz: zz is not a resource of the blueprint",
		"cycle.yaml: spec.actions[3].apply.a.action: type vm has no action reboot",
		"cycle.yaml: spec.actions[3].approval.rules[0]: approvers are required",
		"cycle.yaml: spec.actions[4].apply.a: must set either values or action",
		"cycle.yaml: spec.ui.fields.x.visibleIf: expression",
		"cycle.yaml: spec.approval.rules[0].when: expression",
		"empty.yaml: spec.inputs: schema blueprints/empty.yaml:",
		"empty.yaml: spec.resources: a blueprint needs at least one resource",
		"dup-b.yaml: metadata.name: blueprint dup is already defined in blueprints/dup-a.yaml",
	)
}

func TestNumbers(t *testing.T) {
	repo, problems := load(t, map[string]string{
		".quai/types/vm.yaml": strings.NewReplacer(
			`set: { cpu: "${{ action.cpu }}" }`, "set: { cpu: 4, ratio: 1.5, big: 12345678901234567890 }",
			"inputs: [{ name: cpu, type: integer, required: true }]", "inputs: [{ name: n, type: integer, default: 3 }]",
		).Replace(goodType),
	})
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	a := repo.Types["vm"].Spec.Actions[0]
	if want := map[string]any{"cpu": int64(4), "ratio": 1.5, "big": 12345678901234567890.0}; !reflect.DeepEqual(a.Set, want) {
		t.Errorf("set = %#v, want %#v", a.Set, want)
	}
	if a.Inputs[0].Default != int64(3) {
		t.Errorf("default = %#v", a.Inputs[0].Default)
	}
}

func TestUnreadableRepository(t *testing.T) {
	repo, problems := Load(fstest.MapFS{".quai/types": {Data: []byte("a file, not a directory")}})
	if len(repo.Types) != 0 || len(problems) != 1 || !strings.Contains(problems[0].String(), "cannot list definitions") {
		t.Errorf("Load = %+v, %v", repo, problems)
	}
	if repo, problems := Load(fstest.MapFS{}); len(problems) != 0 || len(repo.Types)+len(repo.Blueprints) != 0 {
		t.Errorf("empty repository: %+v, %v", repo, problems)
	}
}
