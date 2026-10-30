package jsonedit

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

// FuzzJSONEdit makes one random change to a random document: a new scalar, a removed or added
// entry or item, or a new object or array in place of a value. Edit may refuse, but whatever it
// returns must parse, decode to the intent, and be left alone by the same edit. That every byte
// outside the edited spans is unchanged is verified inside Edit.
func FuzzJSONEdit(f *testing.F) {
	inputs, _ := filepath.Glob(filepath.Join(corpusDir, "*", "input.json"))
	for _, name := range inputs {
		src, err := os.ReadFile(name)
		if err != nil {
			f.Fatal(err)
		}
		for op := range uint8(9) {
			f.Add(src, uint(3+op), op, "changed", int64(7))
		}
	}
	for _, src := range []string{
		`{"a":1,"b":["x","y"],"c":{"d":"e"}}`,
		"{\n  \"l\": [\n    1,\n    {\"k\": [ ]}\n  ],\n  \"m\": { }\n}\n",
		"[\r\n\t{ \"a\" : null },\r\n\ttrue\r\n]\r\n",
		`  "just a string"  `,
		"{\"e\": \"\\u00e9\\n\", \"n\": -0.5e-3, \"big\": 18446744073709551615}",
	} {
		for op := range uint8(9) {
			f.Add([]byte(src), uint(op), op, "a\"b\\c", int64(-1))
		}
	}
	f.Fuzz(func(t *testing.T, src []byte, target uint, op uint8, s string, n int64) {
		doc, err := Decode(src)
		if err != nil {
			return
		}
		want, ok := mutate(doc, target, op, s, n)
		if !ok {
			return
		}
		intent, err := document.Normalize(want)
		if err != nil {
			return
		}
		out, err := Edit(src, want)
		if err != nil {
			var refusal *RefusalError
			if !errors.As(err, &refusal) || strings.HasPrefix(refusal.Reason, internalError) {
				t.Fatalf("Edit() error = %v, want nil or a *RefusalError", err)
			}
			return
		}
		got, err := Decode(out)
		if err != nil {
			t.Fatalf("edited file does not decode: %v\n%s", err, out)
		}
		if !document.Equal(got, intent) {
			t.Fatalf("edited file decodes to %#v, want %#v\n%s", got, intent, out)
		}
		again, err := Edit(out, want)
		if err != nil || string(again) != string(out) {
			t.Fatalf("second Edit() changed the file again: err=%v\n%s", err, again)
		}
	})
}

// mutate returns doc with one change made to the node at paths[target], chosen by op.
func mutate(doc any, target uint, op uint8, s string, n int64) (any, bool) {
	paths := nodePaths(doc, document.Path{}, nil)
	at := paths[target%uint(len(paths))]
	switch op % 9 {
	case 0:
		return document.With(doc, at, s), true
	case 1:
		return document.With(doc, at, n), true
	case 2:
		return document.With(doc, at, n%2 == 0), true
	case 3:
		return document.With(doc, at, nil), true
	case 4:
		return document.With(doc, at, float64(n)/7), true
	case 5: // remove the entry or item
		if len(at) == 0 {
			return nil, false
		}
		return document.Without(doc, at), true
	case 6: // add an entry or insert an item
		switch v := document.ValueAt(doc, at).(type) {
		case map[string]any:
			return document.With(doc, at.Key(s), n), true
		case []any:
			return document.With(doc, at, slices.Insert(slices.Clone(v), int(uint64(n)%uint64(len(v)+1)), any(s))), true
		}
		return nil, false
	case 7:
		return document.With(doc, at, map[string]any{s: n, "k": []any{s}}), true
	default:
		return document.With(doc, at, []any{s, map[string]any{"k": n}}), true
	}
}

// nodePaths appends the paths of every node in doc, collections included, in a stable order.
func nodePaths(doc any, at document.Path, out []document.Path) []document.Path {
	out = append(out, slices.Clone(at))
	switch d := doc.(type) {
	case map[string]any:
		for _, k := range document.SortedKeys(d) {
			out = nodePaths(d[k], at.Key(k), out)
		}
	case []any:
		for i, item := range d {
			out = nodePaths(item, at.Index(i), out)
		}
	}
	return out
}
