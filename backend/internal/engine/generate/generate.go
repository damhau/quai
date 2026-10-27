// Package generate turns form values into the content of a resource's region and back
// (docs/design/02-resource-types-and-generators.md#generators). Phase 0 implements the identity
// generator: the file is the form. Managed fields (the owner, fields that mirror locator params,
// fixed labels) are set by Quai; fields the schema does not describe are kept on update; locked
// fields cannot change after creation; fields hidden by `visibleIf` are dropped; and the result
// is validated against the type's file schema.
//
// Errors about what a person entered are *schema.ValidationError, keyed by field path, so that a
// form can show them next to the fields.
package generate

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/engine/schema"
)

// Request kinds.
const (
	Create = "create"
	Update = "update"
	Action = "action"
	Delete = "delete"
)

// Request is a change to one resource of a type.
type Request struct {
	Kind   string
	Params map[string]string // the locator params
	// Values are the form values, for create and update.
	Values map[string]any
	// Current is the region's content in Git, for update, action and delete.
	Current map[string]any
	// ActionName and ActionInputs, for an action. The inputs are checked with ValidateInputs.
	ActionName   string
	ActionInputs map[string]any
	// Owner is the requester or their team, written to the owner field on create.
	Owner string
}

// Identity returns the region's content after the request, for a type with the identity
// generator. It returns nil for a delete.
func Identity(t *definitions.ResourceType, r Request) (map[string]any, error) {
	if t.Spec.Generate.Kind != definitions.GenerateIdentity {
		return nil, fmt.Errorf("type %s uses the %s generator, which is not available yet", t.Metadata.Name, t.Spec.Generate.Kind)
	}
	var doc map[string]any
	switch r.Kind {
	case Create:
		doc = clone(r.Values)
		managed(t, doc, r.Params)
		if f := ownerField(t); f != "" && r.Owner != "" {
			setPath(doc, f, r.Owner)
		}
	case Update:
		doc = clone(r.Values)
		described := t.FileSchema.Properties()
		if len(described) > 0 {
			for k, v := range r.Current {
				if _, set := doc[k]; !set && !slices.Contains(described, k) {
					doc[k] = cloneValue(v)
				}
			}
		}
		if err := keepLocked(t, r.Current, doc); err != nil {
			return nil, err
		}
		managed(t, doc, r.Params)
	case Action:
		a := action(t, r.ActionName)
		if a == nil {
			return nil, fmt.Errorf("type %s has no action %s", t.Metadata.Name, r.ActionName)
		}
		if a.Kind == definitions.ActionImperative {
			return nil, fmt.Errorf("action %s is imperative: it runs an executor and writes no files", a.Name)
		}
		doc = clone(r.Current)
		set, err := a.SetValue.Eval(expr.Vars{Inputs: r.Current, Action: r.ActionInputs, Params: anyMap(r.Params)})
		if err != nil {
			return nil, fmt.Errorf("action %s: %w", a.Name, err)
		}
		fields, _ := set.(map[string]any)
		for _, p := range slices.Sorted(maps.Keys(fields)) {
			if err := applySet(doc, p, fields[p]); err != nil {
				return nil, fmt.Errorf("action %s: %s: %w", a.Name, p, err)
			}
		}
		managed(t, doc, r.Params)
	case Delete:
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown request kind %q", r.Kind)
	}
	visible := map[string]*expr.Program{}
	for p, f := range t.Spec.UI.Fields {
		if f.Visible != nil {
			visible[p] = f.Visible
		}
	}
	doc, err := schema.PruneHidden(doc, visible, expr.Vars{Params: anyMap(r.Params), Previous: r.Current})
	if err != nil {
		return nil, err
	}
	if err := t.FileSchema.Validate(doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func action(t *definitions.ResourceType, name string) *definitions.Action {
	for i := range t.Spec.Actions {
		if t.Spec.Actions[i].Name == name {
			return &t.Spec.Actions[i]
		}
	}
	return nil
}

func ownerField(t *definitions.ResourceType) string {
	if t.Spec.Ownership == nil {
		return ""
	}
	return t.Spec.Ownership.Field
}

// managed sets the fields Quai owns besides the owner: param mirrors and labels.
func managed(t *definitions.ResourceType, doc map[string]any, params map[string]string) {
	for _, p := range slices.Sorted(maps.Keys(t.Spec.ParamFields)) {
		setPath(doc, t.Spec.ParamFields[p], params[p])
	}
	for _, p := range slices.Sorted(maps.Keys(t.Spec.Labels)) {
		setPath(doc, p, t.Spec.Labels[p])
	}
}

// keepLocked restores locked fields the form left out, and refuses changes to them.
func keepLocked(t *definitions.ResourceType, current, doc map[string]any) error {
	var fields []string
	if f := ownerField(t); f != "" {
		fields = append(fields, f)
	}
	fields = append(fields, slices.Collect(maps.Values(t.Spec.ParamFields))...)
	for p, f := range t.Spec.UI.Fields {
		if f.Immutable {
			fields = append(fields, p)
		}
	}
	errs := map[string]string{}
	for _, f := range schema.LockedFields(t.FileSchema, fields...) {
		was, wasSet := getPath(current, f)
		now, nowSet := getPath(doc, f)
		switch {
		case !nowSet && wasSet:
			setPath(doc, f, cloneValue(was))
		case nowSet && (!wasSet || !reflect.DeepEqual(normalize(now), normalize(was))):
			errs[f] = "cannot be changed after creation"
		}
	}
	if len(errs) > 0 {
		return &schema.ValidationError{Fields: errs}
	}
	return nil
}

// applySet writes one value of an action's set: Absent leaves the field alone, null removes it.
func applySet(doc map[string]any, path string, v any) error {
	switch v {
	case expr.Absent:
		return nil
	case nil:
		deletePath(doc, path)
		return nil
	}
	if !setPath(doc, path, v) {
		return errors.New("the path does not exist in the document")
	}
	return nil
}

// segments splits a dotted path; numeric segments index lists.
func segments(path string) []string { return strings.Split(path, ".") }

func getPath(doc map[string]any, path string) (any, bool) {
	var cur any = doc
	for _, seg := range segments(path) {
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[seg]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(c) {
				return nil, false
			}
			cur = c[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// setPath sets the value at a dotted path, creating missing mappings on the way. A list index
// must exist. It reports whether the value was set.
func setPath(doc map[string]any, path string, v any) bool {
	segs := segments(path)
	var cur any = doc
	for i, seg := range segs {
		last := i == len(segs)-1
		switch c := cur.(type) {
		case map[string]any:
			if last {
				c[seg] = v
				return true
			}
			next, ok := c[seg]
			if !ok || next == nil {
				next = map[string]any{}
				c[seg] = next
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(c) {
				return false
			}
			if last {
				c[idx] = v
				return true
			}
			cur = c[idx]
		default:
			return false
		}
	}
	return false
}

func deletePath(doc map[string]any, path string) {
	segs := segments(path)
	parent, ok := getPath(doc, strings.Join(segs[:len(segs)-1], "."))
	if len(segs) == 1 {
		parent, ok = doc, true
	}
	if m, isMap := parent.(map[string]any); ok && isMap {
		delete(m, segs[len(segs)-1])
	}
}

func anyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func clone(v map[string]any) map[string]any {
	out, _ := cloneValue(v).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func cloneValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = cloneValue(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = cloneValue(x)
		}
		return out
	}
	return v
}

// normalize makes numbers comparable across int and float types.
func normalize(v any) any {
	switch v := v.(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case uint64:
		return float64(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = normalize(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = normalize(x)
		}
		return out
	}
	return v
}
