package component

import (
	"reflect"
	"strings"
	"testing"
)

func TestNames(t *testing.T) {
	if got := Names(); !reflect.DeepEqual(got, []string{"argocd.application", "crossplane.claim", "flux.kustomization", "k8s.namespace", "raw"}) {
		t.Errorf("Names = %v", got)
	}
	if Get("kubevirt.vm") != nil {
		t.Error("kubevirt.vm is not built (open question 12)")
	}
}

func TestNamespace(t *testing.T) {
	c := Get("k8s.namespace")
	got, err := c.Render(map[string]any{
		"name":   "shop",
		"labels": map[string]any{"team": "shop"},
		"quota":  map[string]any{"requests.cpu": "8", "requests.memory": "16Gi", "pods": int64(20), "limits.cpu": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "shop", "labels": map[string]any{"team": "shop"}}},
		{"apiVersion": "v1", "kind": "ResourceQuota", "metadata": map[string]any{"name": "quota", "namespace": "shop"},
			"spec": map[string]any{"hard": map[string]any{"requests.cpu": "8", "requests.memory": "16Gi", "pods": int64(20)}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Render =\n%#v\nwant\n%#v", got, want)
	}

	// Without a quota, or with an empty one, only the namespace.
	for _, q := range []any{nil, map[string]any{}} {
		props := map[string]any{"name": "shop"}
		if q != nil {
			props["quota"] = q
		}
		got, err := c.Render(props)
		if err != nil || len(got) != 1 || Key(got[0]) != "Namespace shop" {
			t.Errorf("quota %v: Render = %v, %v", q, got, err)
		}
	}
}

func TestRaw(t *testing.T) {
	cm := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "settings", "namespace": "shop"},
		"data": map[string]any{"greeting": "hello"}}
	in := map[string]any{"manifests": []any{cm}}
	got, err := Get("raw").Render(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []map[string]any{cm}) || Key(got[0]) != "ConfigMap shop/settings" {
		t.Errorf("Render = %#v", got)
	}
	got[0]["data"].(map[string]any)["greeting"] = "changed"
	if cm["data"].(map[string]any)["greeting"] != "hello" {
		t.Error("the rendered manifest shares its values with the properties")
	}
}

func TestClaim(t *testing.T) {
	got, err := Get("crossplane.claim").Render(map[string]any{
		"apiVersion": "database.example.org/v1alpha1", "kind": "PostgreSQLInstance", "name": "orders", "namespace": "shop",
		"labels": map[string]any{"team": "shop"},
		"spec": map[string]any{"parameters": map[string]any{"storageGB": int64(20)},
			"compositionSelector": map[string]any{"matchLabels": map[string]any{"provider": "aws"}}},
	})
	want := []map[string]any{{"apiVersion": "database.example.org/v1alpha1", "kind": "PostgreSQLInstance",
		"metadata": map[string]any{"name": "orders", "namespace": "shop", "labels": map[string]any{"team": "shop"}},
		"spec": map[string]any{"parameters": map[string]any{"storageGB": int64(20)},
			"compositionSelector": map[string]any{"matchLabels": map[string]any{"provider": "aws"}}}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Render = %#v, %v", got, err)
	}
	// Without a spec, none; a null property is not set.
	got, err = Get("crossplane.claim").Render(map[string]any{"apiVersion": "a.b/v1", "kind": "Bucket", "name": "logs", "namespace": "shop", "spec": nil})
	if err != nil || len(got) != 1 || got[0]["spec"] != nil {
		t.Errorf("Render = %#v, %v", got, err)
	}
}

func TestApplication(t *testing.T) {
	c := Get("argocd.application")
	got, err := c.Render(map[string]any{"name": "shop-web", "repoURL": "https://git.example.com/shop/web.git", "path": "deploy",
		"destinationNamespace": "shop", "automated": true, "syncOptions": []any{"CreateNamespace=true"}, "cascadeDelete": true})
	want := []map[string]any{{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "shop-web", "namespace": "argocd", "finalizers": []any{"resources-finalizer.argocd.argoproj.io"}},
		"spec": map[string]any{
			"project":     "default",
			"source":      map[string]any{"repoURL": "https://git.example.com/shop/web.git", "path": "deploy", "targetRevision": "HEAD"},
			"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "shop"},
			"syncPolicy":  map[string]any{"automated": map[string]any{"prune": true, "selfHeal": true}, "syncOptions": []any{"CreateNamespace=true"}},
		}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Render =\n%#v\n%v", got, err)
	}

	// A chart, its version and values, on a cluster Argo CD knows by name.
	got, err = c.Render(map[string]any{"name": "redis", "namespace": "gitops", "project": "shop", "repoURL": "https://charts.example.com",
		"chart": "redis", "targetRevision": "19.6.0", "values": map[string]any{"replicas": int64(3)}, "cluster": "prod-1"})
	if err != nil {
		t.Fatal(err)
	}
	spec := got[0]["spec"].(map[string]any)
	if !reflect.DeepEqual(spec["source"], map[string]any{"repoURL": "https://charts.example.com", "chart": "redis", "targetRevision": "19.6.0",
		"helm": map[string]any{"valuesObject": map[string]any{"replicas": int64(3)}}}) ||
		!reflect.DeepEqual(spec["destination"], map[string]any{"name": "prod-1"}) || spec["project"] != "shop" || spec["syncPolicy"] != nil ||
		got[0]["metadata"].(map[string]any)["namespace"] != "gitops" {
		t.Errorf("Render = %#v", got)
	}
}

func TestFluxKustomization(t *testing.T) {
	c := Get("flux.kustomization")
	got, err := c.Render(map[string]any{"name": "shop", "source": "platform"})
	want := []map[string]any{{"apiVersion": "kustomize.toolkit.fluxcd.io/v1", "kind": "Kustomization",
		"metadata": map[string]any{"name": "shop", "namespace": "flux-system"},
		"spec":     map[string]any{"interval": "10m", "prune": true, "sourceRef": map[string]any{"kind": "GitRepository", "name": "platform"}}}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Render = %#v, %v", got, err)
	}
	got, err = c.Render(map[string]any{"name": "shop", "namespace": "gitops", "sourceKind": "OCIRepository", "source": "manifests",
		"sourceNamespace": "flux-system", "path": "./shop", "interval": "1h", "prune": false, "targetNamespace": "shop",
		"dependsOn": []any{"infra"}, "wait": true, "timeout": "5m", "substitute": map[string]any{"cluster": "prod-1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[0]["spec"], map[string]any{"interval": "1h", "prune": false, "path": "./shop", "targetNamespace": "shop",
		"sourceRef": map[string]any{"kind": "OCIRepository", "name": "manifests", "namespace": "flux-system"},
		"dependsOn": []any{map[string]any{"name": "infra"}}, "wait": true, "timeout": "5m",
		"postBuild": map[string]any{"substitute": map[string]any{"cluster": "prod-1"}}}) {
		t.Errorf("spec = %#v", got[0]["spec"])
	}
}

func TestRenderErrors(t *testing.T) {
	cm := func(name string) map[string]any {
		return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name}}
	}
	for _, tt := range []struct {
		component string
		props     map[string]any
		err       string
	}{
		{"k8s.namespace", map[string]any{}, "with.name is required"},
		{"k8s.namespace", map[string]any{"name": "Shop"}, "with.name must be a DNS label"},
		{"k8s.namespace", map[string]any{"name": "shop", "size": "s"}, "with.size is not one of its properties (name, labels, annotations, quota)"},
		{"k8s.namespace", map[string]any{"name": "shop", "labels": map[string]any{"n": int64(1)}}, "with.labels.n must be a string"},
		{"k8s.namespace", map[string]any{"name": "shop", "labels": "team"}, "with.labels must be a mapping of strings"},
		{"k8s.namespace", map[string]any{"name": "shop", "quota": map[string]any{"cpu": []any{}}}, "with.quota.cpu must be a quantity"},
		{"k8s.namespace", map[string]any{"name": "shop", "quota": "8"}, "with.quota must be a mapping"},
		{"raw", map[string]any{"manifests": []any{}}, "with.manifests must be a list of one or more manifests"},
		{"raw", map[string]any{"manifests": []any{"x"}}, "with.manifests.0 must be a mapping"},
		{"raw", map[string]any{"manifests": []any{map[string]any{"kind": "ConfigMap"}}}, "with.manifests.0.apiVersion must be a non-empty string"},
		{"raw", map[string]any{"manifests": []any{map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}}}, "with.manifests.0.metadata.name"},
		{"raw", map[string]any{"manifests": []any{map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "a", "namespace": ""}}}},
			"with.manifests.0.metadata.namespace"},
		{"raw", map[string]any{"manifests": []any{cm("a"), cm("a")}}, "two manifests are ConfigMap a"},
		{"k8s.namespace", map[string]any{"name": nil}, "with.name is required"},
		{"crossplane.claim", map[string]any{"apiVersion": "v1", "kind": "X", "name": "a", "namespace": "b"}, "with.apiVersion must be the XRD's group and version"},
		{"crossplane.claim", map[string]any{"apiVersion": "a.b/v1", "kind": "postgres", "name": "a", "namespace": "b"}, "with.kind must be the XRD's kind"},
		{"crossplane.claim", map[string]any{"apiVersion": "a.b/v1", "kind": "X", "name": "A", "namespace": "b"}, "with.name must be a DNS subdomain"},
		{"crossplane.claim", map[string]any{"apiVersion": "a.b/v1", "kind": "X", "name": "a", "namespace": "b.c"}, "with.namespace must be a DNS label"},
		{"crossplane.claim", map[string]any{"apiVersion": "a.b/v1", "kind": "X", "name": "a", "namespace": "b", "spec": "x"}, "with.spec must be a mapping"},
		{"argocd.application", map[string]any{"name": "a", "repoURL": "r"}, "with.path or with.chart is required"},
		{"argocd.application", map[string]any{"name": "a", "repoURL": "r", "path": "p", "chart": "c"}, "cannot both be set"},
		{"argocd.application", map[string]any{"name": "a", "repoURL": "r", "chart": "c"}, "with.targetRevision is required with a chart"},
		{"argocd.application", map[string]any{"name": "a", "repoURL": "r", "path": "p", "server": "s", "cluster": "c"}, "with.server and with.cluster"},
		{"argocd.application", map[string]any{"name": "a", "repoURL": "r", "path": "p", "server": "http://k"}, "with.server must be an https:// URL"},
		{"argocd.application", map[string]any{"name": "a", "repoURL": "", "path": "p"}, "with.repoURL must be a non-empty string"},
		{"argocd.application", map[string]any{"name": "a", "repoURL": "r", "path": "p", "automated": "yes"}, "with.automated must be true or false"},
		{"argocd.application", map[string]any{"name": "a", "repoURL": "r", "path": "p", "syncOptions": []any{int64(1)}}, "with.syncOptions.0 must be a non-empty string"},
		{"flux.kustomization", map[string]any{"name": "a", "source": "s", "sourceKind": "HelmRepository"}, "with.sourceKind must be one of GitRepository, OCIRepository, Bucket"},
		{"flux.kustomization", map[string]any{"name": "a", "source": "s", "interval": "ten minutes"}, "with.interval must be a duration"},
		{"flux.kustomization", map[string]any{"name": "a", "source": "s", "timeout": "5"}, "with.timeout must be a duration"},
		{"flux.kustomization", map[string]any{"name": "a", "source": "s", "dependsOn": "infra"}, "with.dependsOn must be a list of strings"},
		{"flux.kustomization", map[string]any{"name": "a", "source": "s", "substitute": map[string]any{"n": int64(1)}}, "with.substitute.n must be a string"},
	} {
		_, err := Get(tt.component).Render(tt.props)
		if err == nil || !strings.Contains(err.Error(), tt.err) || !strings.HasPrefix(err.Error(), "component "+tt.component+": ") {
			t.Errorf("%s %v: err = %v, want %q", tt.component, tt.props, err, tt.err)
		}
	}
}

func TestKey(t *testing.T) {
	for _, tt := range []struct {
		doc  map[string]any
		want string
	}{
		{map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "shop"}}, "Namespace shop"},
		{map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s", "namespace": "shop"}}, "Secret shop/s"},
		{map[string]any{"kind": "Namespace", "metadata": map[string]any{"name": "shop"}}, ""},
		{map[string]any{"apiVersion": "v1", "metadata": map[string]any{"name": "shop"}}, ""},
		{map[string]any{"apiVersion": "v1", "kind": "Namespace"}, ""},
	} {
		if got := Key(tt.doc); got != tt.want {
			t.Errorf("Key(%v) = %q, want %q", tt.doc, got, tt.want)
		}
	}
}
