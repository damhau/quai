// Command celcases exports CEL conformance cases (cel.dev/expr/tests/simple, which cel-go runs)
// as JSON, for the frontend's CEL conformance test. It keeps the files that matter for form
// conditions and the cases that match how Quai runs expressions: type-checked (the backend checks
// every expression when it loads a definition), with macros, and with values JSON can carry (no
// protocol buffer messages, types or enums).
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"cel.dev/expr"
	ctest "cel.dev/expr/conformance/test"
	"google.golang.org/protobuf/encoding/prototext"

	_ "cel.dev/expr/conformance/proto2" // message types the test files mention
	_ "cel.dev/expr/conformance/proto3"
)

// Files are the conformance files a form condition can touch.
var files = []string{"basic", "comparisons", "logic", "lists", "fields", "macros", "string", "string_ext", "optionals", "integer_math", "conversions"}

// Case is one conformance case.
type Case struct {
	File     string         `json:"file"`
	Section  string         `json:"section"`
	Name     string         `json:"name"`
	Expr     string         `json:"expr"`
	Bindings map[string]any `json:"bindings,omitempty"`
	Expect   any            `json:"expect"` // a tagged value, or {"error": true}
}

var errSkip = errors.New("not representable")

func main() {
	dir := flag.String("dir", "", "the tests/simple/testdata directory of cel.dev/expr")
	flag.Parse()
	root, err := os.OpenRoot(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = root.Close() }()
	var cases []Case
	skipped := 0
	for _, f := range files {
		b, err := root.ReadFile(f + ".textproto")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		var file ctest.SimpleTestFile
		if err := (prototext.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, &file); err != nil {
			fmt.Fprintln(os.Stderr, f, err)
			os.Exit(1)
		}
		for _, s := range file.GetSection() {
			for _, t := range s.GetTest() {
				c, err := export(f, s.GetName(), t)
				if err != nil {
					skipped++
					continue
				}
				cases = append(cases, c)
			}
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(cases); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "%d cases exported, %d skipped\n", len(cases), skipped)
}

func export(file, section string, t *ctest.SimpleTest) (Case, error) {
	if t.GetDisableMacros() || t.GetDisableCheck() || t.GetCheckOnly() || t.GetContainer() != "" || strings.Contains(t.GetExpr(), "google.protobuf.") || strings.Contains(t.GetExpr(), "TestAllTypes") {
		return Case{}, errSkip
	}
	c := Case{File: file, Section: section, Name: t.GetName(), Expr: t.GetExpr()}
	for name, ev := range t.GetBindings() {
		v, err := value(ev.GetValue())
		if err != nil || ev.GetValue() == nil {
			return Case{}, errSkip
		}
		if c.Bindings == nil {
			c.Bindings = map[string]any{}
		}
		c.Bindings[name] = v
	}
	switch {
	case t.GetValue() != nil:
		v, err := value(t.GetValue())
		if err != nil {
			return Case{}, err
		}
		c.Expect = v
	case t.GetEvalError() != nil, t.GetAnyEvalErrors() != nil:
		c.Expect = map[string]bool{"error": true}
	case t.GetResultMatcher() == nil:
		c.Expect = map[string]any{"t": "bool", "v": true} // a case without a matcher expects true
	default:
		return Case{}, errSkip
	}
	return c, nil
}

// value converts a CEL value to a tagged JSON value: {"t": "int", "v": "42"}.
func value(v *expr.Value) (any, error) {
	tag := func(t string, val any) map[string]any { return map[string]any{"t": t, "v": val} }
	switch k := v.GetKind().(type) {
	case *expr.Value_NullValue:
		return tag("null", nil), nil
	case *expr.Value_BoolValue:
		return tag("bool", k.BoolValue), nil
	case *expr.Value_Int64Value:
		return tag("int", strconv.FormatInt(k.Int64Value, 10)), nil
	case *expr.Value_Uint64Value:
		return tag("uint", strconv.FormatUint(k.Uint64Value, 10)), nil
	case *expr.Value_DoubleValue:
		f := k.DoubleValue
		switch {
		case math.IsNaN(f):
			return tag("double", "NaN"), nil
		case math.IsInf(f, 1):
			return tag("double", "Infinity"), nil
		case math.IsInf(f, -1):
			return tag("double", "-Infinity"), nil
		}
		return tag("double", f), nil
	case *expr.Value_StringValue:
		return tag("string", k.StringValue), nil
	case *expr.Value_BytesValue:
		return tag("bytes", base64.StdEncoding.EncodeToString(k.BytesValue)), nil
	case *expr.Value_ListValue:
		items := []any{}
		for _, e := range k.ListValue.GetValues() {
			x, err := value(e)
			if err != nil {
				return nil, err
			}
			items = append(items, x)
		}
		return tag("list", items), nil
	case *expr.Value_MapValue:
		entries := []any{}
		for _, e := range k.MapValue.GetEntries() {
			kk, err := value(e.GetKey())
			if err != nil {
				return nil, err
			}
			vv, err := value(e.GetValue())
			if err != nil {
				return nil, err
			}
			entries = append(entries, []any{kk, vv})
		}
		return tag("map", entries), nil
	}
	return nil, errSkip
}
