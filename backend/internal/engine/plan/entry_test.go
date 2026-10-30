package plan

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/locate"
)

const namespacesFile = `# Namespaces on prod-1. The platform team reviews quota changes.
namespaces:
  - name: shop
    quota: { cpu: "8", memory: 16Gi }
  # Orders moves to its own cluster in Q3.
  - name: orders
    quota: { cpu: "4", memory: 8Gi }
  - name: search
    quota: { cpu: "2", memory: 4Gi }
`

// entryType returns a type whose entries are in the collection of the files at path; key names a
// list's items, and is empty for a map.
func entryType(t *testing.T, path, collection, key, order string) (*definitions.ResourceType, fstest.MapFS) {
	t.Helper()
	locate := "{ strategy: entry, path: \"" + path + "\", collection: " + collection + ", keyParam: name"
	if key != "" {
		locate += ", key: " + key
	}
	if order != "" {
		locate += ", order: " + order
	}
	fsys := fstest.MapFS{
		".quai/types/entry.yaml": {Data: []byte("apiVersion: quai.dev/v1beta1\nkind: ResourceType\nmetadata: { name: entry, title: Entry }\nspec:\n" +
			"  locate: " + locate + " }\n  params:\n    - { name: cluster, title: Cluster }\n    - { name: name, title: Name }\n" +
			"  schema: entry.schema.json\n  generate: { kind: identity }\n")},
		"entry.schema.json": {Data: []byte(`{"type": "object", "properties": {"name": {"type": "string"}, "type": {"type": "string"}, "ttl": {"type": "integer"},
  "quota": {"type": "object", "properties": {"cpu": {"type": "string"}, "memory": {"type": "string"}}}}}`)},
	}
	repo, problems := definitions.Load(fsys)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	return repo.Types["entry"], fsys
}

func entryPlan(t *testing.T, typ *definitions.ResourceType, fsys fstest.MapFS, r generate.Request) (*ChangeSet, error) {
	t.Helper()
	cs, err := Type(fsys, typ, r)
	if err == nil && (len(cs.Changes) != 1 || len(cs.Changes[0].Entries) != 1) {
		t.Fatalf("changes = %+v", cs.Changes)
	}
	return cs, err
}

func params(cluster, name string) map[string]string {
	return map[string]string{"cluster": cluster, "name": name}
}

// TestEntryChanges: each kind of change to an entry of a list in a shared file, made inside the
// item, every other byte of the file left as it was.
func TestEntryChanges(t *testing.T) {
	quota := func(cpu string) map[string]any {
		return map[string]any{"quota": map[string]any{"cpu": cpu, "memory": "1Gi"}}
	}
	sorted := "namespaces:\n  - name: orders\n    quota: { cpu: \"4\" }\n  - name: shop\n    quota: { cpu: \"8\" }\n"
	tests := []struct {
		name, order string
		r           generate.Request
		want        string
		file        string
	}{
		{"create at the end", "", generate.Request{Kind: generate.Create, Params: params("prod-1", "billing"), Values: quota("1")},
			namespacesFile + "  - name: billing\n    quota: { cpu: \"1\", memory: 1Gi }\n", ""},
		{"create in key order", "key", generate.Request{Kind: generate.Create, Params: params("prod-1", "payments"), Values: quota("1")},
			strings.Replace(sorted, "  - name: shop", "  - name: payments\n    quota: { cpu: \"1\", memory: 1Gi }\n  - name: shop", 1), sorted},
		{"update", "", generate.Request{Kind: generate.Update, Params: params("prod-1", "orders"), Values: map[string]any{"quota": map[string]any{"cpu": "6", "memory": "8Gi"}}},
			strings.Replace(namespacesFile, `cpu: "4"`, `cpu: "6"`, 1), ""},
		{"delete, with its own comments", "", generate.Request{Kind: generate.Delete, Params: params("prod-1", "orders")},
			strings.Replace(namespacesFile, "  # Orders moves to its own cluster in Q3.\n  - name: orders\n    quota: { cpu: \"4\", memory: 8Gi }\n", "", 1), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			typ, fsys := entryType(t, "clusters/{cluster}/namespaces.yaml", "namespaces", "name", tt.order)
			if tt.file == "" {
				tt.file = namespacesFile
			}
			fsys["clusters/prod-1/namespaces.yaml"] = &fstest.MapFile{Data: []byte(tt.file)}
			cs, err := entryPlan(t, typ, fsys, tt.r)
			if err != nil {
				t.Fatal(err)
			}
			c := cs.Changes[0]
			if c.Op != OpUpdate || string(c.Before) != tt.file || string(c.After) != tt.want {
				t.Errorf("%s:\n%s\nwant:\n%s", c.Op, c.After, tt.want)
			}
			ref := "clusters/prod-1/namespaces.yaml#namespaces[name=" + tt.r.Params["name"] + "]"
			if fp, ok := cs.Fingerprints[ref]; !ok || (fp == locate.Absent) != (tt.r.Kind == generate.Create) {
				t.Errorf("fingerprints = %v", cs.Fingerprints)
			}
		})
	}
}

// TestEntryFormats: entries of maps in JSON and HCL, in key order; HCL realigns the neighbours,
// whose content stays.
func TestEntryFormats(t *testing.T) {
	typ, fsys := entryType(t, "dns/{cluster}.json", "spec.zones", "", "key")
	fsys["dns/prod.json"] = &fstest.MapFile{Data: []byte("{\n  \"spec\": {\n    \"zones\": {\n      \"api\": { \"ttl\": 60 },\n      \"www\": { \"ttl\": 300 }\n    }\n  }\n}\n")}
	cs, err := entryPlan(t, typ, fsys, generate.Request{Kind: generate.Create, Params: params("prod", "shop"), Values: map[string]any{"ttl": 120}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"spec\": {\n    \"zones\": {\n      \"api\": { \"ttl\": 60 },\n      \"shop\": { \"ttl\": 120 },\n      \"www\": { \"ttl\": 300 }\n    }\n  }\n}\n"; string(cs.Changes[0].After) != want {
		t.Errorf("JSON:\n%s", cs.Changes[0].After)
	}

	typ, fsys = entryType(t, "dns/{cluster}.tfvars", "records", "", "")
	src := "records = {\n  www = { type = \"CNAME\" }\n  api = { type = \"A\" }\n}\n"
	fsys["dns/prod.tfvars"] = &fstest.MapFile{Data: []byte(src)}
	cs, err = entryPlan(t, typ, fsys, generate.Request{Kind: generate.Create, Params: params("prod", "mail"), Values: map[string]any{"type": "MX"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "records = {\n  www  = { type = \"CNAME\" }\n  api  = { type = \"A\" }\n  mail = { type = \"MX\" }\n}\n"; string(cs.Changes[0].After) != want {
		t.Errorf("HCL:\n%s", cs.Changes[0].After)
	}
	www := locate.EntryRegion("dns/prod.tfvars", "records", "", "www")
	before, _ := locate.Fingerprint(fsys, www)
	fsys["dns/prod.tfvars"] = &fstest.MapFile{Data: cs.Changes[0].After}
	if after, _ := locate.Fingerprint(fsys, www); after != before {
		t.Error("realigning www changed its fingerprint")
	}
	fsys["dns/prod.tfvars"] = &fstest.MapFile{Data: []byte(src)}
	cs, err = entryPlan(t, typ, fsys, generate.Request{Kind: generate.Delete, Params: params("prod", "www")})
	if err != nil || string(cs.Changes[0].After) != "records = {\n  api = { type = \"A\" }\n}\n" {
		t.Errorf("HCL delete: %v\n%s", err, cs.Changes[0].After)
	}
}

// TestEntryFiles: a new entry where the file or the collection does not exist creates them; a
// request on two entries of one file makes one change; a file whose keys repeat is refused.
func TestEntryFiles(t *testing.T) {
	typ, fsys := entryType(t, "clusters/{cluster}/namespaces.yaml", "spec.namespaces", "name", "")
	cs, err := entryPlan(t, typ, fsys, generate.Request{Kind: generate.Create, Params: params("prod-3", "shop"), Values: map[string]any{"quota": map[string]any{"cpu": "1"}}})
	if err != nil || cs.Changes[0].Op != OpCreate || string(cs.Changes[0].After) != "spec:\n  namespaces:\n    - name: shop\n      quota:\n        cpu: \"1\"\n" {
		t.Fatalf("a new file: %v\n%+v", err, cs)
	}
	fsys["clusters/prod-3/namespaces.yaml"] = &fstest.MapFile{Data: []byte("# Empty for now.\nspec: {}\n")}
	if cs, err = entryPlan(t, typ, fsys, generate.Request{Kind: generate.Create, Params: params("prod-3", "shop")}); err != nil || string(cs.Changes[0].After) != "# Empty for now.\nspec: {namespaces: [{name: shop}]}\n" {
		t.Errorf("a new collection: %v\n%s", err, cs.Changes[0].After)
	}

	cs = &ChangeSet{Fingerprints: map[string]string{}}
	for _, name := range []string{"a", "b"} {
		if _, err := addType(cs, fsys, typ, generate.Request{Kind: generate.Create, Params: params("prod-3", name)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(cs.Changes) != 1 || len(cs.Changes[0].Entries) != 2 || !strings.Contains(string(cs.Changes[0].After), "{name: a}, {name: b}") || len(cs.Fingerprints) != 2 {
		t.Errorf("two entries of one file: %+v", cs)
	}
	if _, err := addType(cs, fsys, typ, generate.Request{Kind: generate.Create, Params: params("prod-3", "a")}); err == nil || !strings.Contains(err.Error(), "written twice") {
		t.Errorf("the same entry twice: %v", err)
	}

	fsys["clusters/prod-4/namespaces.yaml"] = &fstest.MapFile{Data: []byte("spec:\n  namespaces:\n    - name: shop\n    - name: shop\n")}
	if _, err := entryPlan(t, typ, fsys, generate.Request{Kind: generate.Create, Params: params("prod-4", "web")}); err == nil || !strings.Contains(err.Error(), "appears twice") {
		t.Errorf("a file whose keys repeat: %v", err)
	}
	if _, err := entryPlan(t, typ, fsys, generate.Request{Kind: generate.Update, Params: params("prod-3", "web")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("an entry that does not exist: %v", err)
	}
}

// TestEntryOnANewBase: delivery makes an entry change again on the file as it is then, keeping
// what changed in the other entries; a whole-file change is made as planned.
func TestEntryOnANewBase(t *testing.T) {
	typ, fsys := entryType(t, "clusters/{cluster}/namespaces.yaml", "namespaces", "name", "")
	fsys["clusters/prod-1/namespaces.yaml"] = &fstest.MapFile{Data: []byte(namespacesFile)}
	shop, err := entryPlan(t, typ, fsys, generate.Request{Kind: generate.Update, Params: params("prod-1", "shop"), Values: map[string]any{"quota": map[string]any{"cpu": "16", "memory": "16Gi"}}})
	if err != nil {
		t.Fatal(err)
	}
	billing, err := entryPlan(t, typ, fsys, generate.Request{Kind: generate.Create, Params: params("prod-1", "billing")})
	if err != nil {
		t.Fatal(err)
	}
	// The entry changes travel through the database as JSON.
	c := billing.Changes[0]
	b, _ := json.Marshal(c.Entries)
	if c.Entries, err = DecodeEntries(b); err != nil {
		t.Fatal(err)
	}
	if out, err := c.On(c.Before); err != nil || string(out) != string(c.After) {
		t.Errorf("on the planned file: %v\n%s", err, out)
	}
	out, err := c.On(shop.Changes[0].After)
	if want := strings.Replace(namespacesFile, `cpu: "8"`, `cpu: "16"`, 1) + "  - name: billing\n"; err != nil || string(out) != want {
		t.Errorf("on a new base: %v\n%s", err, out)
	}
	if _, err := c.On([]byte("namespaces:\n  - name: billing\n")); !errors.Is(err, ErrExists) {
		t.Errorf("created meanwhile: %v", err)
	}
	whole := Change{Path: "a.yaml", Op: OpUpdate, Before: []byte("a: 1\n"), After: []byte("a: 2\n")}
	if out, err := whole.On([]byte("a: 3\n")); err != nil || string(out) != "a: 2\n" {
		t.Errorf("a whole file: %v %s", err, out)
	}
}

// TestMappingEntries: the mapping generator writes entries too: a size becomes a quota, the
// item's key field is Quai's, and an update leaves what the template does not manage.
func TestMappingEntries(t *testing.T) {
	fsys := fstest.MapFS{
		".quai/types/ns.yaml": {Data: []byte("apiVersion: quai.dev/v1beta1\nkind: ResourceType\nmetadata: { name: ns, title: Namespace }\nspec:\n" +
			"  locate: { strategy: entry, path: \"clusters/{cluster}/namespaces.yaml\", collection: namespaces, key: name, keyParam: name }\n" +
			"  params: [{ name: cluster }, { name: name }]\n  schema: ns.schema.json\n  inputs: size.schema.json\n" +
			"  generate:\n    kind: mapping\n    template: { quota: { cpu: \"${{ inputs.size == 'small' ? '2' : '8' }}\" } }\n")},
		"ns.schema.json":                  {Data: []byte(`{"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}, "quota": {"type": "object"}, "labels": {"type": "object"}}}`)},
		"size.schema.json":                {Data: []byte(`{"type": "object", "additionalProperties": false, "properties": {"size": {"enum": ["small", "large"], "default": "small"}}}`)},
		"clusters/prod-1/namespaces.yaml": {Data: []byte("namespaces:\n  - name: shop\n    labels: { team: shop }\n    quota: { cpu: \"2\" }\n")},
	}
	repo, problems := definitions.Load(fsys)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	ns := repo.Types["ns"]
	cs, err := Type(fsys, ns, generate.Request{Kind: generate.Create, Params: params("prod-1", "billing"), Values: map[string]any{"size": "large"}})
	if err != nil || !strings.HasSuffix(string(cs.Changes[0].After), "  - name: billing\n    quota: { cpu: \"8\" }\n") {
		t.Errorf("create: %v\n%s", err, cs.Changes[0].After)
	}
	cs, err = Type(fsys, ns, generate.Request{Kind: generate.Update, Params: params("prod-1", "shop"), Values: map[string]any{"size": "large"}})
	if err != nil || string(cs.Changes[0].After) != "namespaces:\n  - name: shop\n    labels: { team: shop }\n    quota: { cpu: \"8\" }\n" {
		t.Errorf("update: %v\n%s", err, cs.Changes[0].After)
	}
}
