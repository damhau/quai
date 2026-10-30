package yamledit

import (
	"fmt"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

// The data model is package document's, shared with the other editors.

// path addresses a value in a document: string elements are mapping keys, int elements are
// sequence indices. The empty path is the document root.
type path = document.Path

// change is one difference between the current and the intended document.
type change = document.Change

const (
	setScalar   = document.SetScalar
	replaceNode = document.ReplaceNode
	addKey      = document.AddKey
	removeKey   = document.RemoveKey
	insertItem  = document.InsertItem
	removeItem  = document.RemoveItem
)

// normalize converts a document to its normalized form (document.Normalize).
func normalize(v any, _ path) (any, error) {
	n, err := document.Normalize(v)
	if err != nil {
		return nil, fmt.Errorf("yamledit: %v", err)
	}
	return n, nil
}

func equal(a, b any) bool                  { return document.Equal(a, b) }
func sortedKeys(m map[string]any) []string { return document.SortedKeys(m) }
func valueAt(doc any, p path) any          { return document.ValueAt(doc, p) }
