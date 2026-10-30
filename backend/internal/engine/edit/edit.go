// Package edit picks a file's editor by its name and edits it through one interface
// (docs/design/02-resource-types-and-generators.md#formats): YAML (*.yaml, *.yml), JSON
// (*.json, *.tfvars.json) and HCL (*.tfvars, *.hcl, whose top-level attributes are the
// document). The editors are yamledit, jsonedit and hcledit; they change only the bytes of the
// values that differ and verify every write (docs/design/03-file-editing.md).
package edit

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
	"github.com/damhau/quai/backend/internal/engine/edit/hcledit"
	"github.com/damhau/quai/backend/internal/engine/edit/jsonedit"
	"github.com/damhau/quai/backend/internal/engine/edit/yamledit"
)

// Format is a file format Quai edits.
type Format string

const (
	YAML Format = "yaml"
	JSON Format = "json"
	HCL  Format = "hcl"
)

// FormatOf returns the format of a file by its name, and false for a format Quai does not edit.
func FormatOf(name string) (Format, bool) {
	switch strings.ToLower(path.Ext(name)) {
	case ".yaml", ".yml":
		return YAML, true
	case ".json":
		return JSON, true
	case ".tfvars", ".hcl":
		return HCL, true
	}
	return "", false
}

// Code is a value defined in code (an HCL expression or heredoc): the document leaves it out,
// edits keep it as it is, and writing a value there is refused.
type Code = hcledit.Code

// Decode returns the document of a file, and its values defined in code.
func Decode(name string, src []byte) (doc any, code []Code, err error) {
	f, err := format(name)
	if err != nil {
		return nil, nil, err
	}
	switch f {
	case JSON:
		doc, err = jsonedit.Decode(src)
	case HCL:
		return hcledit.Decode(src, "")
	default:
		doc, err = yamledit.Decode(src)
	}
	return doc, nil, err
}

// Edit returns a file edited so that it decodes to want, changing as few bytes as it can. An edit
// that cannot be made safely is refused (see Refused).
func Edit(name string, src []byte, want any) ([]byte, error) {
	switch f, err := format(name); {
	case err != nil:
		return nil, err
	case f == JSON:
		return jsonedit.Edit(src, want)
	case f == HCL:
		return hcledit.Edit(src, "", want)
	default:
		return yamledit.Edit(src, want)
	}
}

// EditWith is Edit, with opts saying where new keys go and in which order new objects list
// theirs.
func EditWith(name string, src []byte, want any, opts document.Options) ([]byte, error) {
	switch f, err := format(name); {
	case err != nil:
		return nil, err
	case f == JSON:
		return jsonedit.EditWith(src, want, opts)
	case f == HCL:
		return hcledit.EditWith(src, "", want, opts)
	default:
		return yamledit.EditWith(src, want, opts)
	}
}

// Create returns a new file holding doc, its object keys listed in keyOrder first.
func Create(name string, doc any, keyOrder []string) ([]byte, error) {
	switch f, err := format(name); {
	case err != nil:
		return nil, err
	case f == JSON:
		return jsonedit.Create(doc, keyOrder)
	case f == HCL:
		return hcledit.Create(doc, keyOrder)
	default:
		return yamledit.Create(doc, keyOrder)
	}
}

// Elements returns the entries of the object, or the items of the list, at p in a file, with their
// bytes, in file order.
func Elements(name string, src []byte, p document.Path) ([]document.Element, error) {
	switch f, err := format(name); {
	case err != nil:
		return nil, err
	case f == JSON:
		return jsonedit.Elements(src, p)
	case f == HCL:
		return hcledit.Elements(src, "", p)
	default:
		return yamledit.Elements(src, p)
	}
}

// Keys returns, for each object of a file, its keys in the order they are written, by JSON
// pointer.
func Keys(name string, src []byte) (map[string][]string, error) {
	switch f, err := format(name); {
	case err != nil:
		return nil, err
	case f == JSON:
		return jsonedit.Keys(src)
	case f == HCL:
		return hcledit.Keys(src, "")
	default:
		return yamledit.Keys(src)
	}
}

func format(name string) (Format, error) {
	f, ok := FormatOf(name)
	if !ok {
		return "", fmt.Errorf("%s: Quai edits YAML, JSON and HCL files (.yaml, .yml, .json, .tfvars, .hcl)", name)
	}
	return f, nil
}

// Refused reports whether err is an editor's refusal to make an edit it cannot make safely, with
// the document path concerned (nil for the whole file) and the reason.
func Refused(err error) (at document.Path, reason string, ok bool) {
	var y *yamledit.RefusalError
	var j *jsonedit.RefusalError
	var h *hcledit.RefusalError
	switch {
	case errors.As(err, &y):
		return y.At, y.Reason, true
	case errors.As(err, &j):
		return j.At, j.Reason, true
	case errors.As(err, &h):
		return h.At, h.Reason, true
	}
	return nil, "", false
}

// OutsideCode returns schema errors by field without those of fields defined in code: the
// document leaves them out, so a required one reads as missing.
func OutsideCode(fields map[string]string, code []Code) map[string]string {
	out := make(map[string]string, len(fields))
	for f, msg := range fields {
		out[f] = msg
	}
	for _, c := range code {
		delete(out, Field(c.Path))
	}
	return out
}

// Under returns the values defined in code under at, relative to it: those of an entry of a
// shared file, say.
func Under(code []Code, at document.Path) []Code {
	var out []Code
	for _, c := range code {
		if len(c.Path) > len(at) && slices.Equal(c.Path[:len(at)], at) {
			out = append(out, Code{Path: c.Path[len(at):], Text: c.Text})
		}
	}
	return out
}

// Field returns a document path as schema errors and forms name fields: "disks.0.size_gb", "" for
// the document.
func Field(p document.Path) string {
	parts := make([]string, len(p))
	for i, step := range p {
		parts[i] = fmt.Sprint(step)
	}
	return strings.Join(parts, ".")
}
