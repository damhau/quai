package schema

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/engine/edit/yamledit"
)

const examples = "../../../../examples/gitops-repo"

func readDoc(t *testing.T, path string) any {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(examples, path))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := yamledit.Decode(src)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

func compile(t *testing.T, docs map[string]any, path string) *Schema {
	t.Helper()
	c := NewCompiler()
	for _, p := range slices.Sorted(func(yield func(string) bool) {
		for p := range docs {
			if !yield(p) {
				return
			}
		}
	}) {
		if err := c.Add(p, docs[p]); err != nil {
			t.Fatalf("Add(%s): %v", p, err)
		}
	}
	s, err := c.Compile(path)
	if err != nil {
		t.Fatalf("Compile(%s): %v", path, err)
	}
	return s
}

func vmSchema(t *testing.T) *Schema {
	const p = "catalog/schemas/vm.schema.json"
	return compile(t, map[string]any{p: readDoc(t, p)}, p)
}

func fields(t *testing.T, err error) map[string]string {
	t.Helper()
	if err == nil {
		return map[string]string{}
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error %v is not a *ValidationError", err)
	}
	return ve.Fields
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Ported from the prototype's "JSON Schema: defaults, refs, formats, errors keyed by field path".
func TestVMSchema(t *testing.T) {
	s := vmSchema(t)
	defaults, ok := s.Defaults().(map[string]any)
	if !ok {
		t.Fatalf("Defaults() = %#v", s.Defaults())
	}
	if defaults["cpu"] != int64(2) {
		t.Errorf("default cpu = %#v, want 2", defaults["cpu"])
	}
	if want := []any{map[string]any{"size_gb": int64(50), "tier": "standard"}}; !reflect.DeepEqual(defaults["disks"], want) {
		t.Errorf("default disks = %#v, want %#v", defaults["disks"], want)
	}
	if net, _ := defaults["network"].(map[string]any); net["vlan"] != int64(110) {
		t.Errorf("default network = %#v, want vlan 110", defaults["network"])
	}
	if _, ok := defaults["tags"]; ok {
		t.Error("an optional object without defaults is in the defaults")
	}
	if err := s.Validate(defaults); err != nil {
		t.Errorf("the defaults do not validate: %v", err)
	}

	bad := clone(defaults).(map[string]any)
	bad["cpu"] = 99
	bad["extra"] = 1
	bad["disks"] = []any{map[string]any{"tier": "ssd"}}
	bad["network"] = map[string]any{"vlan": 999, "ip": "not-an-ip"}
	errs := fields(t, s.Validate(bad))
	if got, want := keys(errs), []string{"cpu", "disks.0.size_gb", "extra", "network.ip", "network.vlan"}; !slices.Equal(got, want) {
		t.Errorf("error fields = %v, want %v (%v)", got, want, errs)
	}
	for field, want := range map[string]string{
		"cpu":             "must be at most 32",
		"disks.0.size_gb": "is required",
		"extra":           "is not allowed by the schema",
		"network.ip":      "must be a valid ipv4",
		"network.vlan":    "must be one of 110, 120, 130",
	} {
		if errs[field] != want {
			t.Errorf("%s: %q, want %q", field, errs[field], want)
		}
	}

	// Every hand-written example file honours the contract.
	files, err := filepath.Glob(filepath.Join(examples, "envs/*/vms/*.yaml"))
	if err != nil || len(files) < 4 {
		t.Fatalf("example VM files: %v, %v", files, err)
	}
	for _, f := range files {
		rel, _ := filepath.Rel(examples, f)
		if err := s.Validate(readDoc(t, rel)); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}
}

func TestValidationMessages(t *testing.T) {
	doc := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"properties": map[string]any{
			"name":     map[string]any{"type": "string", "minLength": 3, "maxLength": 5, "pattern": "^[a-z]+$"},
			"n":        map[string]any{"type": "integer", "exclusiveMinimum": 0, "exclusiveMaximum": 10, "multipleOf": 2},
			"r":        map[string]any{"type": "number", "minimum": 0.5},
			"kind":     map[string]any{"const": "vm"},
			"tags":     map[string]any{"type": "array", "minItems": 1, "maxItems": 2, "uniqueItems": true},
			"labels":   map[string]any{"type": "object", "minProperties": 1, "maxProperties": 1},
			"email":    map[string]any{"type": "string", "format": "email"},
			"either":   map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}},
			"one":      map[string]any{"oneOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "number"}}},
			"never":    false,
			"nullable": map[string]any{"type": []any{"string", "null"}},
			"obj":      map[string]any{"type": "object"},
			"list":     map[string]any{"type": "array", "items": map[string]any{"enum": []any{"a", 1, nil, []any{1}}}},
		},
	}
	s := compile(t, map[string]any{"s.json": doc}, "s.json")
	tests := []struct {
		field string
		value any
		want  string
	}{
		{"name", "ab", "must be at least 3 characters long"},
		{"name", "abcdef", "must be at most 5 characters long"},
		{"name", "ABC", "must match the pattern ^[a-z]+$"},
		{"name", 1, "must be a string"},
		{"n", 0, "must be greater than 0"},
		{"n", 10, "must be less than 10"},
		{"n", 3, "must be a multiple of 2"},
		{"n", 2.5, "must be an integer"},
		{"r", 0.25, "must be at least 0.5"},
		{"kind", "x", "must be vm"},
		{"tags", []any{}, "must have at least 1 items"},
		{"tags", []any{"a", "b", "c"}, "must have at most 2 items"},
		{"tags", []any{"a", "a"}, "must not contain duplicates"},
		{"labels", map[string]any{}, "must have at least 1 entries"},
		{"labels", map[string]any{"a": 1, "b": 2}, "must have at most 1 entries"},
		{"email", "nope", "must be a valid email"},
		{"either", true, "must match at least one of the allowed forms"},
		{"one", 1, "must match exactly one of the allowed forms"},
		{"never", 1, "is not allowed"},
		{"nullable", 1, "must be null or a string"},
		{"obj", []any{}, "must be an object"},
		{"list.0", "b", `must be one of a, 1, null, [1]`},
	}
	for _, tt := range tests {
		v := map[string]any{}
		if f, ok := strings.CutSuffix(tt.field, ".0"); ok {
			v[f] = []any{tt.value}
		} else {
			v[tt.field] = tt.value
		}
		errs := fields(t, s.Validate(v))
		if errs[tt.field] != tt.want || len(errs) != 1 {
			t.Errorf("%s = %#v: errors %v, want %q", tt.field, tt.value, errs, tt.want)
		}
	}
	if errs := fields(t, s.Validate("not an object")); errs["$"] != "must be an object" {
		t.Errorf("root error = %v, want $: must be an object", errs)
	}
	err := s.Validate(map[string]any{"n": 3, "name": "ab"})
	if err == nil || err.Error() != "invalid document: n: must be a multiple of 2; name: must be at least 3 characters long" {
		t.Errorf("Error() = %v", err)
	}
	if err := s.Validate(map[string]any{"n": struct{}{}}); err == nil {
		t.Error("a value outside the JSON data model validated")
	}
}

func TestDrafts(t *testing.T) {
	draft7 := map[string]any{
		"$schema":     "http://json-schema.org/draft-07/schema#",
		"definitions": map[string]any{"size": map[string]any{"type": "integer", "minimum": 1}},
		"type":        "object",
		"properties":  map[string]any{"size": map[string]any{"$ref": "#/definitions/size", "default": 4}},
	}
	draft2019 := map[string]any{
		"$schema":    "https://json-schema.org/draft/2019-09/schema",
		"$defs":      map[string]any{"size": map[string]any{"type": "integer", "minimum": 1}},
		"type":       "object",
		"properties": map[string]any{"size": map[string]any{"$ref": "#/$defs/size"}},
		// 2019-09 keeps dependentRequired, which draft 7 spells dependencies.
		"dependentRequired": map[string]any{"size": []any{"unit"}},
	}
	noSchema := map[string]any{ // 2020-12 by default: prefixItems
		"type":        "array",
		"prefixItems": []any{map[string]any{"type": "string"}},
	}
	for name, tt := range map[string]struct {
		doc       any
		good, bad any
		field     string
	}{
		"draft 7":  {draft7, map[string]any{"size": 2}, map[string]any{"size": 0}, "size"},
		"2019-09":  {draft2019, map[string]any{"size": 2, "unit": "GB"}, map[string]any{"size": 2}, "$"},
		"2020-12":  {noSchema, []any{"a", 1}, []any{1}, "0"},
		"via JSON": {jsonDoc(t, `{"type":"object","properties":{"cpu":{"type":"integer","maximum":8}}}`), map[string]any{"cpu": 8}, map[string]any{"cpu": 9.0}, "cpu"},
	} {
		s := compile(t, map[string]any{"s.json": tt.doc}, "s.json")
		if err := s.Validate(tt.good); err != nil {
			t.Errorf("%s: valid document: %v", name, err)
		}
		if errs := fields(t, s.Validate(tt.bad)); errs[tt.field] == "" {
			t.Errorf("%s: invalid document: errors %v, want one at %s", name, errs, tt.field)
		}
	}
	s := compile(t, map[string]any{"s.json": draft7}, "s.json")
	if got := s.Defaults(); !reflect.DeepEqual(got, map[string]any{"size": 4}) {
		t.Errorf("draft 7 defaults = %#v", got)
	}
}

func jsonDoc(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRefsBetweenFiles(t *testing.T) {
	docs := map[string]any{
		"schemas/vm.json": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"disk": map[string]any{"$ref": "common/disk.json"},
				"net":  map[string]any{"$ref": "https://acme.example/net.json#/$defs/vlan"},
			},
		},
		"schemas/common/disk.json": map[string]any{
			"type":       "object",
			"properties": map[string]any{"size": map[string]any{"type": "integer", "default": 50, "readOnly": true}},
		},
		"schemas/net.json": map[string]any{
			"$id":   "https://acme.example/net.json",
			"$defs": map[string]any{"vlan": map[string]any{"$ref": "#/$defs/id", "default": 110}, "id": map[string]any{"type": "integer", "enum": []any{110, 120}}},
		},
	}
	s := compile(t, docs, "schemas/vm.json")
	if err := s.Validate(map[string]any{"disk": map[string]any{"size": 10}, "net": 120}); err != nil {
		t.Errorf("valid document: %v", err)
	}
	errs := fields(t, s.Validate(map[string]any{"disk": map[string]any{"size": "x"}, "net": 130}))
	if got := keys(errs); !slices.Equal(got, []string{"disk.size", "net"}) {
		t.Errorf("errors = %v", errs)
	}
	if got, want := s.Defaults(), map[string]any{"disk": map[string]any{"size": 50}, "net": 110}; !reflect.DeepEqual(got, want) {
		t.Errorf("Defaults() = %#v, want %#v", got, want)
	}
	if got := s.ReadOnlyFields(); !slices.Equal(got, []string{"disk.size"}) {
		t.Errorf("ReadOnlyFields() = %v", got)
	}
	if s.Path() != "schemas/vm.json" {
		t.Errorf("Path() = %q", s.Path())
	}
}

func TestNothingIsFetched(t *testing.T) {
	for name, ref := range map[string]string{
		"network":       "https://json.schemastore.org/github-workflow.json",
		"missing file":  "other.json",
		"absolute file": "file:///etc/passwd",
	} {
		c := NewCompiler()
		if err := c.Add("s.json", map[string]any{"$ref": ref}); err != nil {
			t.Fatal(err)
		}
		_, err := c.Compile("s.json")
		if err == nil || !strings.Contains(err.Error(), "is not in the repository") {
			t.Errorf("%s: Compile error = %v, want a refusal", name, err)
		}
	}
}

func TestCompilerErrors(t *testing.T) {
	c := NewCompiler()
	for _, p := range []string{"", "/abs.json", "a/../b.json", "./a.json", "a//b.json", `a\b.json`, "a.json#x"} {
		if err := c.Add(p, map[string]any{}); err == nil {
			t.Errorf("Add(%q) succeeded", p)
		}
	}
	if err := c.Add("a.json", map[string]any{"$id": "https://acme.example/a.json"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Add("a.json", map[string]any{}); err == nil {
		t.Error("a path was added twice")
	}
	if err := c.Add("b.json", map[string]any{"$id": "https://acme.example/a.json"}); err == nil {
		t.Error("an $id was used twice")
	}
	if _, err := c.Compile("c.json"); err == nil {
		t.Error("Compile of a schema that was not added succeeded")
	}
	if _, err := c.Compile("../c.json"); err == nil {
		t.Error("Compile of a bad path succeeded")
	}
	if err := c.Add("bad.json", map[string]any{"type": 12}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Compile("bad.json"); err == nil {
		t.Error("an invalid schema compiled")
	}
}

func TestSource(t *testing.T) {
	files := map[string]any{
		"schemas/vm.json":   map[string]any{"type": "object", "properties": map[string]any{"disk": map[string]any{"$ref": "disk.json"}}},
		"schemas/disk.json": map[string]any{"type": "integer", "minimum": 10},
		"schemas/bad.json":  "not decodable",
	}
	c := NewCompiler()
	c.UseSource(func(path string) (any, error) {
		doc, ok := files[path]
		if !ok {
			return nil, fs.ErrNotExist
		}
		if doc == "not decodable" {
			return nil, errors.New("invalid YAML")
		}
		return doc, nil
	})
	s, err := c.Compile("schemas/vm.json")
	if err != nil {
		t.Fatal(err)
	}
	if errs := fields(t, s.Validate(map[string]any{"disk": 5})); errs["disk"] != "must be at least 10" {
		t.Errorf("errors = %v", errs)
	}
	for path, want := range map[string]string{
		"schemas/none.json": "schemas/none.json is not in the repository",
		"schemas/bad.json":  "schemas/bad.json: invalid YAML",
	} {
		if _, err := c.Compile(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Compile(%s) error = %v, want %q", path, err, want)
		}
	}
	if _, err := NewCompiler().Compile("schemas/vm.json"); err == nil || !strings.Contains(err.Error(), "not in the repository") {
		t.Errorf("Compile without a source: error = %v", err)
	}
}
