package plan

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/edit"
	"github.com/damhau/quai/backend/internal/engine/edit/document"
	"github.com/damhau/quai/backend/internal/engine/edit/yamledit"
	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/generate/component"
	"github.com/damhau/quai/backend/internal/engine/locate"
)

// addDir plans a request on a directory region (docs/design/02, dir), written by the component
// generator: the component's manifests in manifests.yaml, a kustomization.yaml that lists them,
// and the quai.yaml record of the values. A create writes the three files; an update or an action
// reads the values from the record and edits each file in place: the record as the identity
// generator edits a file, the manifests document by document (each addressed by its kind and
// name), and the kustomization so that it lists the manifests, keeping what else it holds. The
// directory's other files are left as they are. A delete removes every file under it.
func addDir(cs *ChangeSet, fsys fs.FS, t *definitions.ResourceType, region locate.Region, r generate.Request) error {
	fp, err := locate.Fingerprint(fsys, region)
	if err != nil {
		return err
	}
	if _, dup := cs.Fingerprints[region.Path]; dup {
		return fmt.Errorf("%s is written twice by the same request", region.Path)
	}
	cs.Fingerprints[region.Path] = fp
	id := t.Metadata.Name + "/" + region.Key
	switch {
	case r.Kind == generate.Create && fp != locate.Absent:
		return fmt.Errorf("%s: %s %w", id, region.Path, ErrExists)
	case r.Kind != generate.Create && fp == locate.Absent:
		return fmt.Errorf("%s: %s %w", id, region.Path, ErrNotFound)
	}
	if r.Kind == generate.Delete {
		// The fingerprint refused symbolic links: every file under the directory is regular.
		return fs.WalkDir(fsys, region.Path, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			before, err := fs.ReadFile(fsys, p)
			if err == nil {
				cs.Changes = append(cs.Changes, Change{Path: p, Op: OpDelete, Before: before})
			}
			return err
		})
	}

	record := region.DocumentFile()
	manifests := path.Join(region.Path, component.Manifests)
	kustomization := path.Join(region.Path, component.Kustomization)
	current := map[string][]byte{}
	if r.Kind != generate.Create {
		for _, f := range []string{record, manifests, kustomization} {
			b, err := fs.ReadFile(fsys, f)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			current[f] = b
		}
		if current[record] == nil {
			return fmt.Errorf("%s: %s has no %s, the record of its values, so Quai cannot edit it", id, region.Path, locate.RecordFile)
		}
		decoded, code, err := edit.Decode(record, current[record])
		if err != nil {
			return fmt.Errorf("%s cannot be read, so Quai will not edit it: %w", record, err)
		}
		doc, ok := decoded.(map[string]any)
		if !ok {
			return fmt.Errorf("%s does not hold a mapping, so Quai will not edit it", record)
		}
		r.Current, r.Code = doc, code
	}
	values, rendered, err := generate.Component(t, r)
	if err != nil {
		return err
	}

	var written int
	write := func(name string, render func(before []byte) ([]byte, error)) error {
		before := current[name]
		after, err := render(before)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		switch {
		case before == nil:
			cs.Changes = append(cs.Changes, Change{Path: name, Op: OpCreate, After: after})
			written++
		case !bytes.Equal(before, after):
			cs.Changes = append(cs.Changes, Change{Path: name, Op: OpUpdate, Before: before, After: after})
			written++
		}
		return nil
	}
	docs := make([]any, len(rendered))
	for i, m := range rendered {
		docs[i] = m
	}
	err = errors.Join(
		write(record, func(before []byte) ([]byte, error) {
			if before == nil {
				return edit.Create(record, values, t.FileSchema.PropertyOrder())
			}
			return edit.Edit(record, before, values)
		}),
		write(manifests, func(before []byte) ([]byte, error) {
			if before == nil {
				return yamledit.CreateDocuments(docs, component.KeyOrder)
			}
			return yamledit.EditDocuments(before, docs, component.Key, component.KeyOrder)
		}),
		write(kustomization, func(before []byte) ([]byte, error) {
			if before == nil {
				return yamledit.Create(component.KustomizationDoc(), component.KustomizationOrder)
			}
			return kustomize(before)
		}),
	)
	if err != nil {
		return err
	}
	if written == 0 {
		return ErrNoChange
	}
	return nil
}

// kustomize returns a directory's kustomization.yaml edited so that it is a Kustomization that
// lists the manifests: its other keys and resources stay.
func kustomize(src []byte) ([]byte, error) {
	decoded, err := yamledit.Decode(src)
	if err != nil {
		return nil, err
	}
	doc, ok := decoded.(map[string]any)
	if !ok && decoded != nil {
		return nil, errors.New("does not hold a mapping, so Quai will not edit it")
	}
	want := component.KustomizationDoc()
	for k, v := range doc {
		if k != "apiVersion" && k != "kind" {
			want[k] = v
		}
	}
	resources, _ := doc["resources"].([]any)
	if !slices.Contains(resources, any(component.Manifests)) {
		resources = append(slices.Clone(resources), component.Manifests)
	}
	want["resources"] = resources
	return yamledit.EditWith(src, want, document.Options{KeyOrder: component.KustomizationOrder})
}
