package lint

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"testing/fstest"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/edit"
	"github.com/damhau/quai/backend/internal/engine/edit/yamledit"
	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/engine/plan"
)

// TestsDir holds the golden examples of each type: .quai/tests/<type>/<case>/.
const TestsDir = ".quai/tests"

// goldenRequest is a golden example's request.yaml: a request on one resource of the type, and the
// files it deletes.
type goldenRequest struct {
	Kind         string            `json:"kind"`
	Params       map[string]string `json:"params,omitempty"` // create
	Key          string            `json:"key,omitempty"`    // update, delete, action
	Values       map[string]any    `json:"values,omitempty"`
	Action       string            `json:"action,omitempty"`
	ActionInputs map[string]any    `json:"actionInputs,omitempty"`
	Owner        string            `json:"owner,omitempty"`
	Deleted      []string          `json:"deleted,omitempty"`
}

// checkTests runs the golden examples (docs/design/02, testing a type): for each case, the
// request in request.yaml, planned on the repository with the files under before/ laid over it,
// must write exactly the files under after/, byte for byte, and delete those it lists.
func (r *Report) checkTests(fsys fs.FS, repo *definitions.Repository) {
	types, err := fs.ReadDir(fsys, TestsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		r.problem(TestsDir, "", "cannot read: %v", err)
		return
	}
	for _, td := range types {
		dir := path.Join(TestsDir, td.Name())
		t := repo.Types[td.Name()]
		if !td.IsDir() || t == nil {
			r.problem(dir, "", "is not the name of a type")
			continue
		}
		cases, err := fs.ReadDir(fsys, dir)
		if err != nil {
			r.problem(dir, "", "cannot read: %v", err)
			continue
		}
		for _, c := range cases {
			if c.IsDir() {
				r.Tests++
				r.checkCase(fsys, t, path.Join(dir, c.Name()))
			}
		}
	}
}

func (r *Report) checkCase(fsys fs.FS, t *definitions.ResourceType, dir string) {
	file := path.Join(dir, "request.yaml")
	src, err := fs.ReadFile(fsys, file)
	if err != nil {
		r.problem(file, "", "cannot read: %v", err)
		return
	}
	var in goldenRequest
	if err := decodeStrict(src, &in); err != nil {
		r.problem(file, "", "%v", err)
		return
	}
	before, err := tree(fsys, path.Join(dir, "before"))
	if err != nil {
		r.problem(dir, "before", "%v", err)
		return
	}
	after, err := tree(fsys, path.Join(dir, "after"))
	if err != nil {
		r.problem(dir, "after", "%v", err)
		return
	}
	over := overlay{base: fsys, files: before}
	req, err := goldenToRequest(over, t, in)
	if err != nil {
		r.problem(file, "", "%v", err)
		return
	}
	cs, err := plan.Type(over, t, req)
	if err != nil {
		r.problem(file, "", "the request fails: %v", err)
		return
	}
	written := map[string]bool{}
	for _, c := range cs.Changes {
		written[c.Path] = true
		switch want, ok := after[c.Path]; {
		case c.Op == plan.OpDelete && !slices.Contains(in.Deleted, c.Path):
			r.problem(file, "deleted", "the request deletes %s", c.Path)
		case c.Op == plan.OpDelete:
		case !ok:
			r.problem(dir, "after/"+c.Path, "the request writes this file, which after/ does not have")
		case !bytes.Equal(want.Data, c.After):
			r.problem(dir, "after/"+c.Path, "the request writes other bytes (%s)", changedLines(want.Data, c.After))
		}
	}
	for _, p := range slices.Sorted(func(yield func(string) bool) {
		for p := range after {
			if !yield(p) {
				return
			}
		}
	}) {
		if !written[p] {
			r.problem(dir, "after/"+p, "the request does not write this file")
		}
	}
	for _, p := range in.Deleted {
		if !written[p] {
			r.problem(file, "deleted", "the request does not delete %s", p)
		}
	}
}

// goldenToRequest makes the generator's request: a create from its params, anything else on the
// resource its key names, read from the files.
func goldenToRequest(fsys fs.FS, t *definitions.ResourceType, in goldenRequest) (generate.Request, error) {
	req := generate.Request{Kind: in.Kind, Params: in.Params, Values: in.Values, ActionName: in.Action, ActionInputs: in.ActionInputs, Owner: in.Owner}
	switch in.Kind {
	case generate.Create:
		return req, nil
	case generate.Update, generate.Delete, generate.Action:
	default:
		return req, fmt.Errorf("kind: must be create, update, delete or action, not %q", in.Kind)
	}
	loc, err := t.Spec.Locate.Locator()
	if err != nil {
		return req, err
	}
	params, err := loc.ParseKey(in.Key)
	if err != nil {
		return req, fmt.Errorf("key: %v", err)
	}
	region, err := loc.Region(params)
	if err != nil {
		return req, fmt.Errorf("key: %v", err)
	}
	req.Params = region.Params
	src, err := fs.ReadFile(fsys, region.Path)
	if err != nil {
		return req, fmt.Errorf("key: %s: %w", in.Key, err)
	}
	decoded, code, err := edit.Decode(region.Path, src)
	if err != nil {
		return req, fmt.Errorf("%s: %w", region.Path, err)
	}
	if req.Current, req.Code, err = locate.Document(decoded, code, region); err != nil {
		return req, fmt.Errorf("%s: %w", region.Path, err)
	}
	return req, nil
}

// tree reads the regular files under dir, by their path relative to it; none when dir does not
// exist.
func tree(fsys fs.FS, dir string) (fstest.MapFS, error) {
	out := fstest.MapFS{}
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == dir {
			return fs.SkipAll
		}
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		b, err := fs.ReadFile(fsys, p)
		out[strings.TrimPrefix(p, dir+"/")] = &fstest.MapFile{Data: b}
		return err
	})
	return out, err
}

// overlay reads files from files first, then from base.
type overlay struct {
	base  fs.FS
	files fstest.MapFS
}

func (o overlay) Open(name string) (fs.File, error) {
	if _, ok := o.files[name]; ok {
		return o.files.Open(name)
	}
	return o.base.Open(name)
}

// decodeStrict decodes a YAML document into out, refusing unknown fields.
func decodeStrict(src []byte, out any) error {
	doc, err := yamledit.Decode(src)
	if err != nil {
		return err
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}
