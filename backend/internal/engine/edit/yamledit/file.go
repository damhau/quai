package yamledit

import (
	"bytes"
	"fmt"
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
	src   []byte
	lines []int    // byte offset of the start of each line
	body  ast.Node // the document's root node; nil for an empty document
	value any      // the decoded, normalized document
	quote byte     // quote character for strings that need quoting
	width int      // indentation step, in columns
}

func parse(src []byte) (*file, error) {
	if bytes.HasPrefix(src, []byte{0xEF, 0xBB, 0xBF}) {
		return nil, refuse(nil, "the file starts with a byte-order mark")
	}
	if !utf8.Valid(src) {
		return nil, refuse(nil, "the file is not valid UTF-8")
	}
	tree, err := parseTree(src)
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
	f := &file{src: src, lines: lineStarts(src), value: value, quote: '"', width: 2}
	if len(tree.Docs) == 1 {
		f.body = tree.Docs[0].Body
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

func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, c := range src {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// detectConventions sets the quote character and indentation step the file already uses.
func (f *file) detectConventions() {
	if f.body != nil {
		var single, double int
		ast.Walk(visitFunc(func(n ast.Node) {
			switch n.GetToken().Type {
			case token.SingleQuoteType:
				single++
			case token.DoubleQuoteType:
				double++
			}
		}), f.body)
		if single > double {
			f.quote = '\''
		}
	}
	width := 0
	for _, line := range strings.Split(string(f.src), "\n") {
		content := strings.TrimLeft(line, " ")
		indent := len(line) - len(content)
		if indent > 0 && strings.TrimSpace(content) != "" && !strings.HasPrefix(content, "#") && (width == 0 || indent < width) {
			width = indent
		}
	}
	if width > 0 {
		f.width = width
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
}

// slot finds the bytes of the scalar at loc.
func (f *file) slot(loc location, p path) (slot, error) {
	s := slot{kind: loc.node.GetToken().Type, indent: loc.indent, flow: loc.flow, block: !loc.flow && loc.owner != nil}
	switch n := loc.node.(type) {
	case *ast.LiteralNode:
		return f.blockSlot(n, s, p)
	case *ast.NullNode:
		if n.Token.Type == token.ImplicitNullType {
			return f.emptySlot(loc, s, p)
		}
	}
	tk := loc.node.GetToken()
	start, ok := f.offset(tk.Position)
	if !ok {
		return slot{}, refuse(p, "the value's position is outside the file")
	}
	raw := strings.TrimSpace(tk.Origin)
	end := start + len(raw)
	if raw == "" || !bytes.HasPrefix(f.src[start:], []byte(raw)) {
		return slot{}, refuse(p, "the value's bytes are not where the syntax tree places them")
	}
	switch tk.Type {
	case token.DoubleQuoteType, token.SingleQuoteType:
		if quotedEnd(f.src, start) != end {
			return slot{}, refuse(p, "the quoted value's bytes are not where the syntax tree places them")
		}
	default:
		// A plain scalar, possibly folded over several lines: its words must be the token's.
		if strings.Join(strings.Fields(raw), " ") != strings.Join(strings.Fields(tk.Value), " ") {
			return slot{}, refuse(p, "the value's bytes are not where the syntax tree places them")
		}
	}
	s.start, s.end = start, end
	return s, nil
}

// emptySlot handles an empty value (`key:` or `-`): the replacement goes right after the
// indicator.
func (f *file) emptySlot(loc location, s slot, p path) (slot, error) {
	if loc.flow || loc.owner == nil {
		return slot{}, refuse(p, "cannot fill an empty value in a flow collection")
	}
	at, ok := f.offset(loc.owner.Position)
	if !ok || !bytes.HasPrefix(f.src[at:], []byte(loc.owner.Value)) {
		return slot{}, refuse(p, "the entry's bytes are not where the syntax tree places them")
	}
	at += len(loc.owner.Value)
	s.start, s.end, s.insert, s.block = at, at, true, false
	return s, nil
}

// blockSlot handles a block scalar (`|` or `>`): the span runs from the header to the end of the
// last non-blank content line, so the line break after it and any trailing blank lines stay.
func (f *file) blockSlot(n *ast.LiteralNode, s slot, p path) (slot, error) {
	if !s.block {
		return slot{}, refuse(p, "the block scalar is not inside a block mapping or sequence")
	}
	header := n.Start.Value
	start, ok := f.offset(n.Start.Position)
	if !ok || !bytes.HasPrefix(f.src[start:], []byte(header)) {
		return slot{}, refuse(p, "the block scalar's bytes are not where the syntax tree places them")
	}
	if strings.ContainsAny(header, "123456789") {
		return slot{}, refuse(p, "the block scalar has an explicit indentation indicator")
	}
	end := start + len(header)
	lineEnd := end + indexOrEnd(f.src[end:], '\n')
	if strings.TrimSpace(string(f.src[end:lineEnd])) != "" {
		return slot{}, refuse(p, "a comment follows the block scalar header")
	}
	contentIndent := 0
	for at := lineEnd + 1; at < len(f.src); {
		next := at + indexOrEnd(f.src[at:], '\n')
		line := strings.TrimSuffix(string(f.src[at:next]), "\r")
		if strings.TrimLeft(line, " ") != "" {
			indent := len(line) - len(strings.TrimLeft(line, " "))
			if contentIndent == 0 {
				if indent <= s.indent {
					break
				}
				contentIndent = indent
			}
			if indent < contentIndent {
				break
			}
			end = at + len(line)
		}
		at = next + 1
	}
	s.start, s.end, s.blockIndent = start, end, contentIndent
	return s, nil
}

func indexOrEnd(b []byte, c byte) int {
	if i := bytes.IndexByte(b, c); i >= 0 {
		return i
	}
	return len(b)
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

// newline returns the line break used by the line holding offset at.
func (f *file) newline(at int) string {
	if i := bytes.IndexByte(f.src[at:], '\n'); i >= 0 {
		if at+i > 0 && f.src[at+i-1] == '\r' {
			return "\r\n"
		}
		return "\n"
	}
	if bytes.HasSuffix(f.src[:at], []byte("\r\n")) || bytes.Contains(f.src, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// textEdit replaces src[start:end] with text.
type textEdit struct {
	start, end int
	text       string
}

// edit turns a scalar change into a text edit.
func (f *file) edit(c change) (textEdit, error) {
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
	text, err := f.render(c.to, s, c.path)
	if err != nil {
		return textEdit{}, err
	}
	if s.insert {
		text = " " + text
	}
	return textEdit{start: s.start, end: s.end, text: text}, nil
}
