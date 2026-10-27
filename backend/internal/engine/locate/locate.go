// Package locate maps resources to their regions in a repository: the part of the repository
// each resource owns (docs/design/02-resource-types-and-generators.md#locators). It implements
// the `file` and `dir` strategies: path patterns such as envs/{env}/vms/{name}.yaml, resource
// keys and IDs, the inventory scan, and region fingerprints (docs/design/03-file-editing.md).
//
// Repositories are read through an fs.FS. Symbolic links are never followed, so a link in a
// repository cannot make Quai read or fingerprint a file outside it.
package locate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
)

// Strategies this package implements.
const (
	File = "file"
	Dir  = "dir"
)

var (
	paramRe   = regexp.MustCompile(`\{([^{}]*)\}`)
	identRe   = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]*$`)
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	nameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

// SafeSegment reports whether v may be a param value: one path segment of 1 to 63 characters,
// letters, digits, '.', '_' and '-', starting with a letter or a digit, without "..". Values
// that are not safe could escape the pattern's directories or hide files.
func SafeSegment(v string) bool {
	return segmentRe.MatchString(v) && !strings.Contains(v, "..")
}

// Pattern is a compiled locator path such as envs/{env}/vms/{name}.yaml.
type Pattern struct {
	source string
	params []string
	prefix string
	re     *regexp.Regexp
}

// Compile checks and compiles a pattern: a clean relative path whose params are each one whole
// or partial path segment, named once, and separated by literal text.
func Compile(source string) (*Pattern, error) {
	if source == "" || strings.HasPrefix(source, "/") || strings.Contains(source, `\`) ||
		slices.ContainsFunc(strings.Split(source, "/"), func(s string) bool { return s == "" || s == "." || s == ".." }) {
		return nil, fmt.Errorf("pattern %q must be a clean relative path", source)
	}
	if strings.Contains(source, "}{") {
		return nil, fmt.Errorf("pattern %q: params must be separated by literal text", source)
	}
	p := &Pattern{source: source}
	expr := "^"
	last := 0
	for _, m := range paramRe.FindAllStringSubmatchIndex(source, -1) {
		name := source[m[2]:m[3]]
		if !identRe.MatchString(name) {
			return nil, fmt.Errorf("pattern %q: {%s} is not a valid param name", source, name)
		}
		if slices.Contains(p.params, name) {
			return nil, fmt.Errorf("pattern %q: {%s} appears twice", source, name)
		}
		p.params = append(p.params, name)
		expr += regexp.QuoteMeta(source[last:m[0]]) + "([^/]+)"
		last = m[1]
	}
	rest := source[last:]
	if strings.ContainsAny(rest, "{}") || strings.Count(source, "{") != len(p.params) {
		return nil, fmt.Errorf("pattern %q has unbalanced braces", source)
	}
	if len(p.params) == 0 {
		return nil, fmt.Errorf("pattern %q needs at least one {param}", source)
	}
	p.re = regexp.MustCompile(expr + regexp.QuoteMeta(rest) + "$")
	first := strings.Index(source, "{")
	p.prefix = source[:strings.LastIndex(source[:first], "/")+1]
	return p, nil
}

func (p *Pattern) String() string { return p.source }

// Params returns the param names in the order they appear.
func (p *Pattern) Params() []string { return slices.Clone(p.params) }

// Prefix returns the fixed directories before the first param, such as "envs/": where an
// inventory scan starts. It is empty when the pattern starts with a param.
func (p *Pattern) Prefix() string { return p.prefix }

// Match returns the params of a path that matches the pattern with safe values.
func (p *Pattern) Match(file string) (map[string]string, bool) {
	m := p.re.FindStringSubmatch(file)
	if m == nil {
		return nil, false
	}
	params := make(map[string]string, len(p.params))
	for i, name := range p.params {
		if !SafeSegment(m[i+1]) {
			return nil, false
		}
		params[name] = m[i+1]
	}
	return params, true
}

// Format returns the path for params, refusing missing or unsafe values.
func (p *Pattern) Format(params map[string]string) (string, error) {
	var err error
	out := paramRe.ReplaceAllStringFunc(p.source, func(m string) string {
		name := m[1 : len(m)-1]
		v := params[name]
		if !SafeSegment(v) && err == nil {
			err = fmt.Errorf("invalid value for {%s}: %q", name, v)
		}
		return v
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// Key returns a resource's key within its type: its param values in pattern order, joined by
// "/". Values cannot contain "/", so the key identifies the params.
func (p *Pattern) Key(params map[string]string) string {
	vals := make([]string, len(p.params))
	for i, name := range p.params {
		vals[i] = params[name]
	}
	return strings.Join(vals, "/")
}

// ParseKey returns the params of a key.
func (p *Pattern) ParseKey(key string) (map[string]string, error) {
	vals := strings.Split(key, "/")
	if len(vals) != len(p.params) {
		return nil, fmt.Errorf("key %q does not have the %d parts of %s", key, len(p.params), p.source)
	}
	params := make(map[string]string, len(vals))
	for i, v := range vals {
		if !SafeSegment(v) {
			return nil, fmt.Errorf("key %q: invalid value for {%s}: %q", key, p.params[i], v)
		}
		params[p.params[i]] = v
	}
	return params, nil
}

// ID returns a resource's identifier across Quai, <repository>/<type>/<key>, such as
// infra-live/vm/prod/web01.
func ID(repository, typeName, key string) string {
	return repository + "/" + typeName + "/" + key
}

// ParseID splits a resource ID. The key is checked only for safe segments; its number of parts
// depends on the type's pattern.
func ParseID(id string) (repository, typeName, key string, err error) {
	parts := strings.SplitN(id, "/", 3)
	if len(parts) != 3 || !nameRe.MatchString(parts[0]) || !nameRe.MatchString(parts[1]) {
		return "", "", "", fmt.Errorf("resource ID %q is not <repository>/<type>/<key>", id)
	}
	for _, v := range strings.Split(parts[2], "/") {
		if !SafeSegment(v) {
			return "", "", "", fmt.Errorf("resource ID %q: invalid key part %q", id, v)
		}
	}
	return parts[0], parts[1], parts[2], nil
}

// Locator finds the regions of one resource type.
type Locator struct {
	strategy string
	pattern  *Pattern
}

// New returns the locator for a strategy and path pattern.
func New(strategy, pattern string) (*Locator, error) {
	if strategy != File && strategy != Dir {
		return nil, fmt.Errorf("locate strategy %q is not supported", strategy)
	}
	p, err := Compile(pattern)
	if err != nil {
		return nil, err
	}
	return &Locator{strategy: strategy, pattern: p}, nil
}

// Pattern returns the locator's path pattern.
func (l *Locator) Pattern() *Pattern { return l.pattern }

// Region is the part of the repository one resource owns: a file, or a directory and everything
// under it.
type Region struct {
	Strategy string
	Path     string
	Key      string
	Params   map[string]string
}

// Region returns the region of the resource with params.
func (l *Locator) Region(params map[string]string) (Region, error) {
	p, err := l.pattern.Format(params)
	if err != nil {
		return Region{}, err
	}
	clean := make(map[string]string, len(l.pattern.params))
	for _, name := range l.pattern.params {
		clean[name] = params[name]
	}
	return Region{Strategy: l.strategy, Path: p, Key: l.pattern.Key(clean), Params: clean}, nil
}

// Scan returns the region of every resource in fsys, sorted by key: each regular file (for
// `file`) or directory (for `dir`) whose path matches the pattern with safe values. Symbolic
// links and .git directories are skipped.
func (l *Locator) Scan(fsys fs.FS) ([]Region, error) {
	root := strings.TrimSuffix(l.pattern.prefix, "/")
	if root == "" {
		root = "."
	}
	var out []Region
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 || (d.IsDir() && d.Name() == ".git") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		wantDir := l.strategy == Dir
		if d.IsDir() != wantDir || (!wantDir && !d.Type().IsRegular()) {
			return nil
		}
		params, ok := l.pattern.Match(p)
		if !ok {
			return nil
		}
		out = append(out, Region{Strategy: l.strategy, Path: p, Key: l.pattern.Key(params), Params: params})
		if wantDir {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", l.pattern.source, err)
	}
	slices.SortFunc(out, func(a, b Region) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

// Absent is the fingerprint of a region that does not exist, such as the file of a resource
// being created: if the file appears before the request is delivered, the request is stale.
const Absent = "absent"

// Fingerprint returns "sha256:" and the hex SHA-256 of a region's bytes, or Absent. For a
// directory it hashes, in path order, each regular file's path relative to the directory and the
// SHA-256 of its content, so that renaming, adding or removing a file changes it too. Symbolic
// links are refused.
func Fingerprint(fsys fs.FS, r Region) (string, error) {
	info, err := fs.Lstat(fsys, r.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return Absent, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symbolic link; Quai does not follow links", r.Path)
	}
	h := sha256.New()
	switch {
	case r.Strategy == File && info.Mode().IsRegular():
		b, err := fs.ReadFile(fsys, r.Path)
		if err != nil {
			return "", err
		}
		h.Write(b)
	case r.Strategy == Dir && info.IsDir():
		err := fs.WalkDir(fsys, r.Path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("%s is a symbolic link; Quai does not follow links", p)
			}
			if !d.Type().IsRegular() {
				return nil
			}
			b, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			rel := strings.TrimPrefix(p, r.Path+"/")
			fmt.Fprintf(h, "%s  %s\n", hex.EncodeToString(sum[:]), rel)
			return nil
		})
		if err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("%s is not a %s", r.Path, map[string]string{File: "regular file", Dir: "directory"}[r.Strategy])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
