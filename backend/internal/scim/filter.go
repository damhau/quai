package scim

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// clause is one comparison of a filter: an attribute, folded to lower case, equal to a value.
type clause struct {
	attr  string
	value string
}

// maxFilter bounds a filter's length; maxClauses its comparisons.
const (
	maxFilter  = 1024
	maxClauses = 4
)

// parseFilter parses the filters Microsoft Entra ID's provisioning sends, and nothing else
// (docs/design/08-identity-access-tenancy.md#scim-provisioning): comparisons `attribute eq
// "value"` of the attributes allowed (in lower case), each at most once, joined by `and`. The
// attribute names and the operators are case-insensitive, the values JSON strings. Anything else
// is an invalidFilter error. The filter's text never reaches SQL: its values become parameters.
func parseFilter(s string, allowed ...string) ([]clause, error) {
	if len(s) > maxFilter {
		return nil, fmt.Errorf("the filter is longer than %d bytes", maxFilter)
	}
	var out []clause
	rest := strings.TrimLeft(s, " ")
	for {
		attr, after, ok := strings.Cut(rest, " ")
		if !ok || attr == "" {
			return nil, fmt.Errorf("the filter must be comparisons such as userName eq %q", "value")
		}
		attr = strings.ToLower(attr)
		if !slices.Contains(allowed, attr) {
			return nil, fmt.Errorf("the filter compares %q: only %s are supported", attr, strings.Join(allowed, ", "))
		}
		if slices.ContainsFunc(out, func(c clause) bool { return c.attr == attr }) {
			return nil, fmt.Errorf("the filter compares %q twice", attr)
		}
		op, after, ok := strings.Cut(strings.TrimLeft(after, " "), " ")
		if !ok || !strings.EqualFold(op, "eq") {
			return nil, fmt.Errorf("the filter's operator must be eq")
		}
		value, after, err := jsonString(strings.TrimLeft(after, " "))
		if err != nil {
			return nil, err
		}
		out = append(out, clause{attr: attr, value: value})
		if len(out) > maxClauses {
			return nil, fmt.Errorf("the filter has more than %d comparisons", maxClauses)
		}
		rest = strings.TrimLeft(after, " ")
		if rest == "" {
			return out, nil
		}
		if len(after) == len(rest) {
			return nil, fmt.Errorf("the filter's comparisons must be joined by and")
		}
		and, next, ok := strings.Cut(rest, " ")
		if !ok || !strings.EqualFold(and, "and") {
			return nil, fmt.Errorf("the filter's comparisons must be joined by and")
		}
		rest = strings.TrimLeft(next, " ")
	}
}

// jsonString reads the JSON string s starts with, and returns it and what follows.
func jsonString(s string) (string, string, error) {
	if !strings.HasPrefix(s, `"`) {
		return "", "", fmt.Errorf("the filter's values must be quoted strings")
	}
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			var v string
			if err := json.Unmarshal([]byte(s[:i+1]), &v); err != nil {
				return "", "", fmt.Errorf("the filter has a malformed string")
			}
			if strings.ContainsFunc(v, unicode.IsControl) {
				return "", "", fmt.Errorf("the filter has a control character")
			}
			return v, s[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("the filter has a string without its closing quote")
}
