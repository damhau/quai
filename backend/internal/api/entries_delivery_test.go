package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/giteatest"
)

// giteaNamespaces serves Quai on Gitea with the namespaces of prod-1 in one shared file, each
// request approved by the platform admins (alice).
func giteaNamespaces(t *testing.T) (*harness, *giteatest.Repo) {
	t.Helper()
	typ := strings.Replace(namespaceType, "  generate: { kind: identity }\n",
		"  generate: { kind: identity }\n  ownership: { team: shop }\n  approval:\n    rules: [{ name: platform, when: \"true\", approvers: [group:platform-admins] }]\n", 1)
	return giteaHarnessFiles(t, "auto", "", nil, map[string]string{
		".quai/types/namespace.yaml":            typ,
		"catalog/schemas/namespace.schema.json": namespaceSchema,
		"clusters/prod-1/namespaces.yaml":       "# Namespaces on prod-1.\nnamespaces:\n  - name: orders\n    quota: { cpu: \"4\", memory: 8Gi }\n  - name: shop\n    quota: { cpu: \"8\", memory: 16Gi }\n",
	})
}

func (u *user) submitEntry(kind, name string, quota map[string]any) RequestView {
	u.h.t.Helper()
	body := map[string]any{"repository": "infra-live", "type": "namespace", "kind": kind, "values": map[string]any{"quota": quota}, "reason": "capacity"}
	if kind == "create" {
		body["params"] = map[string]string{"cluster": "prod-1", "name": name}
	} else {
		body["key"] = "prod-1/" + name
	}
	var r RequestView
	if status, out := u.do(http.MethodPost, Prefix+"/requests/type", body, &r); status != http.StatusCreated {
		u.h.t.Fatalf("submit %s %s = %d %s", kind, name, status, out)
	}
	return u.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
}

func (u *user) approve(id string) {
	u.h.t.Helper()
	if status, body := u.do(http.MethodPost, Prefix+"/requests/"+id+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		u.h.t.Fatalf("approve = %d %s", status, body)
	}
}

// TestEntryRequestsConcurrently: two requests on two entries of one shared file both merge, the
// second rebuilt on the first by making its change again on the new file (docs/design/03,
// concurrency); an outside edit to one entry makes only its request stale.
func TestEntryRequestsConcurrently(t *testing.T) {
	t.Parallel()
	h, remote := giteaNamespaces(t)
	bob, alice := h.as("bob"), h.as("alice")

	// Both append at the end of the list: Git alone cannot merge the second.
	billing := bob.submitEntry("create", "billing", map[string]any{"cpu": "1"})
	payments := bob.submitEntry("create", "payments", map[string]any{"cpu": "2"})
	alice.approve(billing.ID)
	bob.awaitDelivery(billing.ID, "merged", func(v RequestView) bool { return v.Delivery.MergeCommit != "" })
	main := h.giteaHead(t, remote, "main")
	payments = bob.awaitDelivery(payments.ID, "rebuilt", func(v RequestView) bool { return hasEvent(v, "rebuilt", main) })
	if payments.State != "pending_approval" {
		t.Errorf("payments after the rebuild: %s", payments.State)
	}
	alice.approve(payments.ID)
	bob.awaitDelivery(payments.ID, "merged", func(v RequestView) bool { return v.Delivery.MergeCommit != "" })
	g := giteatest.Get(t)
	content, _, err := g.Client.GetFile(remote.Owner, remote.Name, "main", "clusters/prod-1/namespaces.yaml")
	if want := "# Namespaces on prod-1.\nnamespaces:\n  - name: orders\n    quota: { cpu: \"4\", memory: 8Gi }\n  - name: shop\n    quota: { cpu: \"8\", memory: 16Gi }\n" +
		"  - name: billing\n    quota: { cpu: \"1\" }\n  - name: payments\n    quota: { cpu: \"2\" }\n"; err != nil || string(content) != want {
		t.Errorf("main after both merges: %v\n%s", err, content)
	}

	// Someone edits shop on main: the request on shop is stale, the one on orders is rebuilt.
	orders := bob.submitEntry("update", "orders", map[string]any{"cpu": "6", "memory": "8Gi"})
	shop := bob.submitEntry("update", "shop", map[string]any{"cpu": "16", "memory": "16Gi"})
	if err := remote.Write("main", "clusters/prod-1/namespaces.yaml", strings.Replace(string(content), `cpu: "8"`, `cpu: "12"`, 1)); err != nil {
		t.Fatal(err)
	}
	main = h.giteaHead(t, remote, "main")
	shop = bob.awaitDelivery(shop.ID, "stale", func(v RequestView) bool { return v.State == "stale" })
	if !hasEvent(shop, "stale", "clusters/prod-1/namespaces.yaml#namespaces[name=shop]") {
		t.Errorf("the stale event does not name the entry: %+v", shop.Events)
	}
	orders = bob.awaitDelivery(orders.ID, "rebuilt", func(v RequestView) bool { return hasEvent(v, "rebuilt", main) })
	if orders.State != "pending_approval" {
		t.Errorf("orders after the outside edit: %s", orders.State)
	}
}

// TestTfvarsMapRequests: a type over a map in a *.tfvars file, requested through pull requests on
// Gitea: a new entry and an update around a value defined in code.
func TestTfvarsMapRequests(t *testing.T) {
	t.Parallel()
	records := "# DNS records of prod.\nrecords = {\n  api = {\n    type = \"A\"\n    ttl  = var.default_ttl\n  }\n  www = { type = \"CNAME\", ttl = 300 }\n}\n"
	h, remote := giteaHarnessFiles(t, "auto", "", nil, map[string]string{
		".quai/types/record.yaml": "apiVersion: quai.dev/v1beta1\nkind: ResourceType\nmetadata: { name: record, title: DNS record }\nspec:\n" +
			"  locate: { strategy: entry, path: \"dns/{env}.tfvars\", collection: records, keyParam: name }\n" +
			"  params:\n    - { name: env, title: Environment }\n    - { name: name, title: Name }\n" +
			"  schema: catalog/schemas/record.schema.json\n  generate: { kind: identity }\n  ownership: { team: shop }\n" +
			"  approval:\n    rules: [{ name: platform, when: \"true\", approvers: [group:platform-admins] }]\n",
		"catalog/schemas/record.schema.json": `{"type": "object", "required": ["type", "ttl"], "properties": {"type": {"type": "string"}, "ttl": {"type": "integer"}}}`,
		"dns/prod.tfvars":                    records,
	})
	bob, alice := h.as("bob"), h.as("alice")
	var api ResourceDetail
	if status, body := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/record/prod/api", nil, &api); status != http.StatusOK ||
		api.Status != "conforming" || len(api.DefinedInCode) != 1 || api.DefinedInCode[0].Field != "ttl" || api.DefinedInCode[0].Text != "var.default_ttl" {
		t.Fatalf("api = %d %s", status, body)
	}

	submit := func(body map[string]any) {
		t.Helper()
		body["repository"], body["type"], body["reason"] = "infra-live", "record", "mail"
		var r RequestView
		if status, out := bob.do(http.MethodPost, Prefix+"/requests/type", body, &r); status != http.StatusCreated {
			t.Fatalf("submit = %d %s", status, out)
		}
		r = bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
		alice.approve(r.ID)
		bob.awaitDelivery(r.ID, "merged", func(v RequestView) bool { return v.Delivery.MergeCommit != "" })
	}
	submit(map[string]any{"kind": "create", "params": map[string]string{"env": "prod", "name": "mail"}, "values": map[string]any{"type": "MX", "ttl": 3600}})
	submit(map[string]any{"kind": "update", "key": "prod/api", "values": map[string]any{"type": "AAAA"}})
	content, _, err := giteatest.Get(t).Client.GetFile(remote.Owner, remote.Name, "main", "dns/prod.tfvars")
	want := "# DNS records of prod.\nrecords = {\n  api = {\n    type = \"AAAA\"\n    ttl  = var.default_ttl\n  }\n  www  = { type = \"CNAME\", ttl = 300 }\n  mail = { type = \"MX\", ttl = 3600 }\n}\n"
	if err != nil || string(content) != want {
		t.Errorf("main after both requests: %v\n%s", err, content)
	}
}
