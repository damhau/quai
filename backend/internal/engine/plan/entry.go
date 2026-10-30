package plan

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/edit"
	"github.com/damhau/quai/backend/internal/engine/edit/document"
	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/engine/schema"
)

// Entry is a change to one entry of a shared file (the `entry` locator), or to the listed fields
// of an entry or of a whole file (the `field` locator). A change to such a file keeps these
// changes, so that delivery can make them again on the file as it is then: two requests on two
// entries of one file do not conflict (docs/design/03-file-editing.md#concurrency-and-fingerprints).
type Entry struct {
	Op         string `json:"op"`                 // create, update or delete
	Collection string `json:"collection"`         // the dotted path of the list or map; empty: the file
	KeyField   string `json:"keyField,omitempty"` // the field naming a list's items; empty for a map
	Item       string `json:"item"`
	Value      any    `json:"value,omitempty"` // the entry's new value, for create and update
	// Sorted puts a new entry in key order instead of at the end (`order: key`).
	Sorted bool `json:"sorted,omitempty"`
	// KeyOrder is the order of a new entry's keys.
	KeyOrder []string `json:"keyOrder,omitempty"`
	// Fields, for an update of a field region, are the only fields it writes: Value holds them.
	Fields []string `json:"fields,omitempty"`
}

// region returns the region the change is made in, in the file named name.
func (e Entry) region(name string) locate.Region {
	r := locate.Region{Strategy: locate.File, Path: name}
	if e.Collection != "" {
		r = locate.EntryRegion(name, e.Collection, e.KeyField, e.Item)
	}
	if len(e.Fields) > 0 {
		r.Strategy, r.Fields = locate.Field, e.Fields
	}
	return r
}

// DecodeEntries reads the entry changes of a file change as stored, numbers as json.Number so
// that integers stay integers.
func DecodeEntries(b []byte) ([]Entry, error) {
	if len(b) == 0 {
		return nil, nil
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var out []Entry
	if err := d.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// On returns the file's content after the change, made on current, the file as it is now (nil
// when it does not exist): After when the file is as it was planned on, or when the change
// replaces the whole file (its region's fingerprint then says whether it still may); otherwise,
// for a change to entries of a shared file, the same entries changed again on current.
func (c Change) On(current []byte) ([]byte, error) {
	if len(c.Entries) == 0 || bytes.Equal(current, c.Before) {
		return c.After, nil
	}
	out := current
	for _, e := range c.Entries {
		var err error
		if out, err = e.apply(c.Path, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// apply makes the change on src, the file named name (nil when it does not exist), and checks
// that every other entry of the collection keeps its bytes and its place, and that a change to
// fields changes nothing else.
func (e Entry) apply(name string, src []byte) ([]byte, error) {
	r := e.region(name)
	var doc any = map[string]any{}
	var before []locate.Element
	if src != nil {
		d, _, err := edit.Decode(name, src)
		if err != nil {
			return nil, fmt.Errorf("%s cannot be read, so Quai will not edit it: %w", name, err)
		}
		if d != nil {
			doc = d
		}
		var problem string
		if r.Collection != nil {
			if before, problem = r.Elements(src); problem != "" {
				return nil, fmt.Errorf("%s: %s, so Quai will not edit it", name, problem)
			}
		}
	}
	var want any
	if len(e.Fields) > 0 {
		value, _ := e.Value.(map[string]any)
		var err error
		if want, err = locate.Put(doc, r, value); src == nil || errors.Is(err, locate.ErrNoEntry) {
			return nil, fmt.Errorf("%s %w", cmp.Or(e.Item, name), ErrNotFound)
		} else if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	} else {
		coll, err := e.collection(document.ValueAt(doc, r.Collection))
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", name, e.Collection, err)
		}
		if want, err = document.Set(doc, r.Collection, coll); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", name, e.Collection, err)
		}
	}
	if src == nil {
		return edit.Create(name, want, e.KeyOrder)
	}
	opts := document.Options{KeyOrder: e.KeyOrder}
	if e.Sorted && e.KeyField == "" {
		opts.Sorted = []document.Path{r.Collection}
	}
	out, err := edit.EditWith(name, src, want, opts)
	if err != nil {
		return nil, err
	}
	if len(e.Fields) > 0 && !e.fieldsOnly(name, doc, out) {
		return nil, fmt.Errorf("%s: the change would touch more than %s, so Quai will not make it", name, strings.Join(e.Fields, ", "))
	}
	if r.Collection == nil {
		return out, nil
	}
	after, problem := r.Elements(out)
	if problem != "" {
		return nil, fmt.Errorf("%s: after the change, %s", name, problem)
	}
	others := func(src []byte, els []locate.Element) (keys []string, content map[string]string) {
		content = map[string]string{}
		for _, el := range els {
			if el.Key != e.Item {
				keys = append(keys, el.Key)
				content[el.Key] = string(el.Content(src))
			}
		}
		return keys, content
	}
	keysBefore, was := others(src, before)
	keysAfter, is := others(out, after)
	if !slices.Equal(keysBefore, keysAfter) || !maps.Equal(was, is) {
		return nil, fmt.Errorf("%s: the change to %s would touch other entries, so Quai will not make it", name, e.Item)
	}
	return out, nil
}

// fieldsOnly reports whether out, the file after a change to fields, differs from doc, the
// document before, in those fields only.
func (e Entry) fieldsOnly(name string, doc any, out []byte) bool {
	r := e.region(name)
	got, _, err := edit.Decode(name, out)
	if err != nil {
		return false
	}
	was, err1 := locate.Put(doc, r, map[string]any{})
	is, err2 := locate.Put(got, r, map[string]any{})
	return err1 == nil && err2 == nil && document.Equal(was, is)
}

// collection returns the list or map with the change made.
func (e Entry) collection(cur any) (any, error) {
	if e.KeyField == "" {
		m, ok := cur.(map[string]any)
		if cur != nil && !ok {
			return nil, errors.New("is not a map")
		}
		_, exists := m[e.Item]
		if err := e.check(exists); err != nil {
			return nil, err
		}
		out := maps.Clone(m)
		if out == nil {
			out = map[string]any{}
		}
		if e.Op == OpDelete {
			delete(out, e.Item)
		} else {
			out[e.Item] = e.Value
		}
		return out, nil
	}
	list, ok := cur.([]any)
	if cur != nil && !ok {
		return nil, errors.New("is not a list")
	}
	key := func(item any) string {
		m, _ := item.(map[string]any)
		k, _ := m[e.KeyField].(string)
		return k
	}
	i := slices.IndexFunc(list, func(item any) bool { return key(item) == e.Item })
	if err := e.check(i >= 0); err != nil {
		return nil, err
	}
	out := slices.Clone(list)
	switch e.Op {
	case OpCreate:
		at := len(out)
		if e.Sorted {
			if j := slices.IndexFunc(out, func(item any) bool { return key(item) > e.Item }); j >= 0 {
				at = j
			}
		}
		return slices.Insert(out, at, e.Value), nil
	case OpDelete:
		return slices.Delete(out, i, i+1), nil
	default:
		out[i] = e.Value
		return out, nil
	}
}

func (e Entry) check(exists bool) error {
	switch {
	case e.Op == OpCreate && exists:
		return fmt.Errorf("%s %w", e.Item, ErrExists)
	case e.Op != OpCreate && !exists:
		return fmt.Errorf("%s %w", e.Item, ErrNotFound)
	}
	return nil
}

// addEntry plans a request on an entry of a shared file. Another entry of the same request may
// have changed the file already: the change is made on top, and the file's change keeps both.
func addEntry(cs *ChangeSet, fsys fs.FS, t *definitions.ResourceType, region locate.Region, r generate.Request) error {
	fp, err := locate.Fingerprint(fsys, region)
	if err != nil {
		return err
	}
	ref := region.Ref()
	if _, dup := cs.Fingerprints[ref]; dup {
		return fmt.Errorf("%s is written twice by the same request", ref)
	}
	id := t.Metadata.Name + "/" + region.Key
	base, src, prev, err := fileSoFar(cs, fsys, region.Path)
	if err != nil {
		return err
	}
	lo := t.Spec.Locate
	e := Entry{Op: OpUpdate, Collection: lo.Collection, KeyField: lo.Key, Item: region.Item, Sorted: lo.Order == "key"}
	e.KeyOrder = t.FileSchema.PropertyOrder()
	if lo.Key != "" {
		// A list's items are named by their key field: it comes first, and it is the item's key.
		e.KeyOrder = append([]string{lo.Key}, slices.DeleteFunc(slices.Clone(e.KeyOrder), func(k string) bool { return k == lo.Key })...)
		if r.Kind == generate.Create || r.Kind == generate.Update {
			r.Values = maps.Clone(r.Values)
			if r.Values == nil {
				r.Values = map[string]any{}
			}
			r.Values[lo.Key] = region.Item
		}
	}
	switch {
	case r.Kind == generate.Create:
		if fp != locate.Absent {
			return fmt.Errorf("%s: %s %w", id, ref, ErrExists)
		}
		e.Op = OpCreate
	case fp == locate.Absent:
		return fmt.Errorf("%s: %s %w", id, ref, ErrNotFound)
	case r.Kind == generate.Delete:
		e.Op = OpDelete
	default:
		if r.Current, r.Code, err = entryDocument(region, base); err != nil {
			return err
		}
	}
	if e.Op != OpDelete {
		doc, err := generate.Identity(t, r)
		if err != nil {
			return err
		}
		if lo.Key != "" && doc[lo.Key] != region.Item {
			return fmt.Errorf("%s: the field %s names the entry: it must stay %s", id, lo.Key, region.Item)
		}
		e.Value = doc
	}
	return record(cs, region, fp, base, src, prev, e)
}

// addFields plans a request on a field region: an update, or an action, that writes the listed
// fields of an entry or of a whole file and nothing else. The region exists while its entry or
// file does: it is never created or deleted.
func addFields(cs *ChangeSet, fsys fs.FS, t *definitions.ResourceType, region locate.Region, r generate.Request) error {
	id := t.Metadata.Name + "/" + region.Key
	if r.Kind == generate.Create || r.Kind == generate.Delete {
		return fmt.Errorf("%s: type %s writes fields of something it does not own: it has no %s", id, t.Metadata.Name, r.Kind)
	}
	fp, err := locate.Fingerprint(fsys, region)
	if err != nil {
		return err
	}
	ref := region.Ref()
	switch _, dup := cs.Fingerprints[ref]; {
	case dup:
		return fmt.Errorf("%s is written twice by the same request", ref)
	case fp == locate.Absent:
		return fmt.Errorf("%s: %s %w", id, ref, ErrNotFound)
	}
	base, src, prev, err := fileSoFar(cs, fsys, region.Path)
	if err != nil {
		return err
	}
	if r.Current, r.Code, err = entryDocument(region, base); err != nil {
		return err
	}
	doc, err := generate.Identity(t, r)
	if err != nil {
		return err
	}
	if unlisted := locate.Unlisted(region.Fields, doc); len(unlisted) > 0 {
		fields := map[string]string{}
		for _, f := range unlisted {
			fields[f] = fmt.Sprintf("type %s may write only %s", t.Metadata.Name, strings.Join(region.Fields, ", "))
		}
		return &schema.ValidationError{Fields: fields}
	}
	lo := t.Spec.Locate
	e := Entry{Op: OpUpdate, Collection: lo.Collection, KeyField: lo.Key, Item: region.Item, Value: doc, Fields: region.Fields,
		KeyOrder: t.FileSchema.PropertyOrder()}
	return record(cs, region, fp, base, src, prev, e)
}

// fileSoFar returns a file at the base commit (nil when it does not exist) and as the change set
// has it so far, and the index of the change set's change to it, or -1: another entry of the same
// request may have changed the file already.
func fileSoFar(cs *ChangeSet, fsys fs.FS, path string) (base, src []byte, prev int, err error) {
	base, err = fs.ReadFile(fsys, path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, -1, err
	}
	src, prev = base, slices.IndexFunc(cs.Changes, func(c Change) bool { return c.Path == path })
	if prev >= 0 {
		if len(cs.Changes[prev].Entries) == 0 {
			return nil, nil, -1, fmt.Errorf("%s is written twice by the same request", path)
		}
		src = cs.Changes[prev].After
	}
	return base, src, prev, nil
}

// record makes the change e on src and adds it to the change set, with the region's fingerprint:
// on its change to the file, or as a new change.
func record(cs *ChangeSet, region locate.Region, fp string, base, src []byte, prev int, e Entry) error {
	after, err := e.apply(region.Path, src)
	if err != nil {
		return err
	}
	if bytes.Equal(after, src) {
		return ErrNoChange
	}
	cs.Fingerprints[region.Ref()] = fp
	if prev >= 0 {
		cs.Changes[prev].After = after
		cs.Changes[prev].Entries = append(cs.Changes[prev].Entries, e)
		return nil
	}
	op := OpUpdate
	if base == nil {
		op = OpCreate
	}
	cs.Changes = append(cs.Changes, Change{Path: region.Path, Op: op, Before: base, After: after, Entries: []Entry{e}})
	return nil
}

// entryDocument returns a resource's document in src, its file (the entry's value, or the listed
// fields of a field region), and its values defined in code.
func entryDocument(region locate.Region, src []byte) (map[string]any, []edit.Code, error) {
	decoded, code, err := edit.Decode(region.Path, src)
	if err != nil {
		return nil, nil, fmt.Errorf("%s cannot be read, so Quai will not edit it: %w", region.Path, err)
	}
	doc, code, err := locate.Document(decoded, code, region)
	switch {
	case errors.Is(err, locate.ErrNoEntry):
		return nil, nil, fmt.Errorf("%s: %s %w", region.Path, region.Item, ErrNotFound)
	case err != nil:
		return nil, nil, fmt.Errorf("%s: %w, so Quai will not edit it", region.Path, err)
	}
	return doc, code, nil
}
