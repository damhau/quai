package definitions

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/expr"
)

var (
	nameRe       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	identRe      = regexp.MustCompile(`^[a-z][a-zA-Z0-9_]*$`)
	versionRe    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
	placeholder  = regexp.MustCompile(`\{([^{}]*)\}`)
	fieldPathRe  = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)
	approverRe   = regexp.MustCompile(`^(group|user):[^\s:]+$`)
	digestRe     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	durationRe   = regexp.MustCompile(`^[0-9]+[mhd]$`)
	dataFileRe   = regexp.MustCompile(`\.(ya?ml|json|tfvars|hcl)$`)
	reservedDirs = []string{".git", ".quai", BlueprintsDir}

	// Built-in actions every resource has; definitions cannot redefine them.
	reservedActions = []string{"create", "update", "delete", "transfer-ownership"}
	approverKinds   = []string{"owners", "codeowners", "manager"}
	components      = []string{"kubevirt.vm", "crossplane.claim", "argocd.application", "k8s.namespace", "flux.kustomization", "raw"}
)

// check collects the problems of one definition file.
type check struct {
	l    *loader
	file string
	n    int
}

func (c *check) add(field, format string, args ...any) {
	c.n++
	c.l.add(c.file, field, format, args...)
}

func (c *check) ok() bool { return c.n == 0 }

func (c *check) metadata(m Metadata) {
	if !nameRe.MatchString(m.Name) {
		c.add("metadata.name", "must be a lowercase DNS label (a-z, 0-9, -), starting with a letter")
	}
	if strings.TrimSpace(m.Title) == "" {
		c.add("metadata.title", "is required")
	}
	if m.Version != "" && !versionRe.MatchString(m.Version) {
		c.add("metadata.version", "must be a semantic version such as 1.2.0")
	}
}

func (c *check) condition(field, src string) *expr.Program {
	p, err := c.l.env.CompileCondition(src)
	if err != nil {
		c.add(field, "%v", err)
	}
	return p
}

// ruleCondition compiles an approval rule's condition, which may read the plan.
func (c *check) ruleCondition(field, src string) *expr.Program {
	p, err := c.l.rules.CompileCondition(src)
	if err != nil {
		c.add(field, "%v", err)
	}
	return p
}

func (c *check) value(field string, v map[string]any) *expr.Value {
	if v == nil {
		return nil
	}
	cv, err := c.l.env.CompileValue(v)
	if err != nil {
		c.add(field, "%v", err)
	}
	return cv
}

func (c *check) template(field, src string) *expr.Template {
	t, err := c.l.env.CompileTemplate(src)
	if err != nil {
		c.add(field, "%v", err)
	}
	return t
}

// relPath checks a path to a file or directory of the repository.
func (c *check) relPath(field, p string) bool {
	if p == "" {
		c.add(field, "is required")
		return false
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || slices.ContainsFunc(strings.Split(p, "/"), func(s string) bool {
		return s == "" || s == "." || s == ".."
	}) {
		c.add(field, "must be a clean relative path in the repository")
		return false
	}
	return true
}

func (l *loader) resourceType(file string) *ResourceType {
	var t ResourceType
	if !l.read(file, KindResourceType, &t) {
		return nil
	}
	t.Path = file
	c := &check{l: l, file: file}
	c.metadata(t.Metadata)
	s := &t.Spec

	params := c.params(s)
	c.locate(s.Locate, params)
	for _, f := range []struct{ field, value string }{{"spec.stack", s.Stack}, {"spec.address", s.Address}} {
		for _, m := range placeholder.FindAllStringSubmatch(f.value, -1) {
			if !params[m[1]] {
				c.add(f.field, "{%s} is not a param", m[1])
			}
		}
	}
	for _, p := range slices.Sorted(maps.Keys(s.ParamFields)) {
		if !params[p] {
			c.add("spec.paramFields."+p, "is not a param")
		}
		if !fieldPathRe.MatchString(s.ParamFields[p]) {
			c.add("spec.paramFields."+p, "must be a dotted field path")
		}
	}

	if c.relPath("spec.schema", s.Schema) {
		sch, err := l.schemas.Compile(s.Schema)
		if err != nil {
			c.add("spec.schema", "%v", err)
		}
		t.FileSchema, t.InputSchema = sch, sch
	}
	if s.Inputs != "" && c.relPath("spec.inputs", s.Inputs) {
		sch, err := l.schemas.Compile(s.Inputs)
		if err != nil {
			c.add("spec.inputs", "%v", err)
		}
		t.InputSchema = sch
	}

	c.generate(&s.Generate)
	if o := s.Ownership; o != nil {
		set := 0
		for _, b := range []bool{o.Field != "", o.Codeowners, o.Team != ""} {
			if b {
				set++
			}
		}
		if set != 1 {
			c.add("spec.ownership", "must set exactly one of field, codeowners or team")
		}
		if o.Field != "" && !fieldPathRe.MatchString(o.Field) {
			c.add("spec.ownership.field", "must be a dotted field path")
		}
	}
	oneOf(c, "spec.deletion", s.Deletion, "", "allowed", "approval", "forbidden")
	oneOf(c, "spec.catalog", s.Catalog, "", "visible", "hidden")
	c.ui("spec.ui", &s.UI)
	c.approval("spec.approval", &s.Approval)

	names := map[string]bool{}
	for i := range s.Actions {
		a := &s.Actions[i]
		at := fmt.Sprintf("spec.actions[%d]", i)
		c.actionName(at, a.Name, names)
		oneOf(c, at+".kind", a.Kind, "", ActionDeclarative, ActionImperative)
		if a.AvailableIf != "" {
			a.Available = c.condition(at+".availableIf", a.AvailableIf)
		}
		c.inputs(at+".inputs", a.Inputs)
		a.Set = numbersIn(a.Set)
		a.Target = numbersIn(a.Target)
		if a.Kind == ActionImperative {
			if a.Executor == "" || a.Operation == "" {
				c.add(at, "an imperative action needs an executor and an operation")
			}
			if len(a.Set) > 0 {
				c.add(at+".set", "an imperative action does not set fields")
			}
			a.TargetValue = c.value(at+".target", a.Target)
		} else {
			if len(a.Set) == 0 {
				c.add(at+".set", "is required for a declarative action")
			}
			for _, k := range slices.Sorted(maps.Keys(a.Set)) {
				if !fieldPathRe.MatchString(k) {
					c.add(at+".set", "invalid field path %q", k)
				}
			}
			a.SetValue = c.value(at+".set", a.Set)
		}
		if a.Approval != nil {
			c.approval(at+".approval", a.Approval)
		}
	}
	if !c.ok() {
		return nil
	}
	return &t
}

// params checks the params and returns the set of their names.
func (c *check) params(s *TypeSpec) map[string]bool {
	params := map[string]bool{}
	for i, p := range s.Params {
		at := fmt.Sprintf("spec.params[%d]", i)
		if !identRe.MatchString(p.Name) {
			c.add(at+".name", "must start with a lowercase letter and contain only letters, digits and _")
		} else if params[p.Name] {
			c.add(at+".name", "%s is declared twice", p.Name)
		}
		params[p.Name] = true
		oneOf(c, at+".type", p.Type, "", "string", "enum")
		if p.Type == "enum" && len(p.Options) == 0 && !p.Discover {
			c.add(at, "an enum param needs options or discover: true")
		}
		if p.Pattern != "" {
			if _, err := regexp.Compile("^(?:" + p.Pattern + ")$"); err != nil {
				c.add(at+".pattern", "%v", err)
			}
		}
	}
	return params
}

func (c *check) locate(lo Locate, params map[string]bool) {
	oneOf(c, "spec.locate.strategy", lo.Strategy, LocateFile, LocateDir, LocateEntry, LocateField)
	if !c.relPath("spec.locate.path", lo.Path) {
		return
	}
	if slices.Contains(reservedDirs, strings.Split(lo.Path, "/")[0]) {
		c.add("spec.locate.path", "cannot be under %s/", strings.Split(lo.Path, "/")[0])
	}
	if strings.Contains(lo.Path, "}{") {
		c.add("spec.locate.path", "params must be separated by a literal")
	}
	var inPath []string
	for _, m := range placeholder.FindAllStringSubmatch(lo.Path, -1) {
		switch {
		case !params[m[1]]:
			c.add("spec.locate.path", "{%s} is not a param", m[1])
		case slices.Contains(inPath, m[1]):
			c.add("spec.locate.path", "{%s} appears twice", m[1])
		}
		inPath = append(inPath, m[1])
	}
	if strings.Count(lo.Path, "{") != len(inPath) || strings.Count(lo.Path, "}") != len(inPath) {
		c.add("spec.locate.path", "has unbalanced braces")
	}
	for _, p := range slices.Sorted(maps.Keys(params)) {
		if !slices.Contains(inPath, p) && p != lo.KeyParam {
			c.add("spec.params", "%s is not used by spec.locate", p)
		}
	}
	switch lo.Strategy {
	case LocateFile:
		if len(inPath) == 0 {
			c.add("spec.locate.path", "needs at least one {param}")
		}
		if !dataFileRe.MatchString(lo.Path) {
			c.add("spec.locate.path", "must end in .yaml, .yml, .json, .tfvars or .hcl")
		}
	case LocateDir:
		if len(inPath) == 0 {
			c.add("spec.locate.path", "needs at least one {param}")
		}
	case LocateEntry, LocateField:
		if !dataFileRe.MatchString(lo.Path) {
			c.add("spec.locate.path", "must end in .yaml, .yml, .json, .tfvars or .hcl")
		}
		if lo.Collection == "" || !fieldPathRe.MatchString(lo.Collection) {
			c.add("spec.locate.collection", "must be a dotted field path")
		}
		if !params[lo.KeyParam] {
			c.add("spec.locate.keyParam", "must name a param")
		} else if slices.Contains(inPath, lo.KeyParam) {
			c.add("spec.locate.keyParam", "names the entry, not the file: it cannot be in spec.locate.path")
		}
		if lo.Strategy == LocateField && len(lo.Fields) == 0 {
			c.add("spec.locate.fields", "is required for the field strategy")
		}
		for _, f := range lo.Fields {
			if !fieldPathRe.MatchString(f) {
				c.add("spec.locate.fields", "invalid field path %q", f)
			}
		}
	}
	if lo.Strategy != LocateEntry && lo.Order != "" {
		c.add("spec.locate.order", "applies only to the entry strategy")
	}
	oneOf(c, "spec.locate.order", lo.Order, "", "key")
}

func (c *check) generate(g *Generate) {
	if g.Kind == "" {
		g.Kind = GenerateIdentity
	}
	oneOf(c, "spec.generate.kind", g.Kind, GenerateIdentity, GenerateMapping, GenerateComponent, GeneratePlugin)
	g.Template = numbersIn(g.Template)
	g.With = numbersIn(g.With)
	g.Config = numbersIn(g.Config)
	unused := func(field string, set bool) {
		if set {
			c.add("spec.generate."+field, "does not apply to the %s generator", g.Kind)
		}
	}
	unused("template", g.Template != nil && g.Kind != GenerateMapping)
	unused("read", g.Read != nil && g.Kind != GenerateMapping)
	unused("component", g.Component != "" && g.Kind != GenerateComponent)
	unused("with", g.With != nil && g.Kind != GenerateComponent)
	unused("module", g.Module != "" && g.Kind != GeneratePlugin)
	unused("digest", g.Digest != "" && g.Kind != GeneratePlugin)
	unused("config", g.Config != nil && g.Kind != GeneratePlugin)
	switch g.Kind {
	case GenerateMapping:
		if len(g.Template) == 0 {
			c.add("spec.generate.template", "is required for the mapping generator")
		}
		g.TemplateValue = c.value("spec.generate.template", g.Template)
		g.ReadTemplates = map[string]*expr.Template{}
		for _, k := range slices.Sorted(maps.Keys(g.Read)) {
			g.ReadTemplates[k] = c.template("spec.generate.read."+k, g.Read[k])
		}
	case GenerateComponent:
		oneOf(c, "spec.generate.component", g.Component, components...)
	case GeneratePlugin:
		c.relPath("spec.generate.module", g.Module)
		if !digestRe.MatchString(g.Digest) {
			c.add("spec.generate.digest", "must be sha256: followed by 64 hexadecimal digits")
		}
	}
}

func (c *check) ui(at string, ui *UI) {
	for _, k := range slices.Sorted(maps.Keys(ui.Fields)) {
		f := ui.Fields[k]
		if !fieldPathRe.MatchString(k) {
			c.add(at+".fields", "invalid field path %q", k)
		}
		if f.VisibleIf != "" {
			f.Visible = c.condition(at+".fields."+k+".visibleIf", f.VisibleIf)
		}
		ui.Fields[k] = f
	}
}

func (c *check) approval(at string, a *Approval) {
	names := map[string]bool{}
	for i := range a.Rules {
		r := &a.Rules[i]
		rat := fmt.Sprintf("%s.rules[%d]", at, i)
		switch {
		case r.Name == "":
			c.add(rat+".name", "is required")
		case names[r.Name]:
			c.add(rat+".name", "%s is used twice", r.Name)
		}
		names[r.Name] = true
		if len(r.Approvers) == 0 && r.External == "" {
			c.add(rat, "approvers are required, unless the rule is external")
		}
		for _, ap := range r.Approvers {
			if !approverRe.MatchString(ap) && !slices.Contains(approverKinds, ap) {
				c.add(rat+".approvers", "%q is not an approver: use group:<name>, user:<name>, owners, codeowners or manager", ap)
			}
		}
		if r.Min < 0 {
			c.add(rat+".min", "must be at least 0")
		}
		if r.When != "" {
			r.Condition = c.ruleCondition(rat+".when", r.When)
		}
	}
	if a.Expiry != "" && !durationRe.MatchString(a.Expiry) {
		c.add(at+".expiry", "must be a duration such as 30m, 12h or 7d")
	}
	if a.StepUp != "" {
		a.StepUpCondition = c.condition(at+".stepUp", a.StepUp)
	}
}

func (c *check) actionName(at, name string, names map[string]bool) {
	switch {
	case !nameRe.MatchString(name):
		c.add(at+".name", "must be a lowercase DNS label")
	case slices.Contains(reservedActions, name):
		c.add(at+".name", "%s is a built-in action", name)
	case names[name]:
		c.add(at+".name", "%s is defined twice", name)
	}
	names[name] = true
}

func (c *check) inputs(at string, inputs []Input) {
	names := map[string]bool{}
	for i := range inputs {
		in := &inputs[i]
		iat := fmt.Sprintf("%s[%d]", at, i)
		switch {
		case !identRe.MatchString(in.Name):
			c.add(iat+".name", "must start with a lowercase letter and contain only letters, digits and _")
		case names[in.Name]:
			c.add(iat+".name", "%s is declared twice", in.Name)
		}
		names[in.Name] = true
		oneOf(c, iat+".type", in.Type, InputString, InputInteger, InputNumber, InputBoolean, InputEnum)
		if in.Type == InputEnum && len(in.Options) == 0 {
			c.add(iat+".options", "an enum needs options")
		}
		if in.Min != nil && in.Max != nil && *in.Min > *in.Max {
			c.add(iat, "min is greater than max")
		}
		if in.Pattern != "" {
			if _, err := regexp.Compile("^(?:" + in.Pattern + ")$"); err != nil {
				c.add(iat+".pattern", "%v", err)
			}
		}
		in.Default = numbers(in.Default)
		if in.VisibleIf != "" {
			in.Visible = c.condition(iat+".visibleIf", in.VisibleIf)
		}
	}
}

func oneOf(c *check, field, v string, allowed ...string) {
	if slices.Contains(allowed, v) {
		return
	}
	var shown []string
	for _, a := range allowed {
		if a != "" {
			shown = append(shown, a)
		}
	}
	if v == "" {
		c.add(field, "is required: one of %s", strings.Join(shown, ", "))
		return
	}
	c.add(field, "must be one of %s, not %q", strings.Join(shown, ", "), v)
}

func (l *loader) blueprint(file string, types map[string]*ResourceType) *Blueprint {
	var b Blueprint
	if !l.read(file, KindBlueprint, &b) {
		return nil
	}
	b.Path = file
	c := &check{l: l, file: file}
	c.metadata(b.Metadata)
	s := &b.Spec

	if s.Inputs == nil {
		c.add("spec.inputs", "is required: a JSON Schema for the form")
	} else {
		s.Inputs = numbersIn(s.Inputs)
		// The schema is addressed by the blueprint's own path, so its $refs resolve from there.
		if err := l.schemas.Add(file, s.Inputs); err != nil {
			c.add("spec.inputs", "%v", err)
		} else if sch, err := l.schemas.Compile(file); err != nil {
			c.add("spec.inputs", "%v", err)
		} else {
			b.InputSchema = sch
		}
	}
	c.ui("spec.ui", &s.UI)
	c.approval("spec.approval", &s.Approval)

	if len(s.Resources) == 0 {
		c.add("spec.resources", "a blueprint needs at least one resource")
	}
	ids := map[string]int{}
	for i := range s.Resources {
		r := &s.Resources[i]
		at := fmt.Sprintf("spec.resources[%d]", i)
		switch _, dup := ids[r.ID]; {
		case !nameRe.MatchString(r.ID):
			c.add(at+".id", "must be a lowercase DNS label")
		case dup:
			c.add(at+".id", "%s is used twice", r.ID)
		}
		ids[r.ID] = i
		if !nameRe.MatchString(r.Type.Name) {
			c.add(at+".type.name", "must name a resource type")
		} else if r.Type.Repository == "" && types[r.Type.Name] == nil {
			c.add(at+".type", "type %s is not defined in this repository", r.Type.Name)
		}
		r.Params = numbersIn(r.Params)
		r.Values = numbersIn(r.Values)
		r.ParamsValue = c.value(at+".params", r.Params)
		r.ValuesValue = c.value(at+".values", r.Values)
		if r.When != "" {
			r.Condition = c.condition(at+".when", r.When)
		}
	}
	for i, r := range s.Resources {
		for _, d := range r.DependsOn {
			if _, ok := ids[d]; !ok || d == r.ID {
				c.add(fmt.Sprintf("spec.resources[%d].dependsOn", i), "%s is not another resource of the blueprint", d)
			}
		}
	}
	if cycle := dependencyCycle(s.Resources); cycle != nil {
		c.add("spec.resources", "dependsOn has a cycle: %s", strings.Join(cycle, " → "))
	}

	names := map[string]bool{}
	for i := range s.Actions {
		a := &s.Actions[i]
		at := fmt.Sprintf("spec.actions[%d]", i)
		c.actionName(at, a.Name, names)
		c.inputs(at+".inputs", a.Inputs)
		if len(a.Apply) == 0 {
			c.add(at+".apply", "is required: which resources the action changes")
		}
		for _, id := range slices.Sorted(maps.Keys(a.Apply)) {
			ch := a.Apply[id]
			cat := at + ".apply." + id
			ri, ok := ids[id]
			if !ok {
				c.add(cat, "%s is not a resource of the blueprint", id)
			}
			if (ch.Action == "") == (ch.Values == nil) {
				c.add(cat, "must set either values or action")
			}
			if ch.Action != "" && ok {
				if t := types[s.Resources[ri].Type.Name]; s.Resources[ri].Type.Repository == "" && t != nil && !slices.ContainsFunc(t.Spec.Actions, func(ta Action) bool { return ta.Name == ch.Action }) {
					c.add(cat+".action", "type %s has no action %s", t.Metadata.Name, ch.Action)
				}
			}
			ch.Values = numbersIn(ch.Values)
			ch.Inputs = numbersIn(ch.Inputs)
			ch.ValuesValue = c.value(cat+".values", ch.Values)
			ch.InputsValue = c.value(cat+".inputs", ch.Inputs)
			a.Apply[id] = ch
		}
		if a.Approval != nil {
			c.approval(at+".approval", a.Approval)
		}
	}
	if !c.ok() {
		return nil
	}
	return &b
}

// dependencyCycle returns the resource ids of a dependsOn cycle, or nil.
func dependencyCycle(resources []BlueprintResource) []string {
	deps := map[string][]string{}
	for _, r := range resources {
		deps[r.ID] = r.DependsOn
	}
	state := map[string]int{} // 1: on the current path, 2: done
	var stack []string
	var visit func(id string) []string
	visit = func(id string) []string {
		switch state[id] {
		case 1:
			i := slices.Index(stack, id)
			return append(slices.Clone(stack[i:]), id)
		case 2:
			return nil
		}
		state[id] = 1
		stack = append(stack, id)
		for _, d := range deps[id] {
			if _, ok := deps[d]; ok {
				if cycle := visit(d); cycle != nil {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return nil
	}
	for _, r := range resources {
		if cycle := visit(r.ID); cycle != nil {
			return cycle
		}
	}
	return nil
}
