// Package plan turns a request into a change set: the file changes it makes, the fingerprints
// of the regions it read, and a hash of its content (docs/design/04-blueprints-and-change-sets.md
// #change-sets). Files are written through the editors of package edit (YAML, JSON, HCL), so an
// update changes only the bytes that must change, and new files list their keys in the schema's
// order.
//
// Planning reads the repository through an fs.FS at the base commit and writes nothing.
package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/edit"
	"github.com/damhau/quai/backend/internal/engine/edit/yamledit"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/engine/schema"
)

// File operations.
const (
	OpCreate = "create"
	OpUpdate = "update"
	OpDelete = "delete"
)

var (
	// ErrExists is returned when a resource to create already exists.
	ErrExists = errors.New("already exists")
	// ErrNotFound is returned when a resource to change does not exist.
	ErrNotFound = errors.New("does not exist")
	// ErrNoChange is returned when a request would not change any file.
	ErrNoChange = errors.New("this change would not modify anything")
)

// ResourceError is a failure to plan one resource of a blueprint: its condition, its params or
// values, or the document its type makes of them.
type ResourceError struct {
	ID  string // the resource's id in the blueprint
	Err error
}

func (e *ResourceError) Error() string { return "resource " + e.ID + ": " + e.Err.Error() }

func (e *ResourceError) Unwrap() error { return e.Err }

// Change is one file the change set creates, updates or deletes.
type Change struct {
	Path   string
	Op     string
	Before []byte // nil for a create
	After  []byte // nil for a delete
	// Entries are the changes to entries of a shared file that make After from Before, for the
	// entry locator; nil when the change replaces the whole file.
	Entries []Entry
}

// ChangeSet is what a request writes to one repository.
type ChangeSet struct {
	Changes []Change // sorted by path
	// Fingerprints are the regions' fingerprints at the base commit, by region path. Delivery
	// recomputes them; if one differs, the request is stale.
	Fingerprints map[string]string
	// Hash identifies the content: approvals are bound to it.
	Hash string
	// Resources are the resources a blueprint plan creates, for its approval rules.
	Resources []Planned
}

// Planned is a resource a blueprint plan creates.
type Planned struct {
	ID     string // the resource's id in the blueprint
	Type   string
	Key    string
	Params map[string]string
	Values map[string]any
}

// Type plans a request on one resource of a type in the repository fsys.
func Type(fsys fs.FS, t *definitions.ResourceType, r generate.Request) (*ChangeSet, error) {
	cs := &ChangeSet{Fingerprints: map[string]string{}}
	if _, err := addType(cs, fsys, t, r); err != nil {
		return nil, err
	}
	cs.seal()
	return cs, nil
}

func addType(cs *ChangeSet, fsys fs.FS, t *definitions.ResourceType, r generate.Request) (locate.Region, error) {
	if t.Spec.Locate.Strategy != locate.File && t.Spec.Locate.Strategy != locate.Entry {
		return locate.Region{}, fmt.Errorf("type %s: the %s locator is not available yet", t.Metadata.Name, t.Spec.Locate.Strategy)
	}
	loc, err := t.Spec.Locate.Locator()
	if err != nil {
		return locate.Region{}, err
	}
	region, err := loc.Region(r.Params)
	if err != nil {
		return locate.Region{}, err
	}
	if _, ok := edit.FormatOf(region.Path); !ok {
		return locate.Region{}, fmt.Errorf("%s: Quai edits YAML, JSON and HCL files (.yaml, .yml, .json, .tfvars, .hcl)", region.Path)
	}
	if region.Strategy == locate.Entry {
		return region, addEntry(cs, fsys, t, region, r)
	}
	fp, err := locate.Fingerprint(fsys, region)
	if err != nil {
		return locate.Region{}, err
	}
	if _, dup := cs.Fingerprints[region.Path]; dup {
		return locate.Region{}, fmt.Errorf("%s is written twice by the same request", region.Path)
	}
	cs.Fingerprints[region.Path] = fp
	id := t.Metadata.Name + "/" + region.Key

	if r.Kind == generate.Create {
		if fp != locate.Absent {
			return locate.Region{}, fmt.Errorf("%s: %s %w", id, region.Path, ErrExists)
		}
		doc, err := generate.Identity(t, r)
		if err != nil {
			return locate.Region{}, err
		}
		out, err := edit.Create(region.Path, doc, t.FileSchema.PropertyOrder())
		if err != nil {
			return locate.Region{}, err
		}
		cs.Changes = append(cs.Changes, Change{Path: region.Path, Op: OpCreate, After: out})
		return region, nil
	}

	if fp == locate.Absent {
		return locate.Region{}, fmt.Errorf("%s: %s %w", id, region.Path, ErrNotFound)
	}
	before, err := fs.ReadFile(fsys, region.Path)
	if err != nil {
		return locate.Region{}, err
	}
	if r.Kind == generate.Delete {
		cs.Changes = append(cs.Changes, Change{Path: region.Path, Op: OpDelete, Before: before})
		return region, nil
	}
	decoded, code, err := edit.Decode(region.Path, before)
	if err != nil {
		return locate.Region{}, fmt.Errorf("%s cannot be read, so Quai will not edit it: %w", region.Path, err)
	}
	current, ok := decoded.(map[string]any)
	if !ok {
		return locate.Region{}, fmt.Errorf("%s does not hold a mapping, so Quai will not edit it", region.Path)
	}
	r.Current, r.Code = current, code
	doc, err := generate.Identity(t, r)
	if err != nil {
		return locate.Region{}, err
	}
	after, err := edit.Edit(region.Path, before, doc)
	if err != nil {
		return locate.Region{}, err
	}
	if bytes.Equal(before, after) {
		return locate.Region{}, ErrNoChange
	}
	cs.Changes = append(cs.Changes, Change{Path: region.Path, Op: OpUpdate, Before: before, After: after})
	return region, nil
}

// BlueprintRequest creates an instance of a blueprint.
type BlueprintRequest struct {
	Name       string         // the instance's name
	Inputs     map[string]any // the form values; defaults are applied
	Owner      string
	Repository string // the repository's name, for the resource IDs in the instance record
}

// InstancesDir is where instance records live when every resource of a blueprint is in the
// blueprint's own repository.
const InstancesDir = ".quai/instances"

// Blueprint plans the creation of a blueprint instance: each resource whose `when` holds, created
// through its type, and the instance record, in one change set.
func Blueprint(fsys fs.FS, repo *definitions.Repository, b *definitions.Blueprint, r BlueprintRequest) (*ChangeSet, error) {
	if !locate.SafeSegment(r.Name) {
		return nil, fmt.Errorf("instance name %q is not valid", r.Name)
	}
	inputs := map[string]any{}
	if d, ok := b.InputSchema.Defaults().(map[string]any); ok {
		inputs = d
	}
	maps.Copy(inputs, r.Inputs)
	if err := b.InputSchema.Validate(inputs); err != nil {
		return nil, schema.Under("inputs", err)
	}
	vars := expr.Vars{Inputs: inputs, Request: expr.Request{Kind: generate.Create}, User: expr.User{Name: r.Owner}}

	cs := &ChangeSet{Fingerprints: map[string]string{}}
	created := map[string]string{}
	for _, res := range b.Spec.Resources {
		if res.Condition != nil {
			ok, err := res.Condition.Test(vars)
			if err != nil {
				return nil, &ResourceError{ID: res.ID, Err: err}
			}
			if !ok {
				continue
			}
		}
		if res.Type.Repository != "" {
			return nil, &ResourceError{ID: res.ID, Err: errors.New("types in other repositories come with multi-repository change sets")}
		}
		t := repo.Types[res.Type.Name]
		if t == nil {
			return nil, &ResourceError{ID: res.ID, Err: fmt.Errorf("type %s is not defined", res.Type.Name)}
		}
		params, err := evalMap(res.ParamsValue, vars)
		if err != nil {
			return nil, &ResourceError{ID: res.ID, Err: fmt.Errorf("params: %w", err)}
		}
		values, err := evalMap(res.ValuesValue, vars)
		if err != nil {
			return nil, &ResourceError{ID: res.ID, Err: fmt.Errorf("values: %w", err)}
		}
		strParams, err := paramStrings(params)
		if err != nil {
			return nil, &ResourceError{ID: res.ID, Err: fmt.Errorf("params: %w", err)}
		}
		region, err := addType(cs, fsys, t, generate.Request{Kind: generate.Create, Params: strParams, Values: values, Owner: r.Owner})
		if err != nil {
			return nil, &ResourceError{ID: res.ID, Err: err}
		}
		created[res.ID] = locate.ID(r.Repository, t.Metadata.Name, region.Key)
		cs.Resources = append(cs.Resources, Planned{ID: res.ID, Type: t.Metadata.Name, Key: region.Key, Params: strParams, Values: values})
	}

	record := path.Join(InstancesDir, b.Metadata.Name, r.Name+".yaml")
	fp, err := locate.Fingerprint(fsys, locate.Region{Strategy: locate.File, Path: record})
	if err != nil {
		return nil, err
	}
	if fp != locate.Absent {
		return nil, fmt.Errorf("instance %s/%s: %s %w", b.Metadata.Name, r.Name, record, ErrExists)
	}
	cs.Fingerprints[record] = fp
	resources := map[string]any{}
	for id, rid := range created {
		resources[id] = rid
	}
	doc := map[string]any{
		"apiVersion": definitions.APIVersion,
		"kind":       "Instance",
		"metadata":   map[string]any{"name": r.Name, "blueprint": b.Metadata.Name, "blueprintVersion": b.Metadata.Version, "owner": r.Owner},
		"spec":       map[string]any{"inputs": inputs, "resources": resources},
	}
	out, err := yamledit.Create(doc, []string{"apiVersion", "kind", "metadata", "spec", "name", "blueprint", "blueprintVersion", "owner", "inputs", "resources"})
	if err != nil {
		return nil, err
	}
	cs.Changes = append(cs.Changes, Change{Path: record, Op: OpCreate, After: out})
	cs.seal()
	return cs, nil
}

func evalMap(v *expr.Value, vars expr.Vars) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	out, err := v.Eval(vars)
	if err != nil {
		return nil, err
	}
	m, ok := out.(map[string]any)
	if !ok {
		return nil, errors.New("must evaluate to a mapping")
	}
	return m, nil
}

// paramStrings converts param values to text: strings and whole numbers.
func paramStrings(m map[string]any) (map[string]string, error) {
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch v := v.(type) {
		case string:
			out[k] = v
		case int64:
			out[k] = strconv.FormatInt(v, 10)
		default:
			return nil, fmt.Errorf("%s must be text, not %v", k, v)
		}
	}
	return out, nil
}

// seal sorts the changes and computes the hash.
func (cs *ChangeSet) seal() {
	slices.SortFunc(cs.Changes, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })
	cs.Hash = Hash(cs.Changes)
}

// Hash is the content hash of changes, sorted by path: SHA-256 over each change's operation, path
// and the SHA-256 of its new content. Approvals are bound to it.
func Hash(changes []Change) string {
	h := sha256.New()
	for _, c := range changes {
		sum := sha256.Sum256(c.After)
		fmt.Fprintf(h, "%s\x00%s\x00%x\n", c.Op, c.Path, sum)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
