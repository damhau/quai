package yamledit

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// corpusDir holds real-world files and the edits made to them; see corpus_test.go.
var corpusDir = filepath.Join("..", "..", "..", "..", "testdata", "corpus", "yaml")

func readCorpus(t *testing.T, name string) []byte {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(corpusDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func mustDecode(t testing.TB, src []byte) any {
	t.Helper()
	doc, err := decode(src)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return doc
}

// changedLines returns the lines that differ between two texts with the same number of lines.
func changedLines(t *testing.T, before, after string) (removed, added []string) {
	t.Helper()
	b, a := strings.Split(before, "\n"), strings.Split(after, "\n")
	if len(a) != len(b) {
		t.Fatalf("line count changed from %d to %d:\n%s", len(b), len(a), after)
	}
	for i := range b {
		if a[i] != b[i] {
			removed, added = append(removed, b[i]), append(added, a[i])
		}
	}
	slices.Sort(removed)
	slices.Sort(added)
	return removed, added
}

func mustEdit(t *testing.T, src []byte, want any) string {
	t.Helper()
	out, err := Edit(src, want)
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	intent, err := normalize(want, path{})
	if err != nil {
		t.Fatal(err)
	}
	if !equal(mustDecode(t, out), intent) {
		t.Fatalf("Edit result does not decode to the intent:\n%s", out)
	}
	again, err := Edit(out, want)
	if err != nil || string(again) != string(out) {
		t.Fatalf("Edit is not idempotent: err=%v\n%s", err, again)
	}
	return string(out)
}

// Ported from packages/core/test/resourcetype.test.ts: "patchYaml changes only the edited
// values, keeping comments and layout".
func TestEditChangesOnlyTheEditedValues(t *testing.T) {
	src := readCorpus(t, "prod-web01-resize/input.yaml")
	doc := mustDecode(t, src)
	doc = with(doc, path{"cpu"}, 8)
	doc = with(doc, path{"disks", 0, "size_gb"}, 120)
	doc = with(doc, path{"tags", "cost_center"}, "4712")

	out := mustEdit(t, src, doc)
	removed, added := changedLines(t, string(src), out)
	if want := []string{"  - size_gb: 80", `  cost_center: "4711"`, "cpu: 4          # bumped in INC-4411"}; !slices.Equal(removed, want) {
		t.Errorf("removed lines = %q, want %q", removed, want)
	}
	if want := []string{"  - size_gb: 120", `  cost_center: "4712"`, "cpu: 8          # bumped in INC-4411"}; !slices.Equal(added, want) {
		t.Errorf("added lines = %q, want %q", added, want)
	}
}

// Ported from packages/core/test/resourcetype.test.ts: "patchYaml edits inside flow collections
// and handles structural changes" (the scalar half; structural edits arrive with P0-03).
func TestEditInsideFlowCollections(t *testing.T) {
	src := readCorpus(t, "prod-db01-flow-ip/input.yaml")
	doc := mustDecode(t, src)
	out := mustEdit(t, src, with(doc, path{"network", "ip"}, "10.0.13.21"))
	if !strings.Contains(out, "network: { vlan: 130, ip: 10.0.13.21 }\n") {
		t.Errorf("flow mapping not edited in place:\n%s", out)
	}
	if !strings.Contains(out, "  - size_gb: 1000   # data volume\n") {
		t.Errorf("unrelated line changed:\n%s", out)
	}

	same, err := Edit(src, doc)
	if err != nil || string(same) != string(src) {
		t.Errorf("no-op edit changed the file: err=%v\n%s", err, same)
	}
}

func TestEditScalars(t *testing.T) {
	tests := []struct {
		name string
		src  string
		path path
		to   any
		want string
	}{
		{"plain string", "a: x\nb: y\n", path{"a"}, "z", "a: z\nb: y\n"},
		{"integer keeps trailing comment", "cpu: 4   # sized for peak\n", path{"cpu"}, 8, "cpu: 8   # sized for peak\n"},
		{"negative integer", "n: 1\n", path{"n"}, -3, "n: -3\n"},
		{"float", "f: 1.5\n", path{"f"}, 2.25, "f: 2.25\n"},
		{"integral float is an integer", "f: 1.5\n", path{"f"}, 8.0, "f: 8\n"},
		{"large float keeps a decimal point", "f: 1.5\n", path{"f"}, 1e21, "f: 1.0e+21\n"},
		{"small float", "f: 1.5\n", path{"f"}, 1.5e-7, "f: 1.5e-07\n"},
		{"json number", "n: 1\n", path{"n"}, json.Number("42"), "n: 42\n"},
		{"bool", "b: true\n", path{"b"}, false, "b: false\n"},
		{"to null", "v: x\n", path{"v"}, nil, "v: null\n"},
		{"from null", "v: ~\n", path{"v"}, "x", "v: x\n"},
		{"keeps double quotes", `s: "a"` + "\n", path{"s"}, "b", `s: "b"` + "\n"},
		{"keeps single quotes", "s: 'a'\n", path{"s"}, "it's", "s: 'it''s'\n"},
		{"single quotes fall back to double for control characters", "s: 'a'\n", path{"s"}, "a\tb", `s: "a\tb"` + "\n"},
		{"double quotes escape", `s: "a"` + "\n", path{"s"}, `say "hi"`, `s: "say \"hi\""` + "\n"},
		{"quoted string to number", `n: "4711"` + "\n", path{"n"}, 4712, "n: 4712\n"},
		{"number to numeric string is quoted", "n: 4711\n", path{"n"}, "4712", `n: "4712"` + "\n"},
		{"number to word stays plain", "n: 4711\n", path{"n"}, "many", "n: many\n"},
		{"quote style follows the file", "a: 'x'\nb: 'y'\nn: 1\n", path{"n"}, "2", "a: 'x'\nb: 'y'\nn: '2'\n"},
		{"YAML 1.1 boolean is quoted", "s: x\n", path{"s"}, "yes", `s: "yes"` + "\n"},
		{"null word is quoted", "s: x\n", path{"s"}, "null", `s: "null"` + "\n"},
		{"exponent is quoted", "s: x\n", path{"s"}, "1e3", `s: "1e3"` + "\n"},
		{"octal is quoted", "s: x\n", path{"s"}, "0755", `s: "0755"` + "\n"},
		{"sexagesimal is quoted", "s: x\n", path{"s"}, "12:30", `s: "12:30"` + "\n"},
		{"date is quoted", "s: x\n", path{"s"}, "2026-10-27", `s: "2026-10-27"` + "\n"},
		{"ip address stays plain", "ip: 10.0.12.11\n", path{"ip"}, "10.0.12.12", "ip: 10.0.12.12\n"},
		{"version stays plain", "v: ubuntu-24.04\n", path{"v"}, "ubuntu-26.04", "v: ubuntu-26.04\n"},
		{"colon space is quoted", "s: x\n", path{"s"}, "a: b", `s: "a: b"` + "\n"},
		{"comment marker is quoted", "s: x\n", path{"s"}, "a #b", `s: "a #b"` + "\n"},
		{"indicator is quoted", "s: x\n", path{"s"}, "*star", `s: "*star"` + "\n"},
		{"empty string is quoted", "s: x\n", path{"s"}, "", `s: ""` + "\n"},
		{"leading space is quoted", "s: x\n", path{"s"}, " x", `s: " x"` + "\n"},
		{"unicode before the value", "naïve: café # c\n", path{"naïve"}, "thé", "naïve: thé # c\n"},
		{"unicode value", "s: x\n", path{"s"}, "grüezi", "s: grüezi\n"},
		{"trailing spaces after the value", "a: 1   \nb: 2\n", path{"a"}, 5, "a: 5   \nb: 2\n"},
		{"trailing tab after the value", "a: x\t\nb: 2\n", path{"a"}, "y z", "a: y z\t\nb: 2\n"},
		{"trailing spaces after a sequence item", "l:\n  - value: 1    \n", path{"l", 0, "value"}, 2, "l:\n  - value: 2    \n"},
		{"trailing spaces before a comment", "a: 1   # one\n", path{"a"}, 5, "a: 5   # one\n"},
		{"crlf after a comment line", "a: 1\r\n# c\r\nb: 2\r\n", path{"b"}, 5, "a: 1\r\n# c\r\nb: 5\r\n"},
		{"crlf after line comments and blank lines", "a: \"x\" # c\r\n\r\n# d\r\nb: 2\r\nc: 3\r\n", path{"c"}, 4, "a: \"x\" # c\r\n\r\n# d\r\nb: 2\r\nc: 4\r\n"},
		{"mixed line endings", "a: 1 # c\r\nb: 2\nc: 3\r\n", path{"c"}, 4, "a: 1 # c\r\nb: 2\nc: 4\r\n"},
		{"crlf line endings", "a: 1\r\nb: x\r\n", path{"b"}, "z", "a: 1\r\nb: z\r\n"},
		{"single-letter YAML 1.1 boolean is quoted", "b: x\n", path{"b"}, "y", `b: "y"` + "\n"},
		{"flow sequence item", "l: [a, b, 3]\n", path{"l", 1}, "c", "l: [a, c, 3]\n"},
		{"flow item with a comma is quoted", "l: [a, b]\n", path{"l", 0}, "x,y", `l: ["x,y", b]` + "\n"},
		{"flow mapping value with a colon is quoted", "m: {a: x}\n", path{"m", "a"}, "b:c", `m: {a: "b:c"}` + "\n"},
		{"two values on one line", "m: { a: 1, b: 2 }\n", path{"m", "b"}, 3, "m: { a: 1, b: 3 }\n"},
		{"block sequence item", "l:\n  - a\n  - b # second\n", path{"l", 1}, "c", "l:\n  - a\n  - c # second\n"},
		{"nested in a sequence of mappings", "d:\n  - size: 1\n    tier: ssd\n", path{"d", 0, "tier"}, "hdd", "d:\n  - size: 1\n    tier: hdd\n"},
		{"root sequence", "- a\n- b\n", path{1}, "c", "- a\n- c\n"},
		{"quoted key", `"a b": 1` + "\n", path{"a b"}, 2, `"a b": 2` + "\n"},
		{"numeric key", "1: one\n", path{"1"}, "uno", "1: uno\n"},
		{"multi-line plain scalar", "s: one\n  two\nn: 1\n", path{"s"}, "three", "s: three\nn: 1\n"},
		{"empty value", "a:\nb: 1\n", path{"a"}, "x", "a: x\nb: 1\n"},
		{"empty value with a comment", "a: # todo\nb: 1\n", path{"a"}, 2, "a: 2 # todo\nb: 1\n"},
		{"empty value gets a quoted multi-line string", "a:\n", path{"a"}, "x\ny", `a: "x\ny"` + "\n"},
		{"empty sequence item", "l:\n  -\n  - b\n", path{"l", 0}, "a", "l:\n  - a\n  - b\n"},
		{"document header and comments", "---\n# top\na: 1 # one\n...\n", path{"a"}, 2, "---\n# top\na: 2 # one\n...\n"},
		{"multi-line string becomes a literal block", "d: x\nn: 1\n", path{"d"}, "one\ntwo\n", "d: |\n  one\n  two\nn: 1\n"},
		{"literal block without final line break", "d: x\n", path{"d"}, "one\ntwo", "d: |-\n  one\n  two\n"},
		{"literal block keeps blank lines", "d: x\n", path{"d"}, "one\n\ntwo\n", "d: |\n  one\n\n  two\n"},
		{"literal block follows the file's indentation", "m:\n    d: x\n", path{"m", "d"}, "a\nb\n", "m:\n    d: |\n        a\n        b\n"},
		{"literal block in a sequence", "l:\n  - x\n", path{"l", 0}, "a\nb\n", "l:\n  - |\n    a\n    b\n"},
		{"literal block with crlf", "d: x\r\nn: 1\r\n", path{"d"}, "a\nb\n", "d: |\r\n  a\r\n  b\r\nn: 1\r\n"},
		{"several final line breaks are double-quoted", "d: x\n", path{"d"}, "a\n\n", `d: "a\n\n"` + "\n"},
		{"leading space is double-quoted", "d: x\n", path{"d"}, " a\nb", `d: " a\nb"` + "\n"},
		{"multi-line string in a flow collection", "m: {d: x}\n", path{"m", "d"}, "a\nb", `m: {d: "a\nb"}` + "\n"},
		{"double-quoted multi-line stays quoted", `d: "x"` + "\n", path{"d"}, "a\nb", `d: "a\nb"` + "\n"},
		{"literal block is replaced", "d: |\n  one\n  two\nn: 1\n", path{"d"}, "three\n", "d: |\n  three\nn: 1\n"},
		{"literal block keeps its indentation", "d: |\n    one\nn: 1\n", path{"d"}, "a\nb\n", "d: |\n    a\n    b\nn: 1\n"},
		{"literal block keeps trailing blank lines", "d: |\n  one\n\nn: 1\n", path{"d"}, "two\n", "d: |\n  two\n\nn: 1\n"},
		{"strip chomping", "d: |-\n  one\nn: 1\n", path{"d"}, "two", "d: |-\n  two\nn: 1\n"},
		{"keep chomping is replaced", "d: |+\n  one\n\nn: 1\n", path{"d"}, "two\n", "d: |\n  two\n\nn: 1\n"},
		{"single-line folded block stays folded", "d: >-\n  {{ .Name }}_{{ .Os }}\nn: 1\n", path{"d"}, "{{ .Name }}-x", "d: >-\n  {{ .Name }}-x\nn: 1\n"},
		{"single-line folded block with clip", "d: >\n  one\n", path{"d"}, "two\n", "d: >\n  two\n"},
		{"multi-line folded block becomes literal", "d: >\n  one\n  two\nn: 1\n", path{"d"}, "three\nfour\n", "d: |\n  three\n  four\nn: 1\n"},
		{"literal block after a comment", "d: x # note\nn: 1\n", path{"d"}, "a\nb\n", "d: | # note\n  a\n  b\nn: 1\n"},
		{"literal block after a comment with crlf", "d: x # note\r\n", path{"d"}, "a\nb", "d: |- # note\r\n  a\r\n  b\r\n"},
		{"literal block to a single line", "d: |\n  one\n", path{"d"}, "two", "d: |-\n  two\n"},
		{"empty literal block", "d: |\nn: 1\n", path{"d"}, "x\n", "d: |\n  x\nn: 1\n"},
		{"literal block in a sequence item mapping", "l:\n  - d: |\n      one\n    n: 1\n", path{"l", 0, "d"}, "two\n", "l:\n  - d: |\n      two\n    n: 1\n"},
		// goccy decoded a quoted string folded over CRLF lines as "one\ntwo" instead of "one two".
		{"key that is not a string", "200: ok\n201: created\n", path{"200"}, "fine", "200: fine\n201: created\n"},
		{"date stays text", "d: 2001-12-14\nn: 1\n", path{"n"}, 2, "d: 2001-12-14\nn: 2\n"},
		{"quoted string folded over crlf lines", "a: \"one\r\n  two\"\r\nb: 2\r\n", path{"a"}, "three", "a: \"three\"\r\nb: 2\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := []byte(tt.src)
			got := mustEdit(t, src, with(mustDecode(t, src), tt.path, tt.to))
			if got != tt.want {
				t.Errorf("Edit() =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestEditRefuses(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		want   func(doc any) any
		path   string
		reason string
	}{
		{"byte-order mark", "\xef\xbb\xbfa: 1\n", func(any) any { return map[string]any{"a": 2} }, "", "byte-order mark"},
		{"invalid YAML", "a: [1\n", func(any) any { return map[string]any{"a": 2} }, "", "not valid YAML"},
		{"duplicate key", "a: 1\na: 2\n", func(any) any { return map[string]any{"a": 3} }, "", "not valid YAML"},
		{"several documents", "a: 1\n---\nb: 2\n", func(any) any { return map[string]any{"a": 2} }, "", "more than one document"},
		{"infinity", "a: .inf\nb: 1\n", func(any) any { return map[string]any{"a": 1, "b": 2} }, "", "outside the JSON data model"},
		{"anchored value", "a: &x 1\nb: *x\n", func(d any) any { return with(d, path{"a"}, 2) }, "$.a", "anchor"},
		{"alias", "a: &x 1\nb: *x\n", func(d any) any { return with(d, path{"b"}, 2) }, "$.b", "alias"},
		{"inside an anchored mapping", "base: &b\n  x: 1\nuse: *b\n", func(d any) any { return with(d, path{"base", "x"}, 2) }, "$.base", "anchor"},
		{"merge key", "base: &b {x: 1}\nm:\n  <<: *b\n  y: 2\n", func(d any) any { return with(d, path{"m", "y"}, 3) }, "$.m", "merge key"},
		{"tagged value", "a: !!str 12\n", func(d any) any { return with(d, path{"a"}, "13") }, "$.a", "explicit tag"},
		{"indentation indicator", "d: |2\n   x\n", func(d any) any { return with(d, path{"d"}, "y\n") }, "$.d", "indentation indicator"},
		{"comment after a block header", "d: | # note\n  x\n", func(d any) any { return with(d, path{"d"}, "y\n") }, "$.d", "comment follows"},
		{"carriage returns alone", "a: 1\rb: 2\r", func(any) any { return map[string]any{"a": 2} }, "", "line breaks other than"},
		{"next-line character", "a: x\u0085y\n", func(any) any { return map[string]any{"a": 2} }, "", "line breaks other than"},
		// Found by fuzzing: \v is not a printable character, which YAML does not allow.
		{"control character", "-\t\v-", func(any) any { return []any{[]any{"0"}} }, "", "not valid YAML"},
		// yaml/v3 drops a non-specific tag on an empty value from the tree.
		{"empty value with a non-specific tag", "a: !\nb: 1\n", func(d any) any { return with(d, path{"a"}, 1) }, "$.a", "positions do not match"},
		{"empty value in a flow mapping", "m: {a: }\n", func(d any) any { return with(d, path{"m", "a"}, 1) }, "$.m.a", "flow collection"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var doc any
			if d, err := decode([]byte(tt.src)); err == nil {
				doc = d
			}
			_, err := Edit([]byte(tt.src), tt.want(doc))
			var refusal *RefusalError
			if !errors.As(err, &refusal) {
				t.Fatalf("Edit() error = %v, want a *RefusalError", err)
			}
			if refusal.Path != tt.path || !strings.Contains(refusal.Reason, tt.reason) {
				t.Errorf("Edit() refused at %q because %q, want %q and a reason containing %q", refusal.Path, refusal.Reason, tt.path, tt.reason)
			}
		})
	}
}

// TestDecode covers what the JSON data model makes of YAML: keys are text, timestamps stay
// strings, merge keys merge as in yaml/v3, and documents that do not fit are rejected.
func TestDecode(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want any
		err  string
	}{
		{"empty", "", nil, ""},
		{"comments only", "# only\n", nil, ""},
		{"empty document", "---\n", nil, ""},
		{"timestamp", "t: 2001-12-14 21:59:43.10 -5\n", map[string]any{"t": "2001-12-14 21:59:43.10 -5"}, ""},
		{"keys are text", "200: a\ntrue: b\n1.50: c\nnull: d\n", map[string]any{"200": "a", "true": "b", "1.50": "c", "null": "d"}, ""},
		{"alias as a key", "a: &k key\n*k : v\n", map[string]any{"a": "key", "key": "v"}, ""},
		{"yaml 1.2 float", "f: 1e3\n", map[string]any{"f": 1000.0}, ""},
		{"merge keeps explicit entries", "b: &b {x: 1, y: 1}\nm:\n  y: 2\n  <<: *b\n", map[string]any{"b": map[string]any{"x": 1, "y": 1}, "m": map[string]any{"x": 1, "y": 2}}, ""},
		{"merge list, earlier first", "a: &a {x: 1}\nb: &b {x: 2, z: 2}\nm:\n  <<: [*a, *b]\n", map[string]any{"a": map[string]any{"x": 1}, "b": map[string]any{"x": 2, "z": 2}, "m": map[string]any{"x": 1, "z": 2}}, ""},
		{"merge of a list", "a: &a [1]\nm:\n  <<: *a\n", nil, "not a mapping"},
		{"duplicate key", "a: 1\na: 2\n", nil, "defined twice"},
		{"duplicate key text", "1: a\n\"1\": b\n", nil, "defined twice"},
		{"key that is not a scalar", "? [a, b]\n: v\n", nil, "not a scalar"},
		{"anchor that contains itself", "a: &x [*x]\n", nil, "contains itself"},
		{"not a number", "n: .nan\n", nil, "outside the JSON data model"},
		{"invalid", "a: [1\n", nil, "not valid YAML"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decode([]byte(tt.src))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("decode() error = %v, want one containing %q", err, tt.err)
				}
				return
			}
			want, _ := normalize(tt.want, nil)
			if err != nil || !equal(got, want) {
				t.Errorf("decode() = %#v, %v, want %#v", got, err, want)
			}
		})
	}
}

func TestDecodeBoundsAliasExpansion(t *testing.T) {
	var b strings.Builder
	b.WriteString("a0: &a0 [x, x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i < 8; i++ {
		fmt.Fprintf(&b, "a%d: &a%d [", i, i)
		for j := range 10 {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "*a%d", i-1)
		}
		b.WriteString("]\n")
	}
	if _, err := decode([]byte(b.String())); err == nil || !strings.Contains(err.Error(), "too many values") {
		t.Fatalf("decode() error = %v, want too many values", err)
	}
}

func TestEditRejectsInvalidIntent(t *testing.T) {
	for name, want := range map[string]any{
		"unsupported type":  map[string]any{"a": struct{}{}},
		"NaN":               map[string]any{"a": math.NaN()},
		"infinity":          []any{math.Inf(1)},
		"invalid UTF-8":     map[string]any{"a": "\xff"},
		"invalid UTF-8 key": map[string]any{"\xff": 1},
		"invalid number":    map[string]any{"a": json.Number("x")},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Edit([]byte("a: 1\n"), want)
			var refusal *RefusalError
			if err == nil || errors.As(err, &refusal) {
				t.Errorf("Edit() error = %v, want an invalid-intent error", err)
			}
		})
	}
}

func TestEditUnchangedReturnsTheOriginal(t *testing.T) {
	src := []byte("# keep me\na: 1   # and me\nb: [x, 'y']\n")
	want := map[string]any{"a": 1.0, "b": []any{"x", "y"}}
	out, err := Edit(src, want)
	if err != nil || string(out) != string(src) {
		t.Errorf("Edit() = %q, %v; want the original", out, err)
	}
}

func TestPathString(t *testing.T) {
	for want, p := range map[string]path{
		"$":                   {},
		"$.disks[0].size_gb":  {"disks", 0, "size_gb"},
		`$["dotted.key"][2]`:  {"dotted.key", 2},
		`$.a["with space"].b`: {"a", "with space", "b"},
	} {
		if got := p.String(); got != want {
			t.Errorf("path %#v String() = %q, want %q", p, got, want)
		}
	}
}

func TestOutsideUnchanged(t *testing.T) {
	src := []byte("a: 1\nb: 2\n")
	edits := []textEdit{{start: 3, end: 4, text: "10"}, {start: 8, end: 9, text: "3"}}
	if !outsideUnchanged(src, []byte("a: 10\nb: 3\n"), edits) {
		t.Error("a correct splice was reported as changing outside bytes")
	}
	for _, out := range []string{"a: 10\nc: 3\n", "a: 10\nb: 3\n\n", "a: 10\nb: 3", "a: 1\nb: 2\n"} {
		if outsideUnchanged(src, []byte(out), edits) {
			t.Errorf("outsideUnchanged(%q) = true, want false", out)
		}
	}
	if _, err := apply(src, []textEdit{{start: 0, end: 4, text: "x"}, {start: 3, end: 5, text: "y"}}); err == nil {
		t.Error("apply accepted overlapping edits")
	}
}

func TestEditMoreScalars(t *testing.T) {
	tests := []struct {
		name string
		src  string
		path path
		to   any
		want string
	}{
		{"escaped double quote", `s: "a\"b" # c` + "\n", path{"s"}, "x", `s: "x" # c` + "\n"},
		{"escaped single quote", "s: 'it''s' # c\n", path{"s"}, "x", "s: 'x' # c\n"},
		{"last line without a line break", "a: 1\nb: x", path{"b"}, "z", "a: 1\nb: z"},
		{"literal block on the last line", "a: 1\r\nb: x", path{"b"}, "p\nq", "a: 1\r\nb: |-\r\n  p\r\n  q"},
		{"go integer types", "n: 1\n", path{"n"}, uint8(9), "n: 9\n"},
		{"float32", "n: 1\n", path{"n"}, float32(0.5), "n: 0.5\n"},
		{"json number beyond int64", "n: 1\n", path{"n"}, json.Number("18446744073709551615"), "n: 18446744073709551615\n"},
		{"json number float", "n: 1\n", path{"n"}, json.Number("2.5"), "n: 2.5\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := []byte(tt.src)
			got := mustEdit(t, src, with(mustDecode(t, src), tt.path, tt.to))
			if got != tt.want {
				t.Errorf("Edit() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestNormalizeAndEqual(t *testing.T) {
	got, err := normalize(map[string]any{"a": []any{int(1), int8(2), int16(3), int32(4), uint(5), uint16(6), uint32(7)}}, path{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": []any{int64(1), int64(2), int64(3), int64(4), uint64(5), uint64(6), uint64(7)}}
	if !equal(got, want) {
		t.Errorf("normalize() = %#v, want %#v", got, want)
	}
	for _, tt := range []struct {
		a, b any
		want bool
	}{
		{int64(8), 8.0, true},
		{uint64(8), int64(8), true},
		{int64(8), "8", false},
		{"8", int64(8), false},
		{nil, false, false},
		{true, true, true},
		{[]any{int64(1)}, []any{int64(1), int64(2)}, false},
		{map[string]any{"a": nil}, map[string]any{"b": nil}, false},
		{map[string]any{"a": nil}, []any{nil}, false},
		{struct{}{}, struct{}{}, false},
	} {
		if got := equal(tt.a, tt.b); got != tt.want {
			t.Errorf("equal(%#v, %#v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestRefusalErrorMessage(t *testing.T) {
	if got := refuse(nil, "no %s", "way").Error(); got != "yamledit: refused: no way" {
		t.Errorf("file refusal = %q", got)
	}
	if got := refuse(path{"a", 0}, "no").Error(); got != "yamledit: refused at $.a[0]: no" {
		t.Errorf("path refusal = %q", got)
	}
}

func TestVerifyCatchesBadEdits(t *testing.T) {
	src := []byte("a: 1\nb: 2\n")
	want := map[string]any{"a": int64(3), "b": int64(2)}
	good := []textEdit{{start: 3, end: 4, text: "3"}}
	if err := verify(src, []byte("a: 3\nb: 2\n"), good, want); err != nil {
		t.Fatalf("verify() rejected a good edit: %v", err)
	}
	for name, out := range map[string]string{
		"does not parse":         "a: [3\nb: 2\n",
		"more than one document": "a: 3\n---\nb: 2\n",
		"cannot be decoded":      "a: .nan\nb: 2\n",
		"does not decode":        "a: 4\nb: 2\n",
		"bytes outside":          "a: 3\nb: 2 \n",
	} {
		t.Run(name, func(t *testing.T) {
			err := verify(src, []byte(out), good, want)
			if err == nil || !strings.Contains(err.Error(), "verification failed") {
				t.Errorf("verify() = %v, want a verification failure", err)
			}
		})
	}
}
