package expr

import (
	"reflect"
	"strings"
	"testing"
)

// Ported from the prototype's "templates keep types and never inject structure".
func TestValueKeepsTypesAndNeverInjectsStructure(t *testing.T) {
	env := newEnv(t)
	v, err := env.CompileValue(map[string]any{
		"n":     "${{ inputs.n }}",
		"s":     "x-${{ inputs.n }}",
		"evil":  "${{ inputs.evil }}",
		"gone":  "${{ inputs.?nope }}",
		"null":  "${{ null }}",
		"plain": "no bindings",
		"num":   2,
		"list":  []any{"${{ inputs.n }}", "${{ inputs.?nope }}", "${{ [inputs.n] }}"},
		"deep":  map[string]any{"vlan": " ${{ inputs.n }} "},
	})
	if err != nil {
		t.Fatalf("CompileValue: %v", err)
	}
	got, err := v.Eval(Vars{Inputs: map[string]any{"n": 4, "evil": "a\nkind: Secret"}})
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	want := map[string]any{
		"n":     int64(4),
		"s":     "x-4",
		"evil":  "a\nkind: Secret",
		"null":  nil, // null is kept: the generator removes the key; Absent leaves it alone
		"plain": "no bindings",
		"num":   int64(2),
		"list":  []any{int64(4), []any{int64(4)}},
		"deep":  map[string]any{"vlan": int64(4)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Eval =\n%#v\nwant\n%#v", got, want)
	}
}

func TestTemplateInterpolation(t *testing.T) {
	env := newEnv(t)
	vars := Vars{
		Inputs: map[string]any{"name": "web", "n": 3, "r": 1.5, "on": true, "tags": []any{"a", "b"}, "m": map[string]any{"k": "<v>"}},
		Params: map[string]any{"env": "prod"},
	}
	tests := []struct {
		src  string
		want any
	}{
		{"deployments/${{ params.env }}/${{ inputs.name }}", "deployments/prod/web"},
		{"n=${{ inputs.n }} r=${{ inputs.r }} on=${{ inputs.on }}", "n=3 r=1.5 on=true"},
		{"tags=${{ inputs.tags }} m=${{ inputs.m }}", `tags=["a","b"] m={"k":"<v>"}`},
		{"[${{ null }}${{ inputs.?nope }}]", "[]"},
		{"${{ inputs.n }}${{ inputs.n }}", "33"},
		{"no bindings", "no bindings"},
		{"", ""},
		// The closing braces are found outside string and map literals.
		{"${{ {'a': {'b': inputs.n}}.a.b }}", int64(3)},
		{"x${{ '}}' + inputs.name }}", "x}}web"},
		{`${{ "a\"}}" + r'\' + '''}}''' }}`, `a"}}\}}`},
		// Spaces around a single binding keep its type; any other text makes a string.
		{"  ${{ inputs.n }}\n", int64(3)},
		{"${{ inputs.n }}.", "3."},
	}
	for _, tt := range tests {
		tpl, err := env.CompileTemplate(tt.src)
		if err != nil {
			t.Fatalf("CompileTemplate(%q): %v", tt.src, err)
		}
		got, err := tpl.Eval(vars)
		if err != nil {
			t.Fatalf("Eval(%q): %v", tt.src, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q = %#v, want %#v", tt.src, got, tt.want)
		}
	}
}

func TestTemplateErrors(t *testing.T) {
	env := newEnv(t)
	for src, want := range map[string]string{
		"${{ inputs.n ":          "without a closing",
		"${{ }}":                 "empty binding",
		"a ${{ 'open }}":         "unterminated string",
		"${{ deployment.name }}": "undeclared reference",
	} {
		_, err := env.CompileTemplate(src)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CompileTemplate(%q) error = %v, want %q", src, err, want)
		}
	}
	if _, err := env.CompileValue(map[string]any{"a": []any{"${{ nope }}"}}); err == nil || !strings.HasPrefix(err.Error(), "a: [0]: ") {
		t.Errorf("CompileValue error = %v, want the path to the binding", err)
	}

	tpl, err := env.CompileTemplate("x-${{ inputs.missing }}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tpl.Eval(Vars{}); err == nil || !strings.Contains(err.Error(), "no such key") {
		t.Errorf("Eval with a missing key: error = %v, want no such key", err)
	}
	v, err := env.CompileValue(map[string]any{"a": "${{ inputs.missing }}"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Eval(Vars{}); err == nil || !strings.HasPrefix(err.Error(), "a: ") {
		t.Errorf("Value.Eval error = %v, want the key in the message", err)
	}
}

func TestHasBinding(t *testing.T) {
	if !HasBinding("a ${{ b }}") || HasBinding("a {{ b }}") || HasBinding("$ {{") {
		t.Error("HasBinding")
	}
}
