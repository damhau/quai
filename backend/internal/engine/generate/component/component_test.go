package component

import (
	"reflect"
	"strings"
	"testing"
)

func TestNames(t *testing.T) {
	if got := Names(); !reflect.DeepEqual(got, []string{"k8s.namespace", "raw"}) {
		t.Errorf("Names = %v", got)
	}
	if Get("kubevirt.vm") != nil || Get("crossplane.claim") != nil {
		t.Error("only raw and k8s.namespace are built")
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
