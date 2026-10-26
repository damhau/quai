package yamledit

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// FuzzYAMLEdit changes one scalar of a random document to a random value. Edit may refuse, but
// whatever it returns must parse, decode to the intent, and be left alone by the same edit.
func FuzzYAMLEdit(f *testing.F) {
	inputs, _ := filepath.Glob(filepath.Join(corpusDir, "*", "input.yaml"))
	for _, name := range inputs {
		src, err := os.ReadFile(name)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(src, uint(3), uint8(0), "changed", int64(7))
	}
	for _, src := range []string{
		"a: 1\nb: [x, 'y', \"z\"]\nc: {d: e}\n",
		"l:\n  - |\n    text\n  - plain\n    folded\n",
		"k: # empty\nm: { a: 1, b: 2 }\n",
		"x: 'it''s'\r\ny: \"esc\\n\"\r\n",
	} {
		f.Add([]byte(src), uint(1), uint8(0), "a: b\nc", int64(-1))
	}
	f.Fuzz(func(t *testing.T, src []byte, leaf uint, kind uint8, s string, n int64) {
		doc, err := decode(src)
		if err != nil {
			return
		}
		leaves := scalarPaths(doc, path{}, nil)
		if len(leaves) == 0 {
			return
		}
		var v any
		switch kind % 5 {
		case 0:
			v = s
		case 1:
			v = n
		case 2:
			v = n%2 == 0
		case 3:
			v = nil
		case 4:
			v = float64(n) / 7
		}
		if _, err := normalize(v, nil); err != nil {
			return
		}
		want := with(doc, leaves[leaf%uint(len(leaves))], v)

		out, err := Edit(src, want)
		if err != nil {
			var refusal *RefusalError
			if !errors.As(err, &refusal) {
				t.Fatalf("Edit() error = %v, want nil or a *RefusalError", err)
			}
			return
		}
		got, err := decode(out)
		if err != nil {
			t.Fatalf("edited file does not decode: %v\n%s", err, out)
		}
		intent, _ := normalize(want, nil)
		if !equal(got, intent) {
			t.Fatalf("edited file decodes to %#v, want %#v\n%s", got, intent, out)
		}
		again, err := Edit(out, want)
		if err != nil || string(again) != string(out) {
			t.Fatalf("second Edit() changed the file again: err=%v\n%s", err, again)
		}
	})
}

// scalarPaths appends the paths of every scalar in doc, in a stable order.
func scalarPaths(doc any, p path, out []path) []path {
	switch d := doc.(type) {
	case map[string]any:
		keys := sortedKeys(d)
		for _, k := range keys {
			out = scalarPaths(d[k], p.key(k), out)
		}
	case []any:
		for i, item := range d {
			out = scalarPaths(item, p.index(i), out)
		}
	default:
		out = append(out, slices.Clone(p))
	}
	return out
}
