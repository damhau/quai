// Package hcledit edits HCL files that people also maintain (Terraform's `*.tfvars`, and `locals`
// blocks), changing only the bytes of the values that differ.
//
// It implements docs/design/03-file-editing.md for HCL, as yamledit and jsonedit do for YAML and
// JSON. The file is parsed with hclsyntax, which keeps the byte range of every attribute, key and
// value; the semantic change between the current and the intended document is computed (package
// document); each change becomes a text edit on the original bytes; and the result is verified:
// it parses, it decodes to exactly the intended document, values defined in code are
// byte-identical, and every byte outside the edited spans is unchanged.
//
// The document is the body's attributes: a file's top-level attributes, or the attributes of its
// locals block. Only literal values are managed: numbers, quoted strings without interpolation,
// booleans, null, and objects and tuples of literals. A value that is an expression
// (var.default_cpu, "${local.prefix}-web", a function call) or a heredoc is defined in code: it is
// not in the document, it is kept as it is, and writing a value there is refused.
//
// New entries go on a line of their own after their siblings in a multi-line object, tuple or
// body, and after them on the line in a one-line one; a removed entry takes its lines, and its own
// comments, with it. A file that hclwrite.Format (terraform fmt's formatter) leaves as it is stays
// so: the result is formatted, which only realigns the touched groups' equals signs.
package hcledit

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

// RefusalError reports an edit that Edit will not make because it cannot be made safely.
type RefusalError struct {
	// Path is the document path concerned, such as "$.vms.web01.cpu", or empty when the refusal
	// is about the whole file.
	Path   string
	Reason string
	// At is Path as a document path, nil for the whole file.
	At document.Path
}

func (e *RefusalError) Error() string {
	if e.Path == "" {
		return "hcledit: refused: " + e.Reason
	}
	return fmt.Sprintf("hcledit: refused at %s: %s", e.Path, e.Reason)
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

// DefinedInCode is the reason of a refusal to write a value that is an expression or a heredoc.
const DefinedInCode = "the value is defined in code"

// Code is a value defined in code: an expression or a heredoc, which the document leaves out.
type Code struct {
	Path document.Path
	Text string // as written, such as var.default_cpu
}

// Decode returns the literal values of the body block selects (the file's top-level attributes
// when it is empty, its locals block when it is "locals") as a document, and the values defined
// in code, which the document leaves out, in file order.
func Decode(src []byte, block string) (doc any, code []Code, err error) {
	f, err := parse(src, block)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range f.code {
		code = append(code, Code{Path: p, Text: f.text(f.at(p))})
	}
	return f.root.value, code, nil
}

// Edit returns src edited so that the body block selects decodes to want, an object in the JSON
// data model, changing as few bytes as it can. Values defined in code are kept; want setting one
// is refused. When the body already decodes to want, Edit returns src itself. An edit that cannot
// be made safely returns a *RefusalError; any other error means src is not valid HCL or want is
// not a valid document.
func Edit(src []byte, block string, want any) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, refuse(nil, "%s: %v", internalError, r)
		}
	}()
	want, err = document.Normalize(want)
	if err != nil {
		return nil, fmt.Errorf("hcledit: %v", err)
	}
	if _, ok := want.(map[string]any); !ok {
		return nil, fmt.Errorf("hcledit: the document of a body is an object, not %T", want)
	}
	f, err := parse(src, block)
	if err != nil {
		return nil, err
	}
	for _, p := range f.code {
		if has(want, p) {
			return nil, refuse(p, "%s: %s", DefinedInCode, f.text(f.at(p)))
		}
	}
	changes, err := f.expand(document.Diff(f.root.value, want))
	if err != nil {
		return nil, err
	}
	if len(changes) == 0 {
		return src, nil
	}
	s := detect(f)
	edits, err := f.edits(changes, want, s)
	if err != nil {
		return nil, err
	}
	return f.verified(edits, want, s, block)
}

// Create returns a new `*.tfvars` file whose attributes are doc, an object in the JSON data model,
// written as Edit writes new content and formatted. Keys listed in keyOrder come first, in that
// order; the others follow in sorted order. The result is verified to decode to doc.
func Create(doc any, keyOrder []string) ([]byte, error) {
	want, err := document.Normalize(doc)
	if err != nil {
		return nil, fmt.Errorf("hcledit: %v", err)
	}
	m, ok := want.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("hcledit: the document of a body is an object, not %T", want)
	}
	rank := make(map[string]int, len(keyOrder))
	for i, k := range keyOrder {
		if _, ok := rank[k]; !ok {
			rank[k] = i
		}
	}
	s := style{nl: "\n", unit: "  "}
	var b strings.Builder
	for _, k := range s.keys(m, rank) {
		b.WriteString(key(k) + " = " + s.render(m[k], "", true, rank) + "\n")
	}
	out := hclwrite.Format([]byte(b.String()))
	got, code, err := Decode(out, "")
	if err != nil || len(code) > 0 || !document.Equal(got, want) {
		return nil, fmt.Errorf("hcledit: %s: the new file does not decode to the intent (%v)", internalError, err)
	}
	return out, nil
}

// Keys returns, for the body block selects and each object in it, its keys in the order they are
// written, by the JSON pointer of the object ("" for the body). It lets callers keep the order of
// a document they decoded.
func Keys(src []byte, block string) (map[string][]string, error) {
	f, err := parse(src, block)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	var walk func(n *node, ptr string)
	walk = func(n *node, ptr string) {
		switch {
		case n.code:
		case n.object:
			keys := []string{}
			for _, m := range n.members {
				keys = append(keys, m.key)
				walk(m.value, ptr+"/"+strings.ReplaceAll(strings.ReplaceAll(m.key, "~", "~0"), "/", "~1"))
			}
			out[ptr] = keys
		case n.tuple:
			for i, item := range n.items {
				walk(item, fmt.Sprintf("%s/%d", ptr, i))
			}
		}
	}
	walk(f.root, "")
	return out, nil
}

// has reports whether doc holds a value, null included, at p.
func has(doc any, p document.Path) bool {
	for _, step := range p {
		switch d := doc.(type) {
		case map[string]any:
			k, ok := step.(string)
			if !ok {
				return false
			}
			if doc, ok = d[k]; !ok {
				return false
			}
		case []any:
			i, ok := step.(int)
			if !ok || i < 0 || i >= len(d) {
				return false
			}
			doc = d[i]
		default:
			return false
		}
	}
	return true
}

func (f *file) text(n *node) string {
	if n == nil {
		return ""
	}
	return string(f.src[n.start:n.end])
}

// expand turns the replacement of the body, or of an object that holds values defined in code
// somewhere below it, into the removal and addition of its literal keys, so that the code stays.
// Replacing such an object by something else is refused. (Tuples hold no code: a tuple with code
// in it is code as a whole.)
func (f *file) expand(changes []document.Change) ([]document.Change, error) {
	var out []document.Change
	for _, c := range changes {
		n := f.at(c.Path)
		holdsCode := slices.ContainsFunc(f.code, func(p document.Path) bool { return len(p) > len(c.Path) && hasPrefix(p, c.Path) })
		if c.Kind != document.ReplaceNode || n == nil || !n.object || !n.body && !holdsCode {
			out = append(out, c)
			continue
		}
		to, ok := c.To.(map[string]any)
		if !ok {
			return nil, refuse(c.Path, "the object holds values defined in code: it cannot become a %T", c.To)
		}
		from, _ := c.From.(map[string]any)
		for _, k := range document.SortedKeys(from) {
			if _, ok := to[k]; !ok {
				out = append(out, document.Change{Kind: document.RemoveKey, Path: c.Path.Key(k), From: from[k]})
			}
		}
		for _, k := range document.SortedKeys(to) {
			if fv, ok := from[k]; ok {
				nested, err := f.expand(prefixed(c.Path.Key(k), document.Diff(fv, to[k])))
				if err != nil {
					return nil, err
				}
				out = append(out, nested...)
			} else {
				out = append(out, document.Change{Kind: document.AddKey, Path: c.Path.Key(k), To: to[k]})
			}
		}
	}
	return out, nil
}

// prefixed returns changes made relative to a value as changes to the document, at p.
func prefixed(p document.Path, changes []document.Change) []document.Change {
	for i := range changes {
		changes[i].Path = append(slices.Clone(p), changes[i].Path...)
	}
	return changes
}

// style is how the file writes HCL, followed by new content.
type style struct {
	nl        string // "\n" or "\r\n"
	unit      string // one level of indentation
	formatted bool   // hclwrite.Format leaves the file as it is: the result is formatted too
}

func detect(f *file) style {
	s := style{nl: "\n", unit: "  "}
	if bytes.Contains(f.src, []byte("\r\n")) {
		s.nl = "\r\n"
	} else {
		s.formatted = bytes.Equal(hclwrite.Format(f.src), f.src)
	}
	var walk func(n *node) bool
	walk = func(n *node) bool {
		if n.code {
			return false
		}
		if !n.body && f.multiline(n) {
			if ind, outer := indent(f.src, n.firstChild()), indent(f.src, n.start); strings.HasPrefix(ind, outer) && len(ind) > len(outer) {
				s.unit = ind[len(outer):]
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
	return s
}

// firstChild returns the first byte of a container's first entry, or -1.
func (n *node) firstChild() int {
	switch {
	case len(n.members) > 0:
		return n.members[0].keyStart
	case len(n.items) > 0:
		return n.items[0].start
	}
	return -1
}

// multiline reports whether a container puts its entries on lines of their own. A body always
// does.
func (f *file) multiline(n *node) bool {
	if n.body {
		return true
	}
	first := n.firstChild()
	return first >= 0 && bytes.ContainsAny(f.src[n.start:first], "\n")
}

// inMultiline reports whether the value at p sits in a multi-line container.
func (f *file) inMultiline(p document.Path) bool {
	if len(p) == 0 {
		return true
	}
	parent := f.at(p[:len(p)-1])
	return parent != nil && f.multiline(parent)
}

// indent returns the leading whitespace of the line that holds offset at.
func indent(src []byte, at int) string {
	start := lineStart(src, at)
	end := start
	for end < len(src) && (src[end] == ' ' || src[end] == '\t') {
		end++
	}
	return string(src[start:end])
}

func lineStart(src []byte, at int) int { return bytes.LastIndexByte(src[:at], '\n') + 1 }

// lineEnd returns the offset of the line break ending the line that holds at, or len(src).
func lineEnd(src []byte, at int) int {
	i := bytes.IndexByte(src[at:], '\n')
	if i < 0 {
		return len(src)
	}
	if i > 0 && src[at+i-1] == '\r' {
		return at + i - 1
	}
	return at + i
}

// nextLine returns the offset of the line after the one that holds at.
func nextLine(src []byte, at int) int {
	i := bytes.IndexByte(src[at:], '\n')
	if i < 0 {
		return len(src)
	}
	return at + i + 1
}

// key writes an object key or attribute name: bare when it is an identifier, quoted otherwise.
func key(k string) string {
	if hclsyntax.ValidIdentifier(k) {
		return k
	}
	return scalar(k)
}

// scalar writes a normalized scalar as HCL.
func scalar(v any) string {
	var val cty.Value
	switch v := v.(type) {
	case nil:
		return "null"
	case bool:
		val = cty.BoolVal(v)
	case string:
		val = cty.StringVal(v)
	case int64:
		val = cty.NumberIntVal(v)
	case uint64:
		val = cty.NumberUIntVal(v)
	case float64:
		val = cty.NumberFloatVal(v)
	default:
		panic(fmt.Sprintf("scalar %#v", v))
	}
	return string(hclwrite.TokensForValue(val).Bytes())
}

// keys returns m's keys by rank, then in sorted order.
func (s style) keys(m map[string]any, rank map[string]int) []string {
	keys := document.SortedKeys(m)
	slices.SortStableFunc(keys, func(a, b string) int {
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
	})
	return keys
}

// render writes v as new content whose first line starts at indentation ind: objects, and
// tuples holding objects or tuples, on several lines when multiline; on one line otherwise.
func (s style) render(v any, ind string, multiline bool, rank map[string]int) string {
	switch v := v.(type) {
	case map[string]any:
		if len(v) == 0 {
			return "{}"
		}
		var b strings.Builder
		b.WriteString("{")
		for i, k := range s.keys(v, rank) {
			switch {
			case multiline:
				b.WriteString(s.nl + ind + s.unit)
			case i > 0:
				b.WriteString(", ")
			default:
				b.WriteString(" ")
			}
			b.WriteString(key(k) + " = " + s.render(v[k], ind+s.unit, multiline, rank))
		}
		if multiline {
			return b.String() + s.nl + ind + "}"
		}
		return b.String() + " }"
	case []any:
		// Scalars, or a single flat object, on one line; flat objects one per line; anything
		// deeper on several lines.
		nested := slices.ContainsFunc(v, func(item any) bool { return !document.IsScalar(item) })
		flat := !slices.ContainsFunc(v, func(item any) bool { return !flatObject(item) })
		if !multiline || !nested || flat && len(v) == 1 {
			parts := make([]string, len(v))
			for i, item := range v {
				parts[i] = s.render(item, ind, false, rank)
			}
			return "[" + strings.Join(parts, ", ") + "]"
		}
		var b strings.Builder
		b.WriteString("[")
		for _, item := range v {
			b.WriteString(s.nl + ind + s.unit + s.render(item, ind+s.unit, !flat, rank) + ",")
		}
		return b.String() + s.nl + ind + "]"
	}
	return scalar(v)
}

// flatObject reports whether v is an object of scalars.
func flatObject(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for _, item := range m {
		if !document.IsScalar(item) {
			return false
		}
	}
	return true
}

// textEdit replaces src[start:end] with text; start == end inserts.
type textEdit struct {
	start, end int
	text       string
}

// edits turns the changes into text edits on the original bytes. Entries added to or removed from
// one container are handled together, so that their lines and separators stay right.
func (f *file) edits(changes []document.Change, want any, s style) ([]textEdit, error) {
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
			if n == nil || n.code {
				return nil, refuse(c.Path, "%s: no literal value at the path", internalError)
			}
			out = append(out, textEdit{start: n.start, end: n.end, text: s.render(c.To, indent(f.src, n.start), f.inMultiline(c.Path), nil)})
		default:
			parent := c.Path[:len(c.Path)-1]
			k := parent.String()
			g := groups[k]
			if g == nil {
				g = &group{parent: parent, removed: map[int]bool{}}
				groups[k] = g
				order = append(order, k)
			}
			switch c.Kind {
			case document.RemoveKey:
				for i, m := range f.at(parent).members {
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
	for _, k := range order {
		g := groups[k]
		n := f.at(g.parent)
		if n == nil || n.code || !n.object && !n.tuple {
			return nil, refuse(g.parent, "%s: no object or tuple at the path", internalError)
		}
		edits, err := f.containerEdits(n, g.parent, g.removed, g.added, want, s)
		if err != nil {
			return nil, err
		}
		out = append(out, edits...)
	}
	return out, nil
}

// containerEdits removes and adds the entries of one container.
func (f *file) containerEdits(n *node, at document.Path, removed map[int]bool, added []document.Change, want any, s style) ([]textEdit, error) {
	var starts, ends []int
	for _, m := range n.members {
		starts, ends = append(starts, m.keyStart), append(ends, m.value.end)
	}
	for _, item := range n.items {
		starts, ends = append(starts, item.start), append(ends, item.end)
	}
	// An empty container gets its new entries written whole; an empty body, after its last line.
	if len(starts) == 0 {
		if n.body {
			return f.fillBody(n, at, want, s), nil
		}
		return []textEdit{{start: n.start, end: n.end, text: s.render(document.ValueAt(want, at), indent(f.src, n.start), f.inMultiline(at), nil)}}, nil
	}
	var out []textEdit
	multiline := f.multiline(n)
	if !multiline && len(removed) == len(starts) {
		// Nothing of a one-line container stays, code included: it is written anew.
		return []textEdit{{start: n.start, end: n.end, text: s.render(document.ValueAt(want, at), indent(f.src, n.start), f.inMultiline(at), nil)}}, nil
	}
	if multiline {
		// A removed entry takes its lines, and the comment lines right above it unless it is the
		// first entry, whose comments usually describe the container.
		for i := range starts {
			if !removed[i] {
				continue
			}
			from, to := lineStart(f.src, starts[i]), nextLine(f.src, ends[i])
			if i > 0 {
				floor := lineStart(f.src, ends[i-1])
				from = f.ownComments(from, indent(f.src, starts[i]), floor)
				// The blank line above goes too when nothing but a blank line or the container's end
				// follows, so that no blank line is left before a closing brace or at the end.
				if above := lineStart(f.src, from-1); above > floor && f.blank(above) && (i == len(starts)-1 || f.blank(to)) {
					from = above
				}
			}
			out = append(out, textEdit{start: from, end: to})
		}
	} else {
		for i := 0; i < len(starts); {
			if !removed[i] {
				i++
				continue
			}
			j := i
			for j+1 < len(starts) && removed[j+1] {
				j++
			}
			switch {
			case i > 0:
				out = append(out, textEdit{start: ends[i-1], end: ends[j]})
			case j+1 < len(starts):
				out = append(out, textEdit{start: starts[i], end: starts[j+1]})
			default:
				return nil, refuse(at, "%s: every entry of a one-line container removed", internalError)
			}
			i = j + 1
		}
	}
	last := -1
	for i := range starts {
		if !removed[i] {
			last = i
		}
	}
	childIndent := indent(f.src, starts[0])
	for _, c := range added {
		after := last
		if c.Kind == document.InsertItem {
			after = c.After
		}
		// Like the entry it follows: on one line when that one is an object or tuple on one line.
		entry := s.render(c.To, childIndent, multiline && !f.oneLineContainer(n, max(after, 0)), nil)
		blankBefore := ""
		if c.Kind == document.AddKey {
			entry = key(c.Path[len(c.Path)-1].(string)) + " = " + entry
			// A blank line before it when the entries before it are separated by one.
			if after > 0 && f.blankBetween(ends[after-1], starts[after]) {
				blankBefore = s.nl
			}
		}
		switch {
		case !multiline && after < 0:
			out = append(out, textEdit{start: starts[0], end: starts[0], text: entry + ", "})
		case !multiline:
			out = append(out, textEdit{start: ends[after], end: ends[after], text: ", " + entry})
		case after < 0:
			comma := ""
			if n.tuple {
				comma = ","
			}
			at := lineStart(f.src, starts[0])
			out = append(out, textEdit{start: at, end: at, text: childIndent + entry + comma + s.nl})
		default:
			// After the line of the entry it follows; in a tuple, with a comma after that entry
			// when it has none.
			comma, end := "", lineEnd(f.src, ends[after])
			if n.tuple {
				if f.trailingComma(ends[after]) {
					comma = ","
				} else {
					end = ends[after]
					entry = "," + s.nl + childIndent + entry
					out = append(out, textEdit{start: end, end: end, text: entry})
					continue
				}
			} else if f.trailingComma(ends[after]) {
				comma = ","
			}
			out = append(out, textEdit{start: end, end: end, text: s.nl + blankBefore + childIndent + entry + comma})
		}
	}
	return out, nil
}

// oneLineContainer reports whether the i-th entry of n is an object or a tuple written on one
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
	return !v.code && (v.object || v.tuple) && !bytes.ContainsAny(f.src[v.start:v.end], "\n")
}

// fillBody writes the attributes of an empty body: after the last line of a file, or inside the
// braces of a block.
func (f *file) fillBody(n *node, at document.Path, want any, s style) []textEdit {
	m, _ := document.ValueAt(want, at).(map[string]any)
	if n.start == 0 && n.end == len(f.src) {
		var b strings.Builder
		if len(f.src) > 0 && !bytes.HasSuffix(f.src, []byte("\n")) {
			b.WriteString(s.nl)
		}
		for _, k := range s.keys(m, nil) {
			b.WriteString(key(k) + " = " + s.render(m[k], "", true, nil) + s.nl)
		}
		return []textEdit{{start: len(f.src), end: len(f.src), text: b.String()}}
	}
	ind := indent(f.src, n.start)
	var b strings.Builder
	b.WriteString("{" + s.nl)
	for _, k := range s.keys(m, nil) {
		b.WriteString(ind + s.unit + key(k) + " = " + s.render(m[k], ind+s.unit, true, nil) + s.nl)
	}
	return []textEdit{{start: n.start, end: n.end, text: b.String() + ind + "}"}}
}

// ownComments extends a removed entry's first line upwards over the comment lines right above
// it at its indentation, stopping at a blank line or at the line of the entry before (floor).
func (f *file) ownComments(from int, ind string, floor int) int {
	for from > floor {
		prev := lineStart(f.src, from-1)
		line := string(f.src[prev:lineEnd(f.src, prev)])
		trimmed := strings.TrimLeft(line, " \t")
		if prev <= floor || !strings.HasPrefix(line, ind) || len(line)-len(trimmed) != len(ind) ||
			!(strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//")) {
			break
		}
		from = prev
	}
	return from
}

// blank reports whether the line starting at offset at holds only spaces.
func (f *file) blank(at int) bool {
	return at < len(f.src) && strings.TrimSpace(string(f.src[at:lineEnd(f.src, at)])) == ""
}

// blankBetween reports whether a blank line separates the bytes end and start.
func (f *file) blankBetween(end, start int) bool {
	for at := nextLine(f.src, end); at < lineStart(f.src, start); at = nextLine(f.src, at) {
		if f.blank(at) {
			return true
		}
	}
	return false
}

// trailingComma reports whether a comma follows the value ending at end, on its line.
func (f *file) trailingComma(end int) bool {
	rest := strings.TrimLeft(string(f.src[end:lineEnd(f.src, end)]), " \t")
	return strings.HasPrefix(rest, ",")
}

// verified applies the edits and checks the result: bytes outside the edited spans unchanged;
// formatted, when the original was, with only spaces changed by it; it parses; it decodes to
// exactly want; and the values defined in code that stay are byte-identical.
func (f *file) verified(edits []textEdit, want any, s style, block string) ([]byte, error) {
	out, err := apply(f.src, edits)
	if err != nil {
		return nil, err
	}
	if !outsideUnchanged(f.src, out, edits) {
		return nil, refuse(nil, "verification failed: bytes outside the edited values changed")
	}
	if s.formatted {
		formatted := hclwrite.Format(out)
		if spaceless(formatted) != spaceless(out) {
			return nil, refuse(nil, "verification failed: formatting changed more than spaces")
		}
		out = formatted
	}
	g, err := parse(out, block)
	if err != nil {
		return nil, refuse(nil, "verification failed: the edited file does not parse: %v", err)
	}
	if !document.Equal(g.root.value, want) {
		return nil, refuse(nil, "verification failed: the edited file does not decode to the intended document")
	}
	for _, p := range f.code {
		if parent := g.at(p[:len(p)-1]); parent != nil && f.text(f.at(p)) != g.text(g.at(p)) {
			return nil, refuse(p, "verification failed: a value defined in code changed")
		}
	}
	return out, nil
}

func spaceless(b []byte) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, string(b))
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
