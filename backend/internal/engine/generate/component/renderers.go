package component

import (
	"fmt"
	"maps"
	"slices"
)

func init() {
	register(&Component{Name: "raw", Props: []string{"manifests"}, Required: []string{"manifests"}, render: raw})
	register(&Component{Name: "k8s.namespace", Props: []string{"name", "labels", "annotations", "quota"}, Required: []string{"name"}, render: namespace})
}

// raw renders the manifests `with.manifests` lists, as they are: any Kubernetes objects, for when
// no other component fits. Bindings work in every field, as in any `with`.
func raw(props map[string]any) ([]map[string]any, error) {
	list, ok := props["manifests"].([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("with.manifests must be a list of one or more manifests")
	}
	out := make([]map[string]any, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("with.manifests.%d must be a mapping", i)
		}
		for _, k := range []string{"apiVersion", "kind"} {
			if s, _ := m[k].(string); s == "" {
				return nil, fmt.Errorf("with.manifests.%d.%s must be a non-empty string", i, k)
			}
		}
		meta, _ := m["metadata"].(map[string]any)
		if s, _ := meta["name"].(string); s == "" {
			return nil, fmt.Errorf("with.manifests.%d.metadata.name must be a non-empty string", i)
		}
		if ns, ok := meta["namespace"]; ok {
			if s, _ := ns.(string); s == "" {
				return nil, fmt.Errorf("with.manifests.%d.metadata.namespace must be a non-empty string", i)
			}
		}
		out = append(out, clone(m).(map[string]any))
	}
	return out, nil
}

// namespace renders a Namespace, with its labels and annotations, and, when `with.quota` is set,
// a ResourceQuota named quota in it whose hard limits are the quota's entries, such as
// requests.cpu: "8".
func namespace(props map[string]any) ([]map[string]any, error) {
	name, _ := props["name"].(string)
	if !dnsLabel.MatchString(name) {
		return nil, fmt.Errorf("with.name must be a DNS label (lowercase letters, digits and '-', at most 63), not %q", name)
	}
	meta := map[string]any{"name": name}
	for _, k := range []string{"labels", "annotations"} {
		m, err := stringMap(props, k)
		if err != nil {
			return nil, err
		}
		if len(m) > 0 {
			meta[k] = m
		}
	}
	out := []map[string]any{{"apiVersion": "v1", "kind": "Namespace", "metadata": meta}}

	switch q := props["quota"].(type) {
	case nil:
	case map[string]any:
		hard := map[string]any{}
		for _, k := range slices.Sorted(maps.Keys(q)) {
			switch v := q[k].(type) {
			case string, int64, uint64, float64, int:
				hard[k] = v
			case nil:
				// A limit left out of the form is no limit.
			default:
				return nil, fmt.Errorf("with.quota.%s must be a quantity, such as \"8\" or 16Gi", k)
			}
		}
		if len(hard) > 0 {
			out = append(out, map[string]any{
				"apiVersion": "v1", "kind": "ResourceQuota",
				"metadata": map[string]any{"name": "quota", "namespace": name},
				"spec":     map[string]any{"hard": hard},
			})
		}
	default:
		return nil, fmt.Errorf("with.quota must be a mapping of resource names to quantities")
	}
	return out, nil
}
