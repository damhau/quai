package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/giteatest"
)

const teamSchema = `{
  "type": "object",
  "required": ["team"],
  "additionalProperties": false,
  "properties": {
    "name": { "type": "string", "readOnly": true },
    "owner": { "type": "string" },
    "team": { "type": "string" },
    "cpu": { "type": "string" }
  }
}
`

const teamType = `apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: team, title: Team namespace }
spec:
  locate: { strategy: dir, path: "teams/{name}" }
  params: [{ name: name, title: Name, pattern: "[a-z][a-z0-9-]{1,30}" }]
  schema: catalog/schemas/team.schema.json
  generate:
    kind: component
    component: k8s.namespace
    with:
      name: ${{ params.name }}
      labels: { team: "${{ inputs.team }}" }
      quota: { requests.cpu: "${{ inputs.?cpu }}" }
  ownership: { field: owner }
  paramFields: { name: name }
  deletion: allowed
`

const shopManifests = `# The shop team's namespace.
apiVersion: v1
kind: Namespace
metadata:
  name: shop
  labels:
    team: shop
---
apiVersion: v1
kind: ResourceQuota
metadata:
  name: quota
  namespace: shop
spec:
  hard:
    requests.cpu: "8" # cores
`

// TestComponents: a type written by the component generator, one directory per resource
// (docs/design/02, dir and component): its record is read as the resource, and requests create,
// update and delete its directory through pull requests.
func TestComponents(t *testing.T) {
	t.Parallel()
	h, remote := giteaHarnessFiles(t, "auto", "", nil, map[string]string{
		"catalog/schemas/team.schema.json": teamSchema,
		".quai/types/team.yaml":            teamType,
		"teams/shop/quai.yaml":             "name: shop\nowner: bob\nteam: shop\ncpu: \"8\"\n",
		"teams/shop/kustomization.yaml":    "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - manifests.yaml\n",
		"teams/shop/manifests.yaml":        shopManifests,
		"teams/shop/README.md":             "Shop.\n",
	})
	bob := h.as("bob")
	file := func(path string) (string, bool) {
		b, _, err := giteatest.Get(t).Client.GetFile(remote.Owner, remote.Name, "main", path)
		return string(b), err == nil
	}

	var res ResourceDetail
	if status, body := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/team/shop", nil, &res); status != http.StatusOK || res.Status != "conforming" {
		t.Fatalf("the resource = %d %s", status, body)
	}
	if res.Path != "teams/shop" || res.Values["cpu"] != "8" || !strings.HasPrefix(res.Raw, "name: shop\n") {
		t.Errorf("the resource = path %s, values %v, raw %q", res.Path, res.Values, res.Raw)
	}

	// An update edits the record and the quota in place; the rest of the directory stays.
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "team", "kind": "update",
		"key": "shop", "values": map[string]any{"name": "shop", "owner": "bob", "team": "shop", "cpu": "12"}, "reason": "more CPU"}, &r); status != http.StatusCreated {
		t.Fatalf("update = %d %s", status, body)
	}
	bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	if got, _ := file("teams/shop/manifests.yaml"); got != strings.Replace(shopManifests, `"8" # cores`, `"12" # cores`, 1) {
		t.Errorf("the updated manifests =\n%s", got)
	}
	if got, _ := file("teams/shop/quai.yaml"); got != "name: shop\nowner: bob\nteam: shop\ncpu: \"12\"\n" {
		t.Errorf("the updated record =\n%s", got)
	}
	if got, _ := file("teams/shop/README.md"); got != "Shop.\n" {
		t.Errorf("the README = %q", got)
	}

	// A create writes the directory.
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "team", "kind": "create",
		"params": map[string]string{"name": "web"}, "values": map[string]any{"team": "web"}, "reason": "a new team"}, &r); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	if got, _ := file("teams/web/manifests.yaml"); got != "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: web\n  labels:\n    team: web\n" {
		t.Errorf("the new manifests =\n%s", got)
	}
	if got, ok := file("teams/web/quai.yaml"); !ok || got != "name: web\nowner: bob\nteam: web\n" {
		t.Errorf("the new record = %q", got)
	}
	if _, ok := file("teams/web/kustomization.yaml"); !ok {
		t.Error("the new directory has no kustomization.yaml")
	}
	if status, body := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/team/web", nil, &res); status != http.StatusOK || res.Status != "conforming" {
		t.Errorf("the new resource = %d %s", status, body)
	}

	// A delete removes every file of the directory.
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "team", "kind": "delete",
		"key": "shop", "reason": "the team is gone"}, &r); status != http.StatusCreated {
		t.Fatalf("delete = %d %s", status, body)
	}
	bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	for _, p := range []string{"teams/shop/quai.yaml", "teams/shop/manifests.yaml", "teams/shop/kustomization.yaml", "teams/shop/README.md"} {
		if _, ok := file(p); ok {
			t.Errorf("%s is still there", p)
		}
	}
}
