package api

import (
	"net/http"
	"testing"

	"github.com/damhau/quai/backend/internal/engine/generate/plugin/plugintest"
	"github.com/damhau/quai/backend/internal/giteatest"
)

// TestPlugins: a type written by the plugin generator (docs/design/02, plugin): the inventory
// reads its files through the plugin, and requests write them through it, by pull requests that
// keep what the plugin does not manage.
func TestPlugins(t *testing.T) {
	t.Parallel()
	module := plugintest.Build(t, "guest")
	const web = "# The web team's settings.\ncolour = blue\nowner = bob\n"
	h, remote := giteaHarnessFiles(t, "auto", "", nil, map[string]string{
		".quai/plugins/settings.wasm":          string(module),
		".quai/types/settings.yaml":            plugintest.Type(".quai/plugins/settings.wasm", plugintest.Digest(module)),
		"catalog/schemas/settings.schema.json": plugintest.Schema,
		"settings/web.conf":                    web,
	})
	bob := h.as("bob")
	file := func(path string) (string, bool) {
		b, _, err := giteatest.Get(t).Client.GetFile(remote.Owner, remote.Name, "main", path)
		return string(b), err == nil
	}

	var res ResourceDetail
	if status, body := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/settings/web", nil, &res); status != http.StatusOK || res.Status != "conforming" {
		t.Fatalf("the resource = %d %s", status, body)
	}
	if res.Path != "settings/web.conf" || res.Values["colour"] != "blue" || res.Owner != "bob" || res.Raw != web {
		t.Errorf("the resource = path %s, values %v, owner %s, raw %q", res.Path, res.Values, res.Owner, res.Raw)
	}

	// An action edits the file through the plugin; its comment stays.
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "settings", "kind": "action",
		"key": "web", "action": "paint", "actionInputs": map[string]any{"colour": "red"}, "reason": "a new look"}, &r); status != http.StatusCreated {
		t.Fatalf("paint = %d %s", status, body)
	}
	bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	if got, _ := file("settings/web.conf"); got != "# The web team's settings.\ncolour = red\nowner = bob\n" {
		t.Errorf("the painted file = %q", got)
	}

	// A create writes a new file; a value outside the schema is refused before anything is
	// written.
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "settings", "kind": "create",
		"params": map[string]string{"name": "shop"}, "values": map[string]any{"colour": "pink"}, "reason": "a new team"}, nil); status != http.StatusUnprocessableEntity {
		t.Errorf("a colour outside the schema = %d %s", status, body)
	}
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "settings", "kind": "create",
		"params": map[string]string{"name": "shop"}, "values": map[string]any{"colour": "green"}, "reason": "a new team"}, &r); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	if got, ok := file("settings/shop.conf"); !ok || got != "colour = green\nowner = bob\n" {
		t.Errorf("the new file = %q", got)
	}
	if status, body := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/settings/shop", nil, &res); status != http.StatusOK || res.Values["colour"] != "green" {
		t.Errorf("the new resource = %d %s", status, body)
	}
}

// TestINIPlugin: a type written by the repository's example plugin, plugins/ini, built with the
// plugin SDK: a request updates an INI file through it, keeping its comments, spacing and order,
// and creates one.
func TestINIPlugin(t *testing.T) {
	t.Parallel()
	module := plugintest.Example(t, "ini")
	const web = "; The web service.\nowner = bob\n\n; Where it listens.\n[server]\nhost = web.example.com\nport=8080\n"
	h, remote := giteaHarnessFiles(t, "auto", "", nil, map[string]string{
		".quai/plugins/ini.wasm": string(module),
		".quai/types/service.yaml": `apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: service, title: Service }
spec:
  locate: { strategy: file, path: "services/{name}.ini" }
  params: [{ name: name, pattern: "[a-z]+" }]
  schema: catalog/schemas/service.schema.json
  generate:
    kind: plugin
    module: .quai/plugins/ini.wasm
    digest: ` + plugintest.Digest(module) + `
  ownership: { field: owner }
`,
		"catalog/schemas/service.schema.json": `{
  "type": "object",
  "additionalProperties": false,
  "required": ["owner", "server"],
  "properties": {
    "owner": { "type": "string" },
    "server": {
      "type": "object",
      "additionalProperties": false,
      "required": ["host"],
      "properties": { "host": { "type": "string" }, "port": { "type": "string", "pattern": "^[0-9]+$" } }
    },
    "log": { "type": "object", "properties": { "level": { "type": "string", "enum": ["debug", "info", "warn"] } } }
  }
}
`,
		"services/web.ini": web,
	})
	bob := h.as("bob")
	file := func(path string) (string, bool) {
		b, _, err := giteatest.Get(t).Client.GetFile(remote.Owner, remote.Name, "main", path)
		return string(b), err == nil
	}

	var res ResourceDetail
	if status, body := bob.do(http.MethodGet, Prefix+"/resource?id=infra-live/service/web", nil, &res); status != http.StatusOK || res.Status != "conforming" {
		t.Fatalf("the resource = %d %s", status, body)
	}
	if server, _ := res.Values["server"].(map[string]any); server["port"] != "8080" || res.Owner != "bob" {
		t.Errorf("the resource = values %v, owner %s", res.Values, res.Owner)
	}

	// An update changes the port and adds a section; the comments, the port line's spacing and
	// the order stay.
	var r RequestView
	values := map[string]any{"owner": "bob", "server": map[string]any{"host": "web.example.com", "port": "9090"}, "log": map[string]any{"level": "info"}}
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "service", "kind": "update",
		"key": "web", "values": values, "reason": "a new port"}, &r); status != http.StatusCreated {
		t.Fatalf("update = %d %s", status, body)
	}
	bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	if got, _ := file("services/web.ini"); got != "; The web service.\nowner = bob\n\n; Where it listens.\n[server]\nhost = web.example.com\nport=9090\n\n[log]\nlevel = info\n" {
		t.Errorf("the updated file = %q", got)
	}

	// A create writes a new file; a port the schema refuses is refused before anything is
	// written.
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "service", "kind": "create",
		"params": map[string]string{"name": "shop"}, "values": map[string]any{"server": map[string]any{"host": "shop.example.com", "port": "eighty"}},
		"reason": "a shop"}, nil); status != http.StatusUnprocessableEntity {
		t.Errorf("a port outside the schema = %d %s", status, body)
	}
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", map[string]any{"repository": "infra-live", "type": "service", "kind": "create",
		"params": map[string]string{"name": "shop"}, "values": map[string]any{"server": map[string]any{"host": "shop.example.com"}},
		"reason": "a shop"}, &r); status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	if got, ok := file("services/shop.ini"); !ok || got != "owner = bob\n\n[server]\nhost = shop.example.com\n" {
		t.Errorf("the new file = %q", got)
	}
}
