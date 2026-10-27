package schema

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/expr"
)

// The helpers below read the schema document itself. They follow `$ref`s to the documents added
// to the compiler, merging the referring schema's other keywords over the target. A `$ref` back
// to a schema already being read is not followed, so recursive schemas end, and maxDepth bounds
// deeply nested ones.
const (
	maxRefs  = 10
	maxDepth = 32
)

// node is a subschema with the document it belongs to, the URL its relative refs resolve
// against, and the refs followed to reach it.
type node struct {
	m    map[string]any // nil for a boolean schema, or a ref that would recurse
	root any
	base string
	ptr  string // the node's JSON pointer in its document
	refs []string
}

func (s *Schema) rootNode() node {
	b := s.url
	if id := absoluteID(s.doc); id != "" {
		b = id
	}
	return s.resolve(node{m: asMap(s.doc), root: s.doc, base: b, refs: []string{b + "#"}})
}

// child returns the subschema sub of n, at the JSON pointer n.ptr + "/" + rel.
func (s *Schema) child(n node, sub any, rel string) node {
	return s.resolve(node{m: asMap(sub), root: n.root, base: n.base, ptr: n.ptr + "/" + rel, refs: n.refs})
}

// resolve follows the node's `$ref`, if any.
func (s *Schema) resolve(n node) node {
	for range maxRefs {
		ref, ok := n.m["$ref"].(string)
		if !ok {
			return n
		}
		t, key, ok := s.lookup(n, ref)
		if !ok {
			return n
		}
		if slices.Contains(n.refs, key) {
			return node{}
		}
		merged := maps.Clone(t.m)
		if merged == nil {
			merged = map[string]any{}
		}
		for k, v := range n.m {
			if k != "$ref" {
				merged[k] = v
			}
		}
		n = node{m: merged, root: t.root, base: t.base, ptr: t.ptr, refs: append(n.refs[:len(n.refs):len(n.refs)], key)}
	}
	return n
}

// lookup returns the schema a ref points to, and a key that identifies it.
func (s *Schema) lookup(n node, ref string) (node, string, bool) {
	b, err := url.Parse(n.base)
	if err != nil {
		return node{}, "", false
	}
	r, err := url.Parse(ref)
	if err != nil {
		return node{}, "", false
	}
	abs := b.ResolveReference(r)
	frag := abs.Fragment
	abs.Fragment = ""
	root, docBase := n.root, n.base
	u := abs.String()
	if u != strings.SplitN(n.base, "#", 2)[0] {
		doc, ok := s.docs[u]
		if !ok {
			return node{}, "", false
		}
		root, docBase = doc, u
	}
	v, ok := pointer(root, frag)
	if !ok {
		return node{}, "", false
	}
	return node{m: asMap(v), root: root, base: docBase, ptr: frag}, u + "#" + frag, true
}

// pointer returns the value at a JSON pointer such as "/$defs/disk".
func pointer(v any, ptr string) (any, bool) {
	if ptr == "" {
		return v, true
	}
	if !strings.HasPrefix(ptr, "/") {
		return nil, false // a plain-name anchor; not followed
	}
	for _, tok := range strings.Split(ptr[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch c := v.(type) {
		case map[string]any:
			x, ok := c[tok]
			if !ok {
				return nil, false
			}
			v = x
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(c) {
				return nil, false
			}
			v = c[i]
		default:
			return nil, false
		}
	}
	return v, true
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func isObject(m map[string]any) bool {
	switch t := m["type"].(type) {
	case string:
		return t == "object"
	case []any:
		return slices.Contains(t, any("object"))
	}
	return false
}

// Defaults returns the document a new resource starts from: each `default`, and for objects the
// defaults of their properties. An optional object property without defaults is left out. It
// returns nil when the schema has no defaults at all.
func (s *Schema) Defaults() any {
	v, _ := s.defaults(s.rootNode(), 0)
	return v
}

func (s *Schema) defaults(n node, depth int) (any, bool) {
	if n.m == nil || depth > maxDepth {
		return nil, false
	}
	if d, ok := n.m["default"]; ok {
		return clone(d), true
	}
	props, ok := n.m["properties"].(map[string]any)
	if !ok || !isObject(n.m) {
		return nil, false
	}
	required := map[string]bool{}
	if req, ok := n.m["required"].([]any); ok {
		for _, r := range req {
			if r, ok := r.(string); ok {
				required[r] = true
			}
		}
	}
	out := map[string]any{}
	for k, sub := range props {
		v, ok := s.defaults(s.child(n, sub, "properties/"+escape(k)), depth+1)
		if !ok {
			continue
		}
		if m, isMap := v.(map[string]any); isMap && len(m) == 0 && !required[k] {
			continue
		}
		out[k] = v
	}
	return out, true
}

// ReadOnlyFields returns the paths of the properties marked `readOnly`, nested objects included,
// sorted.
func (s *Schema) ReadOnlyFields() []string {
	var out []string
	s.readOnly(s.rootNode(), "", 0, &out)
	slices.Sort(out)
	return out
}

func (s *Schema) readOnly(n node, prefix string, depth int, out *[]string) {
	props, ok := n.m["properties"].(map[string]any)
	if !ok || depth > maxDepth {
		return
	}
	for k, sub := range props {
		c := s.child(n, sub, "properties/"+escape(k))
		p := k
		if prefix != "" {
			p = prefix + "." + k
		}
		if ro, _ := c.m["readOnly"].(bool); ro {
			*out = append(*out, p)
			continue
		}
		s.readOnly(c, p, depth+1, out)
	}
}

// LockedFields returns the fields a person may not change once a resource exists: those the
// schema marks readOnly and those the caller names, such as the owner field, the fields that hold
// locator params and the fields a type declares immutable. The result is sorted, without
// duplicates.
func LockedFields(s *Schema, fields ...string) []string {
	out := slices.Concat(s.ReadOnlyFields(), fields)
	slices.Sort(out)
	return slices.Compact(out)
}

// PruneHidden returns a copy of doc without the fields whose `visibleIf` condition is false, so
// that hidden values never reach Git. visibleIf maps field paths to compiled conditions; each is
// evaluated with vars, its inputs being the document as pruned so far, in path order. A path
// that is not in the document is left alone.
func PruneHidden(doc map[string]any, visibleIf map[string]*expr.Program, vars expr.Vars) (map[string]any, error) {
	out, _ := clone(doc).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	for _, p := range slices.Sorted(maps.Keys(visibleIf)) {
		vars.Inputs = out
		visible, err := visibleIf[p].Test(vars)
		if err != nil {
			return nil, fmt.Errorf("visibleIf of %s: %w", p, err)
		}
		if !visible {
			deletePath(out, p)
		}
	}
	return out, nil
}

// deletePath removes the value at a dotted path, whose numeric segments index lists.
func deletePath(doc map[string]any, path string) {
	segs := strings.Split(path, ".")
	var cur any = doc
	for i, seg := range segs {
		last := i == len(segs)-1
		switch c := cur.(type) {
		case map[string]any:
			if last {
				delete(c, seg)
				return
			}
			cur = c[seg]
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(c) {
				return
			}
			if last {
				// A list item is removed by replacing the list in its parent.
				setPath(doc, segs[:i], slices.Delete(slices.Clone(c), idx, idx+1))
				return
			}
			cur = c[idx]
		default:
			return
		}
	}
}

// setPath replaces the value at an existing path given as segments.
func setPath(doc map[string]any, segs []string, v any) {
	var cur any = doc
	for i, seg := range segs {
		last := i == len(segs)-1
		switch c := cur.(type) {
		case map[string]any:
			if last {
				c[seg] = v
				return
			}
			cur = c[seg]
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(c) {
				return
			}
			if last {
				c[idx] = v
				return
			}
			cur = c[idx]
		default:
			return
		}
	}
}

// clone returns a deep copy of a value in the JSON data model.
func clone(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = clone(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = clone(x)
		}
		return out
	}
	return v
}

func escape(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}

// PropertyOrder returns property names in the order the schema first declares them, breadth
// first from the root and following $refs: for the VM schema, its top-level properties, then
// those of network, then those of a disk. New files list their keys in this order, so they read
// like the hand-written ones. Properties of schemas that were added already decoded, without
// their source, come in sorted order.
func (s *Schema) PropertyOrder() []string {
	var out []string
	seen := map[string]bool{}
	queue := []node{s.rootNode()}
	visited := map[string]bool{}
	for len(queue) > 0 && len(visited) < 10*maxDepth*maxDepth {
		n := queue[0]
		queue = queue[1:]
		id := n.base + "#" + n.ptr
		if n.m == nil || visited[id] {
			continue
		}
		visited[id] = true
		props, _ := n.m["properties"].(map[string]any)
		order := s.keys[strings.SplitN(n.base, "#", 2)[0]][n.ptr+"/properties"]
		if len(order) != len(props) {
			order = slices.Sorted(maps.Keys(props))
		}
		for _, k := range order {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
			queue = append(queue, s.child(n, props[k], "properties/"+escape(k)))
		}
		for _, kw := range []string{"items", "additionalProperties"} {
			if sub, ok := n.m[kw]; ok {
				queue = append(queue, s.child(n, sub, kw))
			}
		}
		for _, kw := range []string{"allOf", "anyOf", "oneOf"} {
			subs, _ := n.m[kw].([]any)
			for i, sub := range subs {
				queue = append(queue, s.child(n, sub, kw+"/"+strconv.Itoa(i)))
			}
		}
	}
	return out
}

// Properties returns the names of the properties the schema declares at its top level, sorted.
func (s *Schema) Properties() []string {
	props, _ := s.rootNode().m["properties"].(map[string]any)
	return slices.Sorted(maps.Keys(props))
}

// Document returns the schema as written, for clients that render forms from it. With
// PropertyOrder, they can show fields in the order the schema declares them.
func (s *Schema) Document() any { return s.doc }
