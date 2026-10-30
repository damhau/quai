package locate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/edit"
	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

// ErrNoEntry is returned when a file does not hold a resource's entry.
var ErrNoEntry = errors.New("the file does not hold the entry")

// NewField returns the locator of fields of something another type owns: the listed fields
// (dotted paths) of each entry of a shared file, with a collection as NewEntry, or of each file
// matching pattern, without one. A field region is never created or deleted: it exists while its
// entry or file does.
func NewField(pattern, collection, keyField, keyParam string, fields []string) (*Locator, error) {
	if len(fields) == 0 {
		return nil, errors.New("a field locator needs fields")
	}
	var l *Locator
	if collection != "" {
		var err error
		if l, err = NewEntry(pattern, collection, keyField, keyParam); err != nil {
			return nil, err
		}
	} else {
		if keyField != "" || keyParam != "" {
			return nil, errors.New("a field locator without a collection has no key")
		}
		p, err := Compile(pattern)
		if err != nil {
			return nil, err
		}
		l = &Locator{pattern: p}
	}
	l.strategy, l.fields = Field, slices.Clone(fields)
	return l, nil
}

// Document returns a resource's document in doc, its file's: the file's own (file), its entry's
// value (entry), or the listed fields of either (field); and its values defined in code, relative
// to it. It returns ErrNoEntry when the file does not hold the entry.
func Document(doc any, code []edit.Code, r Region) (map[string]any, []edit.Code, error) {
	value := doc
	if r.Collection != nil {
		v, at, ok := Value(doc, r)
		if !ok {
			return nil, nil, ErrNoEntry
		}
		value, code = v, edit.Under(code, at)
	}
	m, ok := value.(map[string]any)
	if !ok {
		if r.Collection != nil {
			return nil, nil, fmt.Errorf("the entry %s does not hold a mapping", r.Item)
		}
		return nil, nil, errors.New("the file does not hold a mapping")
	}
	if r.Strategy != Field {
		return m, code, nil
	}
	out := map[string]any{}
	for _, f := range r.Fields {
		if v, ok := lookup(m, dotted(f)); ok {
			set, err := document.Set(out, dotted(f), v)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", f, err)
			}
			out = set.(map[string]any)
		}
	}
	var kept []edit.Code
	for _, c := range code {
		if listed(r.Fields, edit.Field(c.Path)) {
			kept = append(kept, c)
		}
	}
	return out, kept, nil
}

// Put returns doc, a file's document, with value as the resource's document: the whole file, its
// entry's value, or, for a field region, its listed fields set from value (removed where value
// has none) and the rest left as it is.
func Put(doc any, r Region, value map[string]any) (any, error) {
	var base document.Path
	cur := doc
	if r.Collection != nil {
		v, at, ok := Value(doc, r)
		if !ok {
			return nil, ErrNoEntry
		}
		base, cur = at, v
	}
	next := any(value)
	if r.Strategy == Field {
		next = cur
		for _, f := range r.Fields {
			p := dotted(f)
			if v, ok := lookup(value, p); ok {
				var err error
				if next, err = document.Set(next, p, v); err != nil {
					return nil, fmt.Errorf("%s: %w", f, err)
				}
			} else if _, ok := lookup(next, p); ok {
				next = document.Without(next, p)
			}
		}
	}
	return document.Set(doc, base, next)
}

// Unlisted returns the fields of doc, dotted, that are neither one of fields nor under one: what
// a field type may not write.
func Unlisted(fields []string, doc map[string]any) []string {
	var out []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for _, k := range document.SortedKeys(m) {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			switch sub, isMap := m[k].(map[string]any); {
			case listed(fields, p):
			case isMap && slices.ContainsFunc(fields, func(f string) bool { return strings.HasPrefix(f, p+".") }):
				walk(p, sub)
			default:
				out = append(out, p)
			}
		}
	}
	walk("", doc)
	return out
}

// listed reports whether the dotted field p is one of fields or under one.
func listed(fields []string, p string) bool {
	return slices.ContainsFunc(fields, func(f string) bool { return p == f || strings.HasPrefix(p, f+".") })
}

func lookup(doc any, p document.Path) (any, bool) {
	cur := doc
	for _, step := range p {
		switch c := cur.(type) {
		case map[string]any:
			k, _ := step.(string)
			v, ok := c[k]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, ok := step.(int)
			if !ok || i < 0 || i >= len(c) {
				return nil, false
			}
			cur = c[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// fieldFingerprint hashes the bytes of a field region's fields, each with its name, an absent
// one as such; Absent when its file or entry does not exist.
func fieldFingerprint(fsys fs.FS, r Region) (string, error) {
	src, err := readRegular(fsys, r.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return Absent, nil
	}
	if err != nil {
		return "", err
	}
	var base document.Path
	if r.Collection != nil {
		_, el, err := entry(fsys, r)
		if err != nil || el == nil {
			return Absent, err
		}
	}
	doc, _, err := edit.Decode(r.Path, src)
	if err != nil {
		return "", fmt.Errorf("%s: %w", r.Path, err)
	}
	if r.Collection != nil {
		_, base, _ = Value(doc, r)
	}
	h := sha256.New()
	for _, f := range r.Fields {
		p := append(slices.Clone(base), dotted(f)...)
		h.Write([]byte(f + "\x00"))
		if _, ok := lookup(doc, p); ok {
			b, err := valueBytes(r.Path, src, p)
			if err != nil {
				return "", err
			}
			h.Write(b)
		} else {
			h.Write([]byte{1})
		}
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// valueBytes returns the bytes of the entry or item at p, a path that exists in the file, without
// alignment padding.
func valueBytes(name string, src []byte, p document.Path) ([]byte, error) {
	els, err := edit.Elements(name, src, p[:len(p)-1])
	if err != nil {
		return nil, err
	}
	switch step := p[len(p)-1].(type) {
	case int:
		if step < len(els) {
			return els[step].Content(src), nil
		}
	case string:
		for _, el := range els {
			if el.Key == step {
				return el.Content(src), nil
			}
		}
	}
	return nil, fmt.Errorf("%s: %s is not where the document has it", name, edit.Field(p))
}
