package schema

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/engine/expr"
)

// Ported from the prototype's "hidden fields are pruned, Quai-managed fields are locked".
func TestPruneHiddenAndLockedFields(t *testing.T) {
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
	// The VM type's ui.fields: backup_policy is shown only when backups are on.
	visibleIf := map[string]*expr.Program{"backup_policy": cond("inputs.backup == true")}
	doc := map[string]any{"backup": false, "backup_policy": "hourly", "cpu": 2}
	got, err := PruneHidden(doc, visibleIf, expr.Vars{Params: map[string]any{"env": "dev"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"backup": false, "cpu": 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("PruneHidden = %#v, want %#v", got, want)
	}
	if _, ok := doc["backup_policy"]; !ok {
		t.Error("PruneHidden changed its argument")
	}

	// The VM type locks its owner field and the fields it declares immutable.
	if got, want := LockedFields(vmSchema(t), "owner", "template", "network.vlan", "owner"), []string{"network.vlan", "owner", "template"}; !slices.Equal(got, want) {
		t.Errorf("LockedFields = %v, want %v", got, want)
	}
}

func TestPruneHiddenPaths(t *testing.T) {
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
	doc := map[string]any{
		"network": map[string]any{"vlan": 110, "ip": "10.0.0.1"},
		"disks":   []any{map[string]any{"size_gb": 50}, map[string]any{"size_gb": 100}},
		"nested":  []any{[]any{"a", "b"}},
		"mode":    "dhcp",
	}
	visibleIf := map[string]*expr.Program{
		"network.ip": cond("inputs.mode == 'static'"),
		"disks.1":    cond("params.env == 'prod'"),
		"nested.0.0": cond("false"),
		"missing.x":  cond("false"),
		"disks.9":    cond("false"),
		"mode.x":     cond("false"),
		"disks.x":    cond("false"),
	}
	got, err := PruneHidden(doc, visibleIf, expr.Vars{Params: map[string]any{"env": "dev"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"network": map[string]any{"vlan": 110},
		"disks":   []any{map[string]any{"size_gb": 50}},
		"nested":  []any{[]any{"b"}},
		"mode":    "dhcp",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PruneHidden = %#v, want %#v", got, want)
	}

	// Conditions see the document as pruned so far, in path order.
	chain := map[string]*expr.Program{
		"a": cond("false"),
		"b": cond("has(inputs.a)"),
	}
	got, err = PruneHidden(map[string]any{"a": 1, "b": 2, "c": 3}, chain, expr.Vars{})
	if err != nil || !reflect.DeepEqual(got, map[string]any{"c": 3}) {
		t.Errorf("chained PruneHidden = %#v, %v", got, err)
	}

	// CEL is strict: a condition reading a missing field is an error, not false.
	_, err = PruneHidden(map[string]any{}, map[string]*expr.Program{"x": cond("inputs.flag")}, expr.Vars{})
	if err == nil || !strings.Contains(err.Error(), "visibleIf of x") {
		t.Errorf("PruneHidden with a failing condition: error = %v", err)
	}
	if got, err := PruneHidden(nil, nil, expr.Vars{}); err != nil || len(got) != 0 {
		t.Errorf("PruneHidden(nil) = %#v, %v", got, err)
	}
}

func TestReadOnlyFieldsAndDefaults(t *testing.T) {
	doc := map[string]any{
		"type":     "object",
		"required": []any{"spec"},
		"properties": map[string]any{
			"id":   map[string]any{"type": "string", "readOnly": true},
			"spec": map[string]any{"$ref": "#/$defs/spec"},
			"meta": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
			"self": map[string]any{"$ref": "#"},
		},
		"$defs": map[string]any{
			"spec": map[string]any{
				"type": []any{"object", "null"},
				"properties": map[string]any{
					"region": map[string]any{"type": "string", "readOnly": true, "default": "ch"},
					"opts":   map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
				},
			},
		},
	}
	s := compile(t, map[string]any{"s.json": doc}, "s.json")
	if got, want := s.ReadOnlyFields(), []string{"id", "spec.region"}; !slices.Equal(got, want) {
		t.Errorf("ReadOnlyFields() = %v, want %v", got, want)
	}
	// The recursive "self" property is not followed; an optional object without defaults is
	// left out.
	if got, want := s.Defaults(), map[string]any{"spec": map[string]any{"region": "ch"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("Defaults() = %#v, want %#v", got, want)
	}
	if got := compile(t, map[string]any{"s.json": map[string]any{"type": "string"}}, "s.json").Defaults(); got != nil {
		t.Errorf("Defaults() of a schema without defaults = %#v", got)
	}
	if got := compile(t, map[string]any{"s.json": true}, "s.json").Defaults(); got != nil {
		t.Errorf("Defaults() of a boolean schema = %#v", got)
	}

	// Defaults are copies: changing them does not change the schema.
	d := s.Defaults().(map[string]any)
	d["spec"].(map[string]any)["region"] = "us"
	if s.Defaults().(map[string]any)["spec"].(map[string]any)["region"] != "ch" {
		t.Error("Defaults() returned the schema's own value")
	}
}

func TestPointer(t *testing.T) {
	doc := map[string]any{"a/b": map[string]any{"~c": []any{"x", "y"}}}
	for ptr, want := range map[string]any{"": doc, "/a~1b/~0c/1": "y"} {
		if got, ok := pointer(doc, ptr); !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("pointer(%q) = %#v, %v", ptr, got, ok)
		}
	}
	for _, ptr := range []string{"anchor", "/nope", "/a~1b/~0c/2", "/a~1b/~0c/x", "/a~1b/~0c/0/z"} {
		if got, ok := pointer(doc, ptr); ok {
			t.Errorf("pointer(%q) = %#v, want not found", ptr, got)
		}
	}
}
