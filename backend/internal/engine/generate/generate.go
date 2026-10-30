// Package generate turns form values into the content of a resource's region and back
// (docs/design/02-resource-types-and-generators.md#generators): the identity generator (the file
// is the form) and the mapping generator (a template evaluated with the form's values and merged
// into the file). Managed fields (the owner, fields that mirror locator params, fixed labels) are
// set by Quai; fields the schema does not describe are kept on update; locked fields cannot change
// after creation; fields hidden by `visibleIf` are dropped; and the result is validated against
// the type's file schema.
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
	"github.com/damhau/quai/backend/internal/engine/edit"
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
	// Code are the values of the region defined in code (an HCL expression): Current leaves them
	// out, the editor keeps them, and the schema does not count them as missing.
	Code []edit.Code
	// ActionName and ActionInputs, for an action. The inputs are checked with ValidateInputs.
	ActionName   string
	ActionInputs map[string]any
	// Owner is the requester or their team, written to the owner field on create.
	Owner string
}

// Document returns the region's content after the request, by the type's generator. It returns
// nil for a delete.
func Document(t *definitions.ResourceType, r Request) (map[string]any, error) {
	if t.Spec.Generate.Kind == definitions.GenerateMapping {
		return Mapping(t, r)
	}
	return Identity(t, r)
}

// Mapping returns the region's content after the request, for a type with the mapping generator:
// the form's values, with the input schema's defaults, are checked against the input schema and
// bound into the template, which is merged into the region. Only the template's paths are
// managed: a binding that evaluates to null removes its key, an absent one leaves it, a mapping
// merges key by key, and everything the template does not name stays as it is. Actions set
// fields of the region as for the identity generator. It returns nil for a delete.
func Mapping(t *definitions.ResourceType, r Request) (map[string]any, error) {
	g := t.Spec.Generate
	if g.Kind != definitions.GenerateMapping || g.TemplateValue == nil {
		return nil, fmt.Errorf("type %s does not use the mapping generator", t.Metadata.Name)
	}
	if r.Kind != Create && r.Kind != Update {
		return Identity(t, r)
	}
	inputs := Inputs(t, r.Values)
	vars := expr.Vars{Inputs: inputs, Params: anyMap(r.Params), Doc: r.Current, Request: expr.Request{Kind: r.Kind}}
	visible := map[string]*expr.Program{}
	for p, f := range t.Spec.UI.Fields {
		if f.Visible != nil {
			visible[p] = f.Visible
		}
	}
	pruned, err := schema.PruneHidden(inputs, visible, vars)
	if err != nil {
		return nil, err
	}
	if err := t.InputSchema.Validate(pruned); err != nil {
		return nil, err
	}
	vars.Inputs = pruned
	set, err := g.TemplateValue.Eval(vars)
	if err != nil {
		return nil, fmt.Errorf("spec.generate.template: %w", err)
	}
	template, _ := set.(map[string]any)
	doc := merge(map[string]any{}, template)
	if r.Kind == Update {
		doc = merge(r.Current, template)
		if err := keepLocked(t, r.Current, doc); err != nil {
			return nil, err
		}
	}
	managed(t, doc, r.Params)
	if f := ownerField(t); f != "" && r.Owner != "" && r.Kind == Create {
		setPath(doc, f, r.Owner)
	}
	if err := t.FileSchema.Validate(doc); err != nil {
		var ve *schema.ValidationError
		if !errors.As(err, &ve) {
			return nil, err
		}
		if fields := edit.OutsideCode(ve.Fields, r.Code); len(fields) > 0 {
			// The template wrote what the file's schema refuses: the type's mistake, not the form's.
			return nil, fmt.Errorf("type %s: the template writes a file its schema refuses: %v", t.Metadata.Name, &schema.ValidationError{Fields: fields})
		}
	}
	return doc, nil
}

// Inputs returns the form's values with the input schema's defaults for those it leaves out.
func Inputs(t *definitions.ResourceType, values map[string]any) map[string]any {
	inputs := map[string]any{}
	if d, ok := t.InputSchema.Defaults().(map[string]any); ok {
		inputs = d
	}
	maps.Copy(inputs, values)
	return inputs
}

// merge returns base with the evaluated template merged in: each of its keys set, a mapping into
// a mapping key by key, a null one removed; the keys it does not name are left as they are.
func merge(base, template map[string]any) map[string]any {
	out := clone(base)
	for k, v := range template {
		switch v := v.(type) {
		case nil:
			delete(out, k)
		case map[string]any:
			sub, _ := out[k].(map[string]any)
			out[k] = merge(sub, v)
		default:
			out[k] = cloneValue(v)
		}
	}
	return out
}

// Identity returns the region's content after the request, for a type with the identity
// generator, or an action's on a type with the mapping generator. It returns nil for a delete.
func Identity(t *definitions.ResourceType, r Request) (map[string]any, error) {
	switch kind := t.Spec.Generate.Kind; {
	case kind == definitions.GenerateMapping && (r.Kind == Action || r.Kind == Delete):
	case kind != definitions.GenerateIdentity:
		return nil, fmt.Errorf("type %s uses the %s generator, which is not available yet", t.Metadata.Name, kind)
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
		var ve *schema.ValidationError
		if !errors.As(err, &ve) {
			return nil, err
		}
		if fields := edit.OutsideCode(ve.Fields, r.Code); len(fields) > 0 {
			return nil, &schema.ValidationError{Fields: fields}
		}
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
