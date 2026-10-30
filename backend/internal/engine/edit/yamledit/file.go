package yamledit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

// file is a parsed original: its bytes, syntax tree and decoded document, plus the conventions
// detected from it that new content follows.
type file struct {
	src   []byte
	lines []int          // offset of the start of each line
	body  *yaml.Node     // the document's root node; nil for an empty document
	value any            // the decoded, normalized document
	quote byte           // quote character for strings that need quoting
	width int            // indentation step of nested mappings, in columns
	seq   int            // indentation of a block sequence under its key: width, or 0 for `key:\n- item`
	item  int            // offset from an item's '-' to its content, usually 2 for "- "
	rank  map[string]int // the order of new keys, from Create or the options; nil for sorted order
	opts  document.Options
	// like is the sibling node a new entry or item follows while it is written: what the sibling
	// writes in flow style, the new one does too. pad: flowText pads brackets with spaces.
	like *yaml.Node
	pad  bool
}

func parse(src []byte) (*file, error) {
	if bytes.HasPrefix(src, []byte{0xEF, 0xBB, 0xBF}) {
		return nil, refuse(nil, "the file starts with a byte-order mark")
	}
	if !utf8.Valid(src) {
		return nil, refuse(nil, "the file is not valid UTF-8")
	}
	if otherBreaks(src) {
		return nil, refuse(nil, "the file has line breaks other than LF and CRLF")
	}
	docs, err := parseTree(src)
	if err != nil {
		return nil, refuse(nil, "the file is not valid YAML: %v", err)
	}
	if len(docs) > 1 {
		return nil, refuse(nil, "the file has more than one document")
	}
	f := &file{src: src, lines: lineStarts(src), quote: '"', width: 2, seq: -1, item: 2}
	if len(docs) == 1 && len(docs[0].Content) == 1 && !isEmpty(docs[0].Content[0]) {
		f.body = docs[0].Content[0]
	}
	if f.value, err = value(f.body); err != nil {
		return nil, refuse(nil, "%v", err)
	}
	f.detectConventions()
	return f, nil
}

// parseTree parses src into its documents. The parser is not trusted to be panic-free on every
// input, so a panic becomes an error.
func parseTree(src []byte) (docs []*yaml.Node, err error) {
	defer func() {
		if r := recover(); r != nil {
			docs, err = nil, fmt.Errorf("parser failure: %v", r)
		}
	}()
	dec := yaml.NewDecoder(bytes.NewReader(src))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, err
		}
		docs = append(docs, &doc)
	}
}

// decode decodes the first document of src into a normalized document.
func decode(src []byte) (any, error) {
	docs, err := parseTree(src)
	if err != nil {
		return nil, fmt.Errorf("the file is not valid YAML: %v", err)
	}
	if len(docs) == 0 || len(docs[0].Content) == 0 {
		return nil, nil
	}
	return value(docs[0].Content[0])
}

// value converts a syntax tree into a normalized document.
func value(n *yaml.Node) (any, error) {
	if n == nil {
		return nil, nil
	}
	c := converter{budget: maxValues, open: map[*yaml.Node]bool{}}
	v, err := c.convert(n)
	if err != nil {
		return nil, err
	}
	doc, err := normalize(v, path{})
	if err != nil {
		return nil, fmt.Errorf("the file holds a value outside the JSON data model (%w)", err)
	}
	return doc, nil
}

// maxValues bounds the number of values a document may expand to through aliases.
const maxValues = 1 << 20

// converter turns a syntax tree into a document. Scalars are resolved by yaml/v3, except
// timestamps, which keep their text; mapping keys are their text, as the JSON data model has only
// string keys. yaml/v3's own decoder would turn both into other Go types.
type converter struct {
	budget int
	open   map[*yaml.Node]bool // collections being converted, to catch anchors that contain themselves
}

func (c *converter) convert(n *yaml.Node) (any, error) {
	if c.budget--; c.budget < 0 {
		return nil, errors.New("the file's aliases expand to too many values")
	}
	switch n.Kind {
	case yaml.AliasNode:
		if n.Alias == nil {
			return nil, fmt.Errorf("the file is not valid YAML: unknown alias *%s", n.Value)
		}
		return c.convert(n.Alias)
	case yaml.ScalarNode:
		return scalar(n)
	case yaml.SequenceNode, yaml.MappingNode:
		if c.open[n] {
			return nil, fmt.Errorf("the file is not valid YAML: the value of anchor &%s contains itself", n.Anchor)
		}
		c.open[n] = true
		defer delete(c.open, n)
		if n.Kind == yaml.MappingNode {
			return c.mapping(n)
		}
		out := make([]any, len(n.Content))
		for i, item := range n.Content {
			v, err := c.convert(item)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	return nil, fmt.Errorf("the file is not valid YAML: unexpected node kind %d", n.Kind)
}

// mapping converts a mapping. Like yaml/v3's decoder, it adds the entries of the mappings named
// by a merge key (<<) that the mapping does not set itself, earlier mappings first.
func (c *converter) mapping(n *yaml.Node) (any, error) {
	out := map[string]any{}
	lines := map[string]int{}
	var merge *yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if isMerge(k) {
			merge = v
			continue
		}
		key, err := keyText(k)
		if err != nil {
			return nil, err
		}
		if line, ok := lines[key]; ok {
			return nil, fmt.Errorf("the file is not valid YAML: mapping key %q is defined twice (lines %d and %d)", key, line, k.Line)
		}
		lines[key] = k.Line
		if out[key], err = c.convert(v); err != nil {
			return nil, err
		}
	}
	if merge == nil {
		return out, nil
	}
	sources := []*yaml.Node{merge}
	if merge.Kind == yaml.SequenceNode {
		sources = merge.Content
	}
	for _, s := range sources {
		target := s
		if s.Kind == yaml.AliasNode {
			target = s.Alias
		}
		if target == nil || target.Kind != yaml.MappingNode {
			return nil, errors.New("the file is not valid YAML: a merge key's value is not a mapping")
		}
		m, err := c.convert(s)
		if err != nil {
			return nil, err
		}
		for k, v := range m.(map[string]any) {
			if _, ok := out[k]; !ok {
				out[k] = v
			}
		}
	}
	return out, nil
}

// keyText returns a mapping key as a string: its text, whatever type it resolves to.
func keyText(k *yaml.Node) (string, error) {
	if k.Kind == yaml.AliasNode && k.Alias != nil {
		k = k.Alias
	}
	if k.Kind != yaml.ScalarNode {
		return "", errors.New("the file holds a value outside the JSON data model (a mapping key is not a scalar)")
	}
	return k.Value, nil
}

// scalar resolves a scalar the way yaml/v3 does, except that a timestamp stays a string.
func scalar(n *yaml.Node) (any, error) {
	if n.ShortTag() == "!!timestamp" {
		return n.Value, nil
	}
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, fmt.Errorf("the file is not valid YAML: %v", err)
	}
	return v, nil
}

// isMerge reports whether k is a merge key (<<), as yaml/v3 recognizes one.
func isMerge(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.Value == "<<" && (k.Tag == "" || k.Tag == "!" || k.ShortTag() == "!!merge")
}

// otherBreaks reports whether src has line breaks that the parser counts as lines and this
// package does not: a CR on its own, NEL, LS or PS.
func otherBreaks(src []byte) bool {
	for i, c := range src {
		if c == '\r' && (i+1 == len(src) || src[i+1] != '\n') {
			return true
		}
	}
	return bytes.Contains(src, []byte("\u0085")) || bytes.Contains(src, []byte(" ")) || bytes.Contains(src, []byte(" "))
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
	block := func(n *yaml.Node) bool { return n.Style&yaml.FlowStyle == 0 && len(n.Content) > 0 }
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		switch {
		case n.Style&yaml.SingleQuotedStyle != 0:
			single++
		case n.Style&yaml.DoubleQuotedStyle != 0:
			double++
		}
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				switch {
				case v.Kind == yaml.MappingNode && block(v) && width == 0:
					width = max(v.Content[0].Column-k.Column, 0)
				case v.Kind == yaml.SequenceNode && block(v) && seq < 0:
					if d, err := f.dash(v, 0, nil); err == nil {
						seq = max(f.indentAt(d)+1-k.Column, 0)
					}
				}
			}
		case yaml.SequenceNode:
			for i, v := range n.Content {
				if item == 0 && block(n) && v.Kind == yaml.MappingNode && block(v) {
					if d, err := f.dash(n, i, nil); err == nil {
						item = v.Content[0].Column - (f.indentAt(d) + 1)
					}
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(f.body)
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

// offset converts a node's position (1-based line, 1-based column counted in runes) to a byte
// offset in src.
func (f *file) offset(n *yaml.Node) (int, bool) {
	if n.Line < 1 || n.Line > len(f.lines) || n.Column < 1 {
		return 0, false
	}
	at := f.lines[n.Line-1]
	for range n.Column - 1 {
		if at >= len(f.src) || f.src[at] == '\n' {
			return 0, false
		}
		_, size := utf8.DecodeRune(f.src[at:])
		at += size
	}
	return at, true
}

// column returns the column of a node, counted from zero.
func column(n *yaml.Node) int {
	return n.Column - 1
}

// indentAt returns the column of offset at, counted from zero.
func (f *file) indentAt(at int) int {
	return utf8.RuneCount(f.src[f.lineStart(at):at])
}

// location is where a value sits in the syntax tree.
type location struct {
	node   *yaml.Node
	owner  int  // the offset just past the ':' or '-' of the block entry holding node; -1 for none
	dash   bool // the owner is a '-'
	indent int  // indentation of the entry holding node, in columns
	flow   bool // node is inside a flow collection
}

// locate finds the node at p, refusing paths through anchors, aliases, tags and merge keys.
func (f *file) locate(p path) (location, error) {
	loc := location{node: f.body, owner: -1}
	for i, step := range p {
		n := loc.node
		if n == nil {
			return location{}, refuse(p[:i+1], "the value is not in the syntax tree")
		}
		if err := checkUntangled(n, p[:i]); err != nil {
			return location{}, err
		}
		loc.flow = loc.flow || n.Style&yaml.FlowStyle != 0
		switch n.Kind {
		case yaml.MappingNode:
			k, v, err := entry(n, step, p[:i])
			if err != nil {
				return location{}, err
			}
			loc.node, loc.owner, loc.dash, loc.indent = v, -1, false, column(k)
			if !loc.flow {
				if loc.owner, err = f.colon(k, p[:i+1]); err != nil {
					return location{}, err
				}
			}
		case yaml.SequenceNode:
			idx, ok := step.(int)
			if !ok || idx < 0 || idx >= len(n.Content) {
				return location{}, refuse(p[:i+1], "the value is not in the syntax tree")
			}
			loc.node, loc.owner, loc.dash = n.Content[idx], -1, true
			if !loc.flow {
				d, err := f.dash(n, idx, p[:i+1])
				if err != nil {
					return location{}, err
				}
				loc.owner, loc.indent = d+1, f.indentAt(d)
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

// entry finds the key and value of the mapping entry for key step.
func entry(n *yaml.Node, step any, p path) (k, v *yaml.Node, err error) {
	key, ok := step.(string)
	if !ok {
		return nil, nil, refuse(p, "the value is not in the syntax tree")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if isMerge(n.Content[i]) {
			return nil, nil, refuse(p, "the mapping uses a merge key (<<)")
		}
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i]; k.Kind == yaml.ScalarNode && k.Value == key {
			return k, n.Content[i+1], nil
		}
	}
	return nil, nil, refuse(p.Key(key), "the value is not in the syntax tree")
}

// checkUntangled refuses nodes whose edit would reach beyond themselves (anchors and aliases) or
// whose meaning depends on more than their text (tags).
func checkUntangled(n *yaml.Node, p path) error {
	switch {
	case n == nil:
		return nil
	case n.Kind == yaml.AliasNode:
		return refuse(p, "the value is an alias (*); editing it would change the anchored value")
	case n.Anchor != "":
		return refuse(p, "the value has an anchor (&); editing it would also change its aliases")
	case n.Style&yaml.TaggedStyle != 0:
		return refuse(p, "the value has an explicit tag")
	}
	return nil
}

// hasProperties reports whether n has an anchor or an explicit tag before its content.
func hasProperties(n *yaml.Node) bool {
	return n.Anchor != "" || n.Style&yaml.TaggedStyle != 0
}

// scalarValue returns the normalized value of a scalar node.
func scalarValue(n *yaml.Node) (any, bool) {
	if n == nil || n.Kind != yaml.ScalarNode {
		return nil, false
	}
	v, err := value(n)
	return v, err == nil
}

// scalarStyle is how a scalar is written.
type scalarStyle int

const (
	stylePlain scalarStyle = iota
	styleSingle
	styleDouble
	styleLiteral
	styleFolded
)

func styleOf(n *yaml.Node) scalarStyle {
	switch {
	case n.Style&yaml.DoubleQuotedStyle != 0:
		return styleDouble
	case n.Style&yaml.SingleQuotedStyle != 0:
		return styleSingle
	case n.Style&yaml.LiteralStyle != 0:
		return styleLiteral
	case n.Style&yaml.FoldedStyle != 0:
		return styleFolded
	}
	return stylePlain
}

// slot is the span of an original scalar and what its replacement must fit.
type slot struct {
	start, end  int
	kind        scalarStyle // the original scalar's style
	insert      bool        // the original is an empty value (`key:`); the replacement is inserted
	block       bool        // a block scalar may be written here
	blockIndent int         // content indentation of the original block scalar, if it is one
	indent      int         // indentation of the entry holding the value
	flow        bool
	nl          string // line break for multi-line replacements
}

// slot finds the bytes of the scalar at loc.
func (f *file) slot(loc location, p path) (slot, error) {
	s := slot{kind: styleOf(loc.node), indent: loc.indent, flow: loc.flow, block: !loc.flow && loc.owner >= 0}
	if isEmpty(loc.node) {
		if loc.flow || loc.owner < 0 {
			return slot{}, refuse(p, "cannot fill an empty value in a flow collection")
		}
		at := loc.owner
		if !f.emptyAfter(at) {
			return slot{}, refuse(p, "the syntax tree's positions do not match the file")
		}
		s.start, s.end, s.insert, s.block, s.nl = at, at, true, false, f.newline(at)
		return s, nil
	}
	if (s.kind == styleLiteral || s.kind == styleFolded) && !s.block {
		return slot{}, refuse(p, "the block scalar is not inside a block mapping or sequence")
	}
	start, end, blockIndent, err := f.scalarSpan(loc.node, loc.indent, p)
	if err != nil {
		return slot{}, err
	}
	s.start, s.end, s.blockIndent, s.nl = start, end, blockIndent, f.newline(start)
	return s, nil
}

// emptyAfter reports whether only blanks or a comment follow offset at on its line, as they must
// after the ':' or '-' of an empty value. yaml/v3 reports a value that is only a non-specific tag
// (`key: !`) as an untagged empty value.
func (f *file) emptyAfter(at int) bool {
	for at < len(f.src) && (f.src[at] == ' ' || f.src[at] == '\t') {
		at++
	}
	return at == len(f.src) || f.src[at] == '\r' || f.src[at] == '\n' || f.src[at] == '#'
}

// isEmpty reports whether n is an empty value (`key:` or `-`), which has no bytes of its own.
func isEmpty(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.Value == "" && n.Style == 0 && n.Anchor == "" && n.ShortTag() == "!!null"
}

// scalarSpan returns the bytes of a scalar's content, after any anchor and tag. indent is the
// indentation of the entry holding it, which bounds the content of a block scalar. For a block
// scalar it also returns the content indentation. Every span is checked against the node: the
// parser's positions are only trusted when the file holds the node's text there.
func (f *file) scalarSpan(n *yaml.Node, indent int, p path) (start, end, blockIndent int, err error) {
	at, ok := f.offset(n)
	if !ok {
		return 0, 0, 0, refuse(p, "the value's bytes are not where the syntax tree places them")
	}
	style := styleOf(n)
	if hasProperties(n) {
		props, ok := f.propertiesEnd(at)
		if !ok {
			return 0, 0, 0, refuse(p, "the value's bytes are not where the syntax tree places them")
		}
		if style == stylePlain && n.Value == "" {
			// An empty value with an anchor or a tag: the properties are its only bytes.
			return at, props, 0, nil
		}
		at = f.skipBlank(props)
	}
	switch style {
	case styleLiteral, styleFolded:
		return f.blockSpan(at, indent, p)
	case styleSingle, styleDouble:
		q := byte('"')
		if style == styleSingle {
			q = '\''
		}
		if at >= len(f.src) || f.src[at] != q {
			return 0, 0, 0, refuse(p, "the quoted value's bytes are not where the syntax tree places them")
		}
		end = quotedEnd(f.src, at)
		if end < 0 || !sameScalar(f.src[at:end], n) {
			return 0, 0, 0, refuse(p, "the quoted value's bytes are not where the syntax tree places them")
		}
		return at, end, 0, nil
	}
	end, ok = f.plainEnd(at, n.Value)
	if !ok {
		return 0, 0, 0, refuse(p, "the value's bytes are not where the syntax tree places them")
	}
	return at, end, 0, nil
}

// plainEnd returns the end of the plain scalar with the given value that starts at start. The
// scalar may be folded over several lines: its words must be the value's, separated only by
// spaces, tabs and line breaks.
func (f *file) plainEnd(start int, value string) (int, bool) {
	at := start
	words := strings.FieldsFunc(value, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' })
	for i, w := range words {
		if i > 0 {
			gap := at
			for at < len(f.src) && isBlank(f.src[at]) {
				at++
			}
			if at == gap {
				return 0, false
			}
		}
		if !bytes.HasPrefix(f.src[at:], []byte(w)) {
			return 0, false
		}
		at += len(w)
	}
	return at, at > start
}

// sameScalar reports whether text, parsed on its own, is a scalar with n's value.
func sameScalar(text []byte, n *yaml.Node) bool {
	var doc yaml.Node
	if err := yaml.Unmarshal(text, &doc); err != nil || len(doc.Content) != 1 {
		return false
	}
	s := doc.Content[0]
	return s.Kind == yaml.ScalarNode && s.Value == n.Value
}

// propertiesEnd returns the end of the anchor and tag that start at at.
func (f *file) propertiesEnd(at int) (int, bool) {
	end := -1
	for at < len(f.src) && (f.src[at] == '&' || f.src[at] == '!') {
		for at < len(f.src) && !isBlank(f.src[at]) && !strings.ContainsRune(",[]{}", rune(f.src[at])) {
			at++
		}
		end = at
		for at < len(f.src) && (f.src[at] == ' ' || f.src[at] == '\t') {
			at++
		}
	}
	return end, end >= 0
}

// blockSpan handles a block scalar (`|` or `>`) whose header starts at start: the span runs from
// the header to the end of the last non-blank content line, so the line break after it and any
// trailing blank lines stay.
func (f *file) blockSpan(start, indent int, p path) (_, end, contentIndent int, err error) {
	if start >= len(f.src) || (f.src[start] != '|' && f.src[start] != '>') {
		return 0, 0, 0, refuse(p, "the block scalar's bytes are not where the syntax tree places them")
	}
	end = start + 1
	for end < len(f.src) && strings.IndexByte("+-0123456789", f.src[end]) >= 0 {
		end++
	}
	if bytes.ContainsAny(f.src[start:end], "123456789") {
		return 0, 0, 0, refuse(p, "the block scalar has an explicit indentation indicator")
	}
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

// keySpan returns the bytes of the key of a mapping entry.
func (f *file) keySpan(k *yaml.Node, p path) (start, end int, err error) {
	if k.Kind != yaml.ScalarNode || hasProperties(k) || styleOf(k) == styleLiteral || styleOf(k) == styleFolded {
		return 0, 0, refuse(p, "the mapping has a key that is not a scalar")
	}
	start, end, _, err = f.scalarSpan(k, 0, p)
	return start, end, err
}

// colon returns the offset just past the ':' that follows the key k of a block mapping entry.
func (f *file) colon(k *yaml.Node, p path) (int, error) {
	_, end, err := f.keySpan(k, p)
	if err != nil {
		return 0, err
	}
	for end < len(f.src) && (f.src[end] == ' ' || f.src[end] == '\t') {
		end++
	}
	if end >= len(f.src) || f.src[end] != ':' {
		return 0, refuse(p, "the syntax tree's positions do not match the file")
	}
	return end + 1, nil
}

// dash returns the offset of the '-' of item i of a block sequence: right before the item on its
// line, or, when the item starts on a later line, the first token after the previous item.
func (f *file) dash(seq *yaml.Node, i int, p path) (int, error) {
	item := seq.Content[i]
	at, ok := f.offset(item)
	if !ok {
		return 0, refuse(p, "the syntax tree's positions do not match the file")
	}
	j := at - 1
	for j >= 0 && (f.src[j] == ' ' || f.src[j] == '\t') {
		j--
	}
	if j >= 0 && f.isDash(j) {
		return j, nil
	}
	var from int
	if i == 0 {
		if from, ok = f.offset(seq); !ok {
			return 0, refuse(p, "the syntax tree's positions do not match the file")
		}
		if hasProperties(seq) {
			if from, ok = f.propertiesEnd(from); !ok {
				return 0, refuse(p, "the syntax tree's positions do not match the file")
			}
		}
	} else {
		prev, err := f.dash(seq, i-1, p)
		if err != nil {
			return 0, err
		}
		if from, err = f.itemEnd(prev, seq.Content[i-1], p); err != nil {
			return 0, err
		}
	}
	if d := f.skipBlank(from); d < at && f.isDash(d) {
		return d, nil
	}
	return 0, refuse(p, "the syntax tree's positions do not match the file")
}

// isDash reports whether src[at] is the '-' indicator of a block sequence item.
func (f *file) isDash(at int) bool {
	return f.src[at] == '-' && (at+1 == len(f.src) || isBlank(f.src[at+1]))
}

func isBlank(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// skipBlank returns the offset of the next token at or after at, past spaces, tabs, line breaks
// and comments.
func (f *file) skipBlank(at int) int {
	for at < len(f.src) {
		switch {
		case isBlank(f.src[at]):
			at++
		case f.src[at] == '#':
			at += indexOrEnd(f.src[at:], '\n')
		default:
			return at
		}
	}
	return at
}

// nodeStart returns the offset of the first byte of n, its anchor or tag included.
func (f *file) nodeStart(n *yaml.Node, p path) (int, error) {
	at, ok := f.offset(n)
	if !ok {
		return 0, refuse(p, "the syntax tree's positions do not match the file")
	}
	return at, nil
}

// nodeEnd returns the offset just past the last byte of n, not counting a trailing comment.
// indent is the indentation of the entry holding n.
func (f *file) nodeEnd(n *yaml.Node, indent int, p path) (int, error) {
	switch n.Kind {
	case yaml.MappingNode, yaml.SequenceNode:
		if n.Style&yaml.FlowStyle != 0 {
			return f.flowEnd(n, p)
		}
		last := len(n.Content) - 1
		switch {
		case last < 0:
			return 0, refuse(p, "the syntax tree's positions do not match the file")
		case n.Kind == yaml.MappingNode:
			return f.entryEnd(n.Content[last-1], n.Content[last], false, p)
		}
		d, err := f.dash(n, last, p)
		if err != nil {
			return 0, err
		}
		return f.itemEnd(d, n.Content[last], p)
	case yaml.AliasNode:
		at, ok := f.offset(n)
		text := "*" + n.Value
		if !ok || !bytes.HasPrefix(f.src[at:], []byte(text)) {
			return 0, refuse(p, "the syntax tree's positions do not match the file")
		}
		return at + len(text), nil
	}
	if isEmpty(n) {
		return 0, refuse(p, "the syntax tree's positions do not match the file")
	}
	_, end, _, err := f.scalarSpan(n, indent, p)
	return end, err
}

// flowOpen returns the offset just past the '{' or '[' that opens a flow collection.
func (f *file) flowOpen(n *yaml.Node, p path) (int, error) {
	at, ok := f.offset(n)
	if ok && hasProperties(n) {
		at, ok = f.propertiesEnd(at)
		at = f.skipBlank(at)
	}
	open := byte('[')
	if n.Kind == yaml.MappingNode {
		open = '{'
	}
	if !ok || at >= len(f.src) || f.src[at] != open {
		return 0, refuse(p, "the syntax tree's positions do not match the file")
	}
	return at + 1, nil
}

// flowEnd returns the offset just past the '}' or ']' that closes a flow collection.
func (f *file) flowEnd(n *yaml.Node, p path) (int, error) {
	at, err := f.flowOpen(n, p)
	if err != nil {
		return 0, err
	}
	if last := len(n.Content) - 1; last >= 0 {
		if n.Kind == yaml.MappingNode {
			at, err = f.entryEnd(n.Content[last-1], n.Content[last], true, p)
		} else {
			at, err = f.nodeEnd(n.Content[last], 0, p)
		}
		if err != nil {
			return 0, err
		}
	}
	at = f.skipBlank(at)
	if at < len(f.src) && f.src[at] == ',' {
		at = f.skipBlank(at + 1)
	}
	closer := byte(']')
	if n.Kind == yaml.MappingNode {
		closer = '}'
	}
	if at >= len(f.src) || f.src[at] != closer {
		return 0, refuse(p, "the syntax tree's positions do not match the file")
	}
	return at + 1, nil
}

// entryEnd returns the end of a mapping entry's value, or of its ':' when the value is empty. In
// a flow mapping, an entry with an empty value may have no ':'; it ends with its key.
func (f *file) entryEnd(k, v *yaml.Node, flow bool, p path) (int, error) {
	_, keyEnd, err := f.keySpan(k, p)
	if err != nil {
		return 0, err
	}
	colon := keyEnd
	for colon < len(f.src) && (f.src[colon] == ' ' || f.src[colon] == '\t') {
		colon++
	}
	if colon >= len(f.src) || f.src[colon] != ':' {
		if flow && isEmpty(v) {
			return keyEnd, nil
		}
		return 0, refuse(p, "the syntax tree's positions do not match the file")
	}
	if isEmpty(v) {
		return colon + 1, nil
	}
	end, err := f.nodeEnd(v, column(k), p)
	if err == nil && end <= colon {
		return 0, refuse(p, "the syntax tree's positions do not match the file")
	}
	return end, err
}

// itemEnd returns the end of a block sequence item, or of its '-' when the item is empty.
func (f *file) itemEnd(dash int, value *yaml.Node, p path) (int, error) {
	if isEmpty(value) {
		return dash + 1, nil
	}
	return f.nodeEnd(value, f.indentAt(dash), p)
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
		if at+i > 0 && f.src[at+i-1] == '\r' {
			return "\r\n"
		}
		return "\n"
	}
	if bytes.Contains(f.src, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// hasAnchor reports whether any of nodes or anything inside them has an anchor, which aliases
// elsewhere may depend on.
func hasAnchor(nodes ...*yaml.Node) bool {
	for _, n := range nodes {
		if n.Anchor != "" || hasAnchor(n.Content...) {
			return true
		}
	}
	return false
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
	loc, err := f.locate(c.Path)
	if err != nil {
		return textEdit{}, err
	}
	if loc.node == nil && len(c.Path) == 0 && c.From == nil {
		// An empty document, which may hold comments.
		return f.replace(c)
	}
	// The node must hold the value the document decoded to, or the tree and the decoder disagree
	// about what is where.
	if v, ok := scalarValue(loc.node); !ok || !equal(v, c.From) {
		return textEdit{}, refuse(c.Path, "the syntax tree and the decoded document disagree")
	}
	s, err := f.slot(loc, c.Path)
	if err != nil {
		return textEdit{}, err
	}
	text := f.render(c.To, s)
	if s.insert {
		text = " " + text
	}
	return f.keepLineComment(textEdit{start: s.start, end: s.end, text: text, depth: len(c.Path)}), nil
}
