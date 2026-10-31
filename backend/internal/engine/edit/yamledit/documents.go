package yamledit

import (
	"bytes"
	"fmt"
	"unicode/utf8"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

// Document is one document of a multi-document file (docs/design/03-file-editing.md, YAML): its
// bytes, src[Start:End], from its `---` line (or the start of the file) to the next one, and its
// value. Comments before the first `---` belong to the first document.
type Document struct {
	Start, End int
	Value      any
}

// Documents splits src into its documents. A line that starts with `---` starts a document, as
// YAML says even inside a scalar; each part must parse on its own to the document the whole file
// has at its place, or the file is refused. Files with directives (`%YAML`) or document end
// markers (`...`) are refused too. An empty file has no documents; an empty document (`---` and
// nothing) is a document whose value is nil.
func Documents(src []byte) ([]Document, error) {
	if err := multiChecks(src); err != nil {
		return nil, err
	}
	whole, err := parseTree(src)
	if err != nil {
		return nil, refuse(nil, "the file is not valid YAML: %v", err)
	}
	starts := []int{0}
	for _, ls := range lineStarts(src) {
		line := src[ls:]
		switch {
		case marker(line, "---") && ls > 0:
			starts = append(starts, ls)
		case marker(line, "..."):
			return nil, refuse(nil, "the file has a document end marker (...)")
		case len(line) > 0 && line[0] == '%':
			return nil, refuse(nil, "the file has a directive (%%)")
		}
	}
	var out []Document
	pending := -1 // the start of a part without a document: comments that belong to the next one
	for i, start := range starts {
		end := len(src)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		docs, err := parseTree(src[start:end])
		if err != nil {
			return nil, refuse(nil, "the document at byte %d does not parse on its own: %v", start, err)
		}
		switch {
		case len(docs) == 0 && i == 0:
			pending = start
			continue
		case len(docs) != 1:
			return nil, refuse(nil, "the file cannot be split into its documents")
		}
		if pending >= 0 {
			start, pending = pending, -1
		}
		var v any
		if len(docs[0].Content) == 1 && !isEmpty(docs[0].Content[0]) {
			if v, err = value(docs[0].Content[0]); err != nil {
				return nil, refuse(nil, "%v", err)
			}
		}
		out = append(out, Document{Start: start, End: end, Value: v})
	}
	if len(out) != len(whole) {
		return nil, refuse(nil, "the file cannot be split into its documents")
	}
	for i, d := range whole {
		var v any
		if len(d.Content) == 1 && !isEmpty(d.Content[0]) {
			if v, err = value(d.Content[0]); err != nil {
				return nil, refuse(nil, "%v", err)
			}
		}
		if !equal(v, out[i].Value) {
			return nil, refuse(nil, "the file cannot be split into its documents")
		}
	}
	return out, nil
}

func multiChecks(src []byte) error {
	switch {
	case bytes.HasPrefix(src, []byte{0xEF, 0xBB, 0xBF}):
		return refuse(nil, "the file starts with a byte-order mark")
	case !utf8.Valid(src):
		return refuse(nil, "the file is not valid UTF-8")
	case otherBreaks(src):
		return refuse(nil, "the file has line breaks other than LF and CRLF")
	}
	return nil
}

// marker reports whether line starts with the document marker m, followed by a space, a tab, a
// line break or the end of the file.
func marker(line []byte, m string) bool {
	if !bytes.HasPrefix(line, []byte(m)) {
		return false
	}
	rest := line[len(m):]
	return len(rest) == 0 || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r'
}

// EditDocuments returns src edited so that its documents are want, changing as few bytes as it
// can. Each document of want is addressed by key: the document of src with the same non-empty key,
// or, for a document whose key is empty, the document of src at the same index if its key is
// empty too. Each one found is edited in place, as Edit edits a file; the documents of src that
// want does not address are removed, except empty ones, which stay; the others of want are added
// at the end, each after a `---` line, written as Create writes them with keyOrder. The result
// is verified: its documents, without the empty ones, are want's, in its order.
func EditDocuments(src []byte, want []any, key func(any) string, keyOrder []string) ([]byte, error) {
	docs, err := Documents(src)
	if err != nil {
		return nil, err
	}
	normal := make([]any, len(want))
	wantKeys := map[string]int{}
	for i, w := range want {
		if normal[i], err = normalize(w, path{}); err != nil {
			return nil, err
		}
		if k := key(normal[i]); k != "" {
			if _, dup := wantKeys[k]; dup {
				return nil, fmt.Errorf("yamledit: two documents are %s", k)
			}
			wantKeys[k] = i
		}
	}
	// The documents of src, by key; nil ones are not addressed.
	var nonEmpty []int
	srcKeys := map[string]int{}
	for i, d := range docs {
		if d.Value == nil {
			continue
		}
		if k := key(d.Value); k != "" {
			if _, dup := srcKeys[k]; dup {
				return nil, refuse(nil, "two documents of the file are %s", k)
			}
			srcKeys[k] = i
		}
		nonEmpty = append(nonEmpty, i)
	}
	target := map[int]int{} // document of src -> document of want
	for i, w := range normal {
		if k := key(w); k != "" {
			if j, ok := srcKeys[k]; ok {
				target[j] = i
			}
		} else if i < len(nonEmpty) && key(docs[nonEmpty[i]].Value) == "" {
			target[nonEmpty[i]] = i
		}
	}
	nl := "\n"
	if bytes.Contains(src, []byte("\r\n")) {
		nl = "\r\n"
	}
	var out []byte
	var order []any
	placed := map[int]bool{}
	for j, d := range docs {
		part := src[d.Start:d.End]
		i, ok := target[j]
		switch {
		case d.Value == nil:
			out = append(out, part...)
		case ok:
			edited, err := EditWith(part, normal[i], document.Options{KeyOrder: keyOrder})
			if err != nil {
				return nil, err
			}
			out = append(out, edited...)
			order, placed[i] = append(order, normal[i]), true
		}
	}
	for i, w := range normal {
		if placed[i] {
			continue
		}
		text, err := Create(w, keyOrder)
		if err != nil {
			return nil, err
		}
		if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
			out = append(out, nl...)
		}
		if len(bytes.TrimSpace(out)) > 0 {
			out = append(out, "---"+nl...)
		}
		out = append(out, bytes.ReplaceAll(text, []byte("\n"), []byte(nl))...)
		order = append(order, w)
	}
	if err := verifyDocuments(out, order); err != nil {
		return nil, err
	}
	if bytes.Equal(out, src) {
		return src, nil
	}
	return out, nil
}

// CreateDocuments returns a new multi-document file holding docs, each written as Create writes
// it with keyOrder, separated by `---` lines.
func CreateDocuments(docs []any, keyOrder []string) ([]byte, error) {
	var out []byte
	normal := make([]any, len(docs))
	for i, d := range docs {
		var err error
		if normal[i], err = normalize(d, path{}); err != nil {
			return nil, err
		}
		text, err := Create(normal[i], keyOrder)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			out = append(out, "---\n"...)
		}
		out = append(out, text...)
	}
	if err := verifyDocuments(out, normal); err != nil {
		return nil, err
	}
	return out, nil
}

// verifyDocuments checks that out's documents, without the empty ones, are want.
func verifyDocuments(out []byte, want []any) error {
	docs, err := Documents(out)
	if err != nil {
		return fmt.Errorf("yamledit: %s: the edited file does not split into its documents: %v", internalError, err)
	}
	var got []any
	for _, d := range docs {
		if d.Value != nil {
			got = append(got, d.Value)
		}
	}
	if len(got) != len(want) {
		return fmt.Errorf("yamledit: %s: the edited file has %d documents, not %d", internalError, len(got), len(want))
	}
	for i := range got {
		if !equal(got[i], want[i]) {
			return fmt.Errorf("yamledit: %s: document %d of the edited file is not the intent", internalError, i)
		}
	}
	return nil
}
