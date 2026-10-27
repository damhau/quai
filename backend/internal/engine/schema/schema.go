// Package schema compiles the JSON Schemas of a repository and validates documents against them,
// with errors keyed by field path so that forms can show each message next to its field. It also
// computes a schema's defaults and the field helpers forms need: which fields are locked after
// creation and which are hidden by a `visibleIf` condition.
//
// Schemas use draft 2020-12 unless they declare another with `$schema`; drafts 2019-09 and 7 are
// supported too. `format` is an assertion, not an annotation, and patterns are RE2 regular
// expressions. A `$ref` resolves only to schemas added to the Compiler: Quai never fetches a
// schema from the network or the file system (docs/engineering/backend.md, engine rules).
package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/edit/yamledit"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// base is the URL under which repository paths are addressed, so that relative `$ref`s between
// schema files resolve as they do on disk.
const base = "file:///repo/"

// Compiler holds the schema documents of a repository and compiles them. It is not safe for
// concurrent use; the schemas it returns are.
type Compiler struct {
	docs   map[string]any                 // by URL: the repository URL of each document, and its absolute $id
	keys   map[string]map[string][]string // by URL: the key order of documents read from source
	paths  map[string]string
	source func(path string) ([]byte, error)
	c      *jsonschema.Compiler
}

// NewCompiler returns an empty compiler.
func NewCompiler() *Compiler {
	c := &Compiler{docs: map[string]any{}, keys: map[string]map[string][]string{}, paths: map[string]string{}, c: jsonschema.NewCompiler()}
	c.c.DefaultDraft(jsonschema.Draft2020)
	c.c.AssertFormat()
	c.c.UseLoader(loader{c})
	return c
}

// Add adds the schema document at path, a clean path relative to the repository root such as
// "catalog/schemas/vm.schema.json". doc is the decoded document in the JSON data model. A
// document with an absolute `$id` can also be referenced by that URL.
func (c *Compiler) Add(path string, doc any) error {
	u, err := repoURL(path)
	if err != nil {
		return err
	}
	if _, ok := c.docs[u]; ok {
		return fmt.Errorf("schema %s added twice", path)
	}
	c.docs[u] = doc
	c.paths[u] = path
	if id := absoluteID(doc); id != "" && id != u {
		if _, ok := c.docs[id]; ok {
			return fmt.Errorf("schema %s: $id %s is already used by %s", path, id, c.paths[id])
		}
		c.docs[id] = doc
		c.paths[id] = path
	}
	return nil
}

// UseSource sets where schemas that were not added are read from, by repository path: typically
// a file of the repository snapshot being loaded. It returns the file's content, JSON or YAML, or
// an error wrapping fs.ErrNotExist when there is no such file. The order of properties in these
// files is kept for PropertyOrder.
func (c *Compiler) UseSource(fn func(path string) ([]byte, error)) {
	c.source = fn
}

// Compile compiles the schema at path: one added with Add, or else one read from the source.
func (c *Compiler) Compile(path string) (*Schema, error) {
	u, err := repoURL(path)
	if err != nil {
		return nil, err
	}
	doc, ok := c.docs[u]
	if !ok {
		if doc, err = c.fromSource(u); err != nil {
			return nil, err
		}
	}
	sch, err := c.c.Compile(u)
	if err != nil {
		return nil, fmt.Errorf("schema %s: %w", path, err)
	}
	return &Schema{path: path, url: u, doc: doc, docs: c.docs, keys: c.keys, sch: sch}, nil
}

func repoURL(path string) (string, error) {
	if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") || strings.Contains(path, "#") {
		return "", fmt.Errorf("schema path %q must be a relative path in the repository", path)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("schema path %q must be a clean relative path", path)
		}
	}
	return (&url.URL{Scheme: "file", Path: "/repo/" + path}).String(), nil
}

// absoluteID returns the document's top-level `$id` when it is an absolute URL without fragment.
func absoluteID(doc any) string {
	m, ok := doc.(map[string]any)
	if !ok {
		return ""
	}
	id, ok := m["$id"].(string)
	if !ok {
		return ""
	}
	u, err := url.Parse(id)
	if err != nil || !u.IsAbs() {
		return ""
	}
	u.Fragment = ""
	return u.String()
}

// loader serves the documents added to the compiler and nothing else.
type loader struct{ c *Compiler }

func (l loader) Load(u string) (any, error) {
	if doc, ok := l.c.docs[u]; ok {
		return doc, nil
	}
	return l.c.fromSource(u)
}

// fromSource reads the schema at a repository URL from the source and adds it.
func (c *Compiler) fromSource(u string) (any, error) {
	p, inRepo := strings.CutPrefix(u, base)
	if !inRepo {
		return nil, fmt.Errorf("schema %s is not in the repository; Quai does not fetch schemas", u)
	}
	if path, err := url.PathUnescape(p); err == nil {
		p = path
	}
	if c.source == nil {
		return nil, fmt.Errorf("schema %s is not in the repository", p)
	}
	src, err := c.source(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("schema %s is not in the repository", p)
	}
	if err != nil {
		return nil, fmt.Errorf("schema %s: %w", p, err)
	}
	doc, err := yamledit.Decode(src)
	if err != nil {
		return nil, fmt.Errorf("schema %s: %w", p, err)
	}
	keys, err := yamledit.Keys(src)
	if err != nil {
		return nil, fmt.Errorf("schema %s: %w", p, err)
	}
	if err := c.Add(p, doc); err != nil {
		return nil, err
	}
	c.keys[u] = keys
	if id := absoluteID(doc); id != "" {
		c.keys[id] = keys
	}
	return doc, nil
}

// Schema is a compiled schema. It is safe for concurrent use.
type Schema struct {
	path string
	url  string
	doc  any
	docs map[string]any
	keys map[string]map[string][]string
	sch  *jsonschema.Schema
}

// Path returns the repository path the schema was compiled from.
func (s *Schema) Path() string { return s.path }

// ValidationError lists what is wrong with a document, by field path: dotted, with list indices
// as numbers ("disks.0.size_gb"), or "$" for the document itself.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + e.Fields[k]
	}
	return "invalid document: " + strings.Join(parts, "; ")
}

// Validate checks doc, a value in the JSON data model, against the schema. It returns nil or a
// *ValidationError with one message per field, the first found when a field has several.
func (s *Schema) Validate(doc any) error {
	err := s.sch.Validate(doc)
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return fmt.Errorf("schema %s: %w", s.path, err)
	}
	fields := map[string]string{}
	collect(ve, fields)
	if len(fields) == 0 {
		fields[rootPath] = "is invalid"
	}
	return &ValidationError{Fields: fields}
}

const rootPath = "$"

var printer = message.NewPrinter(language.English)

// collect adds the messages of e and its causes. Errors that only group others are skipped; an
// anyOf or oneOf is reported as one message, not as the failures of each alternative.
func collect(e *jsonschema.ValidationError, fields map[string]string) {
	switch e.ErrorKind.(type) {
	case *kind.AnyOf, *kind.OneOf, *kind.Contains, *kind.PropertyNames, *kind.Not:
	default:
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				collect(c, fields)
			}
			return
		}
	}
	add := func(path []string, msg string) {
		p := rootPath
		if len(path) > 0 {
			p = strings.Join(path, ".")
		}
		if _, ok := fields[p]; !ok {
			fields[p] = msg
		}
	}
	loc := e.InstanceLocation
	switch k := e.ErrorKind.(type) {
	case *kind.Required:
		for _, m := range k.Missing {
			add(append(loc[:len(loc):len(loc)], m), "is required")
		}
	case *kind.AdditionalProperties:
		for _, p := range k.Properties {
			add(append(loc[:len(loc):len(loc)], p), "is not allowed by the schema")
		}
	default:
		add(loc, describe(e.ErrorKind))
	}
}

// describe says what a failed keyword wants, for the person filling a form.
func describe(k jsonschema.ErrorKind) string {
	switch k := k.(type) {
	case *kind.Enum:
		vals := make([]string, len(k.Want))
		for i, v := range k.Want {
			vals[i] = display(v)
		}
		return "must be one of " + strings.Join(vals, ", ")
	case *kind.Const:
		return "must be " + display(k.Want)
	case *kind.Type:
		want := make([]string, len(k.Want))
		for i, t := range k.Want {
			want[i] = typePhrase(t)
		}
		return "must be " + strings.Join(want, " or ")
	case *kind.Minimum:
		return "must be at least " + rat(k.Want)
	case *kind.Maximum:
		return "must be at most " + rat(k.Want)
	case *kind.ExclusiveMinimum:
		return "must be greater than " + rat(k.Want)
	case *kind.ExclusiveMaximum:
		return "must be less than " + rat(k.Want)
	case *kind.MultipleOf:
		return "must be a multiple of " + rat(k.Want)
	case *kind.MinLength:
		return fmt.Sprintf("must be at least %d characters long", k.Want)
	case *kind.MaxLength:
		return fmt.Sprintf("must be at most %d characters long", k.Want)
	case *kind.MinItems:
		return fmt.Sprintf("must have at least %d items", k.Want)
	case *kind.MaxItems:
		return fmt.Sprintf("must have at most %d items", k.Want)
	case *kind.MinProperties:
		return fmt.Sprintf("must have at least %d entries", k.Want)
	case *kind.MaxProperties:
		return fmt.Sprintf("must have at most %d entries", k.Want)
	case *kind.Pattern:
		return "must match the pattern " + k.Want
	case *kind.Format:
		return "must be a valid " + k.Want
	case *kind.UniqueItems:
		return "must not contain duplicates"
	case *kind.FalseSchema:
		return "is not allowed"
	case *kind.AnyOf:
		return "must match at least one of the allowed forms"
	case *kind.OneOf:
		return "must match exactly one of the allowed forms"
	}
	return k.LocalizedString(printer)
}

func typePhrase(t string) string {
	switch t {
	case "integer", "object", "array":
		return "an " + t
	case "null":
		return "null"
	}
	return "a " + t
}

func rat(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	f, _ := r.Float64()
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// display shows a value from a schema as a person would write it: strings without quotes,
// lists and objects as JSON.
func display(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case nil:
		return "null"
	case []any, map[string]any:
		b, err := json.Marshal(v)
		if err == nil {
			return string(b)
		}
	}
	return fmt.Sprint(v)
}
