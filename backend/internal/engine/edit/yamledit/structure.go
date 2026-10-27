package yamledit

import (
	"bytes"
	"slices"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/token"
)

// edits turns changes into text edits. Additions and removals in the same collection are
// handled together, so entries added together are written together and removals of neighbours
// do not overlap.
func (f *file) edits(changes []change) ([]textEdit, error) {
	var edits []textEdit
	groups := map[string][]change{}
	var order []path
	for _, c := range changes {
		var e textEdit
		var err error
		switch c.kind {
		case setScalar:
			e, err = f.setScalar(c)
		case replaceNode:
			e, err = f.replace(c)
		default:
			parent := c.path[:len(c.path)-1]
			if _, ok := groups[parent.String()]; !ok {
				order = append(order, parent)
			}
			groups[parent.String()] = append(groups[parent.String()], c)
			continue
		}
		if err != nil {
			return nil, err
		}
		edits = append(edits, e)
	}
	for _, p := range order {
		es, err := f.collectionEdits(p, groups[p.String()])
		if err != nil {
			return nil, err
		}
		edits = append(edits, es...)
	}
	return edits, nil
}

// element is an entry of a mapping or an item of a sequence, with its bytes.
type element struct {
	key    string   // the mapping key; empty for sequence items
	start  int      // first byte: the key, the '-' of a block item, or a flow item's value
	end    int      // end of the value, or of the ':' or '-' when the value is empty
	indent int      // column of the key or the '-', counted from zero
	node   ast.Node // the mapping entry or the item's value
}

// collection is a mapping or a sequence in the original, with its elements.
type collection struct {
	path     path
	flow     bool
	mapping  bool
	open     int // flow: the offset just past '{' or '['
	elements []element
}

func (f *file) collectionAt(p path) (*collection, error) {
	loc, err := f.locate(p)
	if err != nil {
		return nil, err
	}
	c := &collection{path: p}
	var open *token.Token
	switch n := loc.node.(type) {
	case *ast.MappingNode:
		c.flow, c.mapping, open = n.IsFlowStyle, true, n.Start
		for _, mv := range n.Values {
			el, err := f.entryElement(mv, p)
			if err != nil {
				return nil, err
			}
			c.elements = append(c.elements, el)
		}
	case *ast.SequenceNode:
		c.flow, open = n.IsFlowStyle, n.Start
		for i, v := range n.Values {
			el, err := f.itemElement(n, i, v, p.index(i))
			if err != nil {
				return nil, err
			}
			c.elements = append(c.elements, el)
		}
	default:
		return nil, refuse(p, "the value is not a mapping or a list in the syntax tree")
	}
	if c.flow {
		if _, c.open, err = f.tokenSpan(open, p); err != nil {
			return nil, err
		}
	} else if len(c.elements) == 0 {
		return nil, refuse(p, "the syntax tree's positions do not match the file")
	}
	return c, nil
}

func (f *file) entryElement(mv *ast.MappingValueNode, p path) (element, error) {
	if mv.Key.IsMergeKey() {
		return element{}, refuse(p, "the mapping uses a merge key (<<)")
	}
	k, ok := mv.Key.(ast.ScalarNode)
	if !ok {
		return element{}, refuse(p, "the mapping has a key that is not a scalar")
	}
	key := k.GetToken().Value
	if s, ok := k.(*ast.StringNode); ok {
		key = s.Value
	}
	start, _, err := f.tokenSpan(k.GetToken(), p.key(key))
	if err != nil {
		return element{}, err
	}
	end, err := f.entryEnd(mv, p.key(key))
	if err != nil {
		return element{}, err
	}
	return element{key: key, start: start, end: end, indent: k.GetToken().Position.Column - 1, node: mv}, nil
}

func (f *file) itemElement(seq *ast.SequenceNode, i int, v ast.Node, p path) (element, error) {
	if seq.IsFlowStyle {
		start, err := f.nodeStart(v, p)
		if err != nil {
			return element{}, err
		}
		end, err := f.nodeEnd(v, 0, p)
		return element{start: start, end: end, node: v}, err
	}
	if i >= len(seq.Entries) {
		return element{}, refuse(p, "the syntax tree's positions do not match the file")
	}
	dash := seq.Entries[i].Start
	start, _, err := f.tokenSpan(dash, p)
	if err != nil {
		return element{}, err
	}
	end, err := f.itemEnd(dash, v, p)
	return element{start: start, end: end, indent: dash.Position.Column - 1, node: v}, err
}

// index returns the position of the element a change's path names.
func (c *collection) index(p path) int {
	switch step := p[len(p)-1].(type) {
	case string:
		return slices.IndexFunc(c.elements, func(el element) bool { return el.key == step })
	case int:
		if step < len(c.elements) {
			return step
		}
	}
	return -1
}

// addition is a new entry or item, written after the original element `after`.
type addition struct {
	key   string
	value any
	after int
}

// collectionEdits removes and adds the elements of the collection at p.
func (f *file) collectionEdits(p path, changes []change) ([]textEdit, error) {
	c, err := f.collectionAt(p)
	if err != nil {
		return nil, err
	}
	removed := make([]bool, len(c.elements))
	var added []addition
	for _, ch := range changes {
		switch ch.kind {
		case removeKey, removeItem:
			i := c.index(ch.path)
			if i < 0 {
				return nil, refuse(ch.path, "the value is not in the syntax tree")
			}
			if hasAnchor(c.elements[i].node) {
				return nil, refuse(ch.path, "the value has an anchor (&); removing it would break its aliases")
			}
			removed[i] = true
		case addKey:
			added = append(added, addition{key: ch.path[len(ch.path)-1].(string), value: ch.to, after: len(c.elements) - 1})
		case insertItem:
			added = append(added, addition{value: ch.to, after: ch.after})
		}
	}
	var edits []textEdit
	if c.flow {
		edits, err = f.flowRemovals(c, removed)
	} else {
		edits, err = f.blockRemovals(c, removed)
	}
	if err != nil {
		return nil, err
	}
	for len(added) > 0 {
		n := 1
		for n < len(added) && added[n].after == added[0].after {
			n++
		}
		e, err := f.insertion(c, added[:n])
		if err != nil {
			return nil, err
		}
		edits = append(edits, e)
		added = added[n:]
	}
	return edits, nil
}

// blockRemovals removes the lines of removed block elements, with the comment lines right above
// them, except above the first element, where a comment usually describes the whole collection.
func (f *file) blockRemovals(c *collection, removed []bool) ([]textEdit, error) {
	var edits []textEdit
	depth := len(c.path) + 1
	for i := 0; i < len(c.elements); i++ {
		if !removed[i] {
			continue
		}
		el := c.elements[i]
		ls := f.lineStart(el.start)
		if strings.TrimLeft(string(f.src[ls:el.start]), " ") != "" {
			// Only the first entries of a mapping that is a sequence item ("- key: value") share
			// their line; removing them leaves the next kept entry on the '-' line.
			if i != 0 || !c.mapping {
				return nil, refuse(c.path, "an element shares its line with other content")
			}
			j := i
			for j < len(c.elements) && removed[j] {
				j++
			}
			if j == len(c.elements) {
				return nil, refuse(c.path, "the syntax tree's positions do not match the file")
			}
			edits = append(edits, textEdit{start: el.start, end: c.elements[j].start, depth: depth})
			i = j
			continue
		}
		start := ls
		if i > 0 {
			start = f.headComments(ls, el.indent)
		}
		edits = append(edits, textEdit{start: start, end: f.lineAfter(el.end), depth: depth})
	}
	return edits, nil
}

// headComments returns the start of the comment lines right above the line starting at ls that
// are indented like the element.
func (f *file) headComments(ls, indent int) int {
	start := ls
	for start > 0 {
		prev := f.lineStart(start - 1)
		line := strings.TrimRight(string(f.src[prev:start]), "\r\n")
		content := strings.TrimLeft(line, " ")
		if !strings.HasPrefix(content, "#") || len(line)-len(content) != indent {
			break
		}
		start = prev
	}
	return start
}

// flowRemovals removes removed flow elements with the separator before them, or after them for
// leading elements.
func (f *file) flowRemovals(c *collection, removed []bool) ([]textEdit, error) {
	var edits []textEdit
	depth := len(c.path) + 1
	j := 0
	for j < len(removed) && removed[j] {
		j++
	}
	if j > 0 {
		if j == len(c.elements) {
			return nil, refuse(c.path, "the syntax tree's positions do not match the file")
		}
		edits = append(edits, textEdit{start: c.elements[0].start, end: c.elements[j].start, depth: depth})
	}
	for i := j + 1; i < len(c.elements); i++ {
		if removed[i] {
			edits = append(edits, textEdit{start: c.elements[i-1].end, end: c.elements[i].end, depth: depth})
		}
	}
	for _, e := range edits {
		if bytes.IndexByte(f.src[e.start:e.end], '#') >= 0 {
			return nil, refuse(c.path, "removing the element would remove a comment")
		}
	}
	return edits, nil
}

// insertion writes new elements after the original element adds[0].after (-1: before the
// first), in the collection's style.
func (f *file) insertion(c *collection, adds []addition) (textEdit, error) {
	after := adds[0].after
	depth := len(c.path) + 1
	if c.flow {
		parts := make([]string, len(adds))
		for i, a := range adds {
			parts[i] = f.flowText(a.value)
			if c.mapping {
				parts[i] = f.renderKey(a.key, true) + ": " + parts[i]
			}
		}
		text := strings.Join(parts, ", ")
		if after >= 0 {
			return textEdit{start: c.elements[after].end, end: c.elements[after].end, text: ", " + text, depth: depth}, nil
		}
		if len(c.elements) > 0 {
			text += ", "
		}
		return textEdit{start: c.open, end: c.open, text: text, depth: depth}, nil
	}

	indent := c.elements[0].indent
	nl := f.newline(c.elements[max(after, 0)].start)
	lines := make([]string, len(adds))
	for i, a := range adds {
		if c.mapping {
			lines[i] = f.entryText(a.key, a.value, indent, nl)
		} else {
			lines[i] = f.itemText(a.value, indent, nl)
		}
	}
	text := strings.Join(lines, nl)
	var at int
	if after >= 0 {
		at = f.lineAfter(c.elements[after].end)
	} else {
		at = f.lineStart(c.elements[0].start)
		if strings.TrimLeft(string(f.src[at:c.elements[0].start]), " ") != "" {
			return textEdit{}, refuse(c.path, "an element shares its line with other content")
		}
	}
	if at == len(f.src) && !bytes.HasSuffix(f.src, []byte("\n")) {
		return textEdit{start: at, end: at, text: nl + text, depth: depth}, nil
	}
	return textEdit{start: at, end: at, text: text + nl, depth: depth}, nil
}

// replace writes a new value in place of the whole node at c.path.
func (f *file) replace(c change) (textEdit, error) {
	loc, err := f.locate(c.path)
	if err != nil {
		return textEdit{}, err
	}
	depth := len(c.path)
	if loc.node == nil {
		// An empty document: the new content goes after any comments.
		at, nl := len(f.src), f.newline(len(f.src))
		text := f.valueText(c.to, -1, false, nl)
		if at > 0 && !bytes.HasSuffix(f.src, []byte("\n")) {
			text = nl + text
		}
		return textEdit{start: at, end: at, text: text + nl, depth: depth}, nil
	}
	if hasAnchor(loc.node) {
		return textEdit{}, refuse(c.path, "the value has an anchor (&); replacing it would break its aliases")
	}
	if isEmpty(loc.node) && !loc.flow && loc.owner != nil {
		_, ownerEnd, err := f.tokenSpan(loc.owner, c.path)
		if err != nil {
			return textEdit{}, err
		}
		dash := loc.owner.Type == token.SequenceEntryType
		return f.keepLineComment(textEdit{start: ownerEnd, end: ownerEnd, text: f.valueText(c.to, loc.indent, dash, f.newline(ownerEnd)), depth: depth}), nil
	}
	start, err := f.nodeStart(loc.node, c.path)
	if err != nil {
		return textEdit{}, err
	}
	nl := f.newline(start)
	if loc.flow {
		end, err := f.nodeEnd(loc.node, loc.indent, c.path)
		return textEdit{start: start, end: end, text: f.flowText(c.to), depth: depth}, err
	}
	if loc.owner == nil {
		// The document root.
		end, err := f.nodeEnd(loc.node, 0, c.path)
		return textEdit{start: start, end: end, text: f.valueText(c.to, -1, false, nl), depth: depth}, err
	}
	_, ownerEnd, err := f.tokenSpan(loc.owner, c.path)
	if err != nil {
		return textEdit{}, err
	}
	if bytes.IndexByte(f.src[ownerEnd:start], '#') >= 0 {
		return textEdit{}, refuse(c.path, "a comment sits between the entry and its value")
	}
	end, err := f.nodeEnd(loc.node, loc.indent, c.path)
	if err != nil {
		return textEdit{}, err
	}
	dash := loc.owner.Type == token.SequenceEntryType
	return f.keepLineComment(textEdit{start: ownerEnd, end: end, text: f.valueText(c.to, loc.indent, dash, nl), depth: depth}), nil
}

// keepLineComment keeps a comment that follows the edited span on its line on the first line of
// a multi-line replacement. Left where it was, it would end up after the last new line, and
// inside the content of a new block scalar.
func (f *file) keepLineComment(e textEdit) textEdit {
	br := strings.IndexByte(e.text, '\n')
	if br < 0 {
		return e
	}
	if br > 0 && e.text[br-1] == '\r' {
		br--
	}
	lineEnd := e.end + indexOrEnd(f.src[e.end:], '\n')
	if lineEnd > e.end && f.src[lineEnd-1] == '\r' {
		lineEnd--
	}
	rest := string(f.src[e.end:lineEnd])
	if strings.TrimSpace(rest) == "" {
		return e
	}
	e.text = e.text[:br] + rest + e.text[br:]
	e.end = lineEnd
	return e
}
