package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The team type after a schema change (docs/design/02, schema evolution): version 2 adds an
// optional region, renames size to tier and requires it, and ships the migration from size.
const (
	squadType = `apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: team, title: Team, version: 2.0.0 }
spec:
  locate: { strategy: file, path: "teams/{env}/{name}.yaml" }
  params:
    - { name: env, type: enum, discover: true }
    - { name: name, pattern: "[a-z]+" }
  schema: catalog/schemas/team.schema.json
  ownership: { field: owner }
  migrate: "${{ has(doc.size) ? {'tier': doc.size, 'size': null} : {} }}"
`
	squadSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["owner", "tier"],
  "properties": {
    "owner": { "type": "string" },
    "tier": { "type": "string", "enum": ["small", "large"] },
    "region": { "type": "string" }
  }
}`
)

// squadHarness is the example repository with the team type and four teams: web and data still
// use size, which the migration renames; shop has a size the new schema refuses; ops is on the
// new schema already, without the new optional field.
func squadHarness(t *testing.T, typ string) *harness {
	t.Helper()
	h := newHarness(t)
	h.push("catalog/schemas/team.schema.json", squadSchema)
	h.push("teams/dev/web.yaml", "# The web team.\nowner: bob\nsize: small\n")
	h.push("teams/dev/data.yaml", "owner: carol\nsize: large\n")
	h.push("teams/dev/shop.yaml", "owner: bob\nsize: huge\n")
	h.push("teams/dev/ops.yaml", "owner: bob\ntier: large\n")
	h.push(".quai/types/team.yaml", typ)
	return h
}

// TestSchemaEvolution: each row of the schema evolution table, through the API.
func TestSchemaEvolution(t *testing.T) {
	t.Parallel()
	h := squadHarness(t, squadType)
	h.push("blueprints/squad.yaml", `apiVersion: quai.dev/v1beta1
kind: Blueprint
metadata: { name: squad, title: Squad }
spec:
  inputs: { type: object, properties: { name: { type: string } } }
  resources:
    - { id: team, type: { name: team, version: "^1" }, params: { env: dev, name: "${{ inputs.name }}" }, values: { tier: small } }
`)
	bob, alice := h.as("bob"), h.as("alice")
	resource := func(key string) ResourceDetail {
		t.Helper()
		var d ResourceDetail
		if status, body := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/team/"+key, nil, &d); status != http.StatusOK {
			t.Fatalf("%s = %d %s", key, status, body)
		}
		return d
	}
	update := func(key string) (int, string) {
		return bob.do(http.MethodPost, Prefix+"/plans/type", map[string]any{"repository": "infra-live", "type": "team", "kind": "update",
			"key": key, "values": map[string]any{"owner": "bob", "tier": "small"}}, nil)
	}

	// A new optional field: the files stay valid.
	if ops := resource("dev/ops"); ops.Status != "conforming" || ops.Migratable {
		t.Errorf("ops = %+v", ops.ResourceView)
	}

	// A stricter rule: shop stays visible with its problems; edits are blocked until it is fixed
	// in Git, and the migration does not fix it.
	shop := resource("dev/shop")
	if shop.Status != "non-conforming" || shop.Problems["size"] == "" || shop.Problems["tier"] == "" || shop.Migratable {
		t.Errorf("shop = %+v, migratable %v", shop.ResourceView, shop.Migratable)
	}
	if status, body := update("dev/shop"); status != http.StatusConflict || !strings.Contains(body, "fix it in Git before Quai edits it") {
		t.Errorf("editing shop = %d %s", status, body)
	}

	// A renamed field: web is blocked too, and its type's migration fixes it.
	if web := resource("dev/web"); web.Status != "non-conforming" || !web.Migratable {
		t.Errorf("web = %+v, migratable %v", web.ResourceView, web.Migratable)
	}
	if status, body := update("dev/web"); status != http.StatusConflict || !strings.Contains(body, "or with a migrate request on type team") {
		t.Errorf("editing web = %d %s", status, body)
	}
	// The migrate request covers every non-conforming resource of the type, so it needs the right
	// to operate each: bob does not own data.
	migrate := map[string]any{"repository": "infra-live", "type": "team", "kind": "migrate"}
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/type", migrate, nil); status != http.StatusForbidden {
		t.Errorf("bob's migration = %d %s", status, body)
	}
	var p PlanView
	if status, body := alice.do(http.MethodPost, Prefix+"/plans/type", migrate, &p); status != http.StatusOK {
		t.Fatalf("the migration plan = %d %s", status, body)
	}
	var paths []string
	for _, c := range p.Changes {
		paths = append(paths, c.Path+" "+c.Op)
	}
	if p.Kind != "migrate" || p.Target != "infra-live/team" || strings.Join(paths, ", ") != "teams/dev/data.yaml update, teams/dev/web.yaml update" {
		t.Errorf("the migration plan = %+v", p)
	}
	if len(p.Unmigrated) != 1 || p.Unmigrated[0].Key != "dev/shop" || !strings.Contains(p.Unmigrated[0].Reason, "tier") {
		t.Errorf("unmigrated = %+v", p.Unmigrated)
	}
	var r RequestView
	migrate["reason"] = "size is now tier"
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/type", migrate, &r); status != http.StatusCreated || r.Kind != "migrate" {
		t.Fatalf("the migrate request = %d %s", status, body)
	}
	delete(migrate, "reason")
	// It is listed on the resources it fixes, not on those it leaves.
	if data := resource("dev/data"); data.Status == "non-conforming" && (len(data.Requests) != 1 || data.Requests[0].ID != r.ID) {
		t.Errorf("data's requests = %+v", data.Requests)
	}
	if shop := resource("dev/shop"); len(shop.Requests) != 0 {
		t.Errorf("shop's requests = %+v", shop.Requests)
	}
	alice.await(r.ID, "applied")
	if got, _ := h.file("teams/dev/web.yaml"); got != "# The web team.\nowner: bob\ntier: small\n" {
		t.Errorf("the migrated web = %q", got)
	}
	if web := resource("dev/web"); web.Status != "conforming" || web.Migratable {
		t.Errorf("web after the migration = %+v", web.ResourceView)
	}
	if data := resource("dev/data"); data.Status != "conforming" {
		t.Errorf("data after the migration = %+v", data.ResourceView)
	}
	// What is left, the migration does not fix.
	if status, body := alice.do(http.MethodPost, Prefix+"/plans/type", migrate, nil); status != http.StatusUnprocessableEntity || !strings.Contains(body, "fixes none") {
		t.Errorf("a second migration = %d %s", status, body)
	}

	// A major version: the blueprint pinning ^1 is flagged in the catalog, and refused.
	var items []CatalogItem
	bob.do(http.MethodGet, Prefix+"/catalog", nil, &items)
	i := slices.IndexFunc(items, func(it CatalogItem) bool { return it.Name == "squad" })
	if i < 0 || len(items[i].Pins) != 1 || items[i].Pins[0] != "team: pins type team ^1, and the type is at 2.0.0" {
		t.Errorf("the catalog = %+v", items)
	}
	if j := slices.IndexFunc(items, func(it CatalogItem) bool { return it.Name == "app-servers" }); j < 0 || len(items[j].Pins) != 0 {
		t.Errorf("app-servers = %+v", items)
	}
	var bv BlueprintView
	if status, body := bob.do(http.MethodGet, Prefix+"/repositories/infra-live/blueprints/squad", nil, &bv); status != http.StatusOK || len(bv.Pins) != 1 {
		t.Errorf("the blueprint = %d %s", status, body)
	}
	squad := map[string]any{"repository": "infra-live", "blueprint": "squad", "name": "red", "inputs": map[string]any{"name": "red"}}
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/blueprint", squad, nil); status != http.StatusConflict || !strings.Contains(body, "team: pins type team ^1, and the type is at 2.0.0: the blueprint must be updated for it") {
		t.Errorf("a squad = %d %s", status, body)
	}
}

// TestDeletionPolicy: the type's deletion policy (docs/design/02, deletion). approval, the
// default, adds an approval rule (TestDeleteAndUnparseable); allowed makes a delete an ordinary
// change under the type's rules; forbidden refuses it.
func TestDeletionPolicy(t *testing.T) {
	t.Parallel()
	h := squadHarness(t, strings.Replace(squadType, "  ownership: { field: owner }\n", "  ownership: { field: owner }\n  deletion: forbidden\n", 1))
	bob := h.as("bob")
	del := map[string]any{"repository": "infra-live", "type": "team", "kind": "delete", "key": "dev/ops"}
	var tv TypeView
	if status, body := bob.do(http.MethodGet, Prefix+"/repositories/infra-live/types/team", nil, &tv); status != http.StatusOK || tv.Deletion != "forbidden" {
		t.Errorf("the type = %d %s", status, body)
	}
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/type", del, nil); status != http.StatusConflict || !strings.Contains(body, "forbids deletion") {
		t.Errorf("a forbidden delete = %d %s", status, body)
	}

	h.push(".quai/types/team.yaml", strings.Replace(squadType, "  ownership: { field: owner }\n", "  ownership: { field: owner }\n  deletion: allowed\n", 1))
	var r RequestView
	del["reason"] = "gone"
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", del, &r); status != http.StatusCreated || len(r.Rules) != 0 || r.State != "approved" {
		t.Fatalf("an allowed delete = %d %s", status, body)
	}
	bob.await(r.ID, "applied")
	if _, ok := h.file("teams/dev/ops.yaml"); ok {
		t.Error("the deleted team is still in Git")
	}
}
