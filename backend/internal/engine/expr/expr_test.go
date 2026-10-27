package expr

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"cel.dev/cel-go/cel"
)

func newEnv(t *testing.T, opts ...Option) *Env {
	t.Helper()
	env, err := NewEnv(opts...)
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	return env
}

func eval(t *testing.T, env *Env, src string, v Vars) any {
	t.Helper()
	p, err := env.Compile(src)
	if err != nil {
		t.Fatalf("Compile(%q): %v", src, err)
	}
	got, err := p.Eval(v)
	if err != nil {
		t.Fatalf("Eval(%q): %v", src, err)
	}
	return got
}

// Ported from the prototype's "expressions: precedence, ternaries, membership", in CEL.
func TestEvalPrecedenceTernariesMembership(t *testing.T) {
	env := newEnv(t)
	vars := Vars{Inputs: map[string]any{"env": "prod", "size": 3, "tags": []any{"a", "b"}, "name": "Web-01"}}
	tests := []struct {
		src  string
		want any
	}{
		{"inputs.env == 'prod' && inputs.size > 2", true},
		{"1 + 2 * 3", int64(7)},
		{"inputs.size == 1 ? 'a' : inputs.size == 3 ? 'c' : 'z'", "c"},
		{"inputs.env in ['dev', 'prod']", true},
		{"'b' in inputs.tags", true},
		{"!(inputs.size < 2)", true},
		// The prototype's `in` on strings is `contains` in CEL.
		{"inputs.env.contains('ro')", true},
		// Numbers are converted with string(); the strings extension is enabled.
		{"inputs.name.lowerAscii() + '-' + string(inputs.size)", "web-01-3"},
		{"inputs.tags.join(',')", "a,b"},
		// A missing key is read with has() or an optional.
		{"has(inputs.missing)", false},
		{"inputs.?missing.?deep.orValue('none')", "none"},
		{"inputs['env']", "prod"},
		{"[inputs.size, 2.5, null, {'k': inputs.tags}]", []any{int64(3), 2.5, nil, map[string]any{"k": []any{"a", "b"}}}},
	}
	for _, tt := range tests {
		if got := eval(t, env, tt.src, vars); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s = %#v, want %#v", tt.src, got, tt.want)
		}
	}
}

// Ported from "expressions cannot reach prototypes or run code": CEL has no prototypes, no
// statements and only the declared variables; a missing key is an error, not undefined.
func TestEvalStrictness(t *testing.T) {
	env := newEnv(t)
	vars := Vars{Inputs: map[string]any{"s": "x", "n": 1}}
	compileErrors := []string{
		"inputs.x; process.exit()",
		"process.env",
		"deployment.name", // the prototype's variable; not in v1beta1
		"inputs.s + inputs.n + 1 +",
		"'a' + 1",
	}
	for _, src := range compileErrors {
		if _, err := env.Compile(src); err == nil {
			t.Errorf("Compile(%q) succeeded, want an error", src)
		}
	}
	evalErrors := []string{
		"inputs.missing.deep",
		"inputs.constructor",
		"inputs.__proto__",
		"inputs.s + inputs.n", // no implicit conversion
		"inputs.n / 0",
	}
	for _, src := range evalErrors {
		p, err := env.Compile(src)
		if err != nil {
			t.Fatalf("Compile(%q): %v", src, err)
		}
		_, err = p.Eval(vars)
		var e *Error
		if !errors.As(err, &e) || e.Expr != src {
			t.Errorf("Eval(%q) error = %v, want an *Error for the expression", src, err)
		}
	}
}

func TestNumbers(t *testing.T) {
	env := newEnv(t)
	// JSON decodes every number as a float64; whole ones become ints, so integer arithmetic works
	// whether the values came from YAML or from the API.
	var inputs map[string]any
	if err := json.Unmarshal([]byte(`{"cpu": 4, "ratio": 1.5, "big": 9007199254740993}`), &inputs); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(`{"exact": 9007199254740993}`))
	dec.UseNumber()
	var exact map[string]any
	if err := dec.Decode(&exact); err != nil {
		t.Fatal(err)
	}
	inputs["exact"] = exact["exact"]
	inputs["small"] = uint8(7)
	vars := Vars{Inputs: inputs}
	tests := []struct {
		src  string
		want any
	}{
		{"inputs.cpu * 2", int64(8)},
		{"inputs.ratio * 2.0", 3.0},
		{"inputs.cpu > 3.5", true}, // comparisons across int and double
		{"inputs.cpu == 4.0", true},
		{"inputs.exact", int64(9007199254740993)},
		{"inputs.small + 1", int64(8)},
		{"double(inputs.cpu) / 8.0", 0.5},
	}
	for _, tt := range tests {
		if got := eval(t, env, tt.src, vars); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s = %#v, want %#v", tt.src, got, tt.want)
		}
	}
}

func TestVariables(t *testing.T) {
	env := newEnv(t)
	vars := Vars{
		Inputs:   map[string]any{"cpu": 16},
		Params:   map[string]any{"env": "prod", "name": "web01"},
		Doc:      map[string]any{"cpu": 4},
		Previous: map[string]any{"cpu": 4},
		Action:   map[string]any{"size": "large"},
		Request:  Request{Kind: "action", Action: "resize", Reason: "load"},
		User:     User{Name: "alice", Groups: []string{"platform-admins"}},
	}
	tests := []struct {
		src  string
		want any
	}{
		{"params.env == 'prod' && params.name == 'web01'", true},
		{"doc.cpu", int64(4)},
		{"inputs.cpu > 8 && !(has(previous.cpu) && previous.cpu > 8)", true},
		{"action.size", "large"},
		{"request.kind == 'action' && request.action == 'resize' && request.reason == 'load'", true},
		{"user.name == 'alice' && 'platform-admins' in user.groups", true},
	}
	for _, tt := range tests {
		if got := eval(t, env, tt.src, vars); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s = %#v, want %#v", tt.src, got, tt.want)
		}
	}

	// On create there is no previous value and no document: both are empty, not missing.
	create := Vars{Inputs: map[string]any{"cpu": 16}}
	for src, want := range map[string]any{
		"inputs.cpu > 8 && !(has(previous.cpu) && previous.cpu > 8)": true,
		"has(doc.cpu)":      false,
		"size(previous)":    int64(0),
		"request.kind":      "",
		"size(user.groups)": int64(0),
	} {
		if got := eval(t, env, src, create); !reflect.DeepEqual(got, want) {
			t.Errorf("on create, %s = %#v, want %#v", src, got, want)
		}
	}
}

func TestConditions(t *testing.T) {
	env := newEnv(t)
	vars := Vars{Inputs: map[string]any{"environment": "prod", "withBucket": true, "size": 3}}
	tests := []struct {
		src  string
		want bool
	}{
		{"", true}, // an absent condition is true
		{"  ", true},
		{"inputs.environment == 'prod'", true},
		{"inputs.withBucket", true},
		{"inputs.size == 2", false},
	}
	for _, tt := range tests {
		p, err := env.CompileCondition(tt.src)
		if err != nil {
			t.Fatalf("CompileCondition(%q): %v", tt.src, err)
		}
		got, err := p.Test(vars)
		if err != nil || got != tt.want {
			t.Errorf("Test(%q) = %v, %v; want %v", tt.src, got, err, tt.want)
		}
	}

	// A condition whose type is known not to be a bool fails to compile; one whose type is only
	// known at run time fails to evaluate.
	if _, err := env.CompileCondition("inputs.size + 1"); err == nil || !strings.Contains(err.Error(), "must be a bool") {
		t.Errorf("CompileCondition(int) error = %v, want a bool error", err)
	}
	p, err := env.CompileCondition("inputs.size")
	if err != nil {
		t.Fatalf("CompileCondition(dyn): %v", err)
	}
	if _, err := p.Test(vars); err == nil || !strings.Contains(err.Error(), "must be a bool, not int") {
		t.Errorf("Test(dyn int) error = %v, want a bool error", err)
	}
	if _, err := env.Compile(""); err == nil {
		t.Error("Compile(\"\") succeeded; only conditions may be empty")
	}
}

func TestWithType(t *testing.T) {
	// The hook for schema-typed variables: a declared type is checked at compile time.
	env := newEnv(t, WithType(VarInputs, cel.MapType(cel.StringType, cel.IntType)))
	if _, err := env.Compile("inputs.cpu == 'x'"); err == nil {
		t.Error("comparing an int input with a string compiled")
	}
	if got := eval(t, env, "inputs.cpu * 2", Vars{Inputs: map[string]any{"cpu": 2}}); got != int64(4) {
		t.Errorf("inputs.cpu * 2 = %#v, want 4", got)
	}
	if _, err := NewEnv(WithType("deployment", cel.DynType)); err == nil {
		t.Error("WithType accepted an unknown variable")
	}
}

func TestLimits(t *testing.T) {
	env := newEnv(t)
	if _, err := env.Compile(strings.Repeat("1 + ", MaxExpressionSize/4) + "1"); err == nil {
		t.Error("an expression longer than MaxExpressionSize compiled")
	}
	// 2,000 × 2,000 comparisons cost far more than CostLimit.
	big := make([]any, 2000)
	for i := range big {
		big[i] = i
	}
	p, err := env.Compile("inputs.big.all(x, inputs.big.all(y, x != y || x == y))")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Eval(Vars{Inputs: map[string]any{"big": big}}); err == nil || !strings.Contains(err.Error(), "cost limit") {
		t.Errorf("Eval of an expensive expression: error = %v, want the cost limit", err)
	}
}

func TestResults(t *testing.T) {
	env := newEnv(t)
	vars := Vars{Inputs: map[string]any{"n": 1}}
	if got := eval(t, env, "inputs.?nope", vars); got != Absent {
		t.Errorf("inputs.?nope = %#v, want Absent", got)
	}
	if got := eval(t, env, "inputs.?n", vars); got != int64(1) {
		t.Errorf("inputs.?n = %#v, want 1", got)
	}
	for _, src := range []string{
		"b'bytes'",
		"timestamp('2026-01-01T00:00:00Z')",
		"duration('1h')",
		"{1: 'a'}",
		"[optional.none()]",
		"1.0 / 0.0",
		"int",
	} {
		p, err := env.Compile(src)
		if err != nil {
			t.Fatalf("Compile(%q): %v", src, err)
		}
		if got, err := p.Eval(vars); err == nil {
			t.Errorf("Eval(%q) = %#v, want an error: the result is not in the JSON data model", src, got)
		}
	}
}

func TestVarsOutsideTheDataModel(t *testing.T) {
	env := newEnv(t)
	p, err := env.Compile("inputs.x")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{struct{}{}, map[int]any{1: "a"}, func() {}} {
		if _, err := p.Eval(Vars{Inputs: map[string]any{"x": v}}); err == nil {
			t.Errorf("Eval with input %T succeeded", v)
		}
	}
	// Typed slices and maps from Go code are fine.
	got, err := p.Eval(Vars{Inputs: map[string]any{"x": map[string][]string{"a": {"b"}}}})
	if err != nil || !reflect.DeepEqual(got, map[string]any{"a": []any{"b"}}) {
		t.Errorf("Eval(map[string][]string) = %#v, %v", got, err)
	}
}

func TestConcurrentEval(t *testing.T) {
	env := newEnv(t)
	p, err := env.Compile("inputs.n * 2")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := p.Eval(Vars{Inputs: map[string]any{"n": i}})
			if err != nil || got != int64(2*i) {
				t.Errorf("Eval(n=%d) = %v, %v", i, got, err)
			}
		}()
	}
	wg.Wait()
}

func TestNormalize(t *testing.T) {
	type name string
	tests := []struct {
		in   any
		want any
	}{
		{name("a"), "a"},
		{true, true},
		{[2]int{1, 2}, []any{int64(1), int64(2)}},
		{[]string(nil), []any{}},
		{map[string]any(nil), map[string]any{}},
		{float32(1.5), 1.5},
		{json.Number("1.5"), 1.5},
		{json.Number("1e3"), int64(1000)},
		{uint64(7), int64(7)},
		{2.0, int64(2)},
		{1e300, 1e300},
	}
	for _, tt := range tests {
		got, err := normalize(tt.in)
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("normalize(%#v) = %#v, %v; want %#v", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []any{json.Number("x"), uint64(1 << 63), math.NaN(), math.Inf(1), []any{func() {}}, map[string]any{"a": func() {}}, []func(){nil}, map[string]func(){"a": nil}} {
		if got, err := normalize(in); err == nil {
			t.Errorf("normalize(%#v) = %#v, want an error", in, got)
		}
	}
}

func TestAPIDetails(t *testing.T) {
	env := newEnv(t)
	p, err := env.Compile("inputs.n")
	if err != nil {
		t.Fatal(err)
	}
	if p.String() != "inputs.n" || fmt.Sprint(Absent) != "absent" {
		t.Errorf("String() = %q, Absent prints as %q", p.String(), fmt.Sprint(Absent))
	}
	_, err = env.Compile("nope")
	if errors.Unwrap(err) == nil {
		t.Errorf("a compile error does not wrap CEL's error: %v", err)
	}
	if _, err := NewEnv(WithType(VarInputs, nil)); err == nil {
		t.Error("WithType accepted a nil type")
	}

	// Values outside the data model are reported by every entry point.
	bad := Vars{Inputs: map[string]any{"x": func() {}}}
	if _, err := p.Eval(bad); err == nil {
		t.Error("Program.Eval accepted a func")
	}
	if _, err := p.Test(bad); err == nil {
		t.Error("Program.Test accepted a func")
	}
	for name, v := range map[string]Vars{
		"params":   {Params: map[string]any{"x": func() {}}},
		"doc":      {Doc: func() {}},
		"previous": {Previous: map[string]any{"x": func() {}}},
	} {
		if _, err := p.Eval(v); err == nil || !strings.HasPrefix(err.Error(), name+": ") {
			t.Errorf("Eval with a bad %s: error = %v", name, err)
		}
	}
	tpl, err := env.CompileTemplate("${{ inputs.n }}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tpl.Eval(bad); err == nil {
		t.Error("Template.Eval accepted a func")
	}
	v, err := env.CompileValue([]any{"${{ inputs.n }}"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Eval(bad); err == nil {
		t.Error("Value.Eval accepted a func")
	}
	if _, err := env.CompileValue(map[string]any{"a": func() {}}); err == nil {
		t.Error("CompileValue accepted a func")
	}
}
