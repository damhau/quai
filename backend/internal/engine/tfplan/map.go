package tfplan

import (
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/locate"
)

// Claim is how one resource type finds its Terraform resources: in the stacks its stack pattern
// names, under its address pattern, such as module.vm["{name}"] in envs/{env}.
type Claim struct {
	Type    string
	stack   *template
	address *template
	locate  *locate.Pattern
}

// Claims returns the claims of the types that have both a stack and an address, sorted by type.
func Claims(types map[string]*definitions.ResourceType) []Claim {
	var claims []Claim
	for _, name := range slices.Sorted(maps.Keys(types)) {
		t := types[name]
		if t.Spec.Stack == "" || t.Spec.Address == "" {
			continue
		}
		p, err := locate.Compile(t.Spec.Locate.Path)
		if err != nil {
			continue
		}
		claims = append(claims, Claim{Type: name, stack: compileTemplate(t.Spec.Stack, false), address: compileTemplate(t.Spec.Address, true), locate: p})
	}
	return claims
}

// key returns the key of the resource of c that owns address in stack, if any.
func (c Claim) key(stack, address string) (string, bool) {
	params := map[string]string{}
	if !c.stack.match(stack, params) || !c.address.match(address, params) {
		return "", false
	}
	for _, p := range c.locate.Params() {
		if params[p] == "" {
			return "", false
		}
	}
	return c.locate.Key(params), true
}

// Resource is a Quai resource and the changes a plan makes to its Terraform resources.
type Resource struct {
	Type    string   `json:"type"`
	Key     string   `json:"key"`
	Changes []Change `json:"changes"`
}

// Ambiguous is a change that several resources claim.
type Ambiguous struct {
	Change
	Resources []string `json:"resources"` // <type>/<key>
}

// Mapping is a plan's changes, by the resource they belong to.
type Mapping struct {
	Resources []Resource  `json:"resources,omitempty"`
	Unmapped  []Change    `json:"unmapped,omitempty"`  // claimed by no type
	Ambiguous []Ambiguous `json:"ambiguous,omitempty"` // claimed by more than one resource
}

// Map assigns the changes of a plan of stack to the resources that claim them.
func Map(changes []Change, stack string, claims []Claim) Mapping {
	var m Mapping
	index := map[string]int{} // <type>/<key> → position in m.Resources
	for _, ch := range changes {
		var owners []Resource
		for _, c := range claims {
			if key, ok := c.key(stack, ch.Address); ok {
				owners = append(owners, Resource{Type: c.Type, Key: key})
			}
		}
		switch len(owners) {
		case 0:
			m.Unmapped = append(m.Unmapped, ch)
		case 1:
			id := owners[0].Type + "/" + owners[0].Key
			i, ok := index[id]
			if !ok {
				i = len(m.Resources)
				index[id] = i
				m.Resources = append(m.Resources, owners[0])
			}
			m.Resources[i].Changes = append(m.Resources[i].Changes, ch)
		default:
			a := Ambiguous{Change: ch}
			for _, o := range owners {
				a.Resources = append(a.Resources, o.Type+"/"+o.Key)
			}
			m.Ambiguous = append(m.Ambiguous, a)
		}
	}
	slices.SortFunc(m.Resources, func(a, b Resource) int {
		return compare(a.Type+"/"+a.Key, b.Type+"/"+b.Key)
	})
	return m
}

// template is a stack or address pattern: text with {param} placeholders, each standing for a
// safe param value (locate.SafeSegment).
type template struct {
	re     *regexp.Regexp
	params []string
}

var placeholder = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// compileTemplate compiles a pattern that matches a whole string, or, for an address, also the
// addresses inside it: module.vm["web01"] owns module.vm["web01"].terraform_data.vm.
func compileTemplate(source string, prefix bool) *template {
	var b strings.Builder
	t := &template{}
	b.WriteString("^")
	last := 0
	for _, m := range placeholder.FindAllStringSubmatchIndex(source, -1) {
		b.WriteString(regexp.QuoteMeta(source[last:m[0]]))
		b.WriteString(`([A-Za-z0-9][A-Za-z0-9_.-]{0,62})`)
		t.params = append(t.params, source[m[2]:m[3]])
		last = m[1]
	}
	b.WriteString(regexp.QuoteMeta(source[last:]))
	if prefix {
		b.WriteString(`(?:$|\.)`)
	} else {
		b.WriteString("$")
	}
	t.re = regexp.MustCompile(b.String())
	return t
}

// match matches s and adds its params to params; a param already there must have the same value.
func (t *template) match(s string, params map[string]string) bool {
	m := t.re.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	for i, name := range t.params {
		v := m[i+1]
		if !locate.SafeSegment(v) {
			return false
		}
		if prev, ok := params[name]; ok && prev != v {
			return false
		}
		params[name] = v
	}
	return true
}
