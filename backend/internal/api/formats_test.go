package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/giteatest"
)

// formatType is a file/identity type over the example's VM schema, its files at path.
func formatType(name, path string) string {
	return `apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: ` + name + `, title: VM (` + name + `) }
spec:
  locate: { strategy: file, path: "` + path + `" }
  params:
    - { name: env, title: Environment, type: enum, discover: true }
    - { name: name, title: VM name, pattern: "[a-z][a-z0-9-]{1,30}" }
  schema: catalog/schemas/vm.schema.json
  generate: { kind: identity }
  ownership: { field: owner }
  deletion: allowed
`
}

const appJSON = `{
  "owner": "bob",
  "template": "ubuntu-24.04",
  "cpu": 2,
  "memory_gb": 4,
  "disks": [{ "size_gb": 50, "tier": "standard" }],
  "network": { "vlan": 110 }
}
`

const appHCL = `# The booking API, in development.
owner     = "bob"
template  = "ubuntu-24.04"
cpu       = 2
memory_gb = 4
disks     = [{ size_gb = 50, tier = "standard" }]
network   = { vlan = 110 }
tags      = merge(var.default_tags, { app = "booking" })
`

// TestFormats: types whose files are *.tfvars.json and HCL *.tfvars (docs/design/02, formats):
// their resources are read, created, updated with minimal diffs and deleted through pull requests;
// a value defined in code is shown, kept, and cannot be written.
func TestFormats(t *testing.T) {
	t.Parallel()
	h, remote := giteaHarnessFiles(t, "auto", "", nil, map[string]string{
		".quai/types/vm-json.yaml":   formatType("vm-json", "json/{env}/{name}.tfvars.json"),
		".quai/types/vm-hcl.yaml":    formatType("vm-hcl", "hcl/{env}/{name}.tfvars"),
		"json/dev/app01.tfvars.json": appJSON,
		"hcl/dev/app01.tfvars":       appHCL,
	})
	bob := h.as("bob")
	file := func(path string) (string, bool) {
		b, _, err := giteatest.Get(t).Client.GetFile(remote.Owner, remote.Name, "main", path)
		return string(b), err == nil
	}
	doc := map[string]any{"owner": "bob", "template": "ubuntu-24.04", "cpu": 4, "memory_gb": 4,
		"disks": []any{map[string]any{"size_gb": 50, "tier": "standard"}}, "network": map[string]any{"vlan": 110}}

	for _, tt := range []struct {
		typ, path, updated string
	}{
		{"vm-json", "json/dev/app01.tfvars.json", strings.Replace(appJSON, `"cpu": 2,`, `"cpu": 4,`, 1)},
		{"vm-hcl", "hcl/dev/app01.tfvars", strings.Replace(appHCL, "cpu       = 2", "cpu       = 4", 1)},
	} {
		var res ResourceDetail
		if status, body := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/"+tt.typ+"/dev/app01", nil, &res); status != http.StatusOK || res.Status != "conforming" {
			t.Fatalf("%s: the resource = %d %s", tt.typ, status, body)
		}
		if tt.typ == "vm-hcl" && (len(res.DefinedInCode) != 1 || res.DefinedInCode[0].Field != "tags" || !strings.HasPrefix(res.DefinedInCode[0].Text, "merge(")) {
			t.Errorf("%s: defined in code = %+v", tt.typ, res.DefinedInCode)
		}

		// An update changes the one value, nothing else.
		var r RequestView
		if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": tt.typ, "kind": "update",
			"key": "dev/app01", "values": doc, "reason": "more CPU"}, &r); status != http.StatusCreated {
			t.Fatalf("%s: update = %d %s", tt.typ, status, body)
		}
		bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
		if got, _ := file(tt.path); got != tt.updated {
			t.Errorf("%s: the updated file =\n%s\nwant\n%s", tt.typ, got, tt.updated)
		}

		// A new file, written in its format.
		if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": tt.typ, "kind": "create",
			"params": map[string]string{"env": "dev", "name": "web02"}, "values": doc, "reason": "a second one"}, &r); status != http.StatusCreated {
			t.Fatalf("%s: create = %d %s", tt.typ, status, body)
		}
		bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
		created, ok := file(strings.Replace(tt.path, "app01", "web02", 1))
		if !ok || !strings.Contains(created, "ubuntu-24.04") {
			t.Errorf("%s: the new file = %q", tt.typ, created)
		}

		// A delete removes the file.
		if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": tt.typ, "kind": "delete",
			"key": "dev/web02", "reason": "not needed"}, &r); status != http.StatusCreated {
			t.Fatalf("%s: delete = %d %s", tt.typ, status, body)
		}
		bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
		if _, ok := file(strings.Replace(tt.path, "app01", "web02", 1)); ok {
			t.Errorf("%s: the deleted file is still there", tt.typ)
		}
	}
	// The value defined in code cannot be written.
	withTags := map[string]any{"tags": map[string]any{"app": "x"}}
	for k, v := range doc {
		withTags[k] = v
	}
	if status, body := bob.do(http.MethodPost, Prefix+"/plans/type", map[string]any{"repository": "infra-live", "type": "vm-hcl", "kind": "update",
		"key": "dev/app01", "values": withTags}, nil); status != http.StatusUnprocessableEntity || !strings.Contains(body, "defined in code") ||
		!strings.Contains(body, `"location":"body.values.tags"`) {
		t.Errorf("writing a value defined in code = %d %s", status, body)
	}
}
