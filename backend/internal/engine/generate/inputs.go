package generate

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/engine/schema"
)

// ValidateInputs checks the inputs of an action form, as the prototype's validateInputs did:
// defaults are applied (a boolean defaults to false, a required enum to its first option),
// strings from a form are converted to numbers and booleans, inputs hidden by visibleIf are
// dropped, and each input is checked against its type, options, bounds and pattern. vars are
// the variables the visibleIf conditions see besides `action`, which holds the inputs. The
// result has only declared inputs.
func ValidateInputs(defs []definitions.Input, raw map[string]any, vars expr.Vars) (map[string]any, error) {
	values := map[string]any{}
	for _, d := range defs {
		v, set := raw[d.Name]
		if !set || v == nil || v == "" {
			switch {
			case d.Default != nil:
				v, set = d.Default, true
			case d.Type == definitions.InputBoolean:
				v, set = false, true
			case d.Type == definitions.InputEnum && d.Required && len(d.Options) > 0:
				v, set = d.Options[0], true
			default:
				set = false
			}
		}
		if set {
			values[d.Name] = coerce(d, v)
		}
	}
	errs := map[string]string{}
	for _, d := range defs {
		if d.Visible != nil {
			vars.Action = values
			visible, err := d.Visible.Test(vars)
			if err != nil {
				return nil, fmt.Errorf("visibleIf of %s: %w", d.Name, err)
			}
			if !visible {
				delete(values, d.Name)
				continue
			}
		}
		label := d.Title
		if label == "" {
			label = d.Name
		}
		v, set := values[d.Name]
		if !set {
			if d.Required {
				errs[d.Name] = label + " is required"
			}
			continue
		}
		if msg := check(d, v); msg != "" {
			errs[d.Name] = label + " " + msg
		}
	}
	if len(errs) > 0 {
		return nil, &schema.ValidationError{Fields: errs}
	}
	return values, nil
}

// coerce converts form strings to the input's type; values it cannot convert are left as they
// are, for check to report.
func coerce(d definitions.Input, v any) any {
	switch d.Type {
	case definitions.InputInteger, definitions.InputNumber:
		if s, ok := v.(string); ok {
			if i, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				return i
			}
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return f
			}
		}
	case definitions.InputBoolean:
		if s, ok := v.(string); ok {
			return s == "true"
		}
	case definitions.InputString, definitions.InputEnum:
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return v
}

func check(d definitions.Input, v any) string {
	switch d.Type {
	case definitions.InputInteger, definitions.InputNumber:
		f, ok := number(v)
		switch {
		case !ok:
			return "must be a number"
		case d.Type == definitions.InputInteger && f != math.Trunc(f):
			return "must be a whole number"
		case d.Min != nil && f < *d.Min:
			return "must be at least " + strconv.FormatFloat(*d.Min, 'f', -1, 64)
		case d.Max != nil && f > *d.Max:
			return "must be at most " + strconv.FormatFloat(*d.Max, 'f', -1, 64)
		}
	case definitions.InputBoolean:
		if _, ok := v.(bool); !ok {
			return "must be true or false"
		}
	case definitions.InputString, definitions.InputEnum:
		s, ok := v.(string)
		switch {
		case !ok:
			return "must be text"
		case d.Type == definitions.InputEnum && !slices.Contains(d.Options, s):
			return "must be one of " + strings.Join(d.Options, ", ")
		case d.Pattern != "" && !regexp.MustCompile("^(?:"+d.Pattern+")$").MatchString(s):
			return "does not match the expected format"
		}
	}
	return ""
}

func number(v any) (float64, bool) {
	switch v := v.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float64:
		return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	}
	return 0, false
}
