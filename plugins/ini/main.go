// A Quai generator plugin for INI files, a format Quai's built-in editors (YAML, JSON, HCL) do
// not know. The values are an object: a string is a key before the first section, an object is a
// section of keys. INI values are text, so every value is a string.
//
//	owner = web-team          {"owner": "web-team",
//	                           "server": {"host": "web.example.com", "port": "8080"}}
//	[server]
//	host = web.example.com
//	port = 8080
//
// A write keeps what the values do not change: comments, blank lines, the order of the sections
// and keys, each line's spacing and the file's line endings. A key the values no longer hold
// loses its line, a section they no longer hold loses its lines and the comments right above
// it. New keys go after the last key of their section, new sections at the end, both sorted.
package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/damhau/quai/plugins/sdk"
)

func init() { sdk.Register(ini{}) }

// main is not run: Quai calls the functions the SDK exports.
func main() {}

type ini struct{}

var keyRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// doc is the values as the plugin writes them: the keys before the first section, under "", and
// the sections.
type doc map[string]map[string]string

func (ini) Write(r sdk.WriteRequest) (string, error) {
	want, err := fromValues(r.Values)
	if err != nil {
		return "", err
	}
	eol := "\n"
	var blocks []block
	if r.Current != nil {
		if strings.Contains(*r.Current, "\r\n") {
			eol = "\r\n"
		}
		if blocks, err = parse(*r.Current); err != nil {
			return "", err
		}
	} else {
		blocks = []block{{}}
	}

	var out []string
	written := map[string]bool{}
	for i, b := range blocks {
		keys, keep := want[b.name]
		if !keep && i > 0 {
			if i == len(blocks)-1 {
				// The last section goes: so do the blank lines that separated it.
				for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
					out = out[:len(out)-1]
				}
			}
			continue
		}
		written[b.name] = true
		lines := b.write(keys)
		if i == 0 && len(b.lines) == 0 && len(lines) > 0 && len(blocks) > 1 {
			// Keys before the first section, in a file that had nothing there: apart from it.
			lines = append(lines, "")
		}
		out = append(out, lines...)
	}
	var added []string
	for name := range want {
		if !written[name] {
			added = append(added, name)
		}
	}
	slices.Sort(added)
	for _, name := range added {
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, "["+name+"]")
		out = append(out, entries(want[name], nil)...)
	}
	if len(out) == 0 {
		return "", nil
	}
	return strings.Join(out, eol) + eol, nil
}

func (ini) Read(r sdk.ReadRequest) (map[string]any, error) {
	blocks, err := parse(r.Region)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	for _, b := range blocks {
		keys := map[string]any{}
		for _, l := range b.lines {
			if l.key != "" {
				keys[l.key] = l.value
			}
		}
		if b.name == "" {
			for k, v := range keys {
				values[k] = v
			}
			continue
		}
		if _, clash := values[b.name]; clash {
			return nil, fmt.Errorf("line %d: %s is both a key and a section", b.at, b.name)
		}
		values[b.name] = keys
	}
	return values, nil
}

func fromValues(values map[string]any) (doc, error) {
	d := doc{"": {}}
	for k, v := range values {
		switch v := v.(type) {
		case map[string]any:
			if k == "" || k != strings.TrimSpace(k) || strings.ContainsAny(k, "[]\r\n") {
				return nil, fmt.Errorf("%q is not a section name", k)
			}
			d[k] = map[string]string{}
			for kk, vv := range v {
				s, err := text(k+"."+kk, kk, vv)
				if err != nil {
					return nil, err
				}
				d[k][kk] = s
			}
		default:
			s, err := text(k, k, v)
			if err != nil {
				return nil, err
			}
			d[""][k] = s
		}
	}
	return d, nil
}

func text(path, key string, v any) (string, error) {
	if !keyRe.MatchString(key) {
		return "", fmt.Errorf("%s: %q is not an INI key", path, key)
	}
	s, ok := v.(string)
	switch {
	case !ok:
		return "", fmt.Errorf("%s: an INI value is text, a string in the type's schema", path)
	case strings.ContainsAny(s, "\r\n"):
		return "", fmt.Errorf("%s: an INI value is one line", path)
	case s != strings.TrimSpace(s):
		return "", fmt.Errorf("%s: an INI value cannot start or end with spaces", path)
	}
	return s, nil
}

// A block is the keys before the first section (name "") or a section: the comments right above
// its header, the header, and its lines up to the next section's.
type block struct {
	name  string
	at    int // the header's line number
	lines []line
}

// A line of a block: a key = value entry (key set), or the header, a comment or a blank line.
type line struct {
	text   string
	key    string
	prefix string // the entry up to its value: its indentation, key and separator
	value  string
	header bool
}

func parse(src string) ([]block, error) {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	blocks := []block{{}}
	if src == "" {
		return blocks, nil
	}
	seen := map[string]int{}
	for i, text := range strings.Split(strings.TrimSuffix(src, "\n"), "\n") {
		n := i + 1
		t := strings.TrimSpace(text)
		cur := &blocks[len(blocks)-1]
		switch {
		case t == "" || t[0] == ';' || t[0] == '#':
			cur.lines = append(cur.lines, line{text: text})
		case t[0] == '[':
			name := strings.TrimSpace(strings.TrimSuffix(t[1:], "]"))
			if !strings.HasSuffix(t, "]") || name == "" || strings.ContainsAny(name, "[]") {
				return nil, fmt.Errorf("line %d: %s is not a section header", n, t)
			}
			if at, dup := seen[name]; dup {
				return nil, fmt.Errorf("line %d: the section %s is already at line %d", n, name, at)
			}
			seen[name] = n
			// The comments right above the header are the section's.
			k := len(cur.lines)
			for k > 0 && cur.lines[k-1].key == "" && !cur.lines[k-1].header && strings.TrimSpace(cur.lines[k-1].text) != "" {
				k--
			}
			next := block{name: name, at: n, lines: append(slices.Clone(cur.lines[k:]), line{text: text, header: true})}
			cur.lines = cur.lines[:k]
			blocks = append(blocks, next)
		default:
			eq := strings.IndexByte(text, '=')
			key := ""
			if eq >= 0 {
				key = strings.TrimSpace(text[:eq])
			}
			if !keyRe.MatchString(key) {
				return nil, fmt.Errorf("line %d: expected [section], key = value or a ; comment", n)
			}
			for _, l := range cur.lines {
				if l.key == key {
					return nil, fmt.Errorf("line %d: %s is set twice", n, key)
				}
			}
			rest := text[eq+1:]
			value := strings.TrimSpace(rest)
			prefix := text[:eq+1] + rest[:len(rest)-len(strings.TrimLeft(rest, " \t"))]
			cur.lines = append(cur.lines, line{text: text, key: key, prefix: prefix, value: value})
		}
	}
	return blocks, nil
}

// write returns the block's lines with keys: an unchanged entry stays as it is, a changed one
// keeps its prefix, a key keys does not hold loses its line, and new keys go after the last
// entry; in a block without entries, after the header, or before the first section after the
// file's opening comments.
func (b block) write(keys map[string]string) []string {
	var out []string
	lastEntry, anchor := -1, -1
	present := map[string]bool{}
	for _, l := range b.lines {
		if l.key == "" {
			out = append(out, l.text)
			if l.header || (b.name == "" && strings.TrimSpace(l.text) != "") {
				anchor = len(out) - 1
			}
			continue
		}
		v, keep := keys[l.key]
		present[l.key] = true
		switch {
		case !keep:
			continue
		case v == l.value:
			out = append(out, l.text)
		case l.value == "":
			out = append(out, strings.TrimRight(l.prefix, " \t")+" "+v)
		default:
			out = append(out, l.prefix+v)
		}
		lastEntry = len(out) - 1
	}
	at := lastEntry
	if at < 0 {
		at = anchor
	}
	return slices.Insert(out, at+1, entries(keys, present)...)
}

// entries returns the key = value lines of the keys skip does not hold, sorted.
func entries(keys map[string]string, skip map[string]bool) []string {
	var names []string
	for k := range keys {
		if !skip[k] {
			names = append(names, k)
		}
	}
	slices.Sort(names)
	out := make([]string, 0, len(names))
	for _, k := range names {
		if keys[k] == "" {
			out = append(out, k+" =")
		} else {
			out = append(out, k+" = "+keys[k])
		}
	}
	return out
}
