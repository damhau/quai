package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/config"
)

// The example's database type: developers and finops see it, developers request it, and only
// the group shop in prod. bob and carol are in shop; fiona is not.

// TestEntitlements: every path that lists, describes, plans or submits a catalog item enforces
// its entitlements on the server (docs/design/08-identity-access-tenancy.md#entitlements), and
// none of them stops an owner operating what they own.
func TestEntitlements(t *testing.T) {
	h := teamsHarness(t, func(c *config.Config) {
		c.Auth.Dev.Users = append(c.Auth.Dev.Users, config.DevUser{Name: "cat", Groups: []string{"catalog"}})
		c.Auth.Roles = append(c.Auth.Roles, config.RoleMapping{Group: "catalog", Role: "viewer"}, config.RoleMapping{Group: "catalog", Role: "catalog-author"})
	})
	alice, bob, fiona, dana, cat := h.as("alice"), h.as("bob"), h.as("fiona"), h.as("dana"), h.as("cat")
	database := func(env, name string) map[string]any {
		return map[string]any{"repository": "infra-live", "type": "database", "kind": "create",
			"params": map[string]string{"env": env, "name": name}, "values": map[string]any{"engine": "postgres", "size": "small"}}
	}
	refused := func(what string, status int, body, reason string) {
		t.Helper()
		if status != http.StatusForbidden || !strings.Contains(body, reason) {
			t.Errorf("%s = %d %s; want 403 saying %q", what, status, body, reason)
		}
	}
	ok := func(what string, status int, body string, want int) {
		t.Helper()
		if status != want {
			t.Errorf("%s = %d %s; want %d", what, status, body, want)
		}
	}
	catalogItem := func(u *user, name string) *CatalogItem {
		t.Helper()
		var items []CatalogItem
		if status, body := u.do(http.MethodGet, Prefix+"/catalog", nil, &items); status != http.StatusOK {
			t.Fatalf("catalog = %d %s", status, body)
		}
		if i := slices.IndexFunc(items, func(it CatalogItem) bool { return it.Name == name }); i >= 0 {
			return &items[i]
		}
		return nil
	}

	// The catalog says what each may request, environment by environment.
	for name, prod := range map[string]bool{"fiona": false, "bob": true} {
		it := catalogItem(h.as(name), "database")
		if it == nil || it.Entitlements == nil {
			t.Fatalf("%s's catalog: database = %+v", name, it)
		}
		e := it.Entitlements
		if !slices.Equal(e.Visibility, []string{"group:developers", "group:finops"}) || !slices.Equal(e.Requesters, []string{"group:developers"}) ||
			e.CanRequest == nil || !*e.CanRequest || len(e.Environments) != 1 || e.Environments[0].Environment != "prod" ||
			!slices.Equal(e.Environments[0].Requesters, []string{"group:shop"}) || e.Environments[0].CanRequest == nil || *e.Environments[0].CanRequest != prod {
			t.Errorf("%s's database entitlements = %+v", name, e)
		}
	}
	var vm TypeView
	bob.do(http.MethodGet, Prefix+"/repositories/infra-live/types/vm", nil, &vm)
	if vm.Name != "vm" || vm.Entitlements != nil {
		t.Errorf("an unrestricted type shows entitlements: %+v", vm.Entitlements)
	}

	// An environment's narrower list: fiona plans and requests in dev, not in prod, however prod
	// is written; bob, in shop, may.
	status, body := fiona.do(http.MethodPost, Prefix+"/plans/type", database("dev", "fiona-dev"), nil)
	ok("fiona's plan in dev", status, body, http.StatusOK)
	for _, env := range []string{"prod", " prod ", "PROD"} {
		status, body = fiona.do(http.MethodPost, Prefix+"/plans/type", database(env, "fiona-prod"), nil)
		refused("fiona's plan in "+env, status, body, "this catalog item is not open to your groups in prod")
	}
	status, body = fiona.do(http.MethodPost, Prefix+"/requests/type", database("prod", "fiona-prod"), nil)
	refused("fiona's request in prod", status, body, "not open to your groups in prod")
	status, body = bob.do(http.MethodPost, Prefix+"/plans/type", database("prod", "bob-prod"), nil)
	ok("bob's plan in prod", status, body, http.StatusOK)

	// An API token acts with its owner's entitlements.
	var tok CreatedTokenOutput
	if status, body := fiona.do(http.MethodPost, Prefix+"/tokens", map[string]any{"name": "ci", "scopes": []string{"catalog:read", "requests:write"}}, &tok.Body); status != http.StatusCreated {
		t.Fatalf("create token = %d %s", status, body)
	}
	status, body = withToken(t, h, tok.Body.Token, Prefix+"/plans/type", database("dev", "token-dev"))
	ok("fiona's token's plan in dev", status, body, http.StatusOK)
	status, body = withToken(t, h, tok.Body.Token, Prefix+"/requests/type", database("prod", "token-prod"))
	refused("fiona's token's request in prod", status, body, "not open to your groups in prod")

	// A blueprint's own entitlements, and those of the types it creates: its prod list lets
	// finops in, the database's lets only shop in, so nobody but a platform admin requests it in
	// prod.
	h.push("blueprints/shop-db.yaml", `apiVersion: quai.dev/v1beta1
kind: Blueprint
metadata: { name: shop-db, title: Shop database }
spec:
  inputs:
    type: object
    required: [env, db]
    properties:
      env: { type: string, enum: [dev, prod], default: dev }
      db: { type: string, pattern: "^[a-z][a-z0-9-]{1,30}$" }
  entitlements:
    requesters: [group:developers]
    environments:
      prod: [group:finops]
  resources:
    - id: db
      type: { name: database }
      params: { env: "${{ inputs.env }}", name: "${{ inputs.db }}" }
      values: { engine: postgres, size: small }
`)
	alice.do(http.MethodPost, Prefix+"/repositories/infra-live/sync", nil, nil)
	shopDB := func(env, name string) map[string]any {
		return map[string]any{"repository": "infra-live", "blueprint": "shop-db", "name": name, "inputs": map[string]any{"env": env, "db": name}}
	}
	status, body = bob.do(http.MethodPost, Prefix+"/plans/blueprint", shopDB("dev", "bp-dev"), nil)
	ok("bob's blueprint plan in dev", status, body, http.StatusOK)
	status, body = fiona.do(http.MethodPost, Prefix+"/plans/blueprint", shopDB("dev", "bp-dev"), nil)
	ok("fiona's blueprint plan in dev", status, body, http.StatusOK)
	status, body = bob.do(http.MethodPost, Prefix+"/plans/blueprint", shopDB("prod", "bp-prod"), nil)
	refused("bob's blueprint plan in prod", status, body, "this catalog item is not open to your groups in prod")
	status, body = fiona.do(http.MethodPost, Prefix+"/plans/blueprint", shopDB("prod", "bp-prod"), nil)
	refused("fiona's blueprint plan in prod", status, body, "type database is not open to your groups in prod")
	status, body = fiona.do(http.MethodPost, Prefix+"/requests/blueprint", shopDB("prod", "bp-prod"), nil)
	refused("fiona's blueprint request in prod", status, body, "type database is not open to your groups in prod")
	status, body = alice.do(http.MethodPost, Prefix+"/plans/blueprint", shopDB("prod", "bp-prod"), nil)
	ok("a platform admin's blueprint plan in prod", status, body, http.StatusOK)
	// The env input's default decides when the form leaves it out.
	status, body = bob.do(http.MethodPost, Prefix+"/plans/blueprint", map[string]any{"repository": "infra-live", "blueprint": "shop-db", "name": "bp-default", "inputs": map[string]any{"db": "bp-default"}}, nil)
	ok("bob's blueprint plan in the default environment", status, body, http.StatusOK)

	// bob owns an instance of app-servers before the items are hidden from him.
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/blueprint", map[string]any{"repository": "infra-live", "blueprint": "app-servers", "name": "shop-dev",
		"inputs": map[string]any{"environment": "dev", "app": "shop", "withDatabase": false}}, &r); status != http.StatusCreated {
		t.Fatalf("app-servers request = %d %s", status, body)
	}
	bob.await(r.ID, "applied")

	// Hidden from everyone but the group dba: absent from the catalog, and its form, plans and
	// submissions refused, with the reason.
	src, found := h.file(".quai/types/database.yaml")
	if !found || !strings.Contains(src, "visibility: [group:developers, group:finops]") {
		t.Fatal("the example database type changed its visibility")
	}
	h.push(".quai/types/database.yaml", strings.Replace(src, "visibility: [group:developers, group:finops]", "visibility: [group:dba]", 1))
	app, _ := h.file("blueprints/app-servers.yaml")
	h.push("blueprints/app-servers.yaml", strings.Replace(app, "\nspec:\n", "\nspec:\n  entitlements: { visibility: [group:dba] }\n", 1))
	alice.do(http.MethodPost, Prefix+"/repositories/infra-live/sync", nil, nil)

	for _, name := range []string{"database", "app-servers"} {
		if it := catalogItem(bob, name); it != nil {
			t.Errorf("bob's catalog shows hidden %s: %+v", name, it)
		}
		if it := catalogItem(alice, name); it == nil || it.Entitlements == nil || !*it.Entitlements.CanRequest {
			t.Errorf("a platform admin's catalog: %s = %+v", name, it)
		}
		// An auditor sees what is hidden, and may request none of it.
		if it := catalogItem(dana, name); it == nil || it.Entitlements == nil || *it.Entitlements.CanRequest || !strings.Contains(it.Entitlements.Reason, "requester role") {
			t.Errorf("an auditor's catalog: %s = %+v", name, it)
		}
	}
	status, body = bob.do(http.MethodGet, Prefix+"/repositories/infra-live/types/database", nil, nil)
	refused("bob's database form", status, body, "this catalog item is not visible to your groups")
	status, body = bob.do(http.MethodPost, Prefix+"/plans/type", database("dev", "hidden-dev"), nil)
	refused("bob's hidden plan", status, body, "this catalog item is not visible to your groups")
	status, body = bob.do(http.MethodPost, Prefix+"/requests/type", database("dev", "hidden-dev"), nil)
	refused("bob's hidden request", status, body, "this catalog item is not visible to your groups")
	status, body = bob.do(http.MethodGet, Prefix+"/repositories/infra-live/blueprints/app-servers", nil, nil)
	refused("bob's app-servers form", status, body, "this catalog item is not visible to your groups")
	apps := map[string]any{"repository": "infra-live", "blueprint": "app-servers", "name": "shop-test", "inputs": map[string]any{"environment": "dev", "app": "shop"}}
	status, body = bob.do(http.MethodPost, Prefix+"/plans/blueprint", apps, nil)
	refused("bob's hidden blueprint plan", status, body, "this catalog item is not visible to your groups")
	status, body = bob.do(http.MethodPost, Prefix+"/requests/blueprint", apps, nil)
	refused("bob's hidden blueprint request", status, body, "this catalog item is not visible to your groups")
	// A blueprint that creates a hidden type is refused for that type.
	status, body = bob.do(http.MethodPost, Prefix+"/plans/blueprint", shopDB("dev", "bp-hidden"), nil)
	refused("bob's blueprint creating a hidden type", status, body, "type database is not visible to your groups")

	// Owners and readers of what exists keep its page and its day-2 forms: bob's database in prod,
	// and his instance of app-servers.
	status, body = bob.do(http.MethodGet, Prefix+"/repositories/infra-live/types/database?resource=prod/shop", nil, nil)
	ok("the type of bob's database", status, body, http.StatusOK)
	status, body = bob.do(http.MethodGet, Prefix+"/repositories/infra-live/types/database?resource=prod/nope", nil, nil)
	ok("the type of a database that does not exist", status, body, http.StatusNotFound)
	update := map[string]any{"repository": "infra-live", "type": "database", "kind": "update", "key": "prod/shop", "values": map[string]any{"engine": "postgres", "size": "large"}}
	status, body = bob.do(http.MethodPost, Prefix+"/plans/type", update, nil)
	ok("bob's update of his database", status, body, http.StatusOK)
	status, body = bob.do(http.MethodGet, Prefix+"/repositories/infra-live/blueprints/app-servers?instance=shop-dev", nil, nil)
	ok("the blueprint of bob's instance", status, body, http.StatusOK)
	status, body = bob.do(http.MethodGet, Prefix+"/repositories/infra-live/blueprints/app-servers?instance=nope", nil, nil)
	ok("the blueprint of an instance that does not exist", status, body, http.StatusNotFound)
	scale := map[string]any{"repository": "infra-live", "blueprint": "app-servers", "instance": "shop-dev", "action": "scale-web", "actionInputs": map[string]any{"cpu": 8}}
	status, body = bob.do(http.MethodPost, Prefix+"/plans/instance-action", scale, nil)
	ok("bob's action on his instance", status, body, http.StatusOK)

	// A catalog author describes every item, for the designer and previews, and requests none.
	status, body = cat.do(http.MethodGet, Prefix+"/repositories/infra-live/types/database", nil, nil)
	ok("a catalog author's database form", status, body, http.StatusOK)
	status, body = cat.do(http.MethodPost, Prefix+"/plans/type", database("dev", "cat-dev"), nil)
	refused("a catalog author's plan", status, body, "requester role")
	var pv PreviewView
	if status, body := cat.do(http.MethodGet, Prefix+"/repositories/infra-live/preview?branch=main", nil, &pv); status != http.StatusOK {
		t.Fatalf("preview = %d %s", status, body)
	}
	if i := slices.IndexFunc(pv.Items, func(it CatalogItem) bool { return it.Name == "database" }); i < 0 || pv.Items[i].Entitlements == nil ||
		!slices.Equal(pv.Items[i].Entitlements.Visibility, []string{"group:dba"}) || pv.Items[i].Entitlements.CanRequest != nil {
		t.Errorf("the preview's database = %+v", pv.Items)
	}
}

// withToken posts body to path with an API token.
func withToken(t *testing.T, h *harness, token, path string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}
