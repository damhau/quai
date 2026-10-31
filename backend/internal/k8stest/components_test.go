package k8stest_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/damhau/quai/backend/internal/engine/edit/yamledit"
	"github.com/damhau/quai/backend/internal/engine/generate/component"
	"github.com/damhau/quai/backend/internal/k8stest"
)

func TestMain(m *testing.M) { os.Exit(k8stest.Main(m)) }

// manifests renders a component and reads its manifests back from the file Quai writes.
func manifests(t *testing.T, name string, props map[string]any) []*unstructured.Unstructured {
	t.Helper()
	rendered, err := component.Get(name).Render(props)
	if err != nil {
		t.Fatal(err)
	}
	docs := make([]any, len(rendered))
	for i, m := range rendered {
		docs[i] = m
	}
	src, err := yamledit.CreateDocuments(docs, component.KeyOrder)
	if err != nil {
		t.Fatal(err)
	}
	read, err := yamledit.Documents(src)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]*unstructured.Unstructured, len(read))
	for i, d := range read {
		out[i] = &unstructured.Unstructured{Object: d.Value.(map[string]any)}
	}
	return out
}

// TestComponentsDryRun: the API server accepts what each built component renders, in a
// server-side dry run (docs/design/02, component).
func TestComponentsDryRun(t *testing.T) {
	ctx := context.Background()
	c, err := client.New(k8stest.Config(t), client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dryRun := func(u *unstructured.Unstructured) error { return c.Create(ctx, u.DeepCopy(), client.DryRunAll) }

	ns := manifests(t, "k8s.namespace", map[string]any{
		"name":        "quai-shop",
		"labels":      map[string]any{"team": "shop", "app.kubernetes.io/managed-by": "quai"},
		"annotations": map[string]any{"quai.dev/owner": "alice"},
		"quota":       map[string]any{"requests.cpu": "8", "requests.memory": "16Gi", "limits.memory": "32Gi", "pods": int64(20)},
	})
	if len(ns) != 2 {
		t.Fatalf("k8s.namespace rendered %d manifests", len(ns))
	}
	if err := dryRun(ns[0]); err != nil {
		t.Fatalf("Namespace: %v", err)
	}
	// The quota's namespace must exist for its dry run: create it for real.
	if err := c.Create(ctx, ns[0].DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if err := dryRun(ns[1]); err != nil {
		t.Errorf("ResourceQuota: %v", err)
	}

	raw := manifests(t, "raw", map[string]any{"manifests": []any{
		map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "greeting", "namespace": "quai-shop"},
			"data": map[string]any{"message": "hello"}},
		map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "web", "namespace": "quai-shop"},
			"spec": map[string]any{
				"replicas": int64(2),
				"selector": map[string]any{"matchLabels": map[string]any{"app": "web"}},
				"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"app": "web"}},
					"spec": map[string]any{"containers": []any{map[string]any{
						"name": "web", "image": "registry.example.com/web:1.0",
						"resources": map[string]any{"requests": map[string]any{"cpu": "250m", "memory": "128Mi"}},
					}}},
				},
			}},
		map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": map[string]any{"name": "quai-shop-reader"},
			"rules": []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"configmaps"}, "verbs": []any{"get", "list"}}}},
	}})
	for _, u := range raw {
		if err := dryRun(u); err != nil {
			t.Errorf("%s %s: %v", u.GetKind(), u.GetName(), err)
		}
	}

	// The dry run checks what it is given: a quota of no quantity is refused.
	bad := manifests(t, "k8s.namespace", map[string]any{"name": "quai-shop", "quota": map[string]any{"requests.cpu": "lots"}})
	if err := dryRun(bad[1]); err == nil || !strings.Contains(err.Error(), "quantities must match") {
		t.Errorf("a quota of no quantity: err = %v", err)
	}
}
