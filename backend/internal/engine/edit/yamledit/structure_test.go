package yamledit

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// Ported from packages/core/test/resourcetype.test.ts: "patchYaml edits inside flow collections
// and handles structural changes" (the structural half).
func TestEditStructuralChangesKeepComments(t *testing.T) {
	src := readCorpus(t, "prod-db01-flow-ip/input.yaml")
	doc := mustDecode(t, src).(map[string]any)
	delete(doc, "backup_policy")
	doc["backup"] = false
	doc["tags"] = map[string]any{"app": "orders"}

	out := mustEdit(t, src, doc)
	if !strings.Contains(out, "# data volume") {
		t.Errorf("comments did not survive the structural edit:\n%s", out)
	}
	want := strings.Replace(string(src), "backup: true\nbackup_policy: hourly\n", "backup: false\ntags:\n  app: orders\n", 1)
	if out != want {
		t.Errorf("Edit() =\n%s\nwant\n%s", out, want)
	}
}

func TestEditStructure(t *testing.T) {
	m := func(kv ...any) map[string]any {
		out := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	l := func(items ...any) []any { return append([]any{}, items...) }
	tests := []struct {
		name string
		src  string
		edit func(doc any) any
		want string
	}{
		// Keys added to block mappings.
		{"add key", "a: 1\nb: 2\n", set(path{"c"}, 3), "a: 1\nb: 2\nc: 3\n"},
		{"add key at the end of a file without a final line break", "a: 1", set(path{"b"}, 2), "a: 1\nb: 2"},
		{"add key after trailing spaces", "env:\n  - GO111MODULE=on \n  \nscript:\n  - x\n", set(path{"env", 1}, "CGO_ENABLED=0"), "env:\n  - GO111MODULE=on \n  - CGO_ENABLED=0\n  \nscript:\n  - x\n"},
		{"remove entry with trailing spaces", "a: 1   \nb: 2\n", del(path{"a"}), "b: 2\n"},
		{"add key after a trailing comment", "a: 1 # one\n", set(path{"b"}, 2), "a: 1 # one\nb: 2\n"},
		{"add keys in sorted order", "a: 1\n", set(path{"z"}, 3, path{"m"}, 2), "a: 1\nm: 2\nz: 3\n"},
		{"add key to a nested mapping", "tags:\n  app: shop\nnext: 1\n", set(path{"tags", "env"}, "prod"), "tags:\n  app: shop\n  env: prod\nnext: 1\n"},
		{"add nested value into the nearest mapping", "network:\n  vlan: 120\n", set(path{"network", "dns"}, m("primary", "10.0.0.1")), "network:\n  vlan: 120\n  dns:\n    primary: 10.0.0.1\n"},
		{"add key to the inner and the outer mapping", "a: 1\nm:\n  x: 1\n", set(path{"m", "w"}, 2, path{"z"}, 3), "a: 1\nm:\n  x: 1\n  w: 2\nz: 3\n"},
		{"add list with the file's sequence indentation", "disks:\n  - a\n", set(path{"tags"}, l("x", "w")), "disks:\n  - a\ntags:\n  - x\n  - w\n"},
		{"add list in an indentless file", "disks:\n- a\n", set(path{"tags"}, l("x")), "disks:\n- a\ntags:\n- x\n"},
		{"add mapping with four-space indentation", "m:\n    a: 1\n", set(path{"p"}, m("b", 2)), "m:\n    a: 1\np:\n    b: 2\n"},
		{"add list of mappings", "a: 1\n", set(path{"d"}, l(m("size", 1, "tier", "ssd"))), "a: 1\nd:\n  - size: 1\n    tier: ssd\n"},
		{"add list of lists", "a: 1\n", set(path{"d"}, l(l(1, 2))), "a: 1\nd:\n  - - 1\n    - 2\n"},
		{"add key to a mapping in a sequence item", "d:\n  - size: 1\n    tier: ssd\n", set(path{"d", 0, "iops"}, 300), "d:\n  - size: 1\n    tier: ssd\n    iops: 300\n"},
		{"add key to a crlf file with comments", "# head\r\na: 1 # one\r\n\r\n# about b\r\nb: 2\r\n", set(path{"c"}, 3), "# head\r\na: 1 # one\r\n\r\n# about b\r\nb: 2\r\nc: 3\r\n"},
		{"remove key from a crlf file with comments", "a: 1 # one\r\n# about b\r\nb: 2\r\nc: 3\r\n", del(path{"b"}), "a: 1 # one\r\nc: 3\r\n"},
		{"replace a value in a crlf file with comments", "a: 1 # one\r\nb: 2\r\n", set(path{"b"}, m("x", 1)), "a: 1 # one\r\nb:\r\n  x: 1\r\n"},
		{"add key with crlf", "a: 1\r\nb: 2\r\n", set(path{"c"}, m("x", 1)), "a: 1\r\nb: 2\r\nc:\r\n  x: 1\r\n"},
		{"add key that needs quoting", "a: 1\n", set(path{"yes"}, 1), "a: 1\n\"yes\": 1\n"},
		{"add key in the file's quote style", "a: 'x'\n", set(path{"on"}, "1"), "a: 'x'\n'on': '1'\n"},
		{"add multi-line string", "a: 1\n", set(path{"d"}, "x\ny\n"), "a: 1\nd: |\n  x\n  y\n"},
		{"add empty collections", "a: 1\n", set(path{"m"}, m(), path{"l"}, l()), "a: 1\nl: []\nm: {}\n"},
		{"add null", "a: 1\n", set(path{"p"}, nil), "a: 1\np: null\n"},
		{"add key that YAML 1.1 reads as a boolean", "a: 1\n", set(path{"n"}, 1), "a: 1\n\"n\": 1\n"},

		// Keys added to flow mappings.
		{"add key to a flow mapping", "network: { vlan: 130, ip: 10.0.13.20 }\n", set(path{"network", "gw"}, "10.0.13.1"), "network: { vlan: 130, ip: 10.0.13.20, gw: 10.0.13.1 }\n"},
		{"add key to an empty flow mapping", "m: {}\n", set(path{"m", "a"}, 1, path{"m", "b"}, l(1)), "m: {a: 1, b: [1]}\n"},
		{"add nested mapping to a flow mapping", "m: {a: 1}\n", set(path{"m", "b"}, m("c", "x,y")), "m: {a: 1, b: {c: \"x,y\"}}\n"},

		// Keys removed.
		{"remove key", "a: 1\nb: 2\nc: 3\n", del(path{"b"}), "a: 1\nc: 3\n"},
		{"remove key with its comment", "a: 1\n# about b\nb: 2 # two\nc: 3\n", del(path{"b"}), "a: 1\nc: 3\n"},
		{"remove key keeps a comment separated by a blank line", "a: 1\n# about the rest\n\nb: 2\n", del(path{"b"}), "a: 1\n# about the rest\n\n"},
		{"remove first key keeps the header comment", "# header\na: 1\nb: 2\n", del(path{"a"}), "# header\nb: 2\n"},
		{"remove key with a nested mapping", "a:\n  x: 1\n  # inner\n  y: 2\nb: 2\n", del(path{"a"}), "b: 2\n"},
		{"remove last key", "a: 1\nb: 2\n", del(path{"b"}), "a: 1\n"},
		{"remove last key without a final line break", "a: 1\nb: 2", del(path{"b"}), "a: 1\n"},
		{"remove key with a literal block", "d: |\n  x\n  y\nn: 1\n", del(path{"d"}), "n: 1\n"},
		{"remove key with an empty value", "a:\nb: 1\n", del(path{"a"}), "b: 1\n"},
		{"remove key holding an alias", "a: &x 1\nb: *x\nc: 2\n", del(path{"b"}), "a: &x 1\nc: 2\n"},
		{"remove several keys", "a: 1\nb: 2\nc: 3\nd: 4\n", del(path{"b"}, path{"c"}), "a: 1\nd: 4\n"},
		{"remove first key of a sequence item", "l:\n  - a: 1\n    b: 2\n", del(path{"l", 0, "a"}), "l:\n  - b: 2\n"},
		{"remove first keys of a sequence item", "l:\n  - a: 1\n    b: 2\n    c: 3\n", del(path{"l", 0, "a"}, path{"l", 0, "b"}), "l:\n  - c: 3\n"},
		{"remove inner key of a sequence item", "l:\n  - a: 1\n    b: 2\n", del(path{"l", 0, "b"}), "l:\n  - a: 1\n"},
		{"remove key and add another", "a: 1\nb: 2\n", func(d any) any { return set(path{"c"}, 3)(del(path{"b"})(d)) }, "a: 1\nc: 3\n"},
		{"remove from a flow mapping", "m: {a: 1, b: 2, c: 3}\n", del(path{"m", "b"}), "m: {a: 1, c: 3}\n"},
		{"remove first from a flow mapping", "m: {a: 1, b: 2, c: 3}\n", del(path{"m", "a"}), "m: {b: 2, c: 3}\n"},
		{"remove last from a flow mapping", "m: { a: 1, b: 2 }\n", del(path{"m", "b"}), "m: { a: 1 }\n"},
		{"remove leading run from a flow mapping", "m: {a: 1, b: 2, c: 3}\n", del(path{"m", "a"}, path{"m", "b"}), "m: {c: 3}\n"},
		{"remove every key", "tags:\n  a: 1\nn: 1\n", set(path{"tags"}, m()), "tags: {}\nn: 1\n"},
		{"remove every key of a flow mapping", "m: {a: 1}\n", set(path{"m"}, m()), "m: {}\n"},

		// List items.
		{"append item", "l:\n  - a\n  - b\n", set(path{"l"}, l("a", "b", "c")), "l:\n  - a\n  - b\n  - c\n"},
		{"append items in an indentless list", "l:\n- a\nn: 1\n", set(path{"l"}, l("a", "b", "c")), "l:\n- a\n- b\n- c\nn: 1\n"},
		{"append mapping item", "d:\n  - size: 1\n    tier: ssd\n", set(path{"d"}, l(m("size", 1, "tier", "ssd"), m("size", 2, "tier", "hdd"))), "d:\n  - size: 1\n    tier: ssd\n  - size: 2\n    tier: hdd\n"},
		{"append item with the file's item offset", "d:\n  -   a: 1\n", set(path{"d"}, l(m("a", 1), m("a", 2))), "d:\n  -   a: 1\n  -   a: 2\n"},
		{"insert item in the middle", "l:\n  - a\n  - c\n", set(path{"l"}, l("a", "b", "c")), "l:\n  - a\n  - b\n  - c\n"},
		{"insert item at the start", "l:\n  - a\n  - c\n", set(path{"l"}, l("x", "a", "c")), "l:\n  - x\n  - a\n  - c\n"},
		{"change and append", "l:\n  - a\n  - b\n", set(path{"l"}, l("a", "B", "c")), "l:\n  - a\n  - B\n  - c\n"},
		{"remove item", "l:\n  - a\n  - b\n  - c\n", set(path{"l"}, l("a", "c")), "l:\n  - a\n  - c\n"},
		{"remove last item", "l:\n  - a\n  - b\n  - c\n", set(path{"l"}, l("a", "b")), "l:\n  - a\n  - b\n"},
		{"remove first item keeps the comment above the list", "l:\n  # items\n  - a\n  - b\n", set(path{"l"}, l("b")), "l:\n  # items\n  - b\n"},
		{"remove item with its comment", "l:\n  - a\n  # b is old\n  - b\n", set(path{"l"}, l("a")), "l:\n  - a\n"},
		{"remove mapping item", "d:\n  - size: 1\n    tier: ssd\n  - size: 2\n    tier: hdd\n", set(path{"d"}, l(m("size", 2, "tier", "hdd"))), "d:\n  - size: 2\n    tier: hdd\n"},
		{"remove every item", "l:\n  - a\nn: 1\n", set(path{"l"}, l()), "l: []\nn: 1\n"},
		{"append to a flow list", "l: [a, b]\n", set(path{"l"}, l("a", "b", "c")), "l: [a, b, c]\n"},
		{"append to an empty flow list", "l: []\n", set(path{"l"}, l("a", "b")), "l: [a, b]\n"},
		{"insert at the start of a flow list", "l: [b]\n", set(path{"l"}, l("a", "b")), "l: [a, b]\n"},
		{"remove from a flow list", "l: [a, b, c]\n", set(path{"l"}, l("a", "c")), "l: [a, c]\n"},
		{"remove first from a flow list", "l: [a, b, c]\n", set(path{"l"}, l("b", "c")), "l: [b, c]\n"},
		{"remove everything from a flow list", "l: [a, b]\n", set(path{"l"}, l()), "l: []\n"},
		{"replace every item", "l:\n  - a\n  - b\n", set(path{"l"}, l("x")), "l:\n  - x\n"},

		// Type changes.
		{"scalar to mapping keeps the comment on the key's line", "a: 1 # one\nb: 2\n", set(path{"a"}, m("x", 1)), "a: # one\n  x: 1\nb: 2\n"},
		{"empty value with a comment to mapping", "a: # todo\nb: 1\n", set(path{"a"}, m("x", 1)), "a: # todo\n  x: 1\nb: 1\n"},
		{"item with a comment to mapping", "l:\n  - a # first\n", set(path{"l", 0}, m("x", 1, "z", 2)), "l:\n  - x: 1 # first\n    z: 2\n"},
		{"scalar to list", "a: 1\nb: 2\n", set(path{"a"}, l(1, 2)), "a:\n  - 1\n  - 2\nb: 2\n"},
		{"mapping to scalar", "a:\n  x: 1\nb: 2\n", set(path{"a"}, 5), "a: 5\nb: 2\n"},
		{"list to mapping", "a:\n  - 1\nb: 2\n", set(path{"a"}, m("x", 1)), "a:\n  x: 1\nb: 2\n"},
		{"empty value to mapping", "a:\nb: 1\n", set(path{"a"}, m("x", 1)), "a:\n  x: 1\nb: 1\n"},
		{"literal block to mapping", "a: |\n  text\nb: 1\n", set(path{"a"}, m("x", 1)), "a:\n  x: 1\nb: 1\n"},
		{"item scalar to mapping", "l:\n  - a\n  - b\n", set(path{"l", 0}, m("x", 1, "w", 2)), "l:\n  - w: 2\n    x: 1\n  - b\n"},
		{"item scalar to list", "l:\n  - a\n", set(path{"l", 0}, l(1, 2)), "l:\n  - - 1\n    - 2\n"},
		{"item mapping to scalar", "l:\n  - x: 1\n    y: 2\n  - b\n", set(path{"l", 0}, "a"), "l:\n  - a\n  - b\n"},
		{"flow scalar to mapping", "m: {a: 1, b: 2}\n", set(path{"m", "a"}, m("c", 3)), "m: {a: {c: 3}, b: 2}\n"},
		{"flow list item to list", "l: [a, b]\n", set(path{"l", 1}, l(1)), "l: [a, [1]]\n"},
		{"mapping with no key kept", "m:\n  a: 1\nn: 1\n", set(path{"m"}, m("b", 2)), "m:\n  b: 2\nn: 1\n"},

		// Whole documents.
		{"empty file", "", func(any) any { return m("a", 1, "b", l("x")) }, "a: 1\nb:\n  - x\n"},
		{"comments only", "# header\n", func(any) any { return m("a", 1) }, "# header\na: 1\n"},
		{"comment without a final line break", "# header", func(any) any { return m("a", 1) }, "# header\na: 1\n"},
		{"scalar document to mapping", "hello\n", func(any) any { return m("a", 1) }, "a: 1\n"},
		{"mapping document to list", "# keep\na: 1\n", func(any) any { return l(1, m("b", 2)) }, "# keep\n- 1\n- b: 2\n"},
		{"mapping document with no key kept", "a: 1\n", func(any) any { return m("b", 2) }, "b: 2\n"},
		{"document to scalar", "a: 1\n", func(any) any { return "x" }, "x\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := []byte(tt.src)
			var doc any
			if tt.src != "" {
				doc = mustDecode(t, src)
			}
			got := mustEdit(t, src, tt.edit(doc))
			if got != tt.want {
				t.Errorf("Edit() =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestEditStructureRefuses(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		edit   func(doc any) any
		path   string
		reason string
	}{
		{"remove an anchored value", "a: &x 1\nb: *x\n", del(path{"a"}), "$.a", "anchor"},
		{"replace an anchored value", "a: &x {b: 1}\nc: *x\n", set(path{"a"}, 1), "$.a", "anchor"},
		{"add to a mapping with a merge key", "base: &b {x: 1}\nm:\n  <<: *b\n  y: 2\n", set(path{"m", "z"}, 3), "$.m", "merge key"},
		{"add to an aliased mapping", "base: &b {x: 1}\nm: *b\n", set(path{"m", "z"}, 3), "$.m", "alias"},
		{"remove beside a comment in a flow mapping", "m: {a: 1, # c\n  b: 2}\n", del(path{"m", "b"}), "$.m", "remove a comment"},
		{"comment between a key and its value", "a: # note\n  x: 1\nb: 1\n", set(path{"a"}, 5), "$.a", "comment sits between"},
		// Found by fuzzing: goccy's tree has one item here, the decoder two.
		{"tree with fewer items than the document", "- &0\n#\n-", func(d any) any { return append(append([]any{}, d.([]any)...), "x") }, "$", "disagree"},
		// Found by fuzzing: the item's value starts before the end of its '-'.
		{"value before its indicator", "-\t\v-", func(any) any { return []any{[]any{"0"}} }, "$[0]", "positions do not match"},
		{"nested sequence item", "l:\n  - - a\n    - b\n", func(d any) any { return set(path{"l", 0}, []any{"b"})(d) }, "$.l[0]", "shares its line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := []byte(tt.src)
			_, err := Edit(src, tt.edit(mustDecode(t, src)))
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

// set returns an edit that sets each path (followed by its value) in a copy of the document,
// creating the last key of each path when it is missing.
func set(pathsAndValues ...any) func(any) any {
	return func(doc any) any {
		for i := 0; i < len(pathsAndValues); i += 2 {
			doc = with(doc, pathsAndValues[i].(path), pathsAndValues[i+1])
		}
		return doc
	}
}

// with returns a copy of doc with the value at p set to v, adding the last key or appending
// the last item when it is missing.
func with(doc any, p path, v any) any {
	if len(p) == 0 {
		return v
	}
	switch d := doc.(type) {
	case map[string]any:
		out := make(map[string]any, len(d)+1)
		for k, item := range d {
			out[k] = item
		}
		out[p[0].(string)] = with(d[p[0].(string)], p[1:], v)
		return out
	case []any:
		i := p[0].(int)
		if i == len(d) {
			return append(slices.Clone(d), with(nil, p[1:], v))
		}
		out := slices.Clone(d)
		out[i] = with(d[i], p[1:], v)
		return out
	}
	panic("with: path does not exist")
}

// del returns an edit that removes the mapping entries at the given paths.
func del(paths ...path) func(any) any {
	return func(doc any) any {
		for _, p := range paths {
			doc = without(doc, p)
		}
		return doc
	}
}

// without returns a copy of doc without the mapping entry or list item at p.
func without(doc any, p path) any {
	switch d := doc.(type) {
	case map[string]any:
		out := make(map[string]any, len(d))
		for k, v := range d {
			out[k] = v
		}
		if len(p) == 1 {
			delete(out, p[0].(string))
		} else {
			out[p[0].(string)] = without(d[p[0].(string)], p[1:])
		}
		return out
	case []any:
		if len(p) == 1 {
			return slices.Delete(slices.Clone(d), p[0].(int), p[0].(int)+1)
		}
		out := slices.Clone(d)
		out[p[0].(int)] = without(d[p[0].(int)], p[1:])
		return out
	}
	panic("without: path does not exist")
}
