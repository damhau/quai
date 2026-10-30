// Package yamledit edits YAML files that people also maintain, changing only the bytes of the
// values that differ.
//
// It implements docs/design/03-file-editing.md for YAML. The original file is parsed into a
// syntax tree that keeps comments and positions; the semantic change between the current and the
// intended document is computed; each changed scalar becomes a text edit on the original bytes;
// and the result is verified before it is returned: it parses, it decodes to exactly the intended
// document, and every byte outside the edited spans is unchanged. When an edit cannot be made
// safely, Edit refuses with a *RefusalError instead of rewriting the file.
//
// Documents use the JSON data model: mappings with string keys, sequences, strings, numbers,
// booleans and null. Numbers compare by value, so 8 and 8.0 are equal, but 8 and "8" are not.
//
// Changes are made at the smallest node that differs: a scalar is replaced in place, a new entry
// or item is inserted after its siblings in their style, a removed one takes its lines with it,
// and a value whose type changes is replaced whole. New content follows the file's conventions
// (indentation, sequence style, quote style, line endings); new keys are written in sorted order.
package yamledit

import (
	"fmt"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

// Edit returns src edited so that it decodes to want, changing as few bytes as it can.
//
// want is a document in the JSON data model: nil, bool, string, any Go integer or float type,
// json.Number, []any and map[string]any, nested freely. When src already decodes to want, Edit
// returns src itself. An edit that cannot be made safely returns a *RefusalError; any other error
// means want is not a valid document.
func Edit(src []byte, want any) (out []byte, err error) {
	// The parser's positions are not trustworthy on every input. A bug that lets one through as
	// an out-of-range offset must refuse the edit, not crash the caller; the fuzz target treats
	// such a refusal as a failure.
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, refuse(nil, "%s: %v", internalError, r)
		}
	}()
	want, err = normalize(want, path{})
	if err != nil {
		return nil, err
	}
	f, err := parse(src)
	if err != nil {
		return nil, err
	}
	changes := document.Diff(f.value, want)
	if len(changes) == 0 {
		return src, nil
	}
	edits, err := f.edits(changes)
	if err != nil {
		return nil, err
	}
	out, err = apply(src, edits)
	if err != nil {
		return nil, err
	}
	if err := verify(src, out, edits, want); err != nil {
		return nil, err
	}
	return out, nil
}

// Elements returns the entries of the mapping, or the items of the list, at p in src, with their
// bytes, in file order.
func Elements(src []byte, p document.Path) ([]document.Element, error) {
	f, err := parse(src)
	if err != nil {
		return nil, err
	}
	c, err := f.collectionAt(p)
	if err != nil {
		return nil, err
	}
	out := make([]document.Element, len(c.elements))
	for i, el := range c.elements {
		out[i] = document.Element{Key: el.key, Start: el.start, End: el.end}
	}
	return out, nil
}

// Decode returns the first document of src in the JSON data model, as Edit reads it: nil, bool,
// string, int64, uint64, float64, []any and map[string]any. Dates stay text, keys are their
// text, merge keys are merged, and duplicate keys or values outside the data model are errors.
// An empty file decodes to nil.
func Decode(src []byte) (any, error) {
	return decode(src)
}

// internalError starts the reason of a refusal caused by a bug in this package.
const internalError = "internal error"

// RefusalError reports an edit that Edit will not make because it cannot be made safely.
type RefusalError struct {
	// Path is the document path concerned, such as "$.disks[0].size_gb", or empty when the
	// refusal is about the whole file.
	Path   string
	Reason string
	// At is Path as a document path, nil for the whole file.
	At document.Path
}

func (e *RefusalError) Error() string {
	if e.Path == "" {
		return "yamledit: refused: " + e.Reason
	}
	return fmt.Sprintf("yamledit: refused at %s: %s", e.Path, e.Reason)
}

// refuse returns a *RefusalError about p, or about the whole file when p is nil.
func refuse(p path, format string, args ...any) error {
	e := &RefusalError{Reason: fmt.Sprintf(format, args...)}
	if p != nil {
		e.Path, e.At = p.String(), p
	}
	return e
}
