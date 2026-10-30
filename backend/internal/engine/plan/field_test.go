package plan

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/schema"
)

// quotaType is a quota-increase type on the namespaces another type owns: it writes only quota.
func quotaType(t *testing.T, fsys fstest.MapFS, locate string, params ...string) *definitions.ResourceType {
	t.Helper()
	fsys[".quai/types/quota.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: quai.dev/v1beta1\nkind: ResourceType\nmetadata: { name: quota, title: Quota }\nspec:\n" +
		"  locate: " + locate + "\n  params:\n    - { name: cluster, title: Cluster }\n" + strings.Join(params, "") +
		"  schema: quota.schema.json\n  generate: { kind: identity }\n")}
	fsys["quota.schema.json"] = &fstest.MapFile{Data: []byte(`{"type": "object", "properties": {"quota": {"type": "object", "properties": {"cpu": {"type": "string"}, "memory": {"type": "string"}}}}}`)}
	repo, problems := definitions.Load(fsys)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	return repo.Types["quota"]
}

// TestFieldChanges: a field type writes its fields inside the entry and nothing else; a path it
// does not list is refused, and it creates and deletes nothing.
func TestFieldChanges(t *testing.T) {
	fsys := fstest.MapFS{"clusters/prod-1/namespaces.yaml": {Data: []byte(namespacesFile)}}
	typ := quotaType(t, fsys, "{ strategy: field, path: \"clusters/{cluster}/namespaces.yaml\", collection: namespaces, key: name, keyParam: name, fields: [quota] }",
		"    - { name: name, title: Name }\n")
	cs, err := Type(fsys, typ, generate.Request{Kind: generate.Update, Params: params("prod-1", "orders"), Values: map[string]any{"quota": map[string]any{"cpu": "6", "memory": "8Gi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if c := cs.Changes[0]; string(c.After) != strings.Replace(namespacesFile, `cpu: "4"`, `cpu: "6"`, 1) || len(c.Entries) != 1 || c.Entries[0].Fields[0] != "quota" {
		t.Errorf("update:\n%s", c.After)
	}
	if _, ok := cs.Fingerprints["clusters/prod-1/namespaces.yaml#namespaces[name=orders]{quota}"]; !ok {
		t.Errorf("fingerprints = %v", cs.Fingerprints)
	}

	_, err = Type(fsys, typ, generate.Request{Kind: generate.Update, Params: params("prod-1", "orders"), Values: map[string]any{"quota": map[string]any{"cpu": "6"}, "labels": map[string]any{"team": "x"}}})
	var ve *schema.ValidationError
	if !errors.As(err, &ve) || ve.Fields["labels"] != "type quota may write only quota" {
		t.Errorf("an unlisted path: %v", err)
	}
	for _, kind := range []string{generate.Create, generate.Delete} {
		if _, err := Type(fsys, typ, generate.Request{Kind: kind, Params: params("prod-1", "billing")}); err == nil || !strings.Contains(err.Error(), "it has no "+kind) {
			t.Errorf("%s: %v", kind, err)
		}
	}
	if _, err := Type(fsys, typ, generate.Request{Kind: generate.Update, Params: params("prod-1", "billing"), Values: map[string]any{}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("an entry that does not exist: %v", err)
	}

	// On a new base where the entry's other fields changed, the change is made again on it.
	c := cs.Changes[0]
	moved := strings.Replace(namespacesFile, "  - name: orders\n", "  - name: orders\n    labels: { team: orders }\n", 1)
	out, err := c.On([]byte(moved))
	if want := strings.Replace(moved, `cpu: "4"`, `cpu: "6"`, 1); err != nil || string(out) != want {
		t.Errorf("on a new base: %v\n%s", err, out)
	}

	// The fields of whole files.
	fsys = fstest.MapFS{"clusters/prod-1/namespaces.yaml": {Data: []byte("name: prod-1\nquota: { cpu: \"64\" }\nowner: platform\n")}}
	typ = quotaType(t, fsys, "{ strategy: field, path: \"clusters/{cluster}/namespaces.yaml\", fields: [quota] }")
	cs, err = Type(fsys, typ, generate.Request{Kind: generate.Update, Params: map[string]string{"cluster": "prod-1"}, Values: map[string]any{"quota": map[string]any{"cpu": "96"}}})
	if err != nil || string(cs.Changes[0].After) != "name: prod-1\nquota: { cpu: \"96\" }\nowner: platform\n" {
		t.Errorf("a whole file: %v\n%+v", err, cs)
	}
}
