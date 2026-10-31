package plan

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/generate/plugin"
	"github.com/damhau/quai/backend/internal/engine/generate/plugin/plugintest"
	"github.com/damhau/quai/backend/internal/engine/schema"
)

const webSettings = "# The web team's settings.\ncolour = blue\nowner = alice\n"

// settingsRepo is a repository with the settings type, whose files the guest plugin writes, and
// settings/web.conf; config is added to the type's spec.generate.
func settingsRepo(t *testing.T, config string) (fstest.MapFS, *definitions.ResourceType) {
	t.Helper()
	module := plugintest.Build(t, "guest")
	typ := plugintest.Type(".quai/plugins/settings.wasm", plugintest.Digest(module))
	if config != "" {
		typ = strings.Replace(typ, "    digest:", "    config: "+config+"\n    digest:", 1)
	}
	fsys := fstest.MapFS{
		".quai/plugins/settings.wasm":          {Data: module},
		".quai/types/settings.yaml":            {Data: []byte(typ)},
		"catalog/schemas/settings.schema.json": {Data: []byte(plugintest.Schema)},
		"settings/web.conf":                    {Data: []byte(webSettings)},
	}
	repo, problems := definitions.Load(fsys)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	return fsys, repo.Types["settings"]
}

// TestPluginChanges: a type with the plugin generator creates, updates, acts on and deletes its
// files through its plugin, which keeps what it does not manage; the values are the identity
// generator's: the owner on create, the action's fields, the schema checked.
func TestPluginChanges(t *testing.T) {
	fsys, typ := settingsRepo(t, "")
	web := map[string]string{"name": "web"}
	plan := func(r generate.Request) map[string]string {
		t.Helper()
		cs, err := Type(fsys, typ, r)
		if err != nil {
			t.Fatal(err)
		}
		return changes(cs)
	}

	got := plan(generate.Request{Kind: generate.Create, Params: map[string]string{"name": "shop"}, Values: map[string]any{"colour": "green"}, Owner: "bob"})
	if got["settings/shop.conf"] != "create\ncolour = green\nowner = bob\n" || len(got) != 1 {
		t.Errorf("create = %v", got)
	}
	got = plan(generate.Request{Kind: generate.Update, Params: web, Values: map[string]any{"colour": "red", "owner": "alice", "size": "big"}})
	if got["settings/web.conf"] != "update\n# The web team's settings.\ncolour = red\nowner = alice\nsize = big\n" {
		t.Errorf("update = %q", got["settings/web.conf"])
	}
	got = plan(generate.Request{Kind: generate.Action, Params: web, ActionName: "paint", ActionInputs: map[string]any{"colour": "green"}})
	if got["settings/web.conf"] != "update\n# The web team's settings.\ncolour = green\nowner = alice\n" {
		t.Errorf("paint = %q", got["settings/web.conf"])
	}
	cs, err := Type(fsys, typ, generate.Request{Kind: generate.Delete, Params: web})
	if err != nil || len(cs.Changes) != 1 || cs.Changes[0].Op != OpDelete || string(cs.Changes[0].Before) != webSettings {
		t.Errorf("delete = %v, %v", cs, err)
	}

	if _, err := Type(fsys, typ, generate.Request{Kind: generate.Update, Params: web, Values: map[string]any{"colour": "blue", "owner": "alice"}}); !errors.Is(err, ErrNoChange) {
		t.Errorf("an update to the same values: err = %v", err)
	}
	var ve *schema.ValidationError
	if _, err := Type(fsys, typ, generate.Request{Kind: generate.Update, Params: web, Values: map[string]any{"colour": "pink"}}); !errors.As(err, &ve) || ve.Fields["colour"] == "" {
		t.Errorf("a value outside the schema: err = %v", err)
	}
}

// TestPluginRefusals: Quai writes nothing when the plugin cannot read the file, does not read
// back what it wrote, or is not the module the type pins.
func TestPluginRefusals(t *testing.T) {
	web := map[string]string{"name": "web"}
	update := generate.Request{Kind: generate.Update, Params: web, Values: map[string]any{"colour": "red"}}

	fsys, typ := settingsRepo(t, "")
	fsys["settings/web.conf"] = &fstest.MapFile{Data: []byte("colour: blue\n")}
	if _, err := Type(fsys, typ, update); err == nil || !strings.Contains(err.Error(), "settings/web.conf cannot be read, so Quai will not edit it") || !strings.Contains(err.Error(), "line 1: expected key = value") {
		t.Errorf("a file the plugin refuses: err = %v", err)
	}

	fsys, typ = settingsRepo(t, "{ mode: lose }")
	if _, err := Type(fsys, typ, update); err == nil || err.Error() != "plugin .quai/plugins/settings.wasm does not read back what it wrote: colour" {
		t.Errorf("a plugin that loses a value: err = %v", err)
	}

	// The module in the repository at request time must be the one the type pins.
	fsys, typ = settingsRepo(t, "")
	fsys[".quai/plugins/settings.wasm"] = &fstest.MapFile{Data: plugintest.Build(t, "forbidden")}
	if _, err := Type(fsys, typ, update); !errors.Is(err, plugin.ErrDigest) {
		t.Errorf("another module at the type's path: err = %v", err)
	}
}
