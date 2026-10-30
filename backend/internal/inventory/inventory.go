// Package inventory lists the resources of a repository snapshot: every file a type locates,
// with its owner, its stack, and whether it conforms to the type's schema
// (docs/architecture/domain-model.md#resource-status). Files that do not conform or do not
// parse are listed too, never hidden; Quai only refuses to edit them.
package inventory

import (
	"errors"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/edit"
	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/engine/schema"
	"github.com/damhau/quai/backend/internal/repositories"
)

// Statuses a file can have from its content alone.
const (
	Conforming    = "conforming"
	NonConforming = "non-conforming"
	Unparseable   = "unparseable"
)

// Resource is one resource found in a repository.
type Resource struct {
	ID         string
	Repository string
	Type       string
	Key        string
	Path       string
	Params     map[string]string
	Owner      string // a user, team:<name>, or empty when nobody owns it
	Stack      string
	Status     string
	// Problems are schema violations by field, for a non-conforming file; Error says why an
	// unparseable file cannot be read.
	Problems map[string]string
	Error    string
	Document map[string]any
	// Code are the values of the file defined in code (an HCL expression): left out of Document,
	// shown read-only, never written.
	Code []edit.Code
	// Region is the part of the repository the resource owns.
	Region locate.Region
}

// Scan returns every resource of every type in the snapshot, sorted by ID.
func Scan(snap *repositories.Snapshot) ([]Resource, error) {
	var out []Resource
	for _, name := range slices.Sorted(maps.Keys(snap.Definitions.Types)) {
		rs, err := ScanType(snap, snap.Definitions.Types[name])
		if err != nil {
			return nil, err
		}
		out = append(out, rs...)
	}
	return out, nil
}

// ScanType returns the resources of one type. Types whose locator is not implemented yet have
// none.
func ScanType(snap *repositories.Snapshot, t *definitions.ResourceType) ([]Resource, error) {
	loc, err := t.Spec.Locate.Locator()
	if err != nil {
		return nil, nil //nolint:nilerr // entry and field locators come later
	}
	regions, err := loc.Scan(snap.FS)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(regions))
	for _, r := range regions {
		out = append(out, read(snap, t, r))
	}
	return out, nil
}

// Get returns one resource by type and key.
func Get(snap *repositories.Snapshot, typeName, key string) (Resource, error) {
	t := snap.Definitions.Types[typeName]
	if t == nil {
		return Resource{}, fs.ErrNotExist
	}
	loc, err := t.Spec.Locate.Locator()
	if err != nil {
		return Resource{}, err
	}
	params, err := loc.ParseKey(key)
	if err != nil {
		return Resource{}, fs.ErrNotExist
	}
	region, err := loc.Region(params)
	if err != nil {
		return Resource{}, fs.ErrNotExist
	}
	if _, err := fs.Stat(snap.FS, region.Path); errors.Is(err, fs.ErrNotExist) {
		return Resource{}, fs.ErrNotExist
	}
	if region.Collection != nil {
		// The entry is there unless the file is read and does not hold it; a problem with the
		// file is listed with the entries the inventory finds.
		src, err := fs.ReadFile(snap.FS, region.Path)
		if err != nil {
			return Resource{}, err
		}
		doc, _, err := edit.Decode(region.Path, src)
		if err != nil {
			return Resource{}, fs.ErrNotExist
		}
		if _, _, ok := locate.Value(doc, region); !ok {
			return Resource{}, fs.ErrNotExist
		}
		if _, problem := loc.Elements(region.Path, src); problem != "" {
			region.Problem = problem
		}
	}
	return read(snap, t, region), nil
}

func read(snap *repositories.Snapshot, t *definitions.ResourceType, r locate.Region) Resource {
	res := Resource{
		ID: locate.ID(snap.Repository, t.Metadata.Name, r.Key), Repository: snap.Repository, Type: t.Metadata.Name,
		Key: r.Key, Path: r.Path, Params: r.Params, Status: Conforming, Region: r,
	}
	if t.Spec.Stack != "" {
		if p, err := locate.Compile(t.Spec.Stack); err == nil {
			res.Stack, _ = p.Format(r.Params)
		} else {
			res.Stack = t.Spec.Stack
		}
	}
	if o := t.Spec.Ownership; o != nil && o.Team != "" {
		res.Owner = "team:" + o.Team
	}
	src, err := fs.ReadFile(snap.FS, r.Path)
	if err != nil {
		res.Status, res.Error = Unparseable, err.Error()
		return res
	}
	decoded, code, err := edit.Decode(r.Path, src)
	if err != nil {
		res.Status, res.Error = Unparseable, err.Error()
		return res
	}
	// An entry's document is its value, a field region's its listed fields.
	doc, code, err := locate.Document(decoded, code, r)
	if err != nil {
		res.Status, res.Error = Unparseable, err.Error()
		return res
	}
	res.Code = code
	res.Document = doc
	if o := t.Spec.Ownership; o != nil && o.Field != "" {
		if v, ok := lookup(doc, o.Field).(string); ok {
			res.Owner = v
		}
	}
	if err := t.FileSchema.Validate(doc); err != nil {
		var ve *schema.ValidationError
		switch {
		case !errors.As(err, &ve):
			res.Status, res.Error = NonConforming, err.Error()
		case len(edit.OutsideCode(ve.Fields, code)) > 0:
			res.Status, res.Problems = NonConforming, edit.OutsideCode(ve.Fields, code)
		}
	}
	if r.Problem != "" {
		// The file cannot hold its entries safely (a key twice): none can be edited until fixed.
		res.Status, res.Error = NonConforming, r.Problem
	}
	return res
}

func lookup(doc map[string]any, path string) any {
	var cur any = doc
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[seg]
	}
	return cur
}

// Discover returns the values a param has in the repository: the directories at its place in
// the type's path, such as dev, prod and test for {env} in envs/{env}/vms/{name}.yaml.
func Discover(snap *repositories.Snapshot, t *definitions.ResourceType, param string) []string {
	segs := strings.Split(t.Spec.Locate.Path, "/")
	out := []string{}
	for i, seg := range segs {
		if seg != "{"+param+"}" {
			continue
		}
		dir := path.Join(segs[:i]...)
		if dir == "" {
			dir = "."
		}
		entries, err := fs.ReadDir(snap.FS, dir)
		if err != nil {
			return out
		}
		for _, e := range entries {
			if e.IsDir() && locate.SafeSegment(e.Name()) {
				out = append(out, e.Name())
			}
		}
		return out
	}
	return out
}
