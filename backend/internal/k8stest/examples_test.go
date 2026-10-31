package k8stest_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/damhau/quai/backend/internal/engine/edit/yamledit"
	"github.com/damhau/quai/backend/internal/k8stest"
)

const kubernetesExample = "../../../examples/kubernetes-repo"

// object is a document of a file of the example.
type object struct {
	file string
	*unstructured.Unstructured
}

// objects reads the YAML documents of the files under dir, in the example, that match keep.
func objects(t *testing.T, dir string, keep func(p string) bool) []object {
	t.Helper()
	var out []object
	fsys := os.DirFS(kubernetesExample)
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !keep(p) {
			return err
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		docs, err := yamledit.Documents(src)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		for _, d := range docs {
			if m, ok := d.Value.(map[string]any); ok {
				out = append(out, object{p, &unstructured.Unstructured{Object: m}})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// claimCRD is the CRD of an XRD's claims, as Crossplane generates it at run time: the XRD's group
// and claim names, namespaced, with the XRD's schema. Crossplane's own fields of a claim's spec
// (compositionSelector, writeConnectionSecretToRef, ...) and its status are kept as they are:
// what the dry run checks is that the claim's parameters conform to the XRD.
func claimCRD(t *testing.T, xrd *unstructured.Unstructured) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	var x struct {
		Spec struct {
			Group      string                                        `json:"group"`
			ClaimNames apiextensionsv1.CustomResourceDefinitionNames `json:"claimNames"`
			Versions   []struct {
				Name   string `json:"name"`
				Served bool   `json:"served"`
				Schema struct {
					OpenAPIV3Schema apiextensionsv1.JSONSchemaProps `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	b, err := json.Marshal(xrd.Object)
	if err == nil {
		err = json.Unmarshal(b, &x)
	}
	if err != nil {
		t.Fatal(err)
	}
	keep := apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: ptr(true)}
	names := x.Spec.ClaimNames
	names.ListKind = names.Kind + "List"
	names.Singular = strings.ToLower(names.Kind)
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: names.Plural + "." + x.Spec.Group},
		Spec:       apiextensionsv1.CustomResourceDefinitionSpec{Group: x.Spec.Group, Names: names, Scope: apiextensionsv1.NamespaceScoped},
	}
	for i, v := range x.Spec.Versions {
		schema := v.Schema.OpenAPIV3Schema
		spec := schema.Properties["spec"]
		for _, f := range []string{"compositionRef", "compositionSelector", "compositionRevisionRef", "compositionRevisionSelector",
			"compositionUpdatePolicy", "writeConnectionSecretToRef", "publishConnectionDetailsTo", "resourceRef"} {
			if f == "compositionUpdatePolicy" {
				spec.Properties[f] = apiextensionsv1.JSONSchemaProps{Type: "string"}
				continue
			}
			spec.Properties[f] = keep
		}
		schema.Properties["spec"] = spec
		schema.Properties["status"] = keep
		crd.Spec.Versions = append(crd.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{
			Name: v.Name, Served: v.Served, Storage: i == 0,
			Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &schema},
		})
	}
	return crd
}

func ptr[T any](v T) *T { return &v }

// TestKubernetesExampleDryRun: the API server, with the CRDs of the components installed, accepts
// every manifest of the Kubernetes example, those of its resources and those its golden examples
// expect, in server-side dry runs.
func TestKubernetesExampleDryRun(t *testing.T) {
	ctx := context.Background()
	cfg := k8stest.Config(t)
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dryRun := func(u *unstructured.Unstructured) error { return c.Create(ctx, u.DeepCopy(), client.DryRunAll) }

	// The XRDs conform to Crossplane's CRD; their claims get the CRDs Crossplane would make.
	var crds []*apiextensionsv1.CustomResourceDefinition
	for _, xrd := range objects(t, "platform/xrds", func(string) bool { return true }) {
		if err := dryRun(xrd.Unstructured); err != nil {
			t.Fatalf("XRD %s: %v", xrd.GetName(), err)
		}
		crds = append(crds, claimCRD(t, xrd.Unstructured))
	}
	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{CRDs: crds}); err != nil {
		t.Fatal(err)
	}
	// A new client, whose REST mapper knows the claims' CRDs.
	if c, err = client.New(cfg, client.Options{}); err != nil {
		t.Fatal(err)
	}

	manifests := objects(t, ".", func(p string) bool { return path.Base(p) == "manifests.yaml" })
	var kinds []string
	namespaces := map[string]bool{}
	for _, u := range manifests {
		if !slices.Contains(kinds, u.GetKind()) {
			kinds = append(kinds, u.GetKind())
		}
		if u.GetKind() == "Namespace" {
			if err := dryRun(u.Unstructured); err != nil {
				t.Errorf("%s: Namespace %s: %v", u.file, u.GetName(), err)
			}
		}
		if ns := u.GetNamespace(); ns != "" {
			namespaces[ns] = true
		}
	}
	slices.Sort(kinds)
	if want := []string{"Application", "ConfigMap", "Kustomization", "Namespace", "PostgreSQLInstance", "ResourceQuota"}; !slices.Equal(kinds, want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
	// The objects' namespaces exist for real, so that their dry runs are checked in them.
	for ns := range namespaces {
		n := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": ns}}}
		if err := c.Create(ctx, n); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	for _, u := range manifests {
		if u.GetKind() == "Namespace" {
			continue
		}
		if err := dryRun(u.Unstructured); err != nil {
			t.Errorf("%s: %s %s: %v", u.file, u.GetKind(), u.GetName(), err)
		}
	}

	// The dry runs check the schemas: a claim outside its XRD's, an Application without a
	// destination and a Kustomization without a source are refused.
	for _, tt := range []struct {
		kind string
		set  func(u *unstructured.Unstructured)
		want string
	}{
		{"PostgreSQLInstance", func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, int64(5), "spec", "parameters", "storageGB")
		}, "spec.parameters.storageGB"},
		{"Application", func(u *unstructured.Unstructured) { unstructured.RemoveNestedField(u.Object, "spec", "destination") }, "spec.destination"},
		{"Kustomization", func(u *unstructured.Unstructured) { unstructured.RemoveNestedField(u.Object, "spec", "sourceRef") }, "spec.sourceRef"},
	} {
		i := slices.IndexFunc(manifests, func(u object) bool { return u.GetKind() == tt.kind })
		u := manifests[i].DeepCopy()
		tt.set(u)
		if err := dryRun(u); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s with %s wrong: err = %v", tt.kind, tt.want, err)
		}
	}
}
