// Package definitions reads the quai.dev/v1beta1 definitions of a repository: the resource types
// under .quai/types/ and the blueprints under blueprints/. Each definition is decoded strictly
// (unknown fields are errors), validated, and compiled: its schemas with internal/engine/schema
// and its expressions with internal/engine/expr. Anything else, including the prototype's
// quai.dev/v1alpha1, is refused.
//
// A definition with problems is left out and its problems are reported, so that one broken file
// does not take the others down (docs/design/01-repository-connections.md#definitions).
package definitions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/edit/yamledit"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/engine/schema"
)

// APIVersion is the only definition format Quai reads.
const APIVersion = "quai.dev/v1beta1"

// Definition kinds.
const (
	KindResourceType = "ResourceType"
	KindBlueprint    = "Blueprint"
)

// Where definitions live in a repository.
const (
	TypesDir      = ".quai/types"
	BlueprintsDir = "blueprints"
)

// Repository holds the valid definitions of one repository at one commit.
type Repository struct {
	Types      map[string]*ResourceType // by metadata.name
	Blueprints map[string]*Blueprint
}

// Problem is something wrong with a definition.
type Problem struct {
	Path    string // the definition's file
	Field   string // a dotted path in the definition, or empty for the whole file
	Message string
}

func (p Problem) String() string {
	if p.Field == "" {
		return p.Path + ": " + p.Message
	}
	return p.Path + ": " + p.Field + ": " + p.Message
}

// Problems lists the problems found while loading definitions. It is an error.
type Problems []Problem

func (ps Problems) Error() string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.String()
	}
	return strings.Join(lines, "\n")
}

// Load reads every definition in fsys, the files of a repository at one commit. It returns the
// valid definitions and the problems of the others, sorted by file. Schemas referenced by types
// are read from fsys too.
func Load(fsys fs.FS) (*Repository, Problems) {
	l := newLoader(fsys)
	repo := &Repository{Types: map[string]*ResourceType{}, Blueprints: map[string]*Blueprint{}}
	for _, p := range l.files(TypesDir) {
		t := l.resourceType(p)
		if t == nil {
			continue
		}
		if other, ok := repo.Types[t.Metadata.Name]; ok {
			l.add(p, "metadata.name", "type %s is already defined in %s", t.Metadata.Name, other.Path)
			continue
		}
		repo.Types[t.Metadata.Name] = t
	}
	for _, p := range l.files(BlueprintsDir) {
		b := l.blueprint(p, repo.Types)
		if b == nil {
			continue
		}
		if other, ok := repo.Blueprints[b.Metadata.Name]; ok {
			l.add(p, "metadata.name", "blueprint %s is already defined in %s", b.Metadata.Name, other.Path)
			continue
		}
		repo.Blueprints[b.Metadata.Name] = b
	}
	slices.SortStableFunc(l.problems, func(a, b Problem) int { return strings.Compare(a.Path, b.Path) })
	return repo, l.problems
}

type loader struct {
	fsys     fs.FS
	env      *expr.Env
	schemas  *schema.Compiler
	problems Problems
}

func newLoader(fsys fs.FS) *loader {
	env, err := expr.NewEnv()
	if err != nil {
		panic(err) // the default environment is fixed; failing to build it is a bug
	}
	l := &loader{fsys: fsys, env: env, schemas: schema.NewCompiler()}
	l.schemas.UseSource(func(p string) (any, error) {
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		return yamledit.Decode(src)
	})
	return l
}

func (l *loader) add(file, field, format string, args ...any) {
	l.problems = append(l.problems, Problem{Path: file, Field: field, Message: fmt.Sprintf(format, args...)})
}

// files returns the YAML files directly in dir, sorted. A missing directory has none.
func (l *loader) files(dir string) []string {
	entries, err := fs.ReadDir(l.fsys, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		l.add(dir, "", "cannot list definitions: %v", err)
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && (strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
			out = append(out, path.Join(dir, e.Name()))
		}
	}
	return out
}

// read decodes the definition in file into out, after checking its apiVersion and kind.
func (l *loader) read(file, kind string, out any) bool {
	src, err := fs.ReadFile(l.fsys, file)
	if err != nil {
		l.add(file, "", "cannot read: %v", err)
		return false
	}
	doc, err := yamledit.Decode(src)
	if err != nil {
		l.add(file, "", "%v", err)
		return false
	}
	m, ok := doc.(map[string]any)
	if !ok {
		l.add(file, "", "a definition must be a mapping")
		return false
	}
	switch v := m["apiVersion"]; v {
	case APIVersion:
	case "quai.dev/v1alpha1":
		l.add(file, "apiVersion", "quai.dev/v1alpha1 is the prototype's format and is not supported; write the definition in %s", APIVersion)
		return false
	default:
		l.add(file, "apiVersion", "must be %s, not %v", APIVersion, display(v))
		return false
	}
	if m["kind"] != kind {
		l.add(file, "kind", "must be %s in %s/, not %v", kind, path.Dir(file), display(m["kind"]))
		return false
	}
	if err := decodeStrict(m, out); err != nil {
		l.add(file, "", "%v", err)
		return false
	}
	return true
}

// decodeStrict decodes a document in the JSON data model into a definition struct, refusing
// unknown fields. Numbers inside untyped values come back as int64 or float64.
func decodeStrict(doc any, out any) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return fmt.Errorf("%s: must be %s, not %s", te.Field, jsonKind(te.Type), te.Value)
		}
		return errors.New(strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}

func jsonKind(t reflect.Type) string {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		return "a mapping"
	case reflect.Slice:
		return "a list"
	case reflect.Int, reflect.Float64:
		return "a number"
	case reflect.Bool:
		return "true or false"
	}
	return "a " + t.Kind().String()
}

// numbers replaces the json.Numbers that decodeStrict leaves in untyped values with int64 or
// float64, as yamledit.Decode returns them.
func numbers(v any) any {
	switch v := v.(type) {
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i
		}
		f, _ := v.Float64()
		return f
	case map[string]any:
		for k, x := range v {
			v[k] = numbers(x)
		}
	case []any:
		for i, x := range v {
			v[i] = numbers(x)
		}
	}
	return v
}

func numbersIn(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return numbers(m).(map[string]any)
}

func display(v any) string {
	if v == nil {
		return "nothing"
	}
	return fmt.Sprintf("%q", fmt.Sprint(v))
}
