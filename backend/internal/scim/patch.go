package scim

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// operation is one operation of a PATCH (RFC 7644 §3.5.2), its op folded to lower case: add,
// replace or remove. path is as sent, "" without one; value is nil without one.
type operation struct {
	op    string
	path  string
	value json.RawMessage
}

// parsePatch reads a PATCH's body: the PatchOp schema, and 1 to maxOperations operations of add,
// replace or remove, in any case as Microsoft Entra ID sends them ("Replace").
func parsePatch(body object) ([]operation, error) {
	for k := range body {
		if k != "schemas" && k != "operations" {
			return nil, fmt.Errorf("a PATCH has schemas and Operations, not %q", k)
		}
	}
	var schemas []string
	if err := json.Unmarshal(body["schemas"], &schemas); err != nil || !slices.Contains(schemas, schemaPatch) {
		return nil, fmt.Errorf("a PATCH's schemas must be [%q]", schemaPatch)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(body["operations"], &raw); err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("a PATCH must have Operations")
	}
	if len(raw) > maxOperations {
		return nil, fmt.Errorf("a PATCH has more than %d operations", maxOperations)
	}
	out := make([]operation, 0, len(raw))
	for i, r := range raw {
		o, err := asObject(r)
		if err != nil {
			return nil, fmt.Errorf("operation %d: %w", i+1, err)
		}
		var op operation
		for k, v := range o {
			switch k {
			case "op":
				s, err := text("op", v)
				if err != nil {
					return nil, fmt.Errorf("operation %d: %w", i+1, err)
				}
				op.op = strings.ToLower(s)
			case "path":
				if err := json.Unmarshal(v, &op.path); err != nil || len(op.path) > maxPath || strings.ContainsFunc(op.path, unicode.IsControl) {
					return nil, fmt.Errorf("operation %d: path must be an attribute path of at most %d bytes", i+1, maxPath)
				}
			case "value":
				if !null(v) {
					op.value = v
				}
			default:
				return nil, fmt.Errorf("operation %d has op, path and value, not %q", i+1, k)
			}
		}
		if op.op != "add" && op.op != "replace" && op.op != "remove" {
			return nil, fmt.Errorf("operation %d: op must be add, replace or remove", i+1)
		}
		if op.op != "remove" && op.value == nil {
			return nil, fmt.Errorf("operation %d: %s needs a value", i+1, op.op)
		}
		out = append(out, op)
	}
	return out, nil
}

// attribute names an attribute path the way Quai compares it: in lower case, without the schema
// of the resource's core attributes or of the enterprise extension.
func attribute(path, core string) string {
	p := strings.ToLower(path)
	for _, prefix := range []string{strings.ToLower(core) + ":", strings.ToLower(schemaEnterprise) + ":"} {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			return rest
		}
	}
	return p
}

// pathless spreads the value of an operation without a path, an object, into one operation per
// attribute, as if each had its path; an extension's attributes nested in their schema's object
// get theirs, prefixed.
func pathless(op operation) ([]operation, error) {
	o, err := asObject(op.value)
	if err != nil {
		return nil, fmt.Errorf("an operation without a path needs an object for value")
	}
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	slices.Sort(keys) // the same order every time
	var out []operation
	for _, k := range keys {
		if k == strings.ToLower(schemaEnterprise) {
			nested, err := asObject(o[k])
			if err != nil {
				return nil, fmt.Errorf("%s must be an object", schemaEnterprise)
			}
			for nk, nv := range nested {
				out = append(out, operation{op: op.op, path: schemaEnterprise + ":" + nk, value: nv})
			}
			continue
		}
		out = append(out, operation{op: op.op, path: k, value: o[k]})
	}
	return out, nil
}

// memberFilter returns the id a path such as members[value eq "id"] names, if path is one.
func memberFilter(path string) (string, bool, error) {
	attr, f, ok := strings.Cut(path, "[")
	if !ok || !strings.EqualFold(attr, "members") {
		return "", false, nil
	}
	f, ok = strings.CutSuffix(f, "]")
	if !ok {
		return "", true, fmt.Errorf("the path %q has no closing bracket", path)
	}
	clauses, err := parseFilter(f, "value")
	if err != nil {
		return "", true, err
	}
	return clauses[0].value, true, nil
}
