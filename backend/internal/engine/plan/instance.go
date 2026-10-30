package plan

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/edit"
	"github.com/damhau/quai/backend/internal/engine/edit/yamledit"
	"github.com/damhau/quai/backend/internal/engine/expr"
	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/locate"
)

// Instance is a blueprint instance as its record describes it (docs/design/04, Instances).
type Instance struct {
	Path             string
	Name             string
	Blueprint        string
	BlueprintVersion string
	Owner            string
	Inputs           map[string]any
	// Resources maps the blueprint's resource ids to the resources created: <repository>/<type>/<key>.
	Resources map[string]string
}

// InstancePath is where the record of an instance lives in the repository that holds it.
func InstancePath(blueprint, name string) string {
	return path.Join(InstancesDir, blueprint, name+".yaml")
}

// ReadInstance reads an instance record. It returns an error wrapping fs.ErrNotExist when there
// is no record at file, and another when the file is not an instance record.
func ReadInstance(fsys fs.FS, file string) (*Instance, error) {
	src, err := fs.ReadFile(fsys, file)
	if err != nil {
		return nil, err
	}
	decoded, err := yamledit.Decode(src)
	doc, _ := decoded.(map[string]any)
	if err != nil || doc == nil || doc["kind"] != "Instance" {
		return nil, fmt.Errorf("%s is not an instance record", file)
	}
	meta, _ := doc["metadata"].(map[string]any)
	spec, _ := doc["spec"].(map[string]any)
	inst := &Instance{Path: file, Inputs: map[string]any{}, Resources: map[string]string{}}
	inst.Name, _ = meta["name"].(string)
	inst.Blueprint, _ = meta["blueprint"].(string)
	inst.BlueprintVersion, _ = meta["blueprintVersion"].(string)
	inst.Owner, _ = meta["owner"].(string)
	if inputs, ok := spec["inputs"].(map[string]any); ok {
		inst.Inputs = inputs
	}
	resources, _ := spec["resources"].(map[string]any)
	for id, rid := range resources {
		if s, ok := rid.(string); ok {
			inst.Resources[id] = s
		}
	}
	if inst.Name == "" || file != InstancePath(inst.Blueprint, inst.Name) {
		return nil, fmt.Errorf("%s is not an instance record", file)
	}
	return inst, nil
}

// BlueprintActionRequest is a day-2 action on an instance of a blueprint.
type BlueprintActionRequest struct {
	Repository string // the repository planned, which holds the instance and its resources
	Instance   *Instance
	Action     string
	Inputs     map[string]any // the action's inputs, as the form sent them
	User       expr.User
}

// BlueprintAction plans a day-2 action on a blueprint instance. Each resource the action applies
// to either gets the action's values merged into its current document, or runs one of its type's
// actions; a resource the action leaves as it is drops out of the change set. It returns the
// change set, whose Resources hold each changed resource's new document, and the action's inputs
// as checked against its form.
func BlueprintAction(fsys fs.FS, repo *definitions.Repository, b *definitions.Blueprint, r BlueprintActionRequest) (*ChangeSet, map[string]any, error) {
	i := slices.IndexFunc(b.Spec.Actions, func(a definitions.BlueprintAction) bool { return a.Name == r.Action })
	if i < 0 {
		return nil, nil, fmt.Errorf("blueprint %s has no action %s: %w", b.Metadata.Name, r.Action, ErrNotFound)
	}
	a := b.Spec.Actions[i]
	vars := expr.Vars{Inputs: r.Instance.Inputs, User: r.User, Request: expr.Request{Kind: generate.Action, Action: a.Name}}
	inputs, err := generate.ValidateInputs(a.Inputs, r.Inputs, vars)
	if err != nil {
		return nil, nil, err
	}
	vars.Action = inputs
	cs := &ChangeSet{Fingerprints: map[string]string{}}
	for _, id := range slices.Sorted(maps.Keys(a.Apply)) {
		if err := applyTo(cs, fsys, repo, r, id, a.Apply[id], vars); err != nil {
			return nil, nil, &ResourceError{ID: id, Err: err}
		}
	}
	if len(cs.Changes) == 0 {
		return nil, nil, ErrNoChange
	}
	cs.seal()
	return cs, inputs, nil
}

// applyTo plans what a blueprint action does to one resource of the instance.
func applyTo(cs *ChangeSet, fsys fs.FS, repo *definitions.Repository, r BlueprintActionRequest, id string, ch definitions.ResourceChange, vars expr.Vars) error {
	rid, ok := r.Instance.Resources[id]
	if !ok {
		return errors.New("the instance has no such resource")
	}
	repoName, typeName, key, err := locate.ParseID(rid)
	if err != nil {
		return err
	}
	if repoName != r.Repository {
		return errors.New("resources in other repositories come with multi-repository change sets")
	}
	t := repo.Types[typeName]
	if t == nil {
		return fmt.Errorf("type %s is not defined", typeName)
	}
	loc, err := t.Spec.Locate.Locator()
	if err != nil {
		return err
	}
	params, err := loc.ParseKey(key)
	if err != nil {
		return err
	}
	region, err := loc.Region(params)
	if err != nil {
		return err
	}
	current, err := readDocument(fsys, region)
	if err != nil {
		return err
	}
	req := generate.Request{Params: params}
	if ch.Action != "" {
		j := slices.IndexFunc(t.Spec.Actions, func(a definitions.Action) bool { return a.Name == ch.Action })
		if j < 0 {
			return fmt.Errorf("type %s has no action %s", t.Metadata.Name, ch.Action)
		}
		ta := t.Spec.Actions[j]
		if ta.Kind == definitions.ActionImperative {
			return fmt.Errorf("action %s is imperative: it runs an executor and writes no files", ta.Name)
		}
		in, err := evalMap(ch.InputsValue, vars)
		if err != nil {
			return fmt.Errorf("inputs: %w", err)
		}
		typeVars := expr.Vars{Inputs: current, Params: anyMap(params), User: vars.User, Request: expr.Request{Kind: generate.Action, Action: ta.Name}}
		checked, err := generate.ValidateInputs(ta.Inputs, in, typeVars)
		if err != nil {
			return fmt.Errorf("inputs of %s: %w", ta.Name, err)
		}
		req.Kind, req.ActionName, req.ActionInputs = generate.Action, ta.Name, checked
	} else {
		values, err := evalMap(ch.ValuesValue, vars)
		if err != nil {
			return fmt.Errorf("values: %w", err)
		}
		req.Kind, req.Values = generate.Update, merge(current, values)
	}
	region, err = addType(cs, fsys, t, req)
	if errors.Is(err, ErrNoChange) {
		return nil
	}
	if err != nil {
		return err
	}
	next := current
	if i := slices.IndexFunc(cs.Changes, func(c Change) bool { return c.Path == region.Path }); i >= 0 && cs.Changes[i].After != nil {
		if doc, err := documentOf(region, cs.Changes[i].After); err == nil {
			next = doc
		}
	}
	cs.Resources = append(cs.Resources, Planned{ID: id, Type: t.Metadata.Name, Key: region.Key, Params: params, Values: next})
	return nil
}

// readDocument reads the document of a resource: its file, or its entry in a shared file.
func readDocument(fsys fs.FS, region locate.Region) (map[string]any, error) {
	src, err := fs.ReadFile(fsys, region.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", region.Path, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return documentOf(region, src)
}

// documentOf returns the document of a resource in src, its file.
func documentOf(region locate.Region, src []byte) (map[string]any, error) {
	if region.Strategy == locate.Entry || region.Strategy == locate.Field {
		doc, _, err := entryDocument(region, src)
		return doc, err
	}
	decoded, _, err := edit.Decode(region.Path, src)
	if err != nil {
		return nil, fmt.Errorf("%s cannot be read, so Quai will not edit it: %w", region.Path, err)
	}
	doc, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s does not hold a mapping, so Quai will not edit it", region.Path)
	}
	return doc, nil
}

// merge returns base with over merged in: nested mappings merge, anything else is replaced.
func merge(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	maps.Copy(out, base)
	for k, v := range over {
		if vm, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = merge(bm, vm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func anyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
