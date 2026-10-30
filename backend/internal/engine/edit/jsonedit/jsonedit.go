// Package jsonedit edits JSON files that people also maintain, `*.tfvars.json` among them,
// changing only the bytes of the values that differ.
//
// It implements docs/design/03-file-editing.md for JSON, as yamledit does for YAML. The original
// file is parsed keeping the byte range of every key and value; the semantic change between the
// current and the intended document is computed (package document); each change becomes a text
// edit on the original bytes; and the result is verified before it is returned: it parses, it
// decodes to exactly the intended document, and every byte outside the edited spans is unchanged.
// When an edit cannot be made safely, Edit refuses with a *RefusalError instead of rewriting the
// file.
//
// Changes are made at the smallest node that differs: a scalar is replaced in place, a new entry
// or item is appended after its siblings in their style (on its own line, indented like them, in
// a multi-line object or array; separated like them in a one-line one), and a removed one takes
// its separator with it. Key order is kept; new keys are appended in sorted order.
package jsonedit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

// RefusalError reports an edit that Edit will not make because it cannot be made safely.
type RefusalError struct {
	// Path is the document path concerned, such as "$.disks[0].size_gb", or empty when the
	// refusal is about the whole file.
	Path   string
	Reason string
	// At is Path as a document path, nil for the whole file.
	At document.Path
}

func (e *RefusalError) Error() string {
	if e.Path == "" {
		return "jsonedit: refused: " + e.Reason
	}
	return fmt.Sprintf("jsonedit: refused at %s: %s", e.Path, e.Reason)
}

// refuse returns a *RefusalError about p, or about the whole file when p is nil.
func refuse(p document.Path, format string, args ...any) error {
	e := &RefusalError{Reason: fmt.Sprintf(format, args...)}
	if p != nil {
		e.Path, e.At = p.String(), p
	}
	return e
}

// internalError starts the reason of a refusal caused by a bug in this package.
const internalError = "internal error"

// Edit returns src edited so that it decodes to want, changing as few bytes as it can.
//
// want is a document in the JSON data model (see package document). When src already decodes to
// want, Edit returns src itself; a file holding only whitespace gets want written as Create
// writes it. An edit that cannot be made safely returns a *RefusalError; any other error means
// src is not valid JSON or want is not a valid document.
func Edit(src []byte, want any) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, refuse(nil, "%s: %v", internalError, r)
		}
	}()
	want, err = document.Normalize(want)
	if err != nil {
		return nil, fmt.Errorf("jsonedit: %v", err)
	}
	f, err := parse(src)
	if err != nil {
		return nil, err
	}
	if f.root == nil {
		if want == nil {
			return src, nil
		}
		s := detect(f)
		return verified(src, []textEdit{{start: 0, end: len(src), text: s.render(want, "", true, nil) + s.nl}}, want)
	}
	changes := document.Diff(f.root.value, want)
	if len(changes) == 0 {
		return src, nil
	}
	edits, err := f.edits(changes, want)
	if err != nil {
		return nil, err
	}
	return verified(src, edits, want)
}

// Decode returns src in the JSON data model, as Edit reads it: nil, bool, string, int64, uint64,
// float64, []any and map[string]any. Duplicate keys and a byte-order mark are errors; a file
// holding only whitespace decodes to nil.
func Decode(src []byte) (any, error) {
	f, err := parse(src)
	if err != nil {
		return nil, err
	}
	if f.root == nil {
		return nil, nil
	}
	return f.root.value, nil
}

// Elements returns the entries of the object, or the items of the array, at p in src, with their
// bytes (an entry's from its key), in file order.
func Elements(src []byte, p document.Path) ([]document.Element, error) {
	f, err := parse(src)
	if err != nil {
		return nil, err
	}
	n := f.at(p)
	if n == nil || !n.object && !n.array {
		return nil, refuse(p, "the value is not an object or an array")
	}
	var out []document.Element
	for _, m := range n.members {
		out = append(out, document.Element{Key: m.key, Start: m.keyStart, End: m.value.end})
	}
	for _, item := range n.items {
		out = append(out, document.Element{Start: item.start, End: item.end})
	}
	return out, nil
}

// Create returns a new JSON document for doc, a value in the JSON data model, written as Edit
// writes new content: two-space indentation, one entry or item per line. Object keys listed in
// keyOrder come first, in that order; the others follow in sorted order. The result ends with a
// newline and is verified to decode to doc.
func Create(doc any, keyOrder []string) ([]byte, error) {
	want, err := document.Normalize(doc)
	if err != nil {
		return nil, fmt.Errorf("jsonedit: %v", err)
	}
	rank := make(map[string]int, len(keyOrder))
	for i, k := range keyOrder {
		if _, ok := rank[k]; !ok {
			rank[k] = i
		}
	}
	s := style{nl: "\n", unit: "  ", colon: ": "}
	out := []byte(s.render(want, "", true, rank) + "\n")
	got, err := Decode(out)
	if err != nil || !document.Equal(got, want) {
		return nil, fmt.Errorf("jsonedit: %s: the new document does not decode to the intent (%v)", internalError, err)
	}
	return out, nil
}

// Keys returns, for each object in src, its keys in the order they are written, by the JSON
// pointer of the object: "" for the root, "/properties" for the object under the root's
// properties key, "/disks/0" for the first item of an array. It lets callers keep the order of a
// document they decoded.
func Keys(src []byte) (map[string][]string, error) {
	f, err := parse(src)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	var walk func(n *node, ptr string)
	walk = func(n *node, ptr string) {
		switch {
		case n.object:
			keys := []string{}
			for _, m := range n.members {
				keys = append(keys, m.key)
				walk(m.value, ptr+"/"+escapePointer(m.key))
			}
			out[ptr] = keys
		case n.array:
			for i, item := range n.items {
				walk(item, fmt.Sprintf("%s/%d", ptr, i))
			}
		}
	}
	if f.root != nil {
		walk(f.root, "")
	}
	return out, nil
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// style is how the file writes JSON, followed by new content.
type style struct {
	nl    string // "\n" or "\r\n"
	unit  string // one level of indentation; empty for a file written on one line
	colon string // between a key and its value, such as ": "
	pad   bool   // a one-line object has a space inside its braces: { "a": 1 }
}

// detect reads the file's line ending, indentation unit and key separator. A file holding only
// whitespace gets Create's style.
func detect(f *file) style {
	s := style{nl: "\n", unit: "  ", colon: ": "}
	if bytes.Contains(f.src, []byte("\r\n")) {
		s.nl = "\r\n"
	}
	if f.root == nil {
		return s
	}
	s.unit, s.colon = "", ":"
	var walk func(n *node) bool
	walk = func(n *node) bool {
		first := n.firstChild()
		if first < 0 {
			return false
		}
		if f.multiline(n) {
			if ind, outer := indent(f.src, first), indent(f.src, n.start); strings.HasPrefix(ind, outer) && len(ind) > len(outer) {
				s.unit = ind[len(outer):]
			} else {
				s.unit = "  "
			}
			return true
		}
		for _, m := range n.members {
			if walk(m.value) {
				return true
			}
		}
		for _, item := range n.items {
			if walk(item) {
				return true
			}
		}
		return false
	}
	walk(f.root)
	if s.unit != "" {
		s.colon = ": "
	}
	if m := f.firstMember(f.root); m != nil {
		if between := string(f.src[m.keyEnd:m.value.start]); !strings.ContainsAny(between, "\r\n") {
			s.colon = between
		}
	}
	return s
}

// firstChild returns the first byte of an object's first key or an array's first item, or -1.
func (n *node) firstChild() int {
	switch {
	case len(n.members) > 0:
		return n.members[0].keyStart
	case len(n.items) > 0:
		return n.items[0].start
	}
	return -1
}

// multiline reports whether an object or array puts its first entry on a line of its own.
func (f *file) multiline(n *node) bool {
	first := n.firstChild()
	return first >= 0 && bytes.ContainsAny(f.src[n.start:first], "\n")
}

func (f *file) firstMember(n *node) *member {
	for i := range n.members {
		return &n.members[i]
	}
	for _, item := range n.items {
		if m := f.firstMember(item); m != nil {
			return m
		}
	}
	return nil
}

// indent returns the leading whitespace of the line that holds offset at.
func indent(src []byte, at int) string {
	start := bytes.LastIndexByte(src[:at], '\n') + 1
	end := start
	for end < len(src) && (src[end] == ' ' || src[end] == '\t') {
		end++
	}
	return string(src[start:end])
}

// separator is what goes between a container's entries, after the comma: a line break and the
// entries' indentation in a multi-line container, the spacing after its first comma otherwise.
// A one-line container with one entry has no comma to follow: a space when it opens with one or
// the file writes ": " between keys and values, nothing otherwise.
func (f *file) separator(n *node, s style) string {
	if f.multiline(n) {
		return s.nl + indent(f.src, n.firstChild())
	}
	starts := n.childStarts()
	if len(starts) < 2 {
		if bytes.ContainsAny(f.src[n.start+1:starts[0]], " ") || strings.HasSuffix(s.colon, " ") {
			return " "
		}
		return ""
	}
	comma := bytes.LastIndexByte(f.src[:starts[1]], ',')
	return string(f.src[comma+1 : starts[1]])
}

// childStarts and childEnds return the first byte of each entry (its key) or item, and the byte
// after its value.
func (n *node) childStarts() []int {
	var out []int
	for _, m := range n.members {
		out = append(out, m.keyStart)
	}
	for _, item := range n.items {
		out = append(out, item.start)
	}
	return out
}

func (n *node) childEnds() []int {
	var out []int
	for _, m := range n.members {
		out = append(out, m.value.end)
	}
	for _, item := range n.items {
		out = append(out, item.end)
	}
	return out
}

// render writes v as new content whose first line starts at indentation ind: on several lines,
// indented by the style's unit, when multiline and the file has a unit; on one line otherwise.
// Object keys follow rank, then sorted order.
func (s style) render(v any, ind string, multiline bool, rank map[string]int) string {
	multiline = multiline && s.unit != ""
	comma := ","
	if !multiline && strings.HasSuffix(s.colon, " ") {
		comma = ", "
	}
	switch v := v.(type) {
	case map[string]any:
		if len(v) == 0 {
			return "{}"
		}
		keys := document.SortedKeys(v)
		slices.SortStableFunc(keys, func(a, b string) int { return byRank(rank, a, b) })
		var b strings.Builder
		b.WriteString("{")
		if !multiline && s.pad {
			b.WriteString(" ")
		}
		for i, k := range keys {
			if i > 0 {
				b.WriteString(comma)
			}
			if multiline {
				b.WriteString(s.nl + ind + s.unit)
			}
			b.WriteString(scalar(k) + s.colon + s.render(v[k], ind+s.unit, multiline, rank))
		}
		switch {
		case multiline:
			b.WriteString(s.nl + ind)
		case s.pad:
			b.WriteString(" ")
		}
		return b.String() + "}"
	case []any:
		if len(v) == 0 {
			return "[]"
		}
		var b strings.Builder
		b.WriteString("[")
		for i, item := range v {
			if i > 0 {
				b.WriteString(comma)
			}
			if multiline {
				b.WriteString(s.nl + ind + s.unit)
			}
			b.WriteString(s.render(item, ind+s.unit, multiline, rank))
		}
		if multiline {
			b.WriteString(s.nl + ind)
		}
		return b.String() + "]"
	}
	return scalar(v)
}

func byRank(rank map[string]int, a, b string) int {
	ra, oka := rank[a]
	rb, okb := rank[b]
	switch {
	case oka && okb:
		return ra - rb
	case oka:
		return -1
	case okb:
		return 1
	}
	return 0
}

// scalar writes a normalized scalar as JSON. Strings keep <, > and & as they are.
func scalar(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(fmt.Sprintf("encoding %#v: %v", v, err))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// textEdit replaces src[start:end] with text; start == end inserts.
type textEdit struct {
	start, end int
	text       string
}

// edits turns the changes into text edits on the original bytes. Entries added to or removed from
// one container are handled together, so that their separators stay right.
func (f *file) edits(changes []document.Change, want any) ([]textEdit, error) {
	s := detect(f)
	type group struct {
		parent  document.Path
		removed map[int]bool
		added   []document.Change
	}
	groups := map[string]*group{}
	var order []string
	var out []textEdit
	for _, c := range changes {
		switch c.Kind {
		case document.SetScalar, document.ReplaceNode:
			n := f.at(c.Path)
			if n == nil {
				return nil, refuse(c.Path, "%s: no node at the path", internalError)
			}
			out = append(out, textEdit{start: n.start, end: n.end, text: s.render(c.To, indent(f.src, n.start), f.inMultiline(c.Path), nil)})
		default:
			parent := c.Path[:len(c.Path)-1]
			key := parent.String()
			g := groups[key]
			if g == nil {
				g = &group{parent: parent, removed: map[int]bool{}}
				groups[key] = g
				order = append(order, key)
			}
			switch c.Kind {
			case document.RemoveKey:
				n := f.at(parent)
				for i, m := range n.members {
					if m.key == c.Path[len(c.Path)-1] {
						g.removed[i] = true
					}
				}
			case document.RemoveItem:
				g.removed[c.Path[len(c.Path)-1].(int)] = true
			default:
				g.added = append(g.added, c)
			}
		}
	}
	for _, key := range order {
		g := groups[key]
		n := f.at(g.parent)
		if n == nil || !n.object && !n.array {
			return nil, refuse(g.parent, "%s: no object or array at the path", internalError)
		}
		starts, ends := n.childStarts(), n.childEnds()
		if len(starts) == 0 {
			// An empty object or array gets its new entries written whole.
			out = append(out, textEdit{start: n.start, end: n.end,
				text: s.render(document.ValueAt(want, g.parent), indent(f.src, n.start), f.inMultiline(g.parent), nil)})
			continue
		}
		if len(g.removed) == len(starts) {
			return nil, refuse(g.parent, "%s: every entry removed", internalError)
		}
		// A run of removed entries goes with the separator before it, or, at the start, with the
		// one after it.
		for i := 0; i < len(starts); {
			if !g.removed[i] {
				i++
				continue
			}
			j := i
			for j+1 < len(starts) && g.removed[j+1] {
				j++
			}
			if i > 0 {
				out = append(out, textEdit{start: ends[i-1], end: ends[j]})
			} else {
				out = append(out, textEdit{start: starts[i], end: starts[j+1]})
			}
			i = j + 1
		}
		sep := f.separator(n, s)
		childIndent := indent(f.src, starts[0])
		multiline := f.multiline(n)
		for _, c := range g.added {
			// Like the entry it follows: on one line when that one is an object or array on one line.
			sibling := len(starts) - 1
			if c.Kind == document.InsertItem {
				sibling = max(c.After, 0)
			}
			es := s
			es.pad = f.padded(n, sibling)
			text := es.render(c.To, childIndent, multiline && !f.oneLineContainer(n, sibling), nil)
			if c.Kind == document.AddKey {
				text = scalar(c.Path[len(c.Path)-1]) + s.colon + text
				out = append(out, textEdit{start: ends[len(ends)-1], end: ends[len(ends)-1], text: "," + sep + text})
				continue
			}
			if c.After < 0 {
				out = append(out, textEdit{start: starts[0], end: starts[0], text: text + "," + sep})
			} else {
				out = append(out, textEdit{start: ends[c.After], end: ends[c.After], text: "," + sep + text})
			}
		}
	}
	return out, nil
}

// oneLineContainer reports whether the i-th entry of n is an object or an array written on one
// line.
func (f *file) oneLineContainer(n *node, i int) bool {
	var v *node
	switch {
	case i < len(n.members):
		v = n.members[i].value
	case i < len(n.items):
		v = n.items[i]
	default:
		return false
	}
	return (v.object || v.array) && !bytes.ContainsAny(f.src[v.start:v.end], "\n")
}

// padded reports whether the i-th entry of n is an object written { like this }.
func (f *file) padded(n *node, i int) bool {
	var v *node
	switch {
	case i < len(n.members):
		v = n.members[i].value
	case i < len(n.items):
		v = n.items[i]
	default:
		return false
	}
	return v.object && bytes.HasPrefix(f.src[v.start:v.end], []byte("{ "))
}

// inMultiline reports whether the value at p sits in a multi-line object or array, so that new
// content there is written on several lines too. The root counts as multi-line.
func (f *file) inMultiline(p document.Path) bool {
	if len(p) == 0 {
		return true
	}
	parent := f.at(p[:len(p)-1])
	return parent != nil && f.multiline(parent)
}

// verified applies the edits to src and checks the result: it parses, it decodes to exactly
// want, and every byte outside the edited spans is unchanged.
func verified(src []byte, edits []textEdit, want any) ([]byte, error) {
	out, err := apply(src, edits)
	if err != nil {
		return nil, err
	}
	got, err := Decode(out)
	if err != nil {
		return nil, refuse(nil, "verification failed: the edited file does not parse: %v", err)
	}
	if !document.Equal(got, want) {
		return nil, refuse(nil, "verification failed: the edited file does not decode to the intended document")
	}
	if !outsideUnchanged(src, out, edits) {
		return nil, refuse(nil, "verification failed: bytes outside the edited values changed")
	}
	return out, nil
}

// apply applies non-overlapping edits to src, in offset order; at one offset, insertions come
// first, in the order given.
func apply(src []byte, edits []textEdit) ([]byte, error) {
	edits = sortEdits(edits)
	var out bytes.Buffer
	at := 0
	for _, e := range edits {
		if e.start < at || e.end < e.start || e.end > len(src) {
			return nil, refuse(nil, "%s: two edits overlap", internalError)
		}
		out.Write(src[at:e.start])
		out.WriteString(e.text)
		at = e.end
	}
	out.Write(src[at:])
	return out.Bytes(), nil
}

func sortEdits(edits []textEdit) []textEdit {
	edits = slices.Clone(edits)
	slices.SortStableFunc(edits, func(a, b textEdit) int {
		if a.start != b.start {
			return a.start - b.start
		}
		if ai, bi := a.end == a.start, b.end == b.start; ai != bi {
			if ai {
				return -1
			}
			return 1
		}
		return 0
	})
	return edits
}

// outsideUnchanged reports whether out is src with each edit's span replaced by its text and
// nothing else.
func outsideUnchanged(src, out []byte, edits []textEdit) bool {
	edits = sortEdits(edits)
	at, shift := 0, 0
	for _, e := range edits {
		gap := src[at:e.start]
		o := at + shift
		if o+len(gap)+len(e.text) > len(out) || !bytes.Equal(out[o:o+len(gap)], gap) ||
			string(out[o+len(gap):o+len(gap)+len(e.text)]) != e.text {
			return false
		}
		shift += len(e.text) - (e.end - e.start)
		at = e.end
	}
	return bytes.Equal(out[min(at+shift, len(out)):], src[at:]) && len(out) == len(src)+shift
}
