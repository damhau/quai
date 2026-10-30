// Package lint checks a repository as `quai lint` does in its CI
// (docs/design/02-resource-types-and-generators.md#lint): every definition loads, every file a
// type's locator matches parses and conforms to the type's schema, and every such file survives
// a round trip, `write(read(file))` changing no byte. A type that cannot reproduce its existing
// files would rewrite them on the first edit, so its files are reported.
package lint

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/edit"
	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/engine/schema"
	"github.com/damhau/quai/backend/internal/engine/tfplan"
)

// Report is the result of a check.
type Report struct {
	Types      int
	Blueprints int
	Resources  int
	// Problems fail the check. Notes report what this version does not check yet.
	Problems definitions.Problems
	Notes    definitions.Problems
}

// Run checks the repository fsys.
func Run(fsys fs.FS) Report {
	repo, problems := definitions.Load(fsys)
	r := Report{Types: len(repo.Types), Blueprints: len(repo.Blueprints), Problems: problems}
	for _, name := range slices.Sorted(maps.Keys(repo.Types)) {
		r.checkType(fsys, repo.Types[name])
	}
	sortProblems(r.Problems)
	sortProblems(r.Notes)
	return r
}

// Plan checks a plan of stack, the output of terraform show -json read from file, against the
// repository fsys: every Terraform resource it changes belongs to exactly one Quai resource,
// through the types' stack and address patterns.
func Plan(fsys fs.FS, plan []byte, file, stack string) definitions.Problems {
	repo, _ := definitions.Load(fsys)
	changes, err := tfplan.Parse(plan, tfplan.Options{})
	if err != nil {
		return definitions.Problems{{Path: file, Message: err.Error()}}
	}
	m := tfplan.Map(changes, stack, tfplan.Claims(repo.Types))
	var problems definitions.Problems
	for _, c := range m.Unmapped {
		problems = append(problems, definitions.Problem{Path: file, Field: c.Address, Message: fmt.Sprintf("%s by no resource: no type's stack and address match it in %s", pastTense(c.Action), stack)})
	}
	for _, a := range m.Ambiguous {
		problems = append(problems, definitions.Problem{Path: file, Field: a.Address, Message: "claimed by more than one resource: " + strings.Join(a.Resources, ", ")})
	}
	sortProblems(problems)
	return problems
}

func pastTense(action string) string {
	return map[string]string{tfplan.Create: "created", tfplan.Update: "updated", tfplan.Delete: "deleted", tfplan.Replace: "replaced"}[action]
}

func (r *Report) problem(file, field, format string, args ...any) {
	r.Problems = append(r.Problems, definitions.Problem{Path: file, Field: field, Message: fmt.Sprintf(format, args...)})
}

func (r *Report) checkType(fsys fs.FS, t *definitions.ResourceType) {
	if t.Spec.Locate.Strategy != locate.File || t.Spec.Generate.Kind != definitions.GenerateIdentity {
		r.Notes = append(r.Notes, definitions.Problem{Path: t.Path, Message: fmt.Sprintf(
			"the files of type %s are not checked: the %s locator with the %s generator is not available yet",
			t.Metadata.Name, t.Spec.Locate.Strategy, t.Spec.Generate.Kind)})
		return
	}
	loc, err := locate.New(t.Spec.Locate.Strategy, t.Spec.Locate.Path)
	if err != nil {
		r.problem(t.Path, "spec.locate.path", "%v", err)
		return
	}
	regions, err := loc.Scan(fsys)
	if err != nil {
		r.problem(t.Path, "spec.locate", "%v", err)
		return
	}
	for _, region := range regions {
		r.Resources++
		r.checkFile(fsys, t, region)
	}
}

func (r *Report) checkFile(fsys fs.FS, t *definitions.ResourceType, region locate.Region) {
	src, err := fs.ReadFile(fsys, region.Path)
	if err != nil {
		r.problem(region.Path, "", "cannot read: %v", err)
		return
	}
	decoded, code, err := edit.Decode(region.Path, src)
	if err != nil {
		r.problem(region.Path, "", "%v", err)
		return
	}
	doc, ok := decoded.(map[string]any)
	if !ok {
		r.problem(region.Path, "", "must hold a mapping (type %s)", t.Metadata.Name)
		return
	}
	if err := t.FileSchema.Validate(doc); err != nil {
		var ve *schema.ValidationError
		if !errors.As(err, &ve) {
			r.problem(region.Path, "", "%v", err)
			return
		}
		fields := edit.OutsideCode(ve.Fields, code)
		for _, f := range slices.Sorted(maps.Keys(fields)) {
			r.problem(region.Path, f, "%s (schema %s)", fields[f], t.FileSchema.Path())
		}
		if len(fields) > 0 {
			return
		}
	}
	// Round trip: read the file as form values and write them back.
	written, err := generate.Identity(t, generate.Request{Kind: generate.Update, Params: region.Params, Current: doc, Values: doc, Code: code})
	if err != nil {
		r.problem(region.Path, "", "round trip: %v", err)
		return
	}
	out, err := edit.Edit(region.Path, src, written)
	if err != nil {
		r.problem(region.Path, "", "round trip: Quai cannot edit this file: %v", err)
		return
	}
	if !bytes.Equal(out, src) {
		r.problem(region.Path, "", "round trip: Quai would rewrite this file on its first edit (%s)", changedLines(src, out))
	}
}

// changedLines describes the lines a rewrite would change, for the message.
func changedLines(before, after []byte) string {
	a := strings.Split(string(before), "\n")
	b := strings.Split(string(after), "\n")
	var removed, added []string
	for _, l := range a {
		if !slices.Contains(b, l) {
			removed = append(removed, strings.TrimSpace(l))
		}
	}
	for _, l := range b {
		if !slices.Contains(a, l) {
			added = append(added, strings.TrimSpace(l))
		}
	}
	var parts []string
	if len(removed) > 0 {
		parts = append(parts, "removes "+quoteAll(removed))
	}
	if len(added) > 0 {
		parts = append(parts, "adds "+quoteAll(added))
	}
	if len(parts) == 0 {
		return "reorders lines"
	}
	return strings.Join(parts, ", ")
}

func quoteAll(lines []string) string {
	q := make([]string, len(lines))
	for i, l := range lines {
		q[i] = fmt.Sprintf("%q", l)
	}
	return strings.Join(q, ", ")
}

func sortProblems(ps definitions.Problems) {
	slices.SortStableFunc(ps, func(a, b definitions.Problem) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Field, b.Field)
	})
}
