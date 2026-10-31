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

// KeyOrder is the order of the keys of rendered manifests, Kubernetes' usual one; the others
// follow in sorted order.
var KeyOrder = []string{"apiVersion", "kind", "metadata", "name", "namespace", "labels", "annotations", "spec"}

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
