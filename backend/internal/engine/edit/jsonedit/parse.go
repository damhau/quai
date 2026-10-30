package jsonedit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

// maxDepth bounds the nesting the parser follows, so that no input can exhaust the stack.
const maxDepth = 512

// node is a JSON value of the original file, with its byte range.
type node struct {
	start, end int      // the value's bytes: src[start:end]
	members    []member // an object's entries, in file order
	items      []*node  // an array's items
	object     bool
	array      bool
	value      any // the normalized value
}

// member is an object entry: its key, the key's first byte (its opening quote) and the byte
// after its closing quote, and its value.
type member struct {
	key              string
	keyStart, keyEnd int
	value            *node
}

// file is a parsed JSON file: its root value, or nil when the file holds only whitespace.
type file struct {
	src  []byte
	root *node
}

type parser struct {
	src   []byte
	at    int
	depth int
}

// parse reads src as one JSON value, keeping every node's byte range. Duplicate keys, a
// byte-order mark, invalid UTF-8 and anything after the value are errors.
func parse(src []byte) (*file, error) {
	if bytes.HasPrefix(src, []byte("\xef\xbb\xbf")) {
		return nil, refuse(nil, "the file starts with a byte-order mark")
	}
	if !utf8.Valid(src) {
		return nil, fmt.Errorf("jsonedit: the file is not valid UTF-8")
	}
	p := &parser{src: src}
	p.space()
	if p.at == len(src) {
		return &file{src: src}, nil
	}
	root, err := p.value(document.Path{})
	if err != nil {
		return nil, err
	}
	p.space()
	if p.at != len(src) {
		return nil, p.errorf("unexpected %q after the document", src[p.at])
	}
	return &file{src: src, root: root}, nil
}

func (p *parser) errorf(format string, args ...any) error {
	line := 1 + bytes.Count(p.src[:min(p.at, len(p.src))], []byte("\n"))
	return fmt.Errorf("jsonedit: line %d: "+format, append([]any{line}, args...)...)
}

func (p *parser) space() {
	for p.at < len(p.src) {
		switch p.src[p.at] {
		case ' ', '\t', '\n', '\r':
			p.at++
		default:
			return
		}
	}
}

func (p *parser) value(at document.Path) (*node, error) {
	if p.at >= len(p.src) {
		return nil, p.errorf("unexpected end of file")
	}
	switch c := p.src[p.at]; {
	case c == '{':
		return p.object(at)
	case c == '[':
		return p.array(at)
	case c == '"':
		start := p.at
		s, err := p.string()
		if err != nil {
			return nil, err
		}
		return &node{start: start, end: p.at, value: s}, nil
	case c == '-' || c >= '0' && c <= '9':
		return p.number(at)
	default:
		for _, lit := range []struct {
			text  string
			value any
		}{{"true", true}, {"false", false}, {"null", nil}} {
			if bytes.HasPrefix(p.src[p.at:], []byte(lit.text)) {
				start := p.at
				p.at += len(lit.text)
				return &node{start: start, end: p.at, value: lit.value}, nil
			}
		}
		return nil, p.errorf("unexpected %q", c)
	}
}

func (p *parser) object(at document.Path) (*node, error) {
	if p.depth++; p.depth > maxDepth {
		return nil, p.errorf("nested more than %d levels deep", maxDepth)
	}
	defer func() { p.depth-- }()
	n := &node{start: p.at, object: true}
	values := map[string]any{}
	p.at++
	p.space()
	if p.at < len(p.src) && p.src[p.at] == '}' {
		p.at++
		n.end, n.value = p.at, values
		return n, nil
	}
	for {
		p.space()
		if p.at >= len(p.src) || p.src[p.at] != '"' {
			return nil, p.errorf("expected a key")
		}
		keyStart := p.at
		key, err := p.string()
		if err != nil {
			return nil, err
		}
		keyEnd := p.at
		if _, dup := values[key]; dup {
			return nil, refuse(at.Key(key), "the key %q appears twice", key)
		}
		p.space()
		if p.at >= len(p.src) || p.src[p.at] != ':' {
			return nil, p.errorf("expected ':' after a key")
		}
		p.at++
		p.space()
		v, err := p.value(at.Key(key))
		if err != nil {
			return nil, err
		}
		n.members = append(n.members, member{key: key, keyStart: keyStart, keyEnd: keyEnd, value: v})
		values[key] = v.value
		p.space()
		if p.at >= len(p.src) {
			return nil, p.errorf("unexpected end of file in an object")
		}
		switch p.src[p.at] {
		case ',':
			p.at++
		case '}':
			p.at++
			n.end, n.value = p.at, values
			return n, nil
		default:
			return nil, p.errorf("expected ',' or '}'")
		}
	}
}

func (p *parser) array(at document.Path) (*node, error) {
	if p.depth++; p.depth > maxDepth {
		return nil, p.errorf("nested more than %d levels deep", maxDepth)
	}
	defer func() { p.depth-- }()
	n := &node{start: p.at, array: true}
	values := []any{}
	p.at++
	p.space()
	if p.at < len(p.src) && p.src[p.at] == ']' {
		p.at++
		n.end, n.value = p.at, values
		return n, nil
	}
	for {
		p.space()
		v, err := p.value(at.Index(len(values)))
		if err != nil {
			return nil, err
		}
		n.items = append(n.items, v)
		values = append(values, v.value)
		p.space()
		if p.at >= len(p.src) {
			return nil, p.errorf("unexpected end of file in an array")
		}
		switch p.src[p.at] {
		case ',':
			p.at++
		case ']':
			p.at++
			n.end, n.value = p.at, values
			return n, nil
		default:
			return nil, p.errorf("expected ',' or ']'")
		}
	}
}

// string reads a string token and decodes it with the standard library, which checks escapes.
func (p *parser) string() (string, error) {
	start := p.at
	p.at++
	for p.at < len(p.src) {
		switch c := p.src[p.at]; {
		case c == '\\':
			p.at += 2
		case c == '"':
			p.at++
			var s string
			if err := json.Unmarshal(p.src[start:p.at], &s); err != nil {
				return "", p.errorf("invalid string: %v", err)
			}
			return s, nil
		case c < 0x20:
			return "", p.errorf("a control character in a string")
		default:
			p.at++
		}
	}
	return "", p.errorf("unterminated string")
}

// number reads a number token by JSON's grammar and normalizes it.
func (p *parser) number(at document.Path) (*node, error) {
	start := p.at
	if p.src[p.at] == '-' {
		p.at++
	}
	digits := func() int {
		n := 0
		for p.at < len(p.src) && p.src[p.at] >= '0' && p.src[p.at] <= '9' {
			p.at++
			n++
		}
		return n
	}
	intStart := p.at
	if digits() == 0 || p.src[intStart] == '0' && p.at-intStart > 1 {
		return nil, p.errorf("invalid number")
	}
	if p.at < len(p.src) && p.src[p.at] == '.' {
		p.at++
		if digits() == 0 {
			return nil, p.errorf("invalid number")
		}
	}
	if p.at < len(p.src) && (p.src[p.at] == 'e' || p.src[p.at] == 'E') {
		p.at++
		if p.at < len(p.src) && (p.src[p.at] == '+' || p.src[p.at] == '-') {
			p.at++
		}
		if digits() == 0 {
			return nil, p.errorf("invalid number")
		}
	}
	v, err := document.Normalize(json.Number(p.src[start:p.at]))
	if err != nil {
		return nil, fmt.Errorf("jsonedit: %s: %v", at, err)
	}
	return &node{start: start, end: p.at, value: v}, nil
}

// at returns the node at path p, or nil.
func (f *file) at(p document.Path) *node {
	n := f.root
	for _, step := range p {
		if n == nil {
			return nil
		}
		switch step := step.(type) {
		case string:
			var next *node
			for _, m := range n.members {
				if m.key == step {
					next = m.value
				}
			}
			n = next
		case int:
			if step < 0 || step >= len(n.items) {
				return nil
			}
			n = n.items[step]
		}
	}
	return n
}
