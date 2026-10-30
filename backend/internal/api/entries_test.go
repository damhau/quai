package api

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pushFiles commits files to the harness's repository, by path, and pushes them to main.
func (h *harness) pushFiles(files map[string]string) {
	h.t.Helper()
	for p, content := range files {
		full := filepath.Join(h.work, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
	git(h.t, h.work, "add", "--all")
	git(h.t, h.work, "commit", "--quiet", "-m", "more files")
	git(h.t, h.work, "push", "--quiet", "origin", "HEAD:main")
}

const namespaceType = `apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: namespace, title: Namespace }
spec:
  locate: { strategy: entry, path: "clusters/{cluster}/namespaces.yaml", collection: namespaces, key: name, keyParam: name }
  params:
    - { name: cluster, title: Cluster, type: enum, discover: true }
    - { name: name, title: Name }
  schema: catalog/schemas/namespace.schema.json
  generate: { kind: identity }
`

const namespaceSchema = `{"type": "object", "required": ["name"], "additionalProperties": false,
  "properties": {"name": {"type": "string"}, "quota": {"type": "object", "properties": {"cpu": {"type": "string"}, "memory": {"type": "string"}}}}}`

// TestEntryResources: the entries of a shared file are resources (docs/design/02, entry): the
// inventory lists them, a resource's page shows its value and its own bytes, and the entries of
// a file whose keys repeat are non-conforming.
func TestEntryResources(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pushFiles(map[string]string{
		".quai/types/namespace.yaml":            namespaceType,
		"catalog/schemas/namespace.schema.json": namespaceSchema,
		"clusters/prod-1/namespaces.yaml":       "# Namespaces on prod-1.\nnamespaces:\n  - name: shop\n    quota: { cpu: \"8\", memory: 16Gi }\n  - name: orders\n    quota: { cpu: \"4\", memory: 8Gi }\n",
		"clusters/prod-2/namespaces.yaml":       "namespaces:\n  - name: shop\n  - name: shop\n",
	})
	bob := h.as("bob")
	var list []ResourceView
	bob.do(http.MethodGet, Prefix+"/inventory", nil, &list)
	var ids []string
	for _, r := range list {
		if r.Type == "namespace" {
			ids = append(ids, r.ID+" "+r.Status)
		}
	}
	if want := []string{"infra-live/namespace/prod-1/orders conforming", "infra-live/namespace/prod-1/shop conforming", "infra-live/namespace/prod-2/shop non-conforming"}; !slices.Equal(ids, want) {
		t.Errorf("the inventory's namespaces = %q", ids)
	}

	var res ResourceDetail
	if status, body := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/namespace/prod-1/orders", nil, &res); status != http.StatusOK {
		t.Fatalf("orders = %d %s", status, body)
	}
	if res.Path != "clusters/prod-1/namespaces.yaml" || res.Params["name"] != "orders" || res.Document["quota"].(map[string]any)["cpu"] != "4" ||
		res.Raw != "- name: orders\n    quota: { cpu: \"4\", memory: 8Gi }" {
		t.Errorf("orders = %+v", res)
	}
	if status, _ := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/namespace/prod-2/shop", nil, &res); status != http.StatusOK ||
		res.Status != "non-conforming" || !strings.Contains(res.Error, "appears twice") {
		t.Errorf("a shop whose key repeats = %d %+v", status, res)
	}
	if status, _ := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/namespace/prod-1/billing", nil, nil); status != http.StatusNotFound {
		t.Errorf("an entry that does not exist = %d", status)
	}
}
