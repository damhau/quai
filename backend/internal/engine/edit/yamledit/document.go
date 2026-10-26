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

// change is a scalar that differs between the current and the intended document.
type change struct {
	path     path
	from, to any
}

// diff appends to out the scalar changes that turn from into to, in path order. Structural
// changes are refused.
func diff(from, to any, p path, out []change) ([]change, error) {
	if equal(from, to) {
		return out, nil
	}
	if fm, ok := from.(map[string]any); ok {
		if tm, ok := to.(map[string]any); ok {
			for _, k := range sortedKeys(fm) {
				if _, ok := tm[k]; !ok {
					return nil, refuse(p.key(k), "removing a key is not supported yet")
				}
			}
			for _, k := range sortedKeys(tm) {
				fv, ok := fm[k]
				if !ok {
					return nil, refuse(p.key(k), "adding a key is not supported yet")
				}
				var err error
				if out, err = diff(fv, tm[k], p.key(k), out); err != nil {
					return nil, err
				}
			}
			return out, nil
		}
	}
	if fs, ok := from.([]any); ok {
		if ts, ok := to.([]any); ok {
			if len(fs) != len(ts) {
				return nil, refuse(p, "adding or removing list items is not supported yet")
			}
			for i := range fs {
				var err error
				if out, err = diff(fs[i], ts[i], p.index(i), out); err != nil {
					return nil, err
				}
			}
			return out, nil
		}
	}
	if isScalar(from) && isScalar(to) {
		return append(out, change{path: p, from: from, to: to}), nil
	}
	return nil, refuse(p, "changing a value between a scalar, a mapping and a list is not supported yet")
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
