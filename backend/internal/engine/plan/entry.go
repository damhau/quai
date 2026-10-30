package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/edit"
	"github.com/damhau/quai/backend/internal/engine/edit/document"
	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/locate"
)

// Entry is a change to one entry of a shared file (the `entry` locator). A change to such a file
// keeps its entries' changes, so that delivery can make them again on the file as it is then: two
// requests on two entries of one file do not conflict
// (docs/design/03-file-editing.md#concurrency-and-fingerprints).
type Entry struct {
	Op         string `json:"op"`                 // create, update or delete
	Collection string `json:"collection"`         // the dotted path of the list or map
	KeyField   string `json:"keyField,omitempty"` // the field naming a list's items; empty for a map
	Item       string `json:"item"`
	Value      any    `json:"value,omitempty"` // the entry's new value, for create and update
	// Sorted puts a new entry in key order instead of at the end (`order: key`).
	Sorted bool `json:"sorted,omitempty"`
	// KeyOrder is the order of a new entry's keys.
	KeyOrder []string `json:"keyOrder,omitempty"`
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
// that every other entry of the collection keeps its bytes and its place.
func (e Entry) apply(name string, src []byte) ([]byte, error) {
	r := locate.EntryRegion(name, e.Collection, e.KeyField, e.Item)
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
		if before, problem = r.Elements(src); problem != "" {
			return nil, fmt.Errorf("%s: %s, so Quai will not edit it", name, problem)
		}
	}
	coll, err := e.collection(document.ValueAt(doc, r.Collection))
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", name, e.Collection, err)
	}
	want, err := with(doc, r.Collection, coll)
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", name, e.Collection, err)
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

// with returns doc with v at p, creating the objects missing on the way.
func with(doc any, p document.Path, v any) (any, error) {
	if len(p) == 0 {
		return v, nil
	}
	switch d := doc.(type) {
	case nil:
		k, ok := p[0].(string)
		if !ok {
			return nil, fmt.Errorf("item %v does not exist", p[0])
		}
		next, err := with(nil, p[1:], v)
		return map[string]any{k: next}, err
	case map[string]any:
		k, ok := p[0].(string)
		if !ok {
			return nil, errors.New("is not a list")
		}
		next, err := with(d[k], p[1:], v)
		out := maps.Clone(d)
		out[k] = next
		return out, err
	case []any:
		i, ok := p[0].(int)
		if !ok || i >= len(d) {
			return nil, errors.New("is not an object")
		}
		next, err := with(d[i], p[1:], v)
		out := slices.Clone(d)
		out[i] = next
		return out, err
	}
	return nil, errors.New("is not an object")
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
	base, err := fs.ReadFile(fsys, region.Path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	src, prev := base, slices.IndexFunc(cs.Changes, func(c Change) bool { return c.Path == region.Path })
	if prev >= 0 {
		if len(cs.Changes[prev].Entries) == 0 {
			return fmt.Errorf("%s is written twice by the same request", region.Path)
		}
		src = cs.Changes[prev].After
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
	after, err := e.apply(region.Path, src)
	if err != nil {
		return err
	}
	if bytes.Equal(after, src) {
		return ErrNoChange
	}
	cs.Fingerprints[ref] = fp
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

// entryDocument returns an entry's value in its file, and its values defined in code.
func entryDocument(region locate.Region, src []byte) (map[string]any, []edit.Code, error) {
	decoded, code, err := edit.Decode(region.Path, src)
	if err != nil {
		return nil, nil, fmt.Errorf("%s cannot be read, so Quai will not edit it: %w", region.Path, err)
	}
	value, at, ok := locate.Value(decoded, region)
	if !ok {
		return nil, nil, fmt.Errorf("%s: %s %w", region.Path, region.Item, ErrNotFound)
	}
	doc, ok := value.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("%s: the entry %s does not hold a mapping, so Quai will not edit it", region.Path, region.Item)
	}
	return doc, edit.Under(code, at), nil
}
