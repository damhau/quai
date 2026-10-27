package expr

import (
	"errors"
	"fmt"
	"strings"
)

// Templates work on structured values, never on text. A string that is exactly one binding,
// `${{ expr }}` with optional surrounding spaces, is replaced by the binding's value with its
// type: a number stays a number, a list stays a list. A string with bindings among other text is
// interpolated. Because the result is serialized by a YAML or JSON library afterwards, a value
// can never inject structure into a file.

const (
	openDelim  = "${{"
	closeDelim = "}}"
)

// HasBinding reports whether s contains a `${{` binding.
func HasBinding(s string) bool {
	return strings.Contains(s, openDelim)
}

// Template is a string with its bindings compiled. It is safe for concurrent use.
type Template struct {
	src   string
	whole *Program // the string is exactly one binding
	parts []part   // otherwise: text and bindings, in order
}

type part struct {
	text string
	prog *Program // nil for text
}

// CompileTemplate compiles the bindings of s. A string without bindings is a template that
// evaluates to itself.
func (e *Env) CompileTemplate(s string) (*Template, error) {
	t := &Template{src: s}
	rest := s
	for {
		i := strings.Index(rest, openDelim)
		if i < 0 {
			if rest != "" {
				t.parts = append(t.parts, part{text: rest})
			}
			break
		}
		if i > 0 {
			t.parts = append(t.parts, part{text: rest[:i]})
		}
		body, n, err := scanBinding(rest[i+len(openDelim):])
		if err != nil {
			return nil, &Error{Expr: s, Err: err}
		}
		prog, err := e.Compile(body)
		if err != nil {
			return nil, err
		}
		t.parts = append(t.parts, part{prog: prog})
		rest = rest[i+len(openDelim)+n:]
	}
	// One binding with nothing but spaces around it is a whole binding.
	var bindings []*Program
	for _, p := range t.parts {
		if p.prog != nil {
			bindings = append(bindings, p.prog)
		} else if strings.TrimSpace(p.text) != "" {
			return t, nil
		}
	}
	if len(bindings) == 1 {
		t.whole = bindings[0]
	}
	return t, nil
}

// scanBinding returns the expression at the start of s, which follows a `${{`, and the length of
// s up to and including the `}}` that closes it. The closing braces are the first `}}` outside
// string literals and outside the braces of map literals, so `${{ {'a': {'b': 1}} }}` and
// `${{ '}}' }}` work.
func scanBinding(s string) (body string, n int, err error) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'', '"':
			end, err := skipString(s, i)
			if err != nil {
				return "", 0, err
			}
			i = end - 1
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
				continue
			}
			if strings.HasPrefix(s[i:], closeDelim) {
				body = strings.TrimSpace(s[:i])
				if body == "" {
					return "", 0, errors.New("empty binding `${{ }}`")
				}
				return body, i + len(closeDelim), nil
			}
		}
	}
	return "", 0, fmt.Errorf("`%s` without a closing `%s`", openDelim, closeDelim)
}

// skipString returns the index just past the CEL string literal whose opening quote is at s[i]:
// single, double or triple quoted, raw when prefixed with r or R.
func skipString(s string, i int) (int, error) {
	q := s[i : i+1]
	if strings.HasPrefix(s[i:], strings.Repeat(q, 3)) {
		q = strings.Repeat(q, 3)
	}
	raw := i > 0 && (s[i-1] == 'r' || s[i-1] == 'R') && (i < 2 || !isIdentChar(s[i-2]))
	for j := i + len(q); j < len(s); j++ {
		if s[j] == '\\' && !raw {
			j++
			continue
		}
		if strings.HasPrefix(s[j:], q) {
			return j + len(q), nil
		}
	}
	return 0, errors.New("unterminated string literal in binding")
}

func isIdentChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// String returns the template's source.
func (t *Template) String() string { return t.src }

// Eval evaluates the template. A whole binding returns its value, which may be Absent; any other
// template returns a string.
func (t *Template) Eval(v Vars) (any, error) {
	act, err := v.activation()
	if err != nil {
		return nil, err
	}
	return t.eval(act)
}

func (t *Template) eval(act map[string]any) (any, error) {
	if t.whole != nil {
		return t.whole.eval(act)
	}
	var b strings.Builder
	for _, p := range t.parts {
		if p.prog == nil {
			b.WriteString(p.text)
			continue
		}
		v, err := p.prog.eval(act)
		if err != nil {
			return nil, err
		}
		s, err := format(v)
		if err != nil {
			return nil, &Error{Expr: p.prog.src, Err: err}
		}
		b.WriteString(s)
	}
	return b.String(), nil
}

// Value is a template value as decoded from a definition, maps and lists nested freely, with
// every string compiled as a Template. It is safe for concurrent use.
type Value struct {
	root node
}

type node interface{}

type mapNode struct {
	keys  []string
	nodes map[string]node
}

type listNode []node

// CompileValue compiles every string in v, a value in the JSON data model.
func (e *Env) CompileValue(v any) (*Value, error) {
	root, err := e.compileNode(v)
	if err != nil {
		return nil, err
	}
	return &Value{root: root}, nil
}

func (e *Env) compileNode(v any) (node, error) {
	switch v := v.(type) {
	case string:
		if !HasBinding(v) {
			return v, nil
		}
		return e.CompileTemplate(v)
	case map[string]any:
		m := mapNode{keys: make([]string, 0, len(v)), nodes: make(map[string]node, len(v))}
		for k, x := range v {
			n, err := e.compileNode(x)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			m.keys = append(m.keys, k)
			m.nodes[k] = n
		}
		return m, nil
	case []any:
		l := make(listNode, len(v))
		for i, x := range v {
			n, err := e.compileNode(x)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			l[i] = n
		}
		return l, nil
	}
	n, err := normalize(v)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// Eval evaluates every binding in the value. A map entry or list item whose whole binding is
// Absent is left out; the value itself may be Absent.
func (tv *Value) Eval(v Vars) (any, error) {
	act, err := v.activation()
	if err != nil {
		return nil, err
	}
	return evalNode(tv.root, act)
}

func evalNode(n node, act map[string]any) (any, error) {
	switch n := n.(type) {
	case *Template:
		return n.eval(act)
	case mapNode:
		out := make(map[string]any, len(n.keys))
		for _, k := range n.keys {
			x, err := evalNode(n.nodes[k], act)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			if x != Absent {
				out[k] = x
			}
		}
		return out, nil
	case listNode:
		out := make([]any, 0, len(n))
		for i, item := range n {
			x, err := evalNode(item, act)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			if x != Absent {
				out = append(out, x)
			}
		}
		return out, nil
	}
	return n, nil
}
