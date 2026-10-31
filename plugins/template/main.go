// A Quai generator plugin for .env files: one KEY=value line per value, comments and blank lines
// kept. It is the template to start a plugin from: change Write and Read, keep the golden cases
// in testdata up to date, and build it with make (see README.md).
package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/damhau/quai/plugins/sdk"
)

func init() { sdk.Register(dotenv{}) }

// main is not run: Quai calls the functions the SDK exports.
func main() {}

type dotenv struct{}

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Write puts each value on its key's line. A line whose value does not change stays as it is,
// the lines of keys the values no longer hold are removed, and new keys are added at the end,
// sorted.
func (dotenv) Write(r sdk.WriteRequest) (string, error) {
	values := map[string]string{}
	for k, v := range r.Values {
		s, ok := v.(string)
		if !keyRe.MatchString(k) {
			return "", fmt.Errorf("%s is not a variable name", k)
		}
		if !ok || strings.ContainsAny(s, "\r\n") {
			return "", fmt.Errorf("%s: a .env value is one line of text", k)
		}
		values[k] = s
	}
	var lines []string
	if r.Current != nil {
		lines = split(*r.Current)
	}
	var out []string
	written := map[string]bool{}
	for i, line := range lines {
		key, old, ok, err := entry(line)
		if err != nil {
			return "", fmt.Errorf("line %d: %w", i+1, err)
		}
		if !ok {
			out = append(out, line)
			continue
		}
		v, keep := values[key]
		switch {
		case !keep:
		case v == old:
			out = append(out, line)
		default:
			out = append(out, key+"="+quote(v))
		}
		written[key] = true
	}
	var added []string
	for k := range values {
		if !written[k] {
			added = append(added, k)
		}
	}
	slices.Sort(added)
	for _, k := range added {
		out = append(out, k+"="+quote(values[k]))
	}
	if len(out) == 0 {
		return "", nil
	}
	return strings.Join(out, "\n") + "\n", nil
}

// Read returns the value of each KEY=value line.
func (dotenv) Read(r sdk.ReadRequest) (map[string]any, error) {
	values := map[string]any{}
	for i, line := range split(r.Region) {
		key, v, ok, err := entry(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if !ok {
			continue
		}
		if _, dup := values[key]; dup {
			return nil, fmt.Errorf("line %d: %s is set twice", i+1, key)
		}
		values[key] = v
	}
	return values, nil
}

func split(src string) []string {
	if src == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(src, "\n"), "\n")
}

// entry parses a line: a KEY=value entry (ok), or a comment or blank line (not ok).
func entry(line string) (key, value string, ok bool, err error) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", "", false, nil
	}
	key, raw, found := strings.Cut(t, "=")
	key = strings.TrimSpace(key)
	if !found || !keyRe.MatchString(key) {
		return "", "", false, fmt.Errorf("expected KEY=value or a # comment")
	}
	value, err = unquote(strings.TrimSpace(raw))
	return key, value, true, err
}

// quote writes a value as is when it reads back the same, in double quotes otherwise.
func quote(v string) string {
	if v == strings.TrimSpace(v) && !strings.ContainsAny(v, `"'#\`) {
		return v
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}

func unquote(raw string) (string, error) {
	switch {
	case len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'':
		return raw[1 : len(raw)-1], nil
	case len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"':
		var b strings.Builder
		for i := 1; i < len(raw)-1; i++ {
			if raw[i] == '\\' && i+1 < len(raw)-1 {
				i++
			}
			b.WriteByte(raw[i])
		}
		return b.String(), nil
	case strings.ContainsAny(raw, `"'`):
		return "", fmt.Errorf("unbalanced quotes")
	}
	return raw, nil
}
