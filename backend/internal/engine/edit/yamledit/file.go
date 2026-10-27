package yamledit

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

// file is a parsed original: its bytes, syntax tree and decoded document, plus the conventions
// detected from it that new content follows.
type file struct {
	// src is the original with CRLF line breaks turned into LF: goccy miscounts lines in CRLF
	// files after comments, so the tree and every offset come from this view, and edits are
	// mapped back to the original's bytes (original) before they are applied.
	src   []byte
	crs   []int    // offsets in src of the line breaks that are CRLF in the original
	lines []int    // offset in src of the start of each line
	body  ast.Node // the document's root node; nil for an empty document
	value any      // the decoded, normalized document
	quote byte     // quote character for strings that need quoting
	width int      // indentation step of nested mappings, in columns
	seq   int      // indentation of a block sequence under its key: width, or 0 for `key:\n- item`
	item  int      // offset from an item's '-' to its content, usually 2 for "- "
}

func parse(src []byte) (*file, error) {
	if bytes.HasPrefix(src, []byte{0xEF, 0xBB, 0xBF}) {
		return nil, refuse(nil, "the file starts with a byte-order mark")
	}
	if !utf8.Valid(src) {
		return nil, refuse(nil, "the file is not valid UTF-8")
	}
	lf, crs := toLF(src)
	tree, err := parseTree(lf)
	if err != nil {
		return nil, refuse(nil, "the file is not valid YAML: %v", err)
	}
	if len(tree.Docs) > 1 {
		return nil, refuse(nil, "the file has more than one document")
	}
	value, err := decode(src)
	if err != nil {
		return nil, refuse(nil, "%v", err)
	}
	f := &file{src: lf, crs: crs, lines: lineStarts(lf), value: value, quote: '"', width: 2, seq: -1, item: 2}
	if len(tree.Docs) == 1 {
		f.body = tree.Docs[0].Body
	}
	if _, ok := f.body.(*ast.CommentGroupNode); ok {
		f.body = nil // only comments
	}
	f.detectConventions()
	return f, nil
}

// parseTree parses src with comments. The parser is not trusted to be panic-free on every input,
// so a panic becomes an error.
func parseTree(src []byte) (tree *ast.File, err error) {
	defer func() {
		if r := recover(); r != nil {
			tree, err = nil, fmt.Errorf("parser failure: %v", r)
		}
	}()
	tree, err = parser.ParseBytes(src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("%s", yaml.FormatError(err, false, false))
	}
	return tree, nil
}

// decode decodes src into a normalized document.
func decode(src []byte) (doc any, err error) {
	defer func() {
		if r := recover(); r != nil {
			doc, err = nil, fmt.Errorf("decoder failure: %v", r)
		}
	}()
	var v any
	if err := yaml.Unmarshal(src, &v); err != nil {
		return nil, fmt.Errorf("the file cannot be decoded: %s", yaml.FormatError(err, false, false))
	}
	doc, err = normalize(v, path{})
	if err != nil {
		return nil, fmt.Errorf("the file holds a value outside the JSON data model (%w)", err)
	}
	return doc, nil
}

// toLF returns src with CRLF line breaks turned into LF, and the offsets of those line breaks in
// the result.
func toLF(src []byte) ([]byte, []int) {
	if !bytes.Contains(src, []byte("\r\n")) {
		return src, nil
	}
	lf := make([]byte, 0, len(src))
	var crs []int
	for i := 0; i < len(src); i++ {
		if src[i] == '\r' && i+1 < len(src) && src[i+1] == '\n' {
			crs = append(crs, len(lf))
			continue
		}
		lf = append(lf, src[i])
	}
	return lf, crs
}

// original maps an offset in src to the same position in the original bytes.
func (f *file) original(at int) int {
	n, _ := slices.BinarySearch(f.crs, at)
	return at + n
}

func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, c := range src {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// detectConventions sets the quote character, indentation step and sequence style the file
// already uses, from the first examples it finds; files without examples get the defaults.
func (f *file) detectConventions() {
	if f.body == nil {
		f.seq = f.width
		return
	}
	var single, double int
	width, seq, item := 0, -1, 0
	ast.Walk(visitFunc(func(n ast.Node) {
		switch n.GetToken().Type {
		case token.SingleQuoteType:
			single++
		case token.DoubleQuoteType:
			double++
		}
		switch n := n.(type) {
		case *ast.MappingValueNode:
			keyCol := n.Key.GetToken().Position.Column
			switch v := n.Value.(type) {
			case *ast.MappingNode:
				if !v.IsFlowStyle && len(v.Values) > 0 && width == 0 {
					width = max(v.Values[0].Key.GetToken().Position.Column-keyCol, 0)
				}
			case *ast.SequenceNode:
				if !v.IsFlowStyle && len(v.Entries) > 0 && seq < 0 {
					seq = max(v.Entries[0].Start.Position.Column-keyCol, 0)
				}
			}
		case *ast.SequenceNode:
			for i, entry := range n.Entries {
				if m, ok := n.Values[i].(*ast.MappingNode); ok && !n.IsFlowStyle && !m.IsFlowStyle && len(m.Values) > 0 && item == 0 {
					item = m.Values[0].Key.GetToken().Position.Column - entry.Start.Position.Column
				}
			}
		}
	}), f.body)
	if single > double {
		f.quote = '\''
	}
	if width > 0 {
		f.width = width
	}
	f.seq = f.width
	if seq >= 0 {
		f.seq = seq
	}
	if item >= 2 {
		f.item = item
	}
}

type visitFunc func(ast.Node)

func (v visitFunc) Visit(n ast.Node) ast.Visitor {
	v(n)
	return v
}

// offset converts a token position (1-based line, 1-based column counted in runes) to a byte
// offset in src.
func (f *file) offset(pos *token.Position) (int, bool) {
	if pos == nil || pos.Line < 1 || pos.Line > len(f.lines) || pos.Column < 1 {
		return 0, false
	}
	at := f.lines[pos.Line-1]
	for range pos.Column - 1 {
		if at >= len(f.src) || f.src[at] == '\n' {
			return 0, false
		}
		_, size := utf8.DecodeRune(f.src[at:])
		at += size
	}
	return at, true
}

// location is where a value sits in the syntax tree.
type location struct {
	node   ast.Node
	owner  *token.Token // the ':' of the mapping entry or the '-' of the sequence entry holding node
	indent int          // indentation of the entry holding node, in columns
	flow   bool         // node is inside a flow collection
}

// locate finds the node at p, refusing paths through anchors, aliases, tags and merge keys.
func (f *file) locate(p path) (location, error) {
	loc := location{node: f.body}
	for i, step := range p {
		if err := checkUntangled(loc.node, p[:i]); err != nil {
			return location{}, err
		}
		switch n := loc.node.(type) {
		case *ast.MappingNode:
			loc.flow = loc.flow || n.IsFlowStyle
			mv, err := entry(n.Values, step, p[:i])
			if err != nil {
				return location{}, err
			}
			loc = loc.enter(mv)
		case *ast.MappingValueNode:
			loc.flow = loc.flow || n.IsFlowStyle
			mv, err := entry([]*ast.MappingValueNode{n}, step, p[:i])
			if err != nil {
				return location{}, err
			}
			loc = loc.enter(mv)
		case *ast.SequenceNode:
			loc.flow = loc.flow || n.IsFlowStyle
			idx, ok := step.(int)
			if !ok || idx < 0 || idx >= len(n.Values) {
				return location{}, refuse(p[:i+1], "the value is not in the syntax tree")
			}
			loc.node, loc.owner = n.Values[idx], nil
			if idx < len(n.Entries) && n.Entries[idx].Start != nil {
				loc.owner = n.Entries[idx].Start
				loc.indent = loc.owner.Position.Column - 1
			}
		default:
			return location{}, refuse(p[:i+1], "the value is not in the syntax tree")
		}
	}
	if err := checkUntangled(loc.node, p); err != nil {
		return location{}, err
	}
	return loc, nil
}

func (loc location) enter(mv *ast.MappingValueNode) location {
	loc.node, loc.owner = mv.Value, mv.Start
	loc.indent = mv.Key.GetToken().Position.Column - 1
	return loc
}

// entry finds the mapping entry for key step among values.
func entry(values []*ast.MappingValueNode, step any, p path) (*ast.MappingValueNode, error) {
	key, ok := step.(string)
	if !ok {
		return nil, refuse(p, "the value is not in the syntax tree")
	}
	for _, mv := range values {
		if mv.Key.IsMergeKey() {
			return nil, refuse(p, "the mapping uses a merge key (<<)")
		}
	}
	for _, mv := range values {
		k, ok := mv.Key.(ast.ScalarNode)
		if !ok {
			continue
		}
		name := k.GetToken().Value
		if s, ok := k.(*ast.StringNode); ok {
			name = s.Value
		}
		if name == key {
			return mv, nil
		}
	}
	return nil, refuse(p.key(key), "the value is not in the syntax tree")
}

// checkUntangled refuses nodes whose edit would reach beyond themselves (anchors and aliases) or
// whose meaning depends on more than their text (tags).
func checkUntangled(n ast.Node, p path) error {
	switch n.(type) {
	case *ast.AnchorNode:
		return refuse(p, "the value has an anchor (&); editing it would also change its aliases")
	case *ast.AliasNode:
		return refuse(p, "the value is an alias (*); editing it would change the anchored value")
	case *ast.TagNode:
		return refuse(p, "the value has an explicit tag")
	}
	return nil
}

// scalarValue returns the normalized value of a scalar node.
func scalarValue(n ast.Node) (any, bool) {
	switch n := n.(type) {
	case *ast.LiteralNode:
		if n.Value == nil {
			return "", true
		}
		return n.Value.Value, true
	case *ast.StringNode, *ast.IntegerNode, *ast.FloatNode, *ast.BoolNode, *ast.NullNode:
		v, err := normalize(n.(ast.ScalarNode).GetValue(), nil)
		return v, err == nil
	}
	return nil, false
}

// slot is the span of an original scalar and what its replacement must fit.
type slot struct {
	start, end  int
	kind        token.Type // the original scalar's token type
	insert      bool       // the original is an empty value (`key:`); the replacement is inserted
	block       bool       // a block scalar may be written here
	blockIndent int        // content indentation of the original block scalar, if it is one
	indent      int        // indentation of the entry holding the value
	flow        bool
	nl          string // line break for multi-line replacements
}

// slot finds the bytes of the scalar at loc.
func (f *file) slot(loc location, p path) (slot, error) {
	s := slot{kind: loc.node.GetToken().Type, indent: loc.indent, flow: loc.flow, block: !loc.flow && loc.owner != nil}
	if isEmpty(loc.node) {
		if loc.flow || loc.owner == nil {
			return slot{}, refuse(p, "cannot fill an empty value in a flow collection")
		}
		_, at, err := f.tokenSpan(loc.owner, p)
		if err != nil {
			return slot{}, err
		}
		s.start, s.end, s.insert, s.block, s.nl = at, at, true, false, f.newline(at)
		return s, nil
	}
	if _, ok := loc.node.(*ast.LiteralNode); ok && !s.block {
		return slot{}, refuse(p, "the block scalar is not inside a block mapping or sequence")
	}
	start, end, blockIndent, err := f.scalarSpan(loc.node, loc.indent, p)
	if err != nil {
		return slot{}, err
	}
	s.start, s.end, s.blockIndent, s.nl = start, end, blockIndent, f.newline(start)
	return s, nil
}

// isEmpty reports whether n is an empty value (`key:` or `-`), which has no bytes of its own.
func isEmpty(n ast.Node) bool {
	null, ok := n.(*ast.NullNode)
	return ok && null.Token.Type == token.ImplicitNullType
}

// scalarSpan returns the bytes of a scalar node. indent is the indentation of the entry holding
// it, which bounds the content of a block scalar. For a block scalar it also returns the content
// indentation.
func (f *file) scalarSpan(n ast.Node, indent int, p path) (start, end, blockIndent int, err error) {
	if lit, ok := n.(*ast.LiteralNode); ok {
		return f.blockSpan(lit, indent, p)
	}
	tk := n.GetToken()
	raw := strings.TrimSpace(tk.Origin)
	start, ok := f.find(tk.Position, raw)
	if !ok {
		return 0, 0, 0, refuse(p, "the value's bytes are not where the syntax tree places them")
	}
	end = start + len(raw)
	switch tk.Type {
	case token.DoubleQuoteType, token.SingleQuoteType:
		if quotedEnd(f.src, start) != end {
			return 0, 0, 0, refuse(p, "the quoted value's bytes are not where the syntax tree places them")
		}
	default:
		// A plain scalar, possibly folded over several lines: its words must be the token's.
		if strings.Join(strings.Fields(raw), " ") != strings.Join(strings.Fields(tk.Value), " ") {
			return 0, 0, 0, refuse(p, "the value's bytes are not where the syntax tree places them")
		}
	}
	return start, end, 0, nil
}

// blockSpan handles a block scalar (`|` or `>`): the span runs from the header to the end of the
// last non-blank content line, so the line break after it and any trailing blank lines stay.
func (f *file) blockSpan(n *ast.LiteralNode, indent int, p path) (start, end, contentIndent int, err error) {
	header := n.Start.Value
	start, ok := f.find(n.Start.Position, header)
	if !ok {
		return 0, 0, 0, refuse(p, "the block scalar's bytes are not where the syntax tree places them")
	}
	if strings.ContainsAny(header, "123456789") {
		return 0, 0, 0, refuse(p, "the block scalar has an explicit indentation indicator")
	}
	end = start + len(header)
	lineEnd := end + indexOrEnd(f.src[end:], '\n')
	if strings.TrimSpace(string(f.src[end:lineEnd])) != "" {
		return 0, 0, 0, refuse(p, "a comment follows the block scalar header")
	}
	for at := lineEnd + 1; at < len(f.src); {
		next := at + indexOrEnd(f.src[at:], '\n')
		line := strings.TrimSuffix(string(f.src[at:next]), "\r")
		if strings.TrimLeft(line, " ") != "" {
			lineIndent := len(line) - len(strings.TrimLeft(line, " "))
			if contentIndent == 0 {
				if lineIndent <= indent {
					break
				}
				contentIndent = lineIndent
			}
			if lineIndent < contentIndent {
				break
			}
			end = at + len(line)
		}
		at = next + 1
	}
	return start, end, contentIndent, nil
}

// tokenSpan returns the bytes of a token, such as a key, an indicator or a bracket.
func (f *file) tokenSpan(tk *token.Token, p path) (start, end int, err error) {
	raw := strings.TrimSpace(tk.Origin)
	start, ok := f.find(tk.Position, raw)
	if !ok {
		return 0, 0, refuse(p, "the syntax tree's positions do not match the file")
	}
	return start, start + len(raw), nil
}

// find returns the offset of a token's text raw at pos. goccy reports the column of a token
// followed by spaces or tabs on its line shifted right by the length of that whitespace, so when
// raw is not at pos, find looks for it that far to the left: the token must end exactly where
// whitespace reaching up to the reported end begins.
func (f *file) find(pos *token.Position, raw string) (int, bool) {
	at, ok := f.offset(pos)
	if !ok || raw == "" {
		return 0, false
	}
	if bytes.HasPrefix(f.src[at:], []byte(raw)) {
		return at, true
	}
	for start := at - 1; start >= f.lineStart(at); start-- {
		end := start + len(raw)
		if end+(at-start) > len(f.src) || !bytes.HasPrefix(f.src[start:], []byte(raw)) {
			continue
		}
		if strings.Trim(string(f.src[end:end+(at-start)]), " \t") == "" {
			return start, true
		}
	}
	return 0, false
}

// nodeStart returns the offset of the first byte of n.
func (f *file) nodeStart(n ast.Node, p path) (int, error) {
	var tk *token.Token
	switch n := n.(type) {
	case *ast.MappingNode:
		if n.IsFlowStyle || len(n.Values) == 0 {
			tk = n.Start
		} else {
			return f.nodeStart(n.Values[0].Key, p)
		}
	case *ast.MappingValueNode:
		return f.nodeStart(n.Key, p)
	case *ast.SequenceNode:
		tk = n.Start
		if !n.IsFlowStyle && len(n.Entries) > 0 {
			tk = n.Entries[0].Start
		}
	case *ast.LiteralNode:
		tk = n.Start
	case *ast.TagNode:
		tk = n.Start
	case *ast.AnchorNode:
		tk = n.Start
	case *ast.AliasNode:
		tk = n.Start
	default:
		tk = n.GetToken()
	}
	start, _, err := f.tokenSpan(tk, p)
	return start, err
}

// nodeEnd returns the offset just past the last byte of n, not counting a trailing comment.
// indent is the indentation of the entry holding n.
func (f *file) nodeEnd(n ast.Node, indent int, p path) (int, error) {
	switch n := n.(type) {
	case *ast.MappingNode:
		if n.IsFlowStyle || len(n.Values) == 0 {
			_, end, err := f.tokenSpan(n.End, p)
			return end, err
		}
		return f.entryEnd(n.Values[len(n.Values)-1], p)
	case *ast.MappingValueNode:
		return f.entryEnd(n, p)
	case *ast.SequenceNode:
		if n.IsFlowStyle || len(n.Values) == 0 {
			_, end, err := f.tokenSpan(n.End, p)
			return end, err
		}
		last := len(n.Values) - 1
		if last >= len(n.Entries) {
			return 0, refuse(p, "the syntax tree's positions do not match the file")
		}
		return f.itemEnd(n.Entries[last].Start, n.Values[last], p)
	case *ast.TagNode:
		return f.nodeEnd(n.Value, indent, p)
	case *ast.AnchorNode:
		return f.nodeEnd(n.Value, indent, p)
	case *ast.AliasNode:
		_, end, err := f.tokenSpan(n.Value.GetToken(), p)
		return end, err
	}
	if isEmpty(n) {
		return 0, refuse(p, "the syntax tree's positions do not match the file")
	}
	_, end, _, err := f.scalarSpan(n, indent, p)
	return end, err
}

// entryEnd returns the end of a mapping entry's value, or of its ':' when the value is empty.
func (f *file) entryEnd(mv *ast.MappingValueNode, p path) (int, error) {
	if isEmpty(mv.Value) {
		_, end, err := f.tokenSpan(mv.Start, p)
		return end, err
	}
	return f.nodeEnd(mv.Value, mv.Key.GetToken().Position.Column-1, p)
}

// itemEnd returns the end of a block sequence item, or of its '-' when the item is empty.
func (f *file) itemEnd(dash *token.Token, value ast.Node, p path) (int, error) {
	if isEmpty(value) {
		_, end, err := f.tokenSpan(dash, p)
		return end, err
	}
	return f.nodeEnd(value, dash.Position.Column-1, p)
}

func indexOrEnd(b []byte, c byte) int {
	if i := bytes.IndexByte(b, c); i >= 0 {
		return i
	}
	return len(b)
}

// lineStart returns the offset of the start of the line holding at.
func (f *file) lineStart(at int) int {
	return bytes.LastIndexByte(f.src[:at], '\n') + 1
}

// lineAfter returns the offset of the start of the line after the one holding at, or the end of
// the file when that line is the last one and has no line break.
func (f *file) lineAfter(at int) int {
	return at + min(indexOrEnd(f.src[at:], '\n')+1, len(f.src)-at)
}

// quotedEnd returns the offset just past the quoted scalar starting at src[start], or -1.
func quotedEnd(src []byte, start int) int {
	q := src[start]
	for i := start + 1; i < len(src); i++ {
		switch {
		case q == '"' && src[i] == '\\':
			i++
		case src[i] == q && q == '\'' && i+1 < len(src) && src[i+1] == '\'':
			i++
		case src[i] == q:
			return i + 1
		}
	}
	return -1
}

// newline returns the line break the original uses for the line holding offset at.
func (f *file) newline(at int) string {
	if i := bytes.IndexByte(f.src[at:], '\n'); i >= 0 {
		if _, crlf := slices.BinarySearch(f.crs, at+i); crlf {
			return "\r\n"
		}
		return "\n"
	}
	if len(f.crs) > 0 {
		return "\r\n"
	}
	return "\n"
}

// hasAnchor reports whether n or anything inside it has an anchor, which aliases elsewhere may
// depend on.
func hasAnchor(n ast.Node) bool {
	found := false
	ast.Walk(visitFunc(func(n ast.Node) {
		if _, ok := n.(*ast.AnchorNode); ok {
			found = true
		}
	}), n)
	return found
}

// textEdit replaces src[start:end] with text. Edits at the same offset apply deepest first, so
// content added inside a node comes before content added after it.
type textEdit struct {
	start, end int
	text       string
	depth      int
}

// setScalar turns a scalar change into a text edit.
func (f *file) setScalar(c change) (textEdit, error) {
	loc, err := f.locate(c.path)
	if err != nil {
		return textEdit{}, err
	}
	// The node must hold the value the document decoded to, or the tree and the decoder disagree
	// about what is where.
	if v, ok := scalarValue(loc.node); !ok || !equal(v, c.from) {
		return textEdit{}, refuse(c.path, "the syntax tree and the decoded document disagree")
	}
	s, err := f.slot(loc, c.path)
	if err != nil {
		return textEdit{}, err
	}
	text := f.render(c.to, s)
	if s.insert {
		text = " " + text
	}
	return f.keepLineComment(textEdit{start: s.start, end: s.end, text: text, depth: len(c.path)}), nil
}
