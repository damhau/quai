package api

import (
	"net/http"
	"strings"
	"testing"
)

const appType = `apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: app, title: Application }
spec:
  locate: { strategy: file, path: "apps/{env}/{name}.yaml" }
  params: [{ name: env, title: Environment }, { name: name, title: Name }]
  schema: catalog/schemas/app.schema.json
  inputs: catalog/schemas/app-inputs.schema.json
  ownership: { field: owner }
  generate:
    kind: mapping
    template:
      replicas: "${{ inputs.size == 'small' ? 1 : 3 }}"
      resources: { cpu: "${{ inputs.size == 'small' ? '500m' : '2' }}", memory: "${{ inputs.size == 'small' ? '512Mi' : '4Gi' }}" }
      host: "${{ params.name }}.${{ params.env }}.example.com"
      probes: { liveness: /healthz }
    read:
      size: "${{ doc.replicas == 1 ? 'small' : 'large' }}"
`

// TestMappingRequests: requests on a type with the mapping generator write files from its
// template: a new file, then an update that changes only what the template manages and keeps
// what a person added.
func TestMappingRequests(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.pushFiles(map[string]string{
		".quai/types/app.yaml": appType,
		"catalog/schemas/app.schema.json": `{"type": "object", "properties": {"owner": {"type": "string"}, "replicas": {"type": "integer"}, "host": {"type": "string"},
  "resources": {"type": "object"}, "probes": {"type": "object"}, "env": {"type": "object"}}}`,
		"catalog/schemas/app-inputs.schema.json": `{"type": "object", "required": ["size"], "properties": {"size": {"title": "Size", "enum": ["small", "large"], "default": "small"}}}`,
		"apps/prod/orders.yaml":                  "# Orders API.\nowner: bob\nreplicas: 1\nresources: { cpu: 500m, memory: 512Mi }\nhost: orders.prod.example.com\nprobes:\n  liveness: /healthz\n  readiness: /ready # added by hand\nenv:\n  LOG_LEVEL: info\n",
	})
	bob := h.as("bob")
	var orders ResourceDetail
	if status, body := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/app/prod/orders", nil, &orders); status != http.StatusOK ||
		len(orders.Values) != 1 || orders.Values["size"] != "small" || orders.Document["host"] != "orders.prod.example.com" {
		t.Errorf("the edit form of orders starts from = %d %s", status, body)
	}

	var created RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "app", "kind": "create",
		"params": map[string]string{"env": "dev", "name": "shop"}, "values": map[string]any{}}, &created); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	if len(created.Changes) != 1 || created.Changes[0].Path != "apps/dev/shop.yaml" || diffText(created.Changes[0]) !=
		"+owner: bob\n+replicas: 1\n+host: shop.dev.example.com\n+resources:\n+  cpu: 500m\n+  memory: 512Mi\n+probes:\n+  liveness: /healthz\n" {
		t.Errorf("create:\n%s", diffText(created.Changes[0]))
	}

	var updated RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "app", "kind": "update",
		"key": "prod/orders", "values": map[string]any{"size": "large"}}, &updated); status != http.StatusCreated {
		t.Fatalf("update = %d %s", status, body)
	}
	var changed []string
	for _, l := range updated.Changes[0].Diff {
		if l.Op != " " {
			changed = append(changed, l.Op+l.Text)
		}
	}
	if want := "-replicas: 1|-resources: { cpu: 500m, memory: 512Mi }|+replicas: 3|+resources: { cpu: \"2\", memory: 4Gi }"; strings.Join(changed, "|") != want {
		t.Errorf("update:\n%s", diffText(updated.Changes[0]))
	}
}
