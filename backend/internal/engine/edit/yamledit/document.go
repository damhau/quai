package yamledit

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"unicode/utf8"
)

// normalize converts a document to the canonical form used for comparison: nil, bool, string,
// int64, uint64, float64, []any and map[string]any. It rejects anything outside the JSON data
// model, including NaN and infinities.
func normalize(v any, p path) (any, error) {
	switch v := v.(type) {
	case nil, bool, int64, uint64:
		return v, nil
	case string:
		if !utf8.ValidString(v) {
			return nil, fmt.Errorf("yamledit: %s: string is not valid UTF-8", p)
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
			return nil, fmt.Errorf("yamledit: %s: invalid number %q", p, string(v))
		}
		return finite(f, p)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			n, err := normalize(item, p.index(i))
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
				return nil, fmt.Errorf("yamledit: %s: key %q is not valid UTF-8", p, k)
			}
			n, err := normalize(item, p.key(k))
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	}
	return nil, fmt.Errorf("yamledit: %s: unsupported value of type %T", p, v)
}

func finite(f float64, p path) (any, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("yamledit: %s: %v is not a JSON number", p, f)
	}
	return f, nil
}

// equal reports whether two normalized documents are the same. Numbers compare by value.
func equal(a, b any) bool {
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
		return ok && slices.EqualFunc(a, b, equal)
	case map[string]any:
		b, ok := b.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for k, av := range a {
			bv, ok := b[k]
			if !ok || !equal(av, bv) {
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

func isScalar(v any) bool {
	switch v.(type) {
	case []any, map[string]any:
		return false
	}
	return true
}

// changeKind is what a change does to the original.
type changeKind int

const (
	setScalar   changeKind = iota // replace the scalar at path with to
	replaceNode                   // replace the whole node at path with to
	addKey                        // add the entry path[-1]: to to the mapping at path[:-1]
	removeKey                     // remove the mapping entry at path
	insertItem                    // insert to into the sequence at path[:-1], after its original item `after`
	removeItem                    // remove the sequence item at path
)

// change is one difference between the current and the intended document.
type change struct {
	kind     changeKind
	path     path
	from, to any
	after    int // insertItem: index of the original item the new one follows, -1 for the start
}

// diff appends to out the changes that turn from into to, in path order. Mappings are compared
// key by key and lists item by item, so a change touches the smallest node it can.
func diff(from, to any, p path, out []change) []change {
	if equal(from, to) {
		return out
	}
	if fm, ok := from.(map[string]any); ok {
		if tm, ok := to.(map[string]any); ok && (len(fm) == 0 || sharesKey(fm, tm)) {
			for _, k := range sortedKeys(fm) {
				if _, ok := tm[k]; !ok {
					out = append(out, change{kind: removeKey, path: p.key(k), from: fm[k]})
				}
			}
			for _, k := range sortedKeys(tm) {
				if fv, ok := fm[k]; ok {
					out = diff(fv, tm[k], p.key(k), out)
				} else {
					out = append(out, change{kind: addKey, path: p.key(k), to: tm[k]})
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
	if isScalar(from) && isScalar(to) {
		return append(out, change{kind: setScalar, path: p, from: from, to: to})
	}
	return append(out, change{kind: replaceNode, path: p, from: from, to: to})
}

// diffList pairs the items of two lists: equal items at both ends are kept, the items in between
// are compared pairwise, and the surplus is removed or inserted. It reports false when no
// original item would survive, in which case the whole list is replaced.
func diffList(from, to []any, p path, out []change) ([]change, bool) {
	pre := 0
	for pre < len(from) && pre < len(to) && equal(from[pre], to[pre]) {
		pre++
	}
	suf := 0
	for suf < len(from)-pre && suf < len(to)-pre && equal(from[len(from)-1-suf], to[len(to)-1-suf]) {
		suf++
	}
	oldMid, newMid := from[pre:len(from)-suf], to[pre:len(to)-suf]
	paired := min(len(oldMid), len(newMid))
	if len(from) > 0 && pre+suf+paired == 0 {
		return out, false
	}
	for i := range paired {
		out = diff(oldMid[i], newMid[i], p.index(pre+i), out)
	}
	for i := paired; i < len(oldMid); i++ {
		out = append(out, change{kind: removeItem, path: p.index(pre + i), from: oldMid[i]})
	}
	for j := paired; j < len(newMid); j++ {
		out = append(out, change{kind: insertItem, path: p.index(pre + j), to: newMid[j], after: pre + paired - 1})
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

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// valueAt returns the value at p in a normalized document, or nil when there is none.
func valueAt(doc any, p path) any {
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
