package yamledit

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/token"
)

// render writes a normalized scalar as YAML text that fits s.
func (f *file) render(v any, s slot, p path) (string, error) {
	switch v := v.(type) {
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case uint64:
		return strconv.FormatUint(v, 10), nil
	case float64:
		return formatFloat(v), nil
	case string:
		return f.renderString(v, s), nil
	}
	return "", fmt.Errorf("yamledit: %s: cannot write a value of type %T", p, v)
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
	blockOriginal := s.kind == token.LiteralType || s.kind == token.FoldedType
	quoted := s.kind == token.SingleQuoteType || s.kind == token.DoubleQuoteType
	if s.block && !quoted && (blockOriginal || strings.Contains(v, "\n")) {
		indent := s.blockIndent
		if indent == 0 {
			indent = s.indent + f.width
		}
		if lit, ok := literal(v, indent, f.newline(s.start)); ok {
			return lit
		}
	}
	switch {
	case s.kind == token.DoubleQuoteType:
		return strconv.Quote(v)
	case s.kind == token.SingleQuoteType:
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
// that way without an indentation indicator or keep chomping.
func literal(v string, indent int, nl string) (string, bool) {
	header, body := "|-", v
	if strings.HasSuffix(v, "\n") {
		header, body = "|", strings.TrimSuffix(v, "\n")
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
