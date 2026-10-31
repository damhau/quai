package generate

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/engine/schema"
)

const app = `apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: app, title: App }
spec:
  locate: { strategy: file, path: "apps/{env}/{name}.yaml" }
  params: [{ name: env }, { name: name }]
  schema: app.schema.json
  ownership: { field: meta.owner }
  paramFields: { name: meta.name }
  labels: { meta.managed_by: quai }
  ui:
    fields:
      region: { immutable: true }
      debug_level: { visibleIf: "has(inputs.debug) && inputs.debug" }
  actions:
    - name: tune
      inputs: [{ name: replicas, type: integer }]
      set: { replicas: "${{ action.replicas }}", debug: "${{ action.?debug }}", note: "${{ null }}" }
    - name: deep
      set: { "list.5": 1 }
    - { name: restart, kind: imperative, executor: k8s, operation: restart }
`

const appSchema = `{
  "type": "object",
  "properties": {
    "meta": { "type": "object", "properties": { "name": { "type": "string" }, "owner": { "type": "string" }, "managed_by": { "type": "string" } } },
    "region": { "type": "string" },
    "id": { "type": "string", "readOnly": true },
    "replicas": { "type": "integer", "maximum": 10 },
    "debug": { "type": "boolean" },
    "debug_level": { "type": "string" },
    "note": { "type": "string" },
    "list": { "type": "array" }
  }
}`

func appType(t *testing.T, override string) *definitions.ResourceType {
	t.Helper()
	src := app
	if override != "" {
		src = override
	}
	repo, problems := definitions.Load(fstest.MapFS{
		".quai/types/app.yaml": {Data: []byte(src)},
		"app.schema.json":      {Data: []byte(appSchema)},
		"none.schema.json":     {Data: []byte(`{"type": "object"}`)},
	})
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	return repo.Types["app"]
}

var params = map[string]string{"env": "dev", "name": "shop"}

func TestCreate(t *testing.T) {
	ty := appType(t, "")
	got, err := Identity(ty, Request{Kind: Create, Params: params, Owner: "alice", Values: map[string]any{
		"region": "ch", "replicas": 2, "debug": false, "debug_level": "trace",
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"meta":     map[string]any{"name": "shop", "owner": "alice", "managed_by": "quai"},
		"region":   "ch",
		"replicas": 2,
		"debug":    false, // debug_level is hidden while debug is off, so it is dropped
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("create = %#v\nwant %#v", got, want)
	}
}

func TestUpdate(t *testing.T) {
	ty := appType(t, "")
	current := map[string]any{
		"meta":        map[string]any{"name": "shop", "owner": "alice", "managed_by": "quai"},
		"region":      "ch",
		"id":          "a1",
		"replicas":    2,
		"handwritten": "kept: the schema does not describe it",
	}
	// The form sends what it shows; locked fields it leaves out are kept.
	got, err := Identity(ty, Request{Kind: Update, Params: params, Current: current, Values: map[string]any{"replicas": 3}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"meta":        map[string]any{"name": "shop", "owner": "alice", "managed_by": "quai"},
		"region":      "ch",
		"id":          "a1",
		"replicas":    3,
		"handwritten": "kept: the schema does not describe it",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("update = %#v\nwant %#v", got, want)
	}

	// Changing a locked field is refused, field by field.
	_, err = Identity(ty, Request{Kind: Update, Params: params, Current: current, Values: map[string]any{
		"region": "us", "id": "b2", "meta": map[string]any{"owner": "mallory"}, "replicas": 2.0,
	}})
	var ve *schema.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("update of locked fields: %v", err)
	}
	for _, f := range []string{"region", "id", "meta.owner"} {
		if ve.Fields[f] != "cannot be changed after creation" {
			t.Errorf("%s: %q", f, ve.Fields[f])
		}
	}
	// The same value in another number type is not a change.
	if _, err := Identity(ty, Request{Kind: Update, Params: params, Current: current, Values: map[string]any{"region": "ch", "replicas": 2.0}}); err != nil {
		t.Errorf("update with an equal locked value: %v", err)
	}
	// Validation against the file schema.
	if _, err := Identity(ty, Request{Kind: Update, Params: params, Current: current, Values: map[string]any{"replicas": 11}}); !errors.As(err, &ve) || ve.Fields["replicas"] == "" {
		t.Errorf("update beyond the schema: %v", err)
	}
}

func TestActions(t *testing.T) {
	ty := appType(t, "")
	current := map[string]any{"meta": map[string]any{"name": "shop"}, "replicas": 2, "debug": true, "note": "x", "list": []any{1}}
	got, err := Identity(ty, Request{Kind: Action, Params: params, Current: current, ActionName: "tune", ActionInputs: map[string]any{"replicas": 4}})
	if err != nil {
		t.Fatal(err)
	}
	// replicas is set, debug is left alone (absent), note is removed (null).
	want := map[string]any{"meta": map[string]any{"name": "shop", "managed_by": "quai"}, "replicas": int64(4), "debug": true, "list": []any{1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tune = %#v\nwant %#v", got, want)
	}
	if current["replicas"] != 2 {
		t.Error("the action changed the current document")
	}
	for name, want := range map[string]string{
		"deep":    "the path does not exist",
		"restart": "imperative",
		"ghost":   "has no action ghost",
	} {
		if _, err := Identity(ty, Request{Kind: Action, Params: params, Current: current, ActionName: name}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %q", name, err, want)
		}
	}
	if got, err := Identity(ty, Request{Kind: Delete}); got != nil || err != nil {
		t.Errorf("delete = %v, %v", got, err)
	}
	if _, err := Identity(ty, Request{Kind: "rename"}); err == nil {
		t.Error("an unknown kind was accepted")
	}
	mapping := strings.Replace(app, "  schema: app.schema.json", "  schema: app.schema.json\n  inputs: none.schema.json\n  generate: { kind: mapping, template: { region: ch } }", 1)
	if _, err := Identity(appType(t, mapping), Request{Kind: Create}); err == nil || !strings.Contains(err.Error(), "not available yet") {
		t.Errorf("mapping generator: %v", err)
	}
}

func TestPaths(t *testing.T) {
	doc := map[string]any{"a": map[string]any{"b": []any{map[string]any{"c": 1}}}, "s": "x"}
	if v, ok := getPath(doc, "a.b.0.c"); !ok || v != 1 {
		t.Errorf("getPath = %v, %v", v, ok)
	}
	for _, p := range []string{"a.b.1.c", "a.b.x", "s.t", "z"} {
		if _, ok := getPath(doc, p); ok {
			t.Errorf("getPath(%q) found a value", p)
		}
	}
	if !setPath(doc, "a.b.0.c", 2) || !setPath(doc, "new.deep", 3) || !setPath(doc, "a.b.0", "item") {
		t.Error("setPath failed")
	}
	if setPath(doc, "s.t", 1) || setPath(doc, "a.b.x", 1) || setPath(doc, "a.b.0.c.d", 1) {
		t.Error("setPath wrote through a scalar or a bad index")
	}
	deletePath(doc, "new.deep")
	deletePath(doc, "s")
	deletePath(doc, "a.b.0.zz")
	want := map[string]any{"a": map[string]any{"b": []any{"item"}}, "new": map[string]any{}}
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("doc = %#v, want %#v", doc, want)
	}
}

// Ported from the prototype's "input validation: defaults, coercion, visibility, errors".
func TestValidateInputs(t *testing.T) {
	env, err := expr.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	cond := func(src string) *expr.Program {
		p, err := env.CompileCondition(src)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	f := func(v float64) *float64 { return &v }
	defs := []definitions.Input{
		{Name: "environment", Type: definitions.InputEnum, Options: []string{"dev", "prod"}, Required: true},
		{Name: "storageGB", Title: "Storage", Type: definitions.InputInteger, Min: f(10), Max: f(1000), Default: int64(20)},
		{Name: "highAvailability", Type: definitions.InputBoolean, Visible: cond("action.environment == 'prod'")},
		{Name: "namespace", Type: definitions.InputString, Pattern: "[a-z][a-z0-9-]*"},
		{Name: "ratio", Type: definitions.InputNumber},
	}
	ok, err := ValidateInputs(defs, map[string]any{"environment": "dev", "storageGB": "50", "namespace": " shop ", "ratio": "0.5"}, expr.Vars{})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"environment": "dev", "storageGB": int64(50), "namespace": "shop", "ratio": 0.5}; !reflect.DeepEqual(ok, want) {
		t.Errorf("values = %#v, want %#v (highAvailability is hidden outside prod)", ok, want)
	}
	prod, err := ValidateInputs(defs, map[string]any{"environment": "prod", "highAvailability": "true"}, expr.Vars{})
	if err != nil || prod["highAvailability"] != true || prod["storageGB"] != int64(20) {
		t.Errorf("prod values = %#v, %v", prod, err)
	}
	first, err := ValidateInputs(defs, map[string]any{}, expr.Vars{})
	if err != nil || first["environment"] != "dev" || first["highAvailability"] != nil {
		t.Errorf("defaults = %#v, %v", first, err)
	}

	_, err = ValidateInputs(defs, map[string]any{"environment": "staging", "storageGB": 5, "namespace": "Bad_NS", "ratio": "x"}, expr.Vars{})
	var ve *schema.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("invalid inputs: %v", err)
	}
	want := map[string]string{
		"environment": "environment must be one of dev, prod",
		"storageGB":   "Storage must be at least 10",
		"namespace":   "namespace does not match the expected format",
		"ratio":       "ratio must be a number",
	}
	if !reflect.DeepEqual(ve.Fields, want) {
		t.Errorf("errors = %v\nwant %v", ve.Fields, want)
	}
	more := []definitions.Input{
		{Name: "n", Type: definitions.InputInteger, Max: f(3)},
		{Name: "w", Type: definitions.InputInteger},
		{Name: "b", Type: definitions.InputBoolean},
		{Name: "s", Type: definitions.InputString, Required: true},
		{Name: "t", Type: definitions.InputString},
		{Name: "u", Type: definitions.InputInteger},
		{Name: "big", Type: definitions.InputNumber},
	}
	_, err = ValidateInputs(more, map[string]any{"n": 4, "w": 1.5, "b": 1, "t": 3, "u": uint64(2), "big": int(1)}, expr.Vars{})
	if !errors.As(err, &ve) {
		t.Fatalf("more invalid inputs: %v", err)
	}
	want = map[string]string{
		"n": "n must be at most 3",
		"w": "w must be a whole number",
		"b": "b must be true or false",
		"s": "s is required",
		"t": "t must be text",
	}
	if !reflect.DeepEqual(ve.Fields, want) {
		t.Errorf("errors = %v\nwant %v", ve.Fields, want)
	}
	bad := []definitions.Input{{Name: "x", Type: definitions.InputString, Visible: cond("action.y")}}
	if _, err := ValidateInputs(bad, map[string]any{}, expr.Vars{}); err == nil || !strings.Contains(err.Error(), "visibleIf of x") {
		t.Errorf("a failing visibleIf: %v", err)
	}
}
