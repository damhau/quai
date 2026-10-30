package yamledit

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// render writes a normalized scalar as YAML text that fits s.
func (f *file) render(v any, s slot) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case float64:
		return formatFloat(v)
	case string:
		return f.renderString(v, s)
	}
	// Unreachable for normalized documents; verification would refuse the result.
	return fmt.Sprint(v)
}

// formatFloat writes integral numbers as integers (the JSON data model does not tell them
// apart) and keeps a decimal point in exponents, which some parsers need to read a float.
func formatFloat(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	out := strconv.FormatFloat(v, 'g', -1, 64)
	if i := strings.IndexByte(out, 'e'); i >= 0 && !strings.Contains(out[:i], ".") {
		out = out[:i] + ".0" + out[i:]
	}
	return out
}

// renderString keeps the original's quote style where it can, writes multi-line strings as
// literal block scalars where the context allows, and quotes strings that would not read back
// as the same string.
func (f *file) renderString(v string, s slot) string {
	blockOriginal := s.kind == styleLiteral || s.kind == styleFolded
	quoted := s.kind == styleSingle || s.kind == styleDouble
	if s.block && !quoted && (blockOriginal || strings.Contains(v, "\n")) {
		indent := s.blockIndent
		if indent == 0 {
			indent = s.indent + f.width
		}
		if lit, ok := literal(v, indent, s.nl, s.kind == styleFolded); ok {
			return lit
		}
	}
	switch {
	case s.kind == styleDouble:
		return strconv.Quote(v)
	case s.kind == styleSingle:
		if q, ok := singleQuoted(v); ok {
			return q
		}
		return strconv.Quote(v)
	case plainSafe(v, s.flow):
		return v
	case f.quote == '\'':
		if q, ok := singleQuoted(v); ok {
			return q
		}
	}
	return strconv.Quote(v)
}

// literal writes v as a literal block scalar with its content at indent, if it can be written
// that way without an indentation indicator or keep chomping. When the original was folded and
// v is a single line, it keeps the folded header: for one line, folded and literal mean the same.
func literal(v string, indent int, nl string, folded bool) (string, bool) {
	header, body := "|-", v
	if strings.HasSuffix(v, "\n") {
		header, body = "|", strings.TrimSuffix(v, "\n")
	}
	if folded && !strings.Contains(body, "\n") {
		header = ">" + header[1:]
	}
	if indent < 1 || strings.HasSuffix(body, "\n") || body == "" || body[0] == ' ' || body[0] == '\t' {
		return "", false
	}
	for _, r := range body {
		if r != '\n' && r != '\t' && !unicode.IsPrint(r) {
			return "", false
		}
	}
	var b strings.Builder
	b.WriteString(header)
	pad := strings.Repeat(" ", indent)
	for _, line := range strings.Split(body, "\n") {
		b.WriteString(nl)
		if line != "" {
			b.WriteString(pad + line)
		}
	}
	return b.String(), true
}

// singleQuoted writes v in single quotes, if it fits on one line without escapes.
func singleQuoted(v string) (string, bool) {
	for _, r := range v {
		if !unicode.IsPrint(r) {
			return "", false
		}
	}
	return "'" + strings.ReplaceAll(v, "'", "''") + "'", true
}

// notString matches plain scalars that YAML 1.2 or YAML 1.1 parsers read as something other
// than a string: null, booleans (including yes/no/on/off), integers in every base, floats,
// sexagesimal numbers, timestamps, and the merge and value keys. Quoting them keeps them strings
// for every tool that reads the file, not only for this one.
var notString = regexp.MustCompile(`^(?:` +
	`~|null|Null|NULL|` +
	`[yY]|[yY]es|YES|[nN]|[nN]o|NO|[tT]rue|TRUE|[fF]alse|FALSE|[oO]n|ON|[oO]ff|OFF|` +
	`[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])*(?:\.[0-9_]*)?(?:[eE][-+]?[0-9]+)?|` +
	`[-+]?\.[0-9_]+(?:[eE][-+]?[0-9]+)?|` +
	`[-+]?0[bB][01_]+|[-+]?0[oO][0-7_]+|[-+]?0[xX][0-9a-fA-F_]+|` +
	`[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN)|` +
	`[0-9]{4}-[0-9]{1,2}-[0-9]{1,2}(?:[Tt \t].*)?|` +
	`<<|=` +
	`)$`)

// plainSafe reports whether v can be written as a plain scalar and read back as the same string.
func plainSafe(v string, flow bool) bool {
	if v == "" || v != strings.TrimSpace(v) || notString.MatchString(v) {
		return false
	}
	for _, r := range v {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	if strings.ContainsRune("-?:,[]{}#&*!|>'\"%@`", rune(v[0])) {
		return false
	}
	if strings.Contains(v, ": ") || strings.Contains(v, " #") || strings.HasSuffix(v, ":") {
		return false
	}
	if flow && strings.ContainsAny(v, ",[]{}:") {
		return false
	}
	// Belt and braces: the parser must agree.
	var got map[string]any
	if err := yaml.Unmarshal([]byte("k: "+v+"\n"), &got); err != nil {
		return false
	}
	s, ok := got["k"].(string)
	return ok && s == v
}

// renderKey writes a mapping key, quoting it when a plain key would not read back as the same
// string.
func (f *file) renderKey(k string, flow bool) string {
	if plainSafe(k, flow) {
		return k
	}
	if f.quote == '\'' {
		if q, ok := singleQuoted(k); ok {
			return q
		}
	}
	return strconv.Quote(k)
}

// scalarText writes a scalar or an empty collection held by an entry at indent, in block
// context.
func (f *file) scalarText(v any, indent int, nl string) string {
	switch v := v.(type) {
	case map[string]any:
		if len(v) == 0 {
			return "{}"
		}
	case []any:
		if len(v) == 0 {
			return "[]"
		}
	}
	return f.render(v, slot{kind: stylePlain, block: true, indent: indent, nl: nl})
}

// keys returns the keys of a new mapping in the order they are written: sorted, or for Create,
// by rank and then sorted.
func (f *file) keys(m map[string]any) []string {
	keys := sortedKeys(m)
	if f.rank != nil {
		slices.SortStableFunc(keys, func(a, b string) int {
			ra, oka := f.rank[a]
			rb, okb := f.rank[b]
			switch {
			case oka && okb:
				return ra - rb
			case oka:
				return -1
			case okb:
				return 1
			}
			return 0
		})
	}
	return keys
}

// flowText writes v in flow style. New keys are written in sorted order.
func (f *file) flowText(v any) string {
	switch v := v.(type) {
	case map[string]any:
		parts := make([]string, 0, len(v))
		for _, k := range f.keys(v) {
			parts = append(parts, f.renderKey(k, true)+": "+f.flowText(v[k]))
		}
		if f.pad && len(parts) > 0 {
			return "{ " + strings.Join(parts, ", ") + " }"
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = f.flowText(item)
		}
		if f.pad && len(parts) > 0 {
			return "[ " + strings.Join(parts, ", ") + " ]"
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return f.render(v, slot{kind: stylePlain, flow: true})
}

// isCollection reports whether v is a mapping or a list with something in it.
func isCollection(v any) bool {
	switch v := v.(type) {
	case map[string]any:
		return len(v) > 0
	case []any:
		return len(v) > 0
	}
	return false
}

// blockText writes a non-empty mapping or list in block style at indent, with the file's
// indentation and sequence style, without a final line break. New keys are written in sorted
// order.
func (f *file) blockText(v any, indent int, nl string) string {
	var lines []string
	like := f.like
	defer func() { f.like = like }()
	switch v := v.(type) {
	case map[string]any:
		for _, k := range f.keys(v) {
			f.like = childLike(like, k)
			lines = append(lines, f.entryText(k, v[k], indent, nl))
		}
	case []any:
		for _, item := range v {
			f.like = nil
			if like != nil && like.Kind == yaml.SequenceNode && len(like.Content) > 0 {
				f.like = like.Content[0]
			}
			lines = append(lines, f.itemText(item, indent, nl))
		}
	}
	return strings.Join(lines, nl)
}

// entryText writes the mapping entry k: v with its key at indent.
func (f *file) entryText(k string, v any, indent int, nl string) string {
	if text, ok := f.flowLike(v); ok {
		return strings.Repeat(" ", indent) + f.renderKey(k, false) + ": " + text
	}
	return strings.Repeat(" ", indent) + f.renderKey(k, false) + ":" + f.valueText(v, indent, false, nl)
}

// itemText writes the sequence item v with its '-' at indent.
func (f *file) itemText(v any, indent int, nl string) string {
	if text, ok := f.flowLike(v); ok {
		return strings.Repeat(" ", indent) + "-" + strings.Repeat(" ", f.item-1) + text
	}
	return strings.Repeat(" ", indent) + "-" + f.valueText(v, indent, true, nl)
}

// flowLike writes v in flow style when the sibling node it is written like is a collection of
// the same kind in flow style, with brackets padded as the sibling's are.
func (f *file) flowLike(v any) (string, bool) {
	n := f.like
	if n == nil || n.Style&yaml.FlowStyle == 0 || !isCollection(v) {
		return "", false
	}
	if _, isMap := v.(map[string]any); isMap != (n.Kind == yaml.MappingNode) || !isMap && n.Kind != yaml.SequenceNode {
		return "", false
	}
	off, ok := f.offset(n)
	f.pad = ok && off+1 < len(f.src) && f.src[off+1] == ' '
	defer func() { f.pad = false }()
	return f.flowText(v), true
}

// childLike returns the value of key k in the mapping node n, or nil.
func childLike(n *yaml.Node, k string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == k {
			return n.Content[i+1]
		}
	}
	return nil
}

// valueText writes v as the value of an entry at indent: after "key:" or, when dash is set,
// after "-". With indent -1, v is the whole document.
func (f *file) valueText(v any, indent int, dash bool, nl string) string {
	switch {
	case !isCollection(v) && indent < 0:
		return f.scalarText(v, 0, nl)
	case !isCollection(v):
		return " " + f.scalarText(v, indent, nl)
	case indent < 0:
		return f.blockText(v, 0, nl)
	case dash:
		// "- key: value" or "- - item": the first line shares the '-' line.
		inner := f.blockText(v, indent+f.item, nl)
		return strings.Repeat(" ", f.item-1) + inner[indent+f.item:]
	}
	if _, ok := v.(map[string]any); ok {
		return nl + f.blockText(v, indent+f.width, nl)
	}
	return nl + f.blockText(v, indent+f.seq, nl)
}
