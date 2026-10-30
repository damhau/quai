package hcledit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

// node is a value of the original file, with its byte range. A container is an object, a tuple,
// or the body whose attributes are the document (a file's top level, or its locals block).
type node struct {
	start, end int // the value's bytes: src[start:end]
	members    []member
	items      []*node
	object     bool // an object, or the body
	tuple      bool
	body       bool
	code       bool // not a literal: kept as it is, never written
	value      any  // the normalized value, when not code
}

// member is an attribute or an object item: its key, the key's bytes, and its value.
type member struct {
	key              string
	keyStart, keyEnd int
	value            *node
}

// file is a parsed HCL file and the body the document is.
type file struct {
	src  []byte
	root *node // the body
	code []document.Path
}

// parse reads src and builds the tree of the body selected by block: the file's top-level
// attributes when block is empty, the attributes of its one block of that type (locals) otherwise.
func parse(src []byte, block string) (*file, error) {
	if bytes.HasPrefix(src, []byte("\xef\xbb\xbf")) {
		return nil, refuse(nil, "the file starts with a byte-order mark")
	}
	if !utf8.Valid(src) {
		return nil, fmt.Errorf("hcledit: the file is not valid UTF-8")
	}
	hf, diags := hclsyntax.ParseConfig(src, "file.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		if slices.ContainsFunc(diags, func(d *hcl.Diagnostic) bool { return strings.Contains(d.Summary, "redefined") }) {
			return nil, refuse(nil, "an attribute is defined twice: %s", diags.Error())
		}
		return nil, fmt.Errorf("hcledit: %s", diags.Error())
	}
	f := &file{src: src}
	body := hf.Body.(*hclsyntax.Body)
	if block != "" {
		var found []*hclsyntax.Block
		for _, b := range body.Blocks {
			if b.Type == block && len(b.Labels) == 0 {
				found = append(found, b)
			}
		}
		switch len(found) {
		case 0:
			return nil, refuse(nil, "the file has no %s block", block)
		case 1:
			body = found[0].Body
		default:
			return nil, refuse(nil, "the file has %d %s blocks: which one holds the values is ambiguous", len(found), block)
		}
	}
	root := &node{start: body.SrcRange.Start.Byte, end: body.SrcRange.End.Byte, object: true, body: true}
	attrs := make([]*hclsyntax.Attribute, 0, len(body.Attributes))
	for _, a := range body.Attributes {
		attrs = append(attrs, a)
	}
	slices.SortFunc(attrs, func(a, b *hclsyntax.Attribute) int { return a.SrcRange.Start.Byte - b.SrcRange.Start.Byte })
	values := map[string]any{}
	for _, a := range attrs {
		v, err := f.expr(a.Expr, document.Path{a.Name})
		if err != nil {
			return nil, err
		}
		root.members = append(root.members, member{key: a.Name, keyStart: a.NameRange.Start.Byte, keyEnd: a.NameRange.End.Byte, value: v})
		if !v.code {
			values[a.Name] = v.value
		}
	}
	root.value = values
	f.root = root
	return f, nil
}

// expr builds the node of an expression. Literals are numbers, quoted strings without
// interpolation, booleans, null, and objects and tuples of literals; anything else is code.
func (f *file) expr(e hclsyntax.Expression, at document.Path) (*node, error) {
	r := e.Range()
	n := &node{start: r.Start.Byte, end: r.End.Byte}
	codeNode := func() (*node, error) {
		n.code = true
		f.code = append(f.code, at)
		return n, nil
	}
	switch e := e.(type) {
	case *hclsyntax.LiteralValueExpr:
		switch {
		case e.Val.IsNull():
			n.value = nil
		case e.Val.Type().Equals(cty.Bool):
			n.value = e.Val.True()
		case e.Val.Type().Equals(cty.String):
			n.value = e.Val.AsString()
		case e.Val.Type().Equals(cty.Number):
			return f.number(n, string(f.src[n.start:n.end]), at)
		default:
			return codeNode()
		}
		return n, nil
	case *hclsyntax.UnaryOpExpr:
		if lit, ok := e.Val.(*hclsyntax.LiteralValueExpr); ok && e.Op == hclsyntax.OpNegate && lit.Val.Type().Equals(cty.Number) {
			vr := lit.Range()
			return f.number(n, "-"+string(f.src[vr.Start.Byte:vr.End.Byte]), at)
		}
		return codeNode()
	case *hclsyntax.TemplateExpr:
		if bytes.HasPrefix(f.src[n.start:n.end], []byte("<<")) {
			return codeNode() // a heredoc: its layout cannot be rewritten in place
		}
		var s strings.Builder
		for _, part := range e.Parts {
			lit, ok := part.(*hclsyntax.LiteralValueExpr)
			if !ok || !lit.Val.Type().Equals(cty.String) {
				return codeNode()
			}
			s.WriteString(lit.Val.AsString())
		}
		n.value = s.String()
		return n, nil
	case *hclsyntax.TupleConsExpr:
		n.tuple = true
		values := []any{}
		for i, item := range e.Exprs {
			v, err := f.expr(item, at.Index(i))
			if err != nil {
				return nil, err
			}
			if v.code || slices.ContainsFunc(f.code, func(p document.Path) bool { return hasPrefix(p, at) }) {
				// Code in an item would sit at indices that shift when the items around it
				// change: the whole tuple is code.
				f.code = slices.DeleteFunc(f.code, func(p document.Path) bool { return hasPrefix(p, at) })
				return codeNode()
			}
			n.items = append(n.items, v)
			values = append(values, v.value)
		}
		n.value = values
		return n, nil
	case *hclsyntax.ObjectConsExpr:
		n.object = true
		values := map[string]any{}
		for _, item := range e.Items {
			key, ok := objectKey(item.KeyExpr)
			if !ok {
				f.code = slices.DeleteFunc(f.code, func(p document.Path) bool { return hasPrefix(p, at) })
				return codeNode()
			}
			if slices.ContainsFunc(n.members, func(m member) bool { return m.key == key }) {
				return nil, refuse(at.Key(key), "the key %q appears twice", key)
			}
			v, err := f.expr(item.ValueExpr, at.Key(key))
			if err != nil {
				return nil, err
			}
			kr := item.KeyExpr.Range()
			n.members = append(n.members, member{key: key, keyStart: kr.Start.Byte, keyEnd: kr.End.Byte, value: v})
			if !v.code {
				values[key] = v.value
			}
		}
		n.value = values
		return n, nil
	}
	return codeNode()
}

// number decodes a number from its source text, which HCL writes as JSON does.
func (f *file) number(n *node, text string, at document.Path) (*node, error) {
	v, err := document.Normalize(json.Number(text))
	if err != nil {
		return nil, fmt.Errorf("hcledit: %s: %v", at, err)
	}
	n.value = v
	return n, nil
}

// objectKey returns an object item's key when it is a bare identifier or a quoted string
// without interpolation.
func objectKey(e hclsyntax.Expression) (string, bool) {
	k, ok := e.(*hclsyntax.ObjectConsKeyExpr)
	if !ok || k.ForceNonLiteral {
		return "", false
	}
	if name := hcl.ExprAsKeyword(k.Wrapped); name != "" {
		return name, true
	}
	t, ok := k.Wrapped.(*hclsyntax.TemplateExpr)
	if !ok {
		return "", false
	}
	var s strings.Builder
	for _, part := range t.Parts {
		lit, ok := part.(*hclsyntax.LiteralValueExpr)
		if !ok || !lit.Val.Type().Equals(cty.String) {
			return "", false
		}
		s.WriteString(lit.Val.AsString())
	}
	return s.String(), true
}

// hasPrefix reports whether p is at or under prefix.
func hasPrefix(p, prefix document.Path) bool {
	return len(p) >= len(prefix) && slices.Equal(p[:len(prefix)], prefix)
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
