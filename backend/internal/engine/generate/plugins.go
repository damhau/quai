package generate

import (
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/edit"
	"github.com/damhau/quai/backend/internal/engine/edit/document"
	"github.com/damhau/quai/backend/internal/engine/generate/plugin"
)

// Module returns the plugin of a type with the plugin generator, from fsys, the repository's
// files, in the process's runtime: it is refused unless it is the module the type pins.
func Module(fsys fs.FS, t *definitions.ResourceType) (*plugin.Module, error) {
	g := t.Spec.Generate
	return plugin.Default().Load(fsys, g.Module, g.Digest)
}

// Decode returns the document of a resource's file, name in fsys, whose content is src: for a
// type with the plugin generator, the values its plugin reads from it; for the others, the
// file's content as its editor decodes it (edit.Decode).
func Decode(fsys fs.FS, t *definitions.ResourceType, name string, params map[string]string, src []byte) (any, []edit.Code, error) {
	if t.Spec.Generate.Kind != definitions.GeneratePlugin {
		return edit.Decode(name, src)
	}
	m, err := Module(fsys, t)
	if err != nil {
		return nil, nil, err
	}
	values, err := m.Read(src, params, t.Spec.Generate.Config)
	return values, nil, err
}

// Plugin returns a resource's file after a request, for a type with the plugin generator: the
// values the identity generator makes of the request (Current is what the plugin read from the
// file), written by the plugin into current, the file before the request (nil on create). The
// plugin must read back from what it wrote the values it was given. It returns nil for a delete.
func Plugin(fsys fs.FS, t *definitions.ResourceType, r Request, current []byte) ([]byte, error) {
	values, err := Identity(t, r)
	if err != nil || values == nil {
		return nil, err
	}
	m, err := Module(fsys, t)
	if err != nil {
		return nil, err
	}
	cfg := t.Spec.Generate.Config
	out, err := m.Write(values, current, r.Params, cfg)
	if err != nil {
		return nil, err
	}
	back, err := m.Read(out, r.Params, cfg)
	if err != nil {
		return nil, fmt.Errorf("plugin %s cannot read what it wrote: %w", t.Spec.Generate.Module, err)
	}
	want, err := document.Normalize(values)
	if err != nil {
		return nil, err
	}
	if !document.Equal(want, back) {
		return nil, fmt.Errorf("plugin %s does not read back what it wrote: %s", t.Spec.Generate.Module, strings.Join(differing(want.(map[string]any), back), ", "))
	}
	return out, nil
}

// differing names the values that two documents do not hold alike.
func differing(want, got map[string]any) []string {
	var keys []string
	for _, k := range slices.Sorted(maps.Keys(want)) {
		if v, ok := got[k]; !ok || !document.Equal(want[k], v) {
			keys = append(keys, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(got)) {
		if _, ok := want[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}
