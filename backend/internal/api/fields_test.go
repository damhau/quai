package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/giteatest"
)

// TestFieldRequests: a quota-increase type writes only the quota of namespaces another type owns
// (docs/design/02, field). Its resources show only their quota; a path it does not list, a create
// and a delete are refused; and a request on the quota and one on the entry's other fields both
// merge.
func TestFieldRequests(t *testing.T) {
	t.Parallel()
	approval := "  ownership: { team: shop }\n  approval:\n    rules: [{ name: platform, when: \"true\", approvers: [group:platform-admins] }]\n"
	h, remote := giteaHarnessFiles(t, "auto", "", nil, map[string]string{
		".quai/types/namespace.yaml": namespaceType + approval,
		".quai/types/quota.yaml": "apiVersion: quai.dev/v1beta1\nkind: ResourceType\nmetadata: { name: quota, title: Quota increase }\nspec:\n" +
			"  locate: { strategy: field, path: \"clusters/{cluster}/namespaces.yaml\", collection: namespaces, key: name, keyParam: name, fields: [quota] }\n" +
			"  params:\n    - { name: cluster, title: Cluster }\n    - { name: name, title: Name }\n" +
			"  schema: catalog/schemas/quota.schema.json\n  generate: { kind: identity }\n" + approval,
		"catalog/schemas/namespace.schema.json": `{"type": "object", "required": ["name"], "additionalProperties": false, "properties": {"name": {"type": "string"},
  "labels": {"type": "object", "additionalProperties": {"type": "string"}}, "quota": {"type": "object", "properties": {"cpu": {"type": "string"}, "memory": {"type": "string"}}}}}`,
		"catalog/schemas/quota.schema.json": `{"type": "object", "properties": {"quota": {"type": "object", "properties": {"cpu": {"type": "string"}, "memory": {"type": "string"}}}}}`,
		"clusters/prod-1/namespaces.yaml":   "# Namespaces on prod-1.\nnamespaces:\n  - name: orders\n    quota: { cpu: \"4\", memory: 8Gi }\n  - name: shop\n    quota: { cpu: \"8\", memory: 16Gi }\n",
	})
	bob, alice := h.as("bob"), h.as("alice")

	var res ResourceDetail
	if status, body := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/quota/prod-1/orders", nil, &res); status != http.StatusOK || len(res.Document) != 1 || res.Document["quota"] == nil {
		t.Fatalf("the quota of orders = %d %s", status, body)
	}
	var catalog []CatalogItem
	bob.do(http.MethodGet, Prefix+"/catalog", nil, &catalog)
	for _, it := range catalog {
		if it.Name == "quota" {
			t.Error("the catalog offers to create quotas")
		}
	}

	submit := func(typ string, values map[string]any) (int, string, RequestView) {
		t.Helper()
		var r RequestView
		status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": typ, "kind": "update",
			"key": "prod-1/orders", "values": values, "reason": "Q4"}, &r)
		return status, body, r
	}
	if status, body, _ := submit("quota", map[string]any{"quota": map[string]any{"cpu": "6"}, "labels": map[string]any{"team": "x"}}); status != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "body.values.labels") || !strings.Contains(body, "may write only quota") {
		t.Errorf("an unlisted path = %d %s", status, body)
	}
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "quota", "kind": "delete", "key": "prod-1/orders"}, nil); status != http.StatusConflict {
		t.Errorf("a delete = %d %s", status, body)
	}

	// The quota of orders, then its labels through the owning type: the labels merge first, and
	// the quota request is rebuilt on them, not stale.
	status, body, quota := submit("quota", map[string]any{"quota": map[string]any{"cpu": "6", "memory": "8Gi"}})
	if status != http.StatusCreated {
		t.Fatalf("submit the quota = %d %s", status, body)
	}
	quota = bob.awaitDelivery(quota.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	status, body, labels := submit("namespace", map[string]any{"quota": map[string]any{"cpu": "4", "memory": "8Gi"}, "labels": map[string]any{"team": "orders"}})
	if status != http.StatusCreated {
		t.Fatalf("submit the labels = %d %s", status, body)
	}
	labels = bob.awaitDelivery(labels.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	alice.approve(labels.ID)
	bob.awaitDelivery(labels.ID, "merged", func(v RequestView) bool { return v.Delivery.MergeCommit != "" })
	main := h.giteaHead(t, remote, "main")
	quota = bob.awaitDelivery(quota.ID, "rebuilt", func(v RequestView) bool { return hasEvent(v, "rebuilt", main) || v.State == "stale" })
	if quota.State != "pending_approval" {
		t.Fatalf("the quota request after the labels: %s %+v", quota.State, quota.Events)
	}
	alice.approve(quota.ID)
	bob.awaitDelivery(quota.ID, "merged", func(v RequestView) bool { return v.Delivery.MergeCommit != "" })
	content, _, err := giteatest.Get(t).Client.GetFile(remote.Owner, remote.Name, "main", "clusters/prod-1/namespaces.yaml")
	if want := "# Namespaces on prod-1.\nnamespaces:\n  - name: orders\n    quota: { cpu: \"6\", memory: 8Gi }\n    labels: { team: orders }\n  - name: shop\n    quota: { cpu: \"8\", memory: 16Gi }\n"; err != nil || string(content) != want {
		t.Errorf("main after both:\n%s", content)
	}
}
