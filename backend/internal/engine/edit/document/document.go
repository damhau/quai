// Package document is the data model the file editors share (docs/design/03-file-editing.md):
// documents as values of the JSON data model, paths into them, and the changes that turn one
// document into another, each at the smallest node that differs.
//
// A normalized document holds nil, bool, string, int64, uint64, float64, []any and
// map[string]any. Numbers compare by value, so 8 and 8.0 are equal, but 8 and "8" are not.
package document

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Path addresses a value in a document: string elements are mapping keys, int elements are list
// indices. The empty path is the document root.
type Path []any

// Key returns the path of the entry k of the mapping at p.
func (p Path) Key(k string) Path { return append(p[:len(p):len(p)], k) }

// Index returns the path of the item i of the list at p.
func (p Path) Index(i int) Path { return append(p[:len(p):len(p)], i) }

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// String renders p as "$", "$.network.ip", "$.disks[0]" or `$["dotted.key"]`.
func (p Path) String() string {
	var b strings.Builder
	b.WriteString("$")
	for _, step := range p {
		switch step := step.(type) {
		case int:
			fmt.Fprintf(&b, "[%d]", step)
		case string:
			if identifier.MatchString(step) {
				b.WriteString("." + step)
			} else {
				fmt.Fprintf(&b, "[%q]", step)
			}
		}
	}
	return b.String()
}

// Normalize converts a document to its normalized form. It accepts any Go integer or float type,
// json.Number, []any and map[string]any, nested freely, and rejects anything outside the JSON
// data model, including NaN, infinities and strings that are not valid UTF-8.
func Normalize(v any) (any, error) { return normalize(v, Path{}) }

func normalize(v any, p Path) (any, error) {
	switch v := v.(type) {
	case nil, bool, int64, uint64:
		return v, nil
	case string:
		if !utf8.ValidString(v) {
			return nil, fmt.Errorf("%s: string is not valid UTF-8", p)
		}
		return v, nil
	case int:
		return int64(v), nil
	case int8:
		return int64(v), nil
	case int16:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case uint:
		return uint64(v), nil
	case uint8:
		return uint64(v), nil
	case uint16:
		return uint64(v), nil
	case uint32:
		return uint64(v), nil
	case float32:
		return finite(float64(v), p)
	case float64:
		return finite(v, p)
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, nil
		}
		if u, err := strconv.ParseUint(string(v), 10, 64); err == nil {
			return u, nil
		}
		f, err := v.Float64()
		if err != nil {
			return nil, fmt.Errorf("%s: invalid number %q", p, string(v))
		}
		return finite(f, p)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			n, err := normalize(item, p.Index(i))
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			if !utf8.ValidString(k) {
				return nil, fmt.Errorf("%s: key %q is not valid UTF-8", p, k)
			}
			n, err := normalize(item, p.Key(k))
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s: unsupported value of type %T", p, v)
}

func finite(f float64, p Path) (any, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("%s: %v is not a JSON number", p, f)
	}
	return f, nil
}

// Equal reports whether two normalized documents are the same. Numbers compare by value.
func Equal(a, b any) bool {
	switch a := a.(type) {
	case nil:
		return b == nil
	case bool:
		b, ok := b.(bool)
		return ok && a == b
	case string:
		b, ok := b.(string)
		return ok && a == b
	case int64, uint64, float64:
		x, y := number(a), number(b)
		return x != nil && y != nil && x.Cmp(y) == 0
	case []any:
		b, ok := b.([]any)
		return ok && slices.EqualFunc(a, b, Equal)
	case map[string]any:
		b, ok := b.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for k, av := range a {
			bv, ok := b[k]
			if !ok || !Equal(av, bv) {
				return false
			}
		}
		return true
	}
	return false
}

// number returns the exact value of a normalized number, or nil for anything else.
func number(v any) *big.Rat {
	switch v := v.(type) {
	case int64:
		return new(big.Rat).SetInt64(v)
	case uint64:
		return new(big.Rat).SetInt(new(big.Int).SetUint64(v))
	case float64:
		return new(big.Rat).SetFloat64(v)
	}
	return nil
}

// IsScalar reports whether v is neither a list nor a mapping.
func IsScalar(v any) bool {
	switch v.(type) {
	case []any, map[string]any:
		return false
	}
	return true
}

// ChangeKind is what a change does to the original.
type ChangeKind int

const (
	SetScalar   ChangeKind = iota // replace the scalar at Path with To
	ReplaceNode                   // replace the whole node at Path with To
	AddKey                        // add the entry Path[-1]: To to the mapping at Path[:-1]
	RemoveKey                     // remove the mapping entry at Path
	InsertItem                    // insert To into the list at Path[:-1], after its original item After
	RemoveItem                    // remove the list item at Path
)

// Change is one difference between the current and the intended document.
type Change struct {
	Kind     ChangeKind
	Path     Path
	From, To any
	After    int // InsertItem: index of the original item the new one follows, -1 for the start
}

// Diff returns the changes that turn from into to, both normalized. Mappings are compared key by
// key and lists item by item, so a change touches the smallest node it can; a mapping that shares
// no key with its intended value, or a list none of whose items survive, is replaced whole. For
// one mapping, its removed keys come first, then the changes of its other keys in sorted order.
func Diff(from, to any) []Change { return diff(from, to, Path{}, nil) }

func diff(from, to any, p Path, out []Change) []Change {
	if Equal(from, to) {
		return out
	}
	if fm, ok := from.(map[string]any); ok {
		if tm, ok := to.(map[string]any); ok && (len(fm) == 0 || sharesKey(fm, tm)) {
			for _, k := range SortedKeys(fm) {
				if _, ok := tm[k]; !ok {
					out = append(out, Change{Kind: RemoveKey, Path: p.Key(k), From: fm[k]})
				}
			}
			for _, k := range SortedKeys(tm) {
				if fv, ok := fm[k]; ok {
					out = diff(fv, tm[k], p.Key(k), out)
				} else {
					out = append(out, Change{Kind: AddKey, Path: p.Key(k), To: tm[k]})
				}
			}
			return out
		}
	}
	if fs, ok := from.([]any); ok {
		if ts, ok := to.([]any); ok {
			if out, ok := diffList(fs, ts, p, out); ok {
				return out
			}
		}
	}
	if IsScalar(from) && IsScalar(to) {
		return append(out, Change{Kind: SetScalar, Path: p, From: from, To: to})
	}
	return append(out, Change{Kind: ReplaceNode, Path: p, From: from, To: to})
}

// diffList pairs the items of two lists: equal items at both ends are kept, the items in between
// are compared pairwise, and the surplus is removed or inserted. It reports false when no
// original item would survive, in which case the whole list is replaced.
func diffList(from, to []any, p Path, out []Change) ([]Change, bool) {
	pre := 0
	for pre < len(from) && pre < len(to) && Equal(from[pre], to[pre]) {
		pre++
	}
	suf := 0
	for suf < len(from)-pre && suf < len(to)-pre && Equal(from[len(from)-1-suf], to[len(to)-1-suf]) {
		suf++
	}
	oldMid, newMid := from[pre:len(from)-suf], to[pre:len(to)-suf]
	paired := min(len(oldMid), len(newMid))
	if len(from) > 0 && pre+suf+paired == 0 {
		return out, false
	}
	for i := range paired {
		out = diff(oldMid[i], newMid[i], p.Index(pre+i), out)
	}
	for i := paired; i < len(oldMid); i++ {
		out = append(out, Change{Kind: RemoveItem, Path: p.Index(pre + i), From: oldMid[i]})
	}
	for j := paired; j < len(newMid); j++ {
		out = append(out, Change{Kind: InsertItem, Path: p.Index(pre + j), To: newMid[j], After: pre + paired - 1})
	}
	return out, true
}

// sharesKey reports whether a and b have a key in common, so that editing a key by key keeps
// something of the original.
func sharesKey(a, b map[string]any) bool {
	for k := range a {
		if _, ok := b[k]; ok {
			return true
		}
	}
	return false
}

// SortedKeys returns the keys of m in sorted order.
func SortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ValueAt returns the value at p in a normalized document, or nil when there is none.
func ValueAt(doc any, p Path) any {
	for _, step := range p {
		switch d := doc.(type) {
		case map[string]any:
			doc = d[step.(string)]
		case []any:
			i, _ := step.(int)
			if i < 0 || i >= len(d) {
				return nil
			}
			doc = d[i]
		default:
			return nil
		}
	}
	return doc
}

// With returns a copy of doc with v at p; doc is left as it is. The mapping or list p ends in
// must exist; an index equal to a list's length appends to it.
func With(doc any, p Path, v any) any {
	if len(p) == 0 {
		return v
	}
	switch d := doc.(type) {
	case map[string]any:
		out := make(map[string]any, len(d)+1)
		for k, item := range d {
			out[k] = item
		}
		out[p[0].(string)] = With(d[p[0].(string)], p[1:], v)
		return out
	case []any:
		i := p[0].(int)
		if i == len(d) {
			return append(slices.Clone(d), With(nil, p[1:], v))
		}
		out := slices.Clone(d)
		out[i] = With(d[i], p[1:], v)
		return out
	}
	panic(fmt.Sprintf("document.With: %s does not exist", p))
}

// Without returns a copy of doc without the mapping entry or list item at p.
func Without(doc any, p Path) any {
	if len(p) == 0 {
		return nil
	}
	switch d := doc.(type) {
	case map[string]any:
		out := make(map[string]any, len(d))
		for k, v := range d {
			out[k] = v
		}
		if len(p) == 1 {
			delete(out, p[0].(string))
		} else {
			out[p[0].(string)] = Without(d[p[0].(string)], p[1:])
		}
		return out
	case []any:
		i := p[0].(int)
		if len(p) == 1 {
			return slices.Delete(slices.Clone(d), i, i+1)
		}
		out := slices.Clone(d)
		out[i] = Without(d[i], p[1:])
		return out
	}
	panic(fmt.Sprintf("document.Without: %s does not exist", p))
}
