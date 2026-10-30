package jsonedit

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

type p = document.Path

// set returns an edit that sets values at paths.
func set(pairs ...any) func(any) any {
	return func(doc any) any {
		for i := 0; i < len(pairs); i += 2 {
			v, err := document.Normalize(pairs[i+1])
			if err != nil {
				panic(err)
			}
			doc = document.With(doc, pairs[i].(p), v)
		}
		return doc
	}
}

// del returns an edit that removes the entries or items at paths, in order.
func del(paths ...p) func(any) any {
	return func(doc any) any {
		for _, path := range paths {
			doc = document.Without(doc, path)
		}
		return doc
	}
}

func TestEdit(t *testing.T) {
	pretty := "{\n  \"name\": \"web01\",\n  \"cpu\": 4,\n  \"tags\": {\n    \"app\": \"shop\"\n  },\n  \"disks\": [\n    { \"size_gb\": 80 },\n    { \"size_gb\": 200 }\n  ]\n}\n"
	for _, tt := range []struct {
		name, src string
		edit      func(any) any
		want      string
	}{
		{"a scalar, in place", pretty, set(p{"cpu"}, 8),
			"{\n  \"name\": \"web01\",\n  \"cpu\": 8,\n  \"tags\": {\n    \"app\": \"shop\"\n  },\n  \"disks\": [\n    { \"size_gb\": 80 },\n    { \"size_gb\": 200 }\n  ]\n}\n"},
		{"a string with <, > and &", pretty, set(p{"name"}, "a<b>&c"),
			"{\n  \"name\": \"a<b>&c\",\n  \"cpu\": 4,\n  \"tags\": {\n    \"app\": \"shop\"\n  },\n  \"disks\": [\n    { \"size_gb\": 80 },\n    { \"size_gb\": 200 }\n  ]\n}\n"},
		{"a new key, indented like its siblings", pretty, set(p{"tags", "owner"}, "bob"),
			"{\n  \"name\": \"web01\",\n  \"cpu\": 4,\n  \"tags\": {\n    \"app\": \"shop\",\n    \"owner\": \"bob\"\n  },\n  \"disks\": [\n    { \"size_gb\": 80 },\n    { \"size_gb\": 200 }\n  ]\n}\n"},
		{"new keys in sorted order, a new object on several lines", pretty, set(p{"zone"}, "b", p{"network"}, map[string]any{"vlan": 110, "ip": "10.0.0.1"}),
			"{\n  \"name\": \"web01\",\n  \"cpu\": 4,\n  \"tags\": {\n    \"app\": \"shop\"\n  },\n  \"disks\": [\n    { \"size_gb\": 80 },\n    { \"size_gb\": 200 }\n  ],\n  \"network\": {\n    \"ip\": \"10.0.0.1\",\n    \"vlan\": 110\n  },\n  \"zone\": \"b\"\n}\n"},
		{"a key in a one-line object", pretty, set(p{"disks", 0, "tier"}, "ssd"),
			"{\n  \"name\": \"web01\",\n  \"cpu\": 4,\n  \"tags\": {\n    \"app\": \"shop\"\n  },\n  \"disks\": [\n    { \"size_gb\": 80, \"tier\": \"ssd\" },\n    { \"size_gb\": 200 }\n  ]\n}\n"},
		{"the last entry removed", pretty, del(p{"disks"}),
			"{\n  \"name\": \"web01\",\n  \"cpu\": 4,\n  \"tags\": {\n    \"app\": \"shop\"\n  }\n}\n"},
		{"the first entry removed", pretty, del(p{"name"}),
			"{\n  \"cpu\": 4,\n  \"tags\": {\n    \"app\": \"shop\"\n  },\n  \"disks\": [\n    { \"size_gb\": 80 },\n    { \"size_gb\": 200 }\n  ]\n}\n"},
		{"two adjacent entries removed", pretty, del(p{"name"}, p{"cpu"}),
			"{\n  \"tags\": {\n    \"app\": \"shop\"\n  },\n  \"disks\": [\n    { \"size_gb\": 80 },\n    { \"size_gb\": 200 }\n  ]\n}\n"},
		{"an item appended", pretty, set(p{"disks", 2}, map[string]any{"size_gb": 10}),
			"{\n  \"name\": \"web01\",\n  \"cpu\": 4,\n  \"tags\": {\n    \"app\": \"shop\"\n  },\n  \"disks\": [\n    { \"size_gb\": 80 },\n    { \"size_gb\": 200 },\n    { \"size_gb\": 10 }\n  ]\n}\n"},
		{"an item removed", pretty, del(p{"disks", 0}),
			"{\n  \"name\": \"web01\",\n  \"cpu\": 4,\n  \"tags\": {\n    \"app\": \"shop\"\n  },\n  \"disks\": [\n    { \"size_gb\": 200 }\n  ]\n}\n"},
		{"a value changing type", pretty, set(p{"tags"}, []any{"a", "b"}),
			"{\n  \"name\": \"web01\",\n  \"cpu\": 4,\n  \"tags\": [\n    \"a\",\n    \"b\"\n  ],\n  \"disks\": [\n    { \"size_gb\": 80 },\n    { \"size_gb\": 200 }\n  ]\n}\n"},
		{"an unchanged number keeps its text", "{\"cpu\": 8.0, \"memory\": 4}\n", set(p{"memory"}, 16),
			"{\"cpu\": 8.0, \"memory\": 16}\n"},
		{"a minified file stays on one line", `{"a":1,"b":{"c":[1,2]}}`, set(p{"b", "d"}, map[string]any{"x": []any{true}}, p{"b", "c", 2}, 3),
			`{"a":1,"b":{"c":[1,2,3],"d":{"x":[true]}}}`},
		{"spacing after commas kept", `{"tags": ["a", "b"]}`, set(p{"tags", 2}, "c"),
			`{"tags": ["a", "b", "c"]}`},
		{"an item at the start", `{"tags": ["b", "c"]}`, set(p{"tags"}, []any{"a", "b", "c"}),
			`{"tags": ["a", "b", "c"]}`},
		{"an empty object filled", "{\n  \"tags\": {}\n}\n", set(p{"tags", "app"}, "shop"),
			"{\n  \"tags\": {\n    \"app\": \"shop\"\n  }\n}\n"},
		{"CRLF line endings", "{\r\n  \"a\": 1\r\n}\r\n", set(p{"b"}, 2),
			"{\r\n  \"a\": 1,\r\n  \"b\": 2\r\n}\r\n"},
		{"tab indentation", "{\n\t\"a\": {\n\t\t\"b\": 1\n\t}\n}\n", set(p{"a", "c"}, map[string]any{"d": 1}),
			"{\n\t\"a\": {\n\t\t\"b\": 1,\n\t\t\"c\": {\n\t\t\t\"d\": 1\n\t\t}\n\t}\n}\n"},
		{"a tfvars.json map of objects", "{\n  \"vms\": {\n    \"web01\": { \"cpu\": 4 },\n    \"db01\": { \"cpu\": 8 }\n  }\n}\n",
			set(p{"vms", "web01", "cpu"}, 6, p{"vms", "app01"}, map[string]any{"cpu": 2}),
			"{\n  \"vms\": {\n    \"web01\": { \"cpu\": 6 },\n    \"db01\": { \"cpu\": 8 },\n    \"app01\": { \"cpu\": 2 }\n  }\n}\n"},
		{"an empty file", "", set(p{}, map[string]any{"a": 1}), "{\n  \"a\": 1\n}\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := Decode([]byte(tt.src))
			if err != nil {
				t.Fatal(err)
			}
			out, err := Edit([]byte(tt.src), tt.edit(doc))
			if err != nil {
				t.Fatalf("Edit() error = %v", err)
			}
			if string(out) != tt.want {
				t.Errorf("Edit() =\n%s\nwant\n%s", out, tt.want)
			}
		})
	}
}

func TestEditUnchanged(t *testing.T) {
	src := []byte("{ \"a\" : 1.50 , \"b\" : [ ] }\n")
	doc, _ := Decode(src)
	out, err := Edit(src, doc)
	if err != nil || &out[0] != &src[0] {
		t.Errorf("an unchanged document: Edit() = %q, %v; want src itself", out, err)
	}
}

func TestRefusalsAndErrors(t *testing.T) {
	for _, tt := range []struct {
		name, src, fragment string
		refusal             bool
	}{
		{"a byte-order mark", "\xef\xbb\xbf{}", "byte-order mark", true},
		{"a duplicate key", `{"a": 1, "a": 2}`, `"a" appears twice`, true},
		{"not JSON", `{"a": 1,}`, "expected a key", false},
		{"two documents", `{} {}`, "after the document", false},
		{"invalid UTF-8", "{\"a\": \"\xff\"}", "UTF-8", false},
		{"a leading zero", `{"a": 01}`, "invalid number", false},
		{"too deep", strings.Repeat("[", maxDepth+1) + strings.Repeat("]", maxDepth+1), "levels deep", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Edit([]byte(tt.src), map[string]any{})
			var refusal *RefusalError
			if err == nil || !strings.Contains(err.Error(), tt.fragment) || errors.As(err, &refusal) != tt.refusal {
				t.Errorf("Edit() error = %v, want one containing %q (a refusal: %v)", err, tt.fragment, tt.refusal)
			}
		})
	}
	if _, err := Edit([]byte(`{}`), map[string]any{"x": func() {}}); err == nil {
		t.Error("an intent outside the JSON data model")
	}
}

func TestDecode(t *testing.T) {
	got, err := Decode([]byte(`{"i": -3, "u": 18446744073709551615, "f": 1.5, "e": 1e3, "s": "é\n", "n": null, "b": [true, {}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"i": int64(-3), "u": uint64(18446744073709551615), "f": 1.5, "e": float64(1000), "s": "é\n", "n": nil,
		"b": []any{true, map[string]any{}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Decode() = %#v, want %#v", got, want)
	}
	if got, err := Decode([]byte(" \n")); got != nil || err != nil {
		t.Errorf("whitespace decodes to %v, %v", got, err)
	}
}

func TestCreate(t *testing.T) {
	out, err := Create(map[string]any{"tags": map[string]any{"b": "2", "a": "1"}, "name": "web01", "cpu": 4, "list": []any{1, "x"}},
		[]string{"name", "cpu"})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"name\": \"web01\",\n  \"cpu\": 4,\n  \"list\": [\n    1,\n    \"x\"\n  ],\n  \"tags\": {\n    \"a\": \"1\",\n    \"b\": \"2\"\n  }\n}\n"
	if string(out) != want {
		t.Errorf("Create() =\n%s\nwant\n%s", out, want)
	}
}

func TestKeys(t *testing.T) {
	got, err := Keys([]byte(`{"b": 1, "a/b": {"z": 1, "y": 2}, "l": [{"k": 1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"": {"b", "a/b", "l"}, "/a~1b": {"z", "y"}, "/l/0": {"k"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Keys() = %v, want %v", got, want)
	}
}
