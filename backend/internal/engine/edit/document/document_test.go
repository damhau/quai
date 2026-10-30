package document

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeAndEqual(t *testing.T) {
	got, err := Normalize(map[string]any{"i": 8, "f": float32(1.5), "n": json.Number("18446744073709551615"), "l": []any{uint8(1), "x"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"i": int64(8), "f": 1.5, "n": uint64(18446744073709551615), "l": []any{uint64(1), "x"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Normalize() = %#v, want %#v", got, want)
	}
	if !Equal(int64(8), 8.0) || Equal(int64(8), "8") || Equal(map[string]any{"a": nil}, map[string]any{}) {
		t.Error("Equal compares numbers by value and nothing else across types")
	}
	for _, bad := range []any{math.NaN(), struct{}{}, map[string]any{"a": []any{"\xff"}}} {
		if _, err := Normalize(bad); err == nil {
			t.Errorf("Normalize(%#v) accepted", bad)
		}
	}
	if _, err := Normalize(map[string]any{"a": []any{"\xff"}}); err == nil || !strings.Contains(err.Error(), "$.a[0]") {
		t.Errorf("the error names the path: %v", err)
	}
}

func TestDiff(t *testing.T) {
	from := map[string]any{"cpu": int64(4), "tags": map[string]any{"a": "1"}, "disks": []any{int64(1), int64(2), int64(3)}, "old": true}
	to := map[string]any{"cpu": int64(8), "tags": map[string]any{"a": "1", "b": "2"}, "disks": []any{int64(1), int64(3)}, "net": "x"}
	var got []string
	for _, c := range Diff(from, to) {
		got = append(got, c.Path.String()+" "+[]string{"set", "replace", "add", "remove", "insert", "remove-item"}[c.Kind])
	}
	want := []string{"$.old remove", "$.cpu set", "$.disks[1] remove-item", "$.net add", "$.tags.b add"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff() = %v, want %v", got, want)
	}
	if c := Diff(map[string]any{"a": int64(1)}, map[string]any{"b": int64(1)}); len(c) != 1 || c[0].Kind != ReplaceNode {
		t.Errorf("a mapping sharing no key is replaced: %+v", c)
	}
	if c := Diff([]any{"a", "b"}, []any{"a", "x", "b"}); len(c) != 1 || c[0].Kind != InsertItem || c[0].After != 0 {
		t.Errorf("an item inserted after the first: %+v", c)
	}
}

func TestPathAndCopies(t *testing.T) {
	if s := (Path{"disks", 0, "a.b"}).String(); s != `$.disks[0]["a.b"]` {
		t.Errorf("String() = %s", s)
	}
	doc := map[string]any{"l": []any{"a"}, "m": map[string]any{"k": "v"}}
	with := With(doc, Path{"l", 1}, "b")
	without := Without(with, Path{"m", "k"})
	if !reflect.DeepEqual(without, map[string]any{"l": []any{"a", "b"}, "m": map[string]any{}}) {
		t.Errorf("With and Without = %#v", without)
	}
	if !reflect.DeepEqual(doc, map[string]any{"l": []any{"a"}, "m": map[string]any{"k": "v"}}) {
		t.Errorf("the original changed: %#v", doc)
	}
	if v := ValueAt(with, Path{"l", 1}); v != "b" {
		t.Errorf("ValueAt() = %v", v)
	}
}
