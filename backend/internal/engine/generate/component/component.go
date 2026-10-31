// Package component holds the built-in renderers of the component generator
// (docs/design/02-resource-types-and-generators.md#component-built-in-renderers-for-kubernetes):
// each turns its properties, the evaluated `with` of a type, into Kubernetes manifests. The
// generator writes them into the resource's directory as one multi-document file, with a
// kustomization.yaml that lists it and the quai.yaml record of the values.
//
// Rendering is pure: the same properties give the same manifests.
package component

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Files a component writes in its directory, besides the record (locate.RecordFile).
const (
	Manifests     = "manifests.yaml"
	Kustomization = "kustomization.yaml"
)

// KeyOrder is the order of the keys of rendered manifests: Kubernetes' usual one, then the
// order of their upstream documentation for a Flux Kustomization's spec (interval, path, then
// sorted), an Argo CD Application's and a claim's (parameters first); the others follow in sorted
// order.
var KeyOrder = []string{"apiVersion", "kind", "metadata", "name", "namespace", "labels", "annotations", "finalizers", "spec",
	"interval", "project", "source", "repoURL", "targetRevision", "chart", "path", "helm", "destination", "server", "syncPolicy",
	"parameters"}

// Component is a built-in renderer.
type Component struct {
	Name string
	// Props are the properties `with` may set; Required those it must.
	Props    []string
	Required []string
	render   func(props map[string]any) ([]map[string]any, error)
}

var components = map[string]*Component{}

func register(c *Component) { components[c.Name] = c }

// Get returns the component with that name, or nil when this version does not have it.
func Get(name string) *Component { return components[name] }

// Names returns the components this version has, sorted.
func Names() []string { return slices.Sorted(maps.Keys(components)) }

// Render returns the manifests for props: each a mapping with an apiVersion, a kind and a
// metadata.name, no two with the same kind, namespace and name. Errors name the property at
// fault, under `with`.
func (c *Component) Render(props map[string]any) ([]map[string]any, error) {
	// A property whose value is null is not set: `${{ inputs.?x }}` leaves it out when the form
	// does.
	props = maps.Clone(props)
	maps.DeleteFunc(props, func(_ string, v any) bool { return v == nil })
	for _, k := range slices.Sorted(maps.Keys(props)) {
		if !slices.Contains(c.Props, k) {
			return nil, fmt.Errorf("component %s: with.%s is not one of its properties (%s)", c.Name, k, strings.Join(c.Props, ", "))
		}
	}
	for _, k := range c.Required {
		if _, ok := props[k]; !ok {
			return nil, fmt.Errorf("component %s: with.%s is required", c.Name, k)
		}
	}
	out, err := c.render(props)
	if err != nil {
		return nil, fmt.Errorf("component %s: %w", c.Name, err)
	}
	seen := map[string]bool{}
	for i, m := range out {
		k := Key(m)
		if k == "" {
			return nil, fmt.Errorf("component %s: manifest %d needs an apiVersion, a kind and a metadata.name", c.Name, i)
		}
		if seen[k] {
			return nil, fmt.Errorf("component %s: two manifests are %s", c.Name, k)
		}
		seen[k] = true
	}
	return out, nil
}

// Key addresses a manifest in its file: its kind and its name, after its namespace if it has
// one, such as "ResourceQuota shop/quota"; "" when it has no apiVersion, kind or name.
func Key(doc any) string {
	m, _ := doc.(map[string]any)
	api, _ := m["apiVersion"].(string)
	kind, _ := m["kind"].(string)
	meta, _ := m["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if api == "" || kind == "" || name == "" {
		return ""
	}
	if ns, _ := meta["namespace"].(string); ns != "" {
		name = ns + "/" + name
	}
	return kind + " " + name
}

// KustomizationDoc is the kustomization.yaml of a component's directory: it lists the manifests,
// and not the record, so that Argo CD, Flux and kubectl apply the manifests only.
func KustomizationDoc() map[string]any {
	return map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"resources":  []any{Manifests},
	}
}

// KustomizationOrder is the order of a kustomization.yaml's keys.
var KustomizationOrder = []string{"apiVersion", "kind", "resources"}

// dnsLabel is a Kubernetes name that must be a DNS label (RFC 1123), such as a namespace's.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// dnsSubdomain is a Kubernetes name that must be a DNS subdomain (RFC 1123), as most objects'
// names: DNS labels joined by dots, at most 253 characters.
var dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// name returns the name at prop, checked to be a DNS label or, with subdomain, a DNS subdomain;
// def when the prop is not set and def is not "".
func name(props map[string]any, prop, def string, subdomain bool) (string, error) {
	v, ok := props[prop]
	if !ok && def != "" {
		return def, nil
	}
	s, _ := v.(string)
	switch {
	case subdomain && (len(s) > 253 || !dnsSubdomain.MatchString(s)):
		return "", fmt.Errorf("with.%s must be a DNS subdomain (lowercase letters, digits, '-' and '.', at most 253), not %q", prop, s)
	case !subdomain && !dnsLabel.MatchString(s):
		return "", fmt.Errorf("with.%s must be a DNS label (lowercase letters, digits and '-', at most 63), not %q", prop, s)
	}
	return s, nil
}

// str returns the non-empty string at prop, def when it is not set.
func str(props map[string]any, prop, def string) (string, error) {
	v, ok := props[prop]
	if !ok {
		return def, nil
	}
	if s, _ := v.(string); s != "" {
		return s, nil
	}
	return "", fmt.Errorf("with.%s must be a non-empty string", prop)
}

// boolean returns the boolean at prop, def when it is not set.
func boolean(props map[string]any, prop string, def bool) (bool, error) {
	v, ok := props[prop]
	if !ok {
		return def, nil
	}
	if b, ok := v.(bool); ok {
		return b, nil
	}
	return false, fmt.Errorf("with.%s must be true or false", prop)
}

// mapping returns a copy of the mapping at prop, nil when it is not set.
func mapping(props map[string]any, prop string) (map[string]any, error) {
	v, ok := props[prop]
	if !ok {
		return nil, nil
	}
	if m, ok := v.(map[string]any); ok {
		return clone(m).(map[string]any), nil
	}
	return nil, fmt.Errorf("with.%s must be a mapping", prop)
}

// stringList returns the list of non-empty strings at prop, nil when it is not set.
func stringList(props map[string]any, prop string) ([]any, error) {
	v, ok := props[prop]
	if !ok {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("with.%s must be a list of strings", prop)
	}
	for i, x := range list {
		if s, _ := x.(string); s == "" {
			return nil, fmt.Errorf("with.%s.%d must be a non-empty string", prop, i)
		}
	}
	return slices.Clone(list), nil
}

// metadata returns an object's metadata: its name, its namespace unless it is "", and the labels
// and annotations props set.
func metadata(props map[string]any, name, namespace string) (map[string]any, error) {
	meta := map[string]any{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	for _, k := range []string{"labels", "annotations"} {
		m, err := stringMap(props, k)
		if err != nil {
			return nil, err
		}
		if len(m) > 0 {
			meta[k] = m
		}
	}
	return meta, nil
}

// stringMap returns the map of strings at prop, nil when it is not set, or an error naming it.
func stringMap(props map[string]any, prop string) (map[string]any, error) {
	v, ok := props[prop]
	if !ok || v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("with.%s must be a mapping of strings", prop)
	}
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if _, ok := m[k].(string); !ok {
			return nil, fmt.Errorf("with.%s.%s must be a string", prop, k)
		}
	}
	return clone(m).(map[string]any), nil
}

func clone(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = clone(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = clone(x)
		}
		return out
	}
	return v
}
