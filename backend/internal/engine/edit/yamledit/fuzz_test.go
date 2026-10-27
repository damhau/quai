package yamledit

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// FuzzYAMLEdit makes one random change to a random document: a new scalar, a removed or added
// entry or item, or a new mapping or list in place of a value. Edit may refuse, but whatever it
// returns must parse, decode to the intent, and be left alone by the same edit. That every byte
// outside the edited spans is unchanged is verified inside Edit.
func FuzzYAMLEdit(f *testing.F) {
	inputs, _ := filepath.Glob(filepath.Join(corpusDir, "*", "input.yaml"))
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
		"a: 1\nb: [x, 'y', \"z\"]\nc: {d: e}\n",
		"l:\n  - |\n    text\n  - plain\n    folded\n",
		"k: # empty\nm: { a: 1, b: 2 }\n",
		"x: 'it''s'\r\ny: \"esc\\n\"\r\n",
		"# header\nd:\n- a: 1\n  b: 2\n- c\n",
		"m:\n    a: 1\n    # comment\n    b: [1, 2]\nn: {}\n",
	} {
		for op := range uint8(9) {
			f.Add([]byte(src), uint(op), op, "a: b\nc", int64(-1))
		}
	}
	f.Fuzz(func(t *testing.T, src []byte, target uint, op uint8, s string, n int64) {
		doc, err := decode(src)
		if err != nil {
			return
		}
		want, ok := mutate(doc, target, op, s, n)
		if !ok {
			return
		}
		if _, err := normalize(want, nil); err != nil {
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

// mutate returns doc with one change made to the node at paths[target], chosen by op.
func mutate(doc any, target uint, op uint8, s string, n int64) (any, bool) {
	paths := nodePaths(doc, path{}, nil)
	p := paths[target%uint(len(paths))]
	switch op % 9 {
	case 0:
		return with(doc, p, s), true
	case 1:
		return with(doc, p, n), true
	case 2:
		return with(doc, p, n%2 == 0), true
	case 3:
		return with(doc, p, nil), true
	case 4:
		return with(doc, p, float64(n)/7), true
	case 5: // remove the entry or item
		if len(p) == 0 {
			return nil, false
		}
		return without(doc, p), true
	case 6: // add an entry or insert an item
		switch v := valueAt(doc, p).(type) {
		case map[string]any:
			return with(doc, p.key(s), n), true
		case []any:
			return with(doc, p, slices.Insert(slices.Clone(v), int(uint64(n)%uint64(len(v)+1)), any(s))), true
		}
		return nil, false
	case 7:
		return with(doc, p, map[string]any{s: n, "k": []any{s}}), true
	default:
		return with(doc, p, []any{s, map[string]any{"k": n}}), true
	}
}

// nodePaths appends the paths of every node in doc, collections included, in a stable order.
func nodePaths(doc any, p path, out []path) []path {
	out = append(out, slices.Clone(p))
	switch d := doc.(type) {
	case map[string]any:
		for _, k := range sortedKeys(d) {
			out = nodePaths(d[k], p.key(k), out)
		}
	case []any:
		for i, item := range d {
			out = nodePaths(item, p.index(i), out)
		}
	}
	return out
}
