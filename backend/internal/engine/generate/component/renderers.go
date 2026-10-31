package component

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

func init() {
	register(&Component{Name: "raw", Props: []string{"manifests"}, Required: []string{"manifests"}, render: raw})
	register(&Component{Name: "k8s.namespace", Props: []string{"name", "labels", "annotations", "quota"}, Required: []string{"name"}, render: namespace})
	register(&Component{Name: "crossplane.claim",
		Props:    []string{"apiVersion", "kind", "name", "namespace", "labels", "annotations", "spec"},
		Required: []string{"apiVersion", "kind", "name", "namespace"}, render: claim})
	register(&Component{Name: "argocd.application",
		Props: []string{"name", "namespace", "labels", "annotations", "project", "repoURL", "targetRevision", "path", "chart", "values",
			"server", "cluster", "destinationNamespace", "automated", "syncOptions", "cascadeDelete"},
		Required: []string{"name", "repoURL"}, render: application})
	register(&Component{Name: "flux.kustomization",
		Props: []string{"name", "namespace", "labels", "annotations", "sourceKind", "source", "sourceNamespace", "path", "interval",
			"prune", "targetNamespace", "dependsOn", "wait", "timeout", "substitute"},
		Required: []string{"name", "source"}, render: kustomization})
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
	n, err := name(props, "name", "", false)
	if err != nil {
		return nil, err
	}
	meta, err := metadata(props, n, "")
	if err != nil {
		return nil, err
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
				"metadata": map[string]any{"name": "quota", "namespace": n},
				"spec":     map[string]any{"hard": hard},
			})
		}
	default:
		return nil, fmt.Errorf("with.quota must be a mapping of resource names to quantities")
	}
	return out, nil
}

var (
	// customAPIVersion is a custom resource's group/version: a group with a dot, such as
	// database.example.org/v1alpha1.
	customAPIVersion = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+/v[0-9]+((alpha|beta)[0-9]+)?$`)
	kindName         = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	// duration is a Go duration as Flux reads it, such as 10m or 1h30m.
	duration = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$`)
)

// claim renders a Crossplane claim of any XRD: the kind and the group/version the XRD names, in
// a namespace, with `with.spec` as its spec (the XRD's parameters, and Crossplane's fields such
// as compositionSelector). With Crossplane 2 it renders a namespaced composite resource the same
// way.
func claim(props map[string]any) ([]map[string]any, error) {
	api, _ := props["apiVersion"].(string)
	if !customAPIVersion.MatchString(api) {
		return nil, fmt.Errorf("with.apiVersion must be the XRD's group and version, such as database.example.org/v1alpha1, not %q", api)
	}
	kind, _ := props["kind"].(string)
	if !kindName.MatchString(kind) {
		return nil, fmt.Errorf("with.kind must be the XRD's kind, such as PostgreSQLInstance, not %q", kind)
	}
	n, err := name(props, "name", "", true)
	if err != nil {
		return nil, err
	}
	ns, err := name(props, "namespace", "", false)
	if err != nil {
		return nil, err
	}
	meta, err := metadata(props, n, ns)
	if err != nil {
		return nil, err
	}
	m := map[string]any{"apiVersion": api, "kind": kind, "metadata": meta}
	spec, err := mapping(props, "spec")
	if err != nil {
		return nil, err
	}
	if len(spec) > 0 {
		m["spec"] = spec
	}
	return []map[string]any{m}, nil
}

// application renders an Argo CD Application: a source (a path in a Git repository, or a Helm
// chart and its version, with values), a destination (the cluster Argo CD runs in unless server
// or cluster names another), and, with automated, a sync policy that prunes and heals.
func application(props map[string]any) ([]map[string]any, error) {
	n, err := name(props, "name", "", true)
	if err != nil {
		return nil, err
	}
	ns, err := name(props, "namespace", "argocd", false)
	if err != nil {
		return nil, err
	}
	meta, err := metadata(props, n, ns)
	if err != nil {
		return nil, err
	}
	project, err := str(props, "project", "default")
	if err != nil {
		return nil, err
	}
	repo, err := str(props, "repoURL", "")
	if err != nil {
		return nil, err
	}
	source := map[string]any{"repoURL": repo}
	path, chart := props["path"], props["chart"]
	switch {
	case path != nil && chart != nil:
		return nil, fmt.Errorf("with.path and with.chart cannot both be set: a source is a path in a repository or a chart")
	case path == nil && chart == nil:
		return nil, fmt.Errorf("with.path or with.chart is required: a path in a repository, or a Helm chart")
	case chart != nil:
		if source["chart"], err = str(props, "chart", ""); err != nil {
			return nil, err
		}
		if _, ok := props["targetRevision"]; !ok {
			return nil, fmt.Errorf("with.targetRevision is required with a chart: its version")
		}
	default:
		if source["path"], err = str(props, "path", ""); err != nil {
			return nil, err
		}
	}
	if source["targetRevision"], err = str(props, "targetRevision", "HEAD"); err != nil {
		return nil, err
	}
	values, err := mapping(props, "values")
	if err != nil {
		return nil, err
	}
	if len(values) > 0 {
		source["helm"] = map[string]any{"valuesObject": values}
	}

	destination := map[string]any{}
	switch server, cluster := props["server"], props["cluster"]; {
	case server != nil && cluster != nil:
		return nil, fmt.Errorf("with.server and with.cluster cannot both be set: a destination is a server's URL or a cluster's name")
	case cluster != nil:
		if destination["name"], err = str(props, "cluster", ""); err != nil {
			return nil, err
		}
	default:
		s, err := str(props, "server", "https://kubernetes.default.svc")
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(s, "https://") {
			return nil, fmt.Errorf("with.server must be an https:// URL, not %q", s)
		}
		destination["server"] = s
	}
	if _, ok := props["destinationNamespace"]; ok {
		if destination["namespace"], err = name(props, "destinationNamespace", "", false); err != nil {
			return nil, err
		}
	}

	spec := map[string]any{"project": project, "source": source, "destination": destination}
	sync := map[string]any{}
	automated, err := boolean(props, "automated", false)
	if err != nil {
		return nil, err
	}
	if automated {
		sync["automated"] = map[string]any{"prune": true, "selfHeal": true}
	}
	options, err := stringList(props, "syncOptions")
	if err != nil {
		return nil, err
	}
	if len(options) > 0 {
		sync["syncOptions"] = options
	}
	if len(sync) > 0 {
		spec["syncPolicy"] = sync
	}
	cascade, err := boolean(props, "cascadeDelete", false)
	if err != nil {
		return nil, err
	}
	if cascade {
		// Deleting the Application deletes what it deployed.
		meta["finalizers"] = []any{"resources-finalizer.argocd.argoproj.io"}
	}
	return []map[string]any{{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "metadata": meta, "spec": spec}}, nil
}

// fluxSourceKinds are the sources a Flux Kustomization reads.
var fluxSourceKinds = []string{"GitRepository", "OCIRepository", "Bucket"}

// kustomization renders a Flux Kustomization: the path in a source that Flux applies, every
// interval, pruning what leaves it unless prune is false.
func kustomization(props map[string]any) ([]map[string]any, error) {
	n, err := name(props, "name", "", true)
	if err != nil {
		return nil, err
	}
	ns, err := name(props, "namespace", "flux-system", false)
	if err != nil {
		return nil, err
	}
	meta, err := metadata(props, n, ns)
	if err != nil {
		return nil, err
	}
	kind, err := str(props, "sourceKind", "GitRepository")
	if err != nil {
		return nil, err
	}
	if !slices.Contains(fluxSourceKinds, kind) {
		return nil, fmt.Errorf("with.sourceKind must be one of %s, not %q", strings.Join(fluxSourceKinds, ", "), kind)
	}
	source, err := name(props, "source", "", true)
	if err != nil {
		return nil, err
	}
	ref := map[string]any{"kind": kind, "name": source}
	if _, ok := props["sourceNamespace"]; ok {
		if ref["namespace"], err = name(props, "sourceNamespace", "", false); err != nil {
			return nil, err
		}
	}
	interval, err := str(props, "interval", "10m")
	if err != nil {
		return nil, err
	}
	if !duration.MatchString(interval) {
		return nil, fmt.Errorf("with.interval must be a duration, such as 10m or 1h, not %q", interval)
	}
	prune, err := boolean(props, "prune", true)
	if err != nil {
		return nil, err
	}
	spec := map[string]any{"interval": interval, "prune": prune, "sourceRef": ref}
	if _, ok := props["path"]; ok {
		if spec["path"], err = str(props, "path", ""); err != nil {
			return nil, err
		}
	}
	if _, ok := props["targetNamespace"]; ok {
		if spec["targetNamespace"], err = name(props, "targetNamespace", "", false); err != nil {
			return nil, err
		}
	}
	deps, err := stringList(props, "dependsOn")
	if err != nil {
		return nil, err
	}
	if len(deps) > 0 {
		refs := make([]any, len(deps))
		for i, d := range deps {
			refs[i] = map[string]any{"name": d}
		}
		spec["dependsOn"] = refs
	}
	if _, ok := props["wait"]; ok {
		if spec["wait"], err = boolean(props, "wait", false); err != nil {
			return nil, err
		}
	}
	if _, ok := props["timeout"]; ok {
		t, err := str(props, "timeout", "")
		if err != nil {
			return nil, err
		}
		if !duration.MatchString(t) {
			return nil, fmt.Errorf("with.timeout must be a duration, such as 5m, not %q", t)
		}
		spec["timeout"] = t
	}
	substitute, err := stringMap(props, "substitute")
	if err != nil {
		return nil, err
	}
	if len(substitute) > 0 {
		spec["postBuild"] = map[string]any{"substitute": substitute}
	}
	return []map[string]any{{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization", "metadata": meta, "spec": spec}}, nil
}
