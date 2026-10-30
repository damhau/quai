// Package locate maps resources to their regions in a repository: the part of the repository
// each resource owns (docs/design/02-resource-types-and-generators.md#locators). It implements
// the `file`, `dir` and `entry` strategies: path patterns such as envs/{env}/vms/{name}.yaml,
// resource keys and IDs, the inventory scan, and region fingerprints
// (docs/design/03-file-editing.md). An entry is a keyed item of a list, or an entry of a map, in
// a shared file; its region is the item's bytes, read through the file's editor (package edit).
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
	"strconv"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/edit"
	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

// Strategies this package implements.
const (
	File  = "file"
	Dir   = "dir"
	Entry = "entry"
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
	// An entry locator's collection in each file, the field naming a list's items (empty for a
	// map, whose keys name its entries), and the param whose value is the item's key.
	collection document.Path
	keyField   string
	keyParam   string
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

// NewEntry returns the locator of the entries of a shared file: in each file matching pattern,
// the list or map at collection (a dotted path), whose items are named by their keyField (a list)
// or whose keys name its entries (a map, keyField empty); the key is the value of keyParam.
func NewEntry(pattern, collection, keyField, keyParam string) (*Locator, error) {
	p, err := Compile(pattern)
	if err != nil {
		return nil, err
	}
	if collection == "" || keyParam == "" || slices.Contains(p.params, keyParam) {
		return nil, fmt.Errorf("an entry locator needs a collection and a key param that is not in its path")
	}
	return &Locator{strategy: Entry, pattern: p, collection: dotted(collection), keyField: keyField, keyParam: keyParam}, nil
}

// dotted returns a dotted field path as a document path: digits are list indices.
func dotted(field string) document.Path {
	var p document.Path
	for _, step := range strings.Split(field, ".") {
		if i, err := strconv.Atoi(step); err == nil {
			p = append(p, i)
		} else {
			p = append(p, step)
		}
	}
	return p
}

// Pattern returns the locator's path pattern.
func (l *Locator) Pattern() *Pattern { return l.pattern }

// Strategy returns the locator's strategy.
func (l *Locator) Strategy() string { return l.strategy }

// Region is the part of the repository one resource owns: a file, a directory and everything
// under it, or an entry of a shared file.
type Region struct {
	Strategy string
	Path     string
	Key      string
	Params   map[string]string
	// An entry's collection in the file and its key there.
	Collection document.Path
	Item       string
	// Problem says why an entry cannot be edited, such as a key that appears twice in its
	// collection.
	Problem string
	// keyField names a list's items, for an entry the locator returned.
	keyField string
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
	r := Region{Strategy: l.strategy, Path: p, Key: l.pattern.Key(clean), Params: clean}
	if l.strategy == Entry {
		item := params[l.keyParam]
		if !SafeSegment(item) {
			return Region{}, fmt.Errorf("{%s} is not a valid key: %q", l.keyParam, item)
		}
		r.Collection, r.Item, r.Key, r.keyField = l.collection, item, joinKey(r.Key, item), l.keyField
		r.Params[l.keyParam] = item
	}
	return r, nil
}

func joinKey(fileKey, item string) string {
	if fileKey == "" {
		return item
	}
	return fileKey + "/" + item
}

// ParseKey returns the params of a resource key: its file's params, and for an entry the key
// param, its last part.
func (l *Locator) ParseKey(key string) (map[string]string, error) {
	if l.strategy != Entry {
		return l.pattern.ParseKey(key)
	}
	fileKey, item := "", key
	if i := strings.LastIndex(key, "/"); i >= 0 {
		fileKey, item = key[:i], key[i+1:]
	}
	params := map[string]string{}
	if len(l.pattern.params) > 0 || fileKey != "" {
		var err error
		if params, err = l.pattern.ParseKey(fileKey); err != nil {
			return nil, err
		}
	}
	if !SafeSegment(item) {
		return nil, fmt.Errorf("key %q: invalid value for {%s}: %q", key, l.keyParam, item)
	}
	params[l.keyParam] = item
	return params, nil
}

// Element is an entry of a shared file: its key (a list item's key field) and its bytes.
type Element = document.Element

// Elements returns the entries of a shared file with their bytes, in file order, and a problem
// when the file cannot hold them safely: it cannot be read, its collection is not a list or a map,
// an item has no valid key, or a key appears twice. A file without the collection has none.
func (l *Locator) Elements(name string, src []byte) ([]Element, string) {
	doc, _, err := edit.Decode(name, src)
	if err != nil {
		return nil, fmt.Sprintf("the file cannot be read: %v", err)
	}
	coll := document.ValueAt(doc, l.collection)
	if coll == nil {
		return nil, ""
	}
	_, isMap := coll.(map[string]any)
	list, isList := coll.([]any)
	if !isMap && !isList || isMap != (l.keyField == "") {
		want := "a list"
		if l.keyField == "" {
			want = "a map"
		}
		return nil, fmt.Sprintf("%s is not %s", field(l.collection), want)
	}
	els, err := edit.Elements(name, src, l.collection)
	if err != nil {
		return nil, fmt.Sprintf("the entries of %s cannot be read: %v", field(l.collection), err)
	}
	out := make([]Element, 0, len(els))
	for i, el := range els {
		key := el.Key
		if isList {
			m, _ := list[i].(map[string]any)
			key, _ = m[l.keyField].(string)
		}
		if !SafeSegment(key) {
			return nil, fmt.Sprintf("entry %d of %s has no valid %s", i, field(l.collection), cmpOr(l.keyField, "key"))
		}
		if slices.ContainsFunc(out, func(e Element) bool { return e.Key == key }) {
			return nil, fmt.Sprintf("the key %q appears twice in %s", key, field(l.collection))
		}
		el.Key = key
		out = append(out, el)
	}
	return out, ""
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func field(p document.Path) string { return edit.Field(p) }

// Scan returns the region of every resource in fsys, sorted by key: each regular file (for
// `file`) or directory (for `dir`) whose path matches the pattern with safe values, or each entry
// of such files (for `entry`). Symbolic links and .git directories are skipped. The entries of a
// file that cannot hold them safely carry its problem; a file that cannot be read has none.
func (l *Locator) Scan(fsys fs.FS) ([]Region, error) {
	if l.strategy == Entry {
		files, err := l.files(fsys)
		if err != nil {
			return nil, err
		}
		var out []Region
		for _, file := range files {
			src, err := fs.ReadFile(fsys, file.Path)
			if err != nil {
				return nil, err
			}
			els, problem := l.Elements(file.Path, src)
			if problem != "" {
				els, _ = l.lenient(file.Path, src)
			}
			for _, el := range els {
				params := maps(file.Params)
				params[l.keyParam] = el.Key
				out = append(out, Region{Strategy: Entry, Path: file.Path, Key: joinKey(file.Key, el.Key), Params: params,
					Collection: l.collection, Item: el.Key, Problem: problem, keyField: l.keyField})
			}
		}
		slices.SortFunc(out, func(a, b Region) int { return strings.Compare(a.Key, b.Key) })
		return out, nil
	}
	return l.files(fsys)
}

// Files returns the files the locator's pattern matches (for `dir`, the directories), as file
// regions.
func (l *Locator) Files(fsys fs.FS) ([]Region, error) { return l.files(fsys) }

// Value returns an entry's value in its file's document and its path there, and false when the
// file holds no such entry.
func Value(doc any, r Region) (any, document.Path, bool) {
	switch coll := document.ValueAt(doc, r.Collection).(type) {
	case map[string]any:
		if v, ok := coll[r.Item]; ok {
			return v, r.Collection.Key(r.Item), true
		}
	case []any:
		for i, item := range coll {
			if m, ok := item.(map[string]any); ok && r.keyField != "" && m[r.keyField] == r.Item {
				return m, r.Collection.Index(i), true
			}
		}
	}
	return nil, nil, false
}

// lenient returns the entries of a file that cannot hold them safely, so that they are listed
// with its problem: each valid key once.
func (l *Locator) lenient(name string, src []byte) ([]Element, error) {
	doc, _, err := edit.Decode(name, src)
	if err != nil {
		return nil, err
	}
	var out []Element
	add := func(key string) {
		if SafeSegment(key) && !slices.ContainsFunc(out, func(e Element) bool { return e.Key == key }) {
			out = append(out, Element{Key: key})
		}
	}
	switch coll := document.ValueAt(doc, l.collection).(type) {
	case map[string]any:
		for _, k := range document.SortedKeys(coll) {
			add(k)
		}
	case []any:
		for _, item := range coll {
			if m, ok := item.(map[string]any); ok {
				k, _ := m[l.keyField].(string)
				add(k)
			}
		}
	}
	return out, nil
}

func maps(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// files returns the regular files (or, for `dir`, the directories) matching the pattern.
func (l *Locator) files(fsys fs.FS) ([]Region, error) {
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
		strategy := l.strategy
		if strategy == Entry {
			strategy = File
		}
		out = append(out, Region{Strategy: strategy, Path: p, Key: l.pattern.Key(params), Params: params})
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

// Bytes returns an entry's bytes in its file, or nil when the file or the entry does not exist.
func Bytes(fsys fs.FS, r Region) ([]byte, error) {
	src, el, err := entry(fsys, r)
	if el == nil || err != nil {
		return nil, err
	}
	return src[el.Start:el.End], nil
}

// entry returns an entry's file and its element there, or a nil element when the file or the
// entry does not exist.
func entry(fsys fs.FS, r Region) ([]byte, *Element, error) {
	if r.Strategy != Entry {
		return nil, nil, fmt.Errorf("%s is not an entry", r.Path)
	}
	if info, err := fs.Lstat(fsys, r.Path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("%s is a symbolic link; Quai does not follow links", r.Path)
	}
	src, err := fs.ReadFile(fsys, r.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	l := &Locator{strategy: Entry, collection: r.Collection, keyField: r.keyField}
	els, problem := l.Elements(r.Path, src)
	if problem != "" {
		return nil, nil, fmt.Errorf("%s: %s", r.Path, problem)
	}
	for i := range els {
		if els[i].Key == r.Item {
			return src, &els[i], nil
		}
	}
	return src, nil, nil
}

// EntryRegion returns the region of the entry item of the collection (a dotted path) in the file
// at path; keyField names a list's items, and is empty for a map.
func EntryRegion(path, collection, keyField, item string) Region {
	return Region{Strategy: Entry, Path: path, Key: item, Params: map[string]string{}, Collection: dotted(collection), Item: item, keyField: keyField}
}

// Elements returns the entries of an entry's collection in src, its file, as Locator.Elements
// does.
func (r Region) Elements(src []byte) ([]Element, string) {
	l := &Locator{strategy: Entry, collection: r.Collection, keyField: r.keyField}
	return l.Elements(r.Path, src)
}

// KeyField returns the field naming the items of an entry's list, or "" for a map.
func (r Region) KeyField() string { return r.keyField }

// Ref returns how a change set names the region in its fingerprints: its path, and for an entry
// '#' and its collection and key, such as clusters/prod-1/namespaces.yaml#namespaces[name=orders]
// for an item of a list or zones/prod.json#spec.zones[shop.example.com] for an entry of a map.
func (r Region) Ref() string {
	if r.Strategy != Entry {
		return r.Path
	}
	key := r.Item
	if r.keyField != "" {
		key = r.keyField + "=" + r.Item
	}
	return r.Path + "#" + dottedString(r.Collection) + "[" + key + "]"
}

var refRe = regexp.MustCompile(`^(.+)#([^#\[\]=]+)\[(?:([^#\[\]=]+)=)?([^#\[\]=]+)\]$`)

// RefPath returns the path of the file or directory a reference names.
func RefPath(ref string) string {
	if m := refRe.FindStringSubmatch(ref); m != nil {
		return m[1]
	}
	return ref
}

// FingerprintRef returns the fingerprint of the region a reference names in fsys: an entry, a
// directory or a file.
func FingerprintRef(fsys fs.FS, ref string) (string, error) {
	if m := refRe.FindStringSubmatch(ref); m != nil {
		return Fingerprint(fsys, EntryRegion(m[1], m[2], m[3], m[4]))
	}
	strategy := File
	if info, err := fs.Lstat(fsys, ref); err == nil && info.IsDir() {
		strategy = Dir
	}
	return Fingerprint(fsys, Region{Strategy: strategy, Path: ref})
}

func dottedString(p document.Path) string {
	steps := make([]string, len(p))
	for i, s := range p {
		steps[i] = fmt.Sprint(s)
	}
	return strings.Join(steps, ".")
}

// Fingerprint returns "sha256:" and the hex SHA-256 of a region's bytes, or Absent. For an entry
// it hashes the entry's bytes only, without the padding that aligns it with its neighbours (HCL's
// `key   = value`), so that a change to another entry of the file does not change it. For a directory it hashes, in path order, each regular file's path relative to the
// directory and the SHA-256 of its content, so that renaming, adding or removing a file changes
// it too. Symbolic links are refused.
func Fingerprint(fsys fs.FS, r Region) (string, error) {
	if r.Strategy == Entry {
		src, el, err := entry(fsys, r)
		if err != nil {
			return "", err
		}
		if el == nil {
			return Absent, nil
		}
		sum := sha256.Sum256(el.Content(src))
		return "sha256:" + hex.EncodeToString(sum[:]), nil
	}
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
