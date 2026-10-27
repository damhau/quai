package expr

import "testing"

// FuzzCompileTemplate checks that no string, however malformed, makes compiling or evaluating a
// template panic, and that a compiled template without bindings evaluates to itself.
func FuzzCompileTemplate(f *testing.F) {
	for _, s := range []string{
		"${{ inputs.n }}",
		"x-${{ inputs.n }}-${{ params.env }}",
		"${{ {'a': {'b': 1}}.a.b }}",
		`${{ "a\"}}" + r'\' + '''}}''' }}`,
		"${{ inputs.?nope }}",
		"${{",
		"}}${{}}",
		"plain",
	} {
		f.Add(s)
	}
	env, err := NewEnv()
	if err != nil {
		f.Fatal(err)
	}
	vars := Vars{Inputs: map[string]any{"n": 1, "s": "x"}, Params: map[string]any{"env": "prod"}}
	f.Fuzz(func(t *testing.T, s string) {
		tpl, err := env.CompileTemplate(s)
		if err != nil {
			return
		}
		got, err := tpl.Eval(vars)
		if !HasBinding(s) && (err != nil || got != s) {
			t.Fatalf("template without bindings %q = %#v, %v", s, got, err)
		}
	})
}
