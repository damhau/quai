package expr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

// Absent is the result of an expression that evaluates to `optional.none()`, such as
// `inputs.?nope` when inputs has no nope. A template key bound to it is left out, so the
// generator leaves the file's value as it is; null, by contrast, removes the key.
var Absent = absent{}

type absent struct{}

func (absent) String() string { return "absent" }

// normalize converts a value in the JSON data model to the form CEL evaluates: whole numbers
// become int64 and other numbers float64, nested maps and lists are converted, and anything
// outside the data model is an error.
func normalize(v any) (any, error) {
	switch v := v.(type) {
	case nil, bool, string, int64:
		return v, nil
	case int:
		return int64(v), nil
	case float64:
		return number(v)
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, nil
		}
		f, err := v.Float64()
		if err != nil {
			return nil, fmt.Errorf("number %s: %w", v, err)
		}
		return number(f)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			n, err := normalize(x)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			n, err := normalize(x)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = n
		}
		return out, nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool:
		return rv.Bool(), nil
	case reflect.String:
		return rv.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		u := rv.Uint()
		if u > math.MaxInt64 {
			return nil, fmt.Errorf("integer %d is too large", u)
		}
		return int64(u), nil
	case reflect.Float32, reflect.Float64:
		return number(rv.Float())
	case reflect.Slice, reflect.Array:
		if rv.Kind() == reflect.Slice && rv.IsNil() {
			return []any{}, nil
		}
		out := make([]any, rv.Len())
		for i := range out {
			n, err := normalize(rv.Index(i).Interface())
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}
			out[i] = n
		}
		return out, nil
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map keys must be strings, not %s", rv.Type().Key())
		}
		out := make(map[string]any, rv.Len())
		for it := rv.MapRange(); it.Next(); {
			k := it.Key().String()
			n, err := normalize(it.Value().Interface())
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = n
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported value of type %T", v)
}

// number returns f as an int64 when it is a whole number that fits, and as a float64 otherwise.
func number(f float64) (any, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("number %v is not finite", f)
	}
	if f == math.Trunc(f) && f >= math.MinInt64 && f < math.MaxInt64 {
		return int64(f), nil
	}
	return f, nil
}

// fromCEL converts a result to the JSON data model. Only the top level may be an optional value.
func fromCEL(v ref.Val, top bool) (any, error) {
	switch v := v.(type) {
	case types.Null:
		return nil, nil
	case types.Bool:
		return bool(v), nil
	case types.Int:
		return int64(v), nil
	case types.Uint:
		return uint64(v), nil
	case types.Double:
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("result %v is not a finite number", f)
		}
		return f, nil
	case types.String:
		return string(v), nil
	case *types.Optional:
		if !top {
			return nil, errors.New("an optional value can only be the whole result")
		}
		if !v.HasValue() {
			return Absent, nil
		}
		return fromCEL(v.GetValue(), false)
	case traits.Lister:
		n, ok := v.Size().(types.Int)
		if !ok {
			return nil, errors.New("list without a size")
		}
		out := make([]any, 0, int(n))
		for it := v.Iterator(); it.HasNext() == types.True; {
			x, err := fromCEL(it.Next(), false)
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, nil
	case traits.Mapper:
		out := map[string]any{}
		for it := v.Iterator(); it.HasNext() == types.True; {
			k := it.Next()
			ks, ok := k.(types.String)
			if !ok {
				return nil, fmt.Errorf("map keys must be strings, not %s", k.Type().TypeName())
			}
			x, err := fromCEL(v.Get(k), false)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", string(ks), err)
			}
			out[string(ks)] = x
		}
		return out, nil
	}
	return nil, fmt.Errorf("a result of type %s cannot be written; convert it with string()", v.Type().TypeName())
}

// format returns how a value is interpolated into a string: strings as they are, null and
// Absent as nothing, anything else as JSON.
func format(v any) (string, error) {
	switch v := v.(type) {
	case nil, absent:
		return "", nil
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return string(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}
