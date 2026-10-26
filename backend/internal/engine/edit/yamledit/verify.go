package yamledit

import (
	"bytes"
	"slices"
)

// apply applies non-overlapping edits to src.
func apply(src []byte, edits []textEdit) ([]byte, error) {
	edits = slices.Clone(edits)
	slices.SortFunc(edits, func(a, b textEdit) int { return a.start - b.start })
	var out bytes.Buffer
	at := 0
	for _, e := range edits {
		if e.start < at || e.end < e.start || e.end > len(src) {
			return nil, refuse(nil, "two edits overlap")
		}
		out.Write(src[at:e.start])
		out.WriteString(e.text)
		at = e.end
	}
	out.Write(src[at:])
	return out.Bytes(), nil
}

// verify checks an edited file before it is written: it parses as one document, it decodes to
// exactly want, and every byte outside the edited spans is unchanged.
func verify(src, out []byte, edits []textEdit, want any) error {
	tree, err := parseTree(out)
	if err != nil {
		return refuse(nil, "verification failed: the edited file does not parse: %v", err)
	}
	if len(tree.Docs) > 1 {
		return refuse(nil, "verification failed: the edited file has more than one document")
	}
	got, err := decode(out)
	if err != nil {
		return refuse(nil, "verification failed: %v", err)
	}
	if !equal(got, want) {
		return refuse(nil, "verification failed: the edited file does not decode to the intended document")
	}
	if !outsideUnchanged(src, out, edits) {
		return refuse(nil, "verification failed: bytes outside the edited values changed")
	}
	return nil
}

// outsideUnchanged reports whether out is src with each edit's span replaced by its text and
// nothing else.
func outsideUnchanged(src, out []byte, edits []textEdit) bool {
	edits = slices.Clone(edits)
	slices.SortFunc(edits, func(a, b textEdit) int { return a.start - b.start })
	at, shift := 0, 0
	for _, e := range edits {
		gap := src[at:e.start]
		o := at + shift
		if o+len(gap)+len(e.text) > len(out) || !bytes.Equal(out[o:o+len(gap)], gap) ||
			string(out[o+len(gap):o+len(gap)+len(e.text)]) != e.text {
			return false
		}
		shift += len(e.text) - (e.end - e.start)
		at = e.end
	}
	return bytes.Equal(out[min(at+shift, len(out)):], src[at:]) && len(out) == len(src)+shift
}
