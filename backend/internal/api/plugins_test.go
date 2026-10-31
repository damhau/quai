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
