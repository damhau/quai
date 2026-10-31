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
	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/engine/schema"
	"github.com/damhau/quai/backend/internal/engine/tfplan"
)

// Report is the result of a check.
type Report struct {
	Types      int
	Blueprints int
	Resources  int
	Tests      int // golden examples
	// NonConforming counts, by type, the resources whose files do not conform to its schema
	// (docs/design/02, schema evolution); Migratable those of them the type's migration fixes,
	// which a migrate request will, and which are not problems.
	NonConforming map[string]int
	Migratable    map[string]int
	// Problems fail the check.
	Problems definitions.Problems
}

// Run checks the repository fsys.
func Run(fsys fs.FS) Report {
	repo, problems := definitions.Load(fsys)
	r := Report{Types: len(repo.Types), Blueprints: len(repo.Blueprints), Problems: problems,
		NonConforming: map[string]int{}, Migratable: map[string]int{}}
	for _, name := range slices.Sorted(maps.Keys(repo.Types)) {
		r.checkType(fsys, repo.Types[name])
	}
	for _, name := range slices.Sorted(maps.Keys(repo.Blueprints)) {
		b := repo.Blueprints[name]
		for _, msg := range b.Pins(repo) {
			r.problem(b.Path, "spec.resources", "%s", msg)
		}
	}
	r.checkTests(fsys, repo)
	sortProblems(r.Problems)
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
	if t.Spec.Generate.Kind == definitions.GeneratePlugin {
		// The module must be the one the type pins, import nothing a plugin may not, and export
		// the ABI: what Quai checks before running it.
		if _, err := generate.Module(fsys, t); err != nil {
			r.problem(t.Path, "spec.generate.module", "%v", err)
			return
		}
	}
	loc, err := t.Spec.Locate.Locator()
	if err != nil {
		r.problem(t.Path, "spec.locate.path", "%v", err)
		return
	}
	if loc.Strategy() == locate.Entry || loc.Strategy() == locate.Field {
		// A shared file that cannot hold its entries safely is one problem, for the file.
		files, err := loc.Files(fsys)
		if err != nil {
			r.problem(t.Path, "spec.locate", "%v", err)
			return
		}
		for _, file := range files {
			src, err := fs.ReadFile(fsys, file.Path)
			if err != nil {
				r.problem(file.Path, "", "cannot read: %v", err)
				continue
			}
			if _, problem := loc.Elements(file.Path, src); problem != "" {
				r.problem(file.Path, "", "%s", problem)
			}
		}
	}
	regions, err := loc.Scan(fsys)
	if err != nil {
		r.problem(t.Path, "spec.locate", "%v", err)
		return
	}
	for _, region := range regions {
		r.Resources++
		switch {
		case region.Strategy == locate.Dir:
			r.checkDir(fsys, t, region)
		case region.Problem == "":
			r.checkFile(fsys, t, region)
		}
	}
}

// checkDir checks a directory region: its record conforms to the type's schema, and planning an
// update with the values read back from it changes no file.
func (r *Report) checkDir(fsys fs.FS, t *definitions.ResourceType, region locate.Region) {
	file := region.DocumentFile()
	src, err := fs.ReadFile(fsys, file)
	if errors.Is(err, fs.ErrNotExist) {
		r.problem(region.Path, "", "has no %s, the record of its values (type %s)", locate.RecordFile, t.Metadata.Name)
		return
	}
	if err != nil {
		r.problem(file, "", "cannot read: %v", err)
		return
	}
	decoded, code, err := edit.Decode(file, src)
	if err != nil {
		r.problem(file, "", "%v", err)
		return
	}
	doc, ok := decoded.(map[string]any)
	if !ok {
		r.problem(file, "", "must hold a mapping (type %s)", t.Metadata.Name)
		return
	}
	if !r.conforms(file, t, doc, code) {
		return
	}
	values, err := generate.Read(t, doc, region.Params)
	if err != nil {
		r.problem(file, "", "round trip: %v", err)
		return
	}
	cs, err := plan.Type(fsys, t, generate.Request{Kind: generate.Update, Params: region.Params, Current: doc, Values: values, Code: code})
	switch {
	case errors.Is(err, plan.ErrNoChange):
	case err != nil:
		r.problem(region.Path, "", "round trip: %v", err)
	default:
		for _, c := range cs.Changes {
			if c.Before == nil {
				r.problem(c.Path, "", "round trip: Quai would write this file on its first edit")
			} else {
				r.problem(c.Path, "", "round trip: Quai would rewrite this file on its first edit (%s)", changedLines(c.Before, c.After))
			}
		}
	}
}

// conforms checks a resource's document against the type's file schema, reporting each field
// that does not conform, except those defined in code.
func (r *Report) conforms(where string, t *definitions.ResourceType, doc map[string]any, code []edit.Code) bool {
	err := t.FileSchema.Validate(doc)
	if err == nil {
		return true
	}
	var ve *schema.ValidationError
	if !errors.As(err, &ve) {
		r.problem(where, "", "%v", err)
		return false
	}
	fields := edit.OutsideCode(ve.Fields, code)
	for _, f := range slices.Sorted(maps.Keys(fields)) {
		r.problem(where, f, "%s (schema %s)", fields[f], t.FileSchema.Path())
	}
	return len(fields) == 0
}

// conformsOrMigrates is conforms for a resource of a type with a migration: a file that does not
// conform but that the migration makes conform is counted, not a problem.
func (r *Report) conformsOrMigrates(where string, t *definitions.ResourceType, region locate.Region, doc map[string]any, code []edit.Code) bool {
	err := t.FileSchema.Validate(doc)
	var ve *schema.ValidationError
	if err == nil || (errors.As(err, &ve) && len(edit.OutsideCode(ve.Fields, code)) == 0) {
		return true
	}
	r.NonConforming[t.Metadata.Name]++
	if t.Spec.MigrateTemplate != nil {
		if _, err := generate.Identity(t, generate.Request{Kind: generate.Migrate, Params: region.Params, Current: doc, Code: code}); err == nil {
			r.Migratable[t.Metadata.Name]++
			return false
		}
	}
	return r.conforms(where, t, doc, code)
}

func (r *Report) checkFile(fsys fs.FS, t *definitions.ResourceType, region locate.Region) {
	src, err := fs.ReadFile(fsys, region.Path)
	if err != nil {
		r.problem(region.Path, "", "cannot read: %v", err)
		return
	}
	decoded, code, err := generate.Decode(fsys, t, region.Path, region.Params, src)
	if err != nil {
		r.problem(region.Path, "", "%v", err)
		return
	}
	// An entry's document is its value in the file, a field region's its listed fields; the round
	// trip writes it back there.
	where := region.Path
	if region.Collection != nil {
		where += " (" + region.Item + ")"
	}
	doc, code, err := locate.Document(decoded, code, region)
	if err != nil {
		r.problem(where, "", "must hold a mapping (type %s): %v", t.Metadata.Name, err)
		return
	}
	if !r.conformsOrMigrates(where, t, region, doc, code) {
		return
	}
	// Round trip: read the file as form values and write them back.
	values, err := generate.Read(t, doc, region.Params)
	if err != nil {
		r.problem(where, "", "round trip: %v", err)
		return
	}
	out, err := r.roundTrip(fsys, t, region, src, decoded, doc, values, code)
	var ue uneditable
	switch {
	case errors.As(err, &ue):
		r.problem(where, "", "round trip: Quai cannot edit this file: %v", ue.error)
		return
	case err != nil:
		r.problem(where, "", "round trip: %v", err)
		return
	}
	if !bytes.Equal(out, src) {
		r.problem(where, "", "round trip: Quai would rewrite this file on its first edit (%s)", changedLines(src, out))
	}
}

// roundTrip writes values back into src, the file of a region whose document is doc, decoded
// whole: through the type's plugin, or into the region's place in the file with its editor.
func (r *Report) roundTrip(fsys fs.FS, t *definitions.ResourceType, region locate.Region, src []byte, decoded any, doc, values map[string]any, code []edit.Code) ([]byte, error) {
	req := generate.Request{Kind: generate.Update, Params: region.Params, Current: doc, Values: values, Code: code}
	if t.Spec.Generate.Kind == definitions.GeneratePlugin {
		return generate.Plugin(fsys, t, req, src)
	}
	written, err := generate.Document(t, req)
	if err != nil {
		return nil, err
	}
	whole, err := locate.Put(decoded, region, written)
	if err != nil {
		return nil, err
	}
	out, err := edit.Edit(region.Path, src, whole)
	if err != nil {
		return nil, uneditable{err}
	}
	return out, nil
}

// uneditable is an error of a file's editor: the file is one Quai cannot edit.
type uneditable struct{ error }

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
