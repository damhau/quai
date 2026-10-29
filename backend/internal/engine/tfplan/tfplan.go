// Package tfplan reads Terraform plans (the output of `terraform show -json`, parsed with
// hashicorp/terraform-json) and maps their resource changes to Quai resources through each
// type's stack and address patterns (docs/design/02-resource-types-and-generators.md#terraform-addresses).
//
// What it keeps is what docs/design/06-runs-status-and-drift.md allows Quai to store: each
// change's address, action and the names of the attributes it changes; attribute values only
// when asked for, and never those Terraform marks sensitive.
package tfplan

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	tfjson "github.com/hashicorp/terraform-json"
)

// Actions of a change.
const (
	Create  = "create"
	Update  = "update"
	Delete  = "delete"
	Replace = "replace"
)

// Value is an attribute's value before and after a change.
type Value struct {
	Before any `json:"before,omitempty"`
	After  any `json:"after,omitempty"`
	// Unknown: the new value is known only after the apply.
	Unknown bool `json:"unknown,omitempty"`
}

// Change is what a plan does to one Terraform resource instance.
type Change struct {
	Address string `json:"address"`
	Action  string `json:"action"`
	// Attributes are the attributes an update or a replace changes, as dotted paths such as
	// input.cpu or disks.0.size_gb, sorted; for a create or a delete, the whole resource changes.
	Attributes []string `json:"attributes,omitempty"`
	// Values are the before and after values of the changed attributes, when Options.Values
	// asks for them, except sensitive ones.
	Values map[string]Value `json:"values,omitempty"`
}

// Options say what Parse keeps.
type Options struct {
	// Values keeps the values of changed attributes that are not sensitive.
	Values bool
}

// Parse reads a plan and returns its changes to managed resources, without no-ops and reads,
// sorted by address.
func Parse(data []byte, opts Options) ([]Change, error) {
	var p tfjson.Plan
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("not a Terraform plan in JSON (terraform show -json): %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("not a Terraform plan in JSON (terraform show -json): %w", err)
	}
	var hidden []string
	if opts.Values {
		hidden = sensitiveValues(&p)
	}
	var changes []Change
	for _, rc := range p.ResourceChanges {
		if rc == nil || rc.Change == nil || rc.Mode != tfjson.ManagedResourceMode {
			continue
		}
		c := Change{Address: rc.Address}
		a := rc.Change.Actions
		switch {
		case a.Replace():
			c.Action = Replace
		case a.Create():
			c.Action = Create
		case a.Update():
			c.Action = Update
		case a.Delete():
			c.Action = Delete
		default: // no-op, read, forget: nothing changes in the infrastructure
			continue
		}
		if c.Action == Update || c.Action == Replace {
			var leaves []leaf
			ch := rc.Change
			walk("", ch.Before, ch.After, ch.BeforeSensitive, ch.AfterSensitive, ch.AfterUnknown, &leaves)
			for _, l := range leaves {
				c.Attributes = append(c.Attributes, l.path)
				// Terraform does not mark every copy of a sensitive value (terraform_data's output
				// holds its input unmarked): a value holding one is not kept either.
				if opts.Values && !l.sensitive && !holds(l.before, hidden) && !holds(l.after, hidden) {
					if c.Values == nil {
						c.Values = map[string]Value{}
					}
					v := Value{Before: l.before, Unknown: l.unknown}
					if !l.unknown {
						v.After = l.after
					}
					c.Values[l.path] = v
				}
			}
			slices.Sort(c.Attributes)
		}
		changes = append(changes, c)
	}
	slices.SortFunc(changes, func(a, b Change) int { return compare(a.Address, b.Address) })
	return changes, nil
}

// sensitiveValues returns the strings a plan marks sensitive: in resource and output changes,
// and the values of sensitive variables.
func sensitiveValues(p *tfjson.Plan) []string {
	var out []string
	for _, rc := range p.ResourceChanges {
		if rc != nil && rc.Change != nil {
			collect(rc.Change.Before, rc.Change.BeforeSensitive, &out)
			collect(rc.Change.After, rc.Change.AfterSensitive, &out)
		}
	}
	for _, c := range p.OutputChanges {
		if c != nil {
			collect(c.Before, c.BeforeSensitive, &out)
			collect(c.After, c.AfterSensitive, &out)
		}
	}
	if p.Config != nil && p.Config.RootModule != nil {
		for name, v := range p.Config.RootModule.Variables {
			if pv := p.Variables[name]; v != nil && v.Sensitive && pv != nil {
				collect(pv.Value, true, &out)
			}
		}
	}
	return out
}

// collect adds the strings of v that marker marks sensitive.
func collect(v, marker any, out *[]string) {
	switch v := v.(type) {
	case string:
		if marked(marker) && v != "" {
			*out = append(*out, v)
		}
	case map[string]any:
		for k, e := range v {
			collect(e, child(marker, k), out)
		}
	case []any:
		for i, e := range v {
			collect(e, index(marker, i), out)
		}
	}
}

// holds reports whether v is, or holds a string containing, one of the sensitive strings.
func holds(v any, sensitive []string) bool {
	switch v := v.(type) {
	case string:
		for _, s := range sensitive {
			if v == s || len(s) >= 4 && strings.Contains(v, s) {
				return true
			}
		}
	case map[string]any:
		for _, e := range v {
			if holds(e, sensitive) {
				return true
			}
		}
	case []any:
		for _, e := range v {
			if holds(e, sensitive) {
				return true
			}
		}
	}
	return false
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// leaf is one attribute a change changes.
type leaf struct {
	path               string
	before, after      any
	sensitive, unknown bool
}

// walk compares before and after, with Terraform's sensitive and unknown markers for them (true
// for the whole value, or an object or array of markers), and records the attributes that
// change: values that differ, and values that are unknown until the apply.
func walk(path string, before, after, beforeSensitive, afterSensitive, afterUnknown any, out *[]leaf) {
	// A value is kept only if nothing in it is sensitive.
	sensitive := anyMarked(beforeSensitive) || anyMarked(afterSensitive)
	if marked(afterUnknown) {
		*out = append(*out, leaf{path: path, before: before, sensitive: sensitive, unknown: true})
		return
	}
	// A sensitive value is one attribute, whatever it holds: not even the names of its keys
	// are kept.
	if marked(beforeSensitive) || marked(afterSensitive) {
		if !reflect.DeepEqual(before, after) {
			*out = append(*out, leaf{path: path, sensitive: true})
		}
		return
	}
	bm, bIsMap := before.(map[string]any)
	am, aIsMap := after.(map[string]any)
	bl, bIsList := before.([]any)
	al, aIsList := after.([]any)
	switch {
	case (bIsMap || before == nil) && (aIsMap || after == nil) && (bIsMap || aIsMap):
		keys := slices.Sorted(maps.Keys(bm))
		for k := range am {
			if _, ok := bm[k]; !ok {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			walk(join(path, k), bm[k], am[k], child(beforeSensitive, k), child(afterSensitive, k), child(afterUnknown, k), out)
		}
	case (bIsList || before == nil) && (aIsList || after == nil) && (bIsList || aIsList):
		for i := range max(len(bl), len(al)) {
			var b, a any
			if i < len(bl) {
				b = bl[i]
			}
			if i < len(al) {
				a = al[i]
			}
			walk(join(path, strconv.Itoa(i)), b, a, index(beforeSensitive, i), index(afterSensitive, i), index(afterUnknown, i), out)
		}
	case !reflect.DeepEqual(before, after):
		*out = append(*out, leaf{path: path, before: before, after: after, sensitive: sensitive})
	}
}

func join(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

// marked reports whether a marker covers the whole value.
func marked(m any) bool { b, _ := m.(bool); return b }

// anyMarked reports whether a marker covers the value or any part of it.
func anyMarked(m any) bool {
	switch m := m.(type) {
	case bool:
		return m
	case map[string]any:
		for _, v := range m {
			if anyMarked(v) {
				return true
			}
		}
	case []any:
		for _, v := range m {
			if anyMarked(v) {
				return true
			}
		}
	}
	return false
}

// child is the marker of an object's attribute: a whole-value marker covers its attributes.
func child(m any, k string) any {
	if mm, ok := m.(map[string]any); ok {
		return mm[k]
	}
	return m
}

// index is the marker of an array's element.
func index(m any, i int) any {
	if l, ok := m.([]any); ok {
		if i < len(l) {
			return l[i]
		}
		return nil
	}
	return m
}

// Summary counts changes as Terraform does: a replace adds one resource and destroys one.
func Summary(changes []Change) (add, change, destroy int) {
	for _, c := range changes {
		switch c.Action {
		case Create:
			add++
		case Update:
			change++
		case Delete:
			destroy++
		case Replace:
			add++
			destroy++
		}
	}
	return add, change, destroy
}
