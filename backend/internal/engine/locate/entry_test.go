package locate

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

const namespaces = `# Namespaces on prod-1. The platform team reviews quota changes.
namespaces:
  - name: shop
    quota: { cpu: "8", memory: 16Gi }
  - name: orders
    quota: { cpu: "4", memory: 8Gi }
`

func entryFS() fstest.MapFS {
	return fstest.MapFS{
		"clusters/prod-1/namespaces.yaml": {Data: []byte(namespaces)},
		"clusters/prod-2/namespaces.yaml": {Data: []byte("namespaces:\n  - name: shop\n    quota: { cpu: \"2\" }\n  - name: shop\n    quota: { cpu: \"4\" }\n")},
		"clusters/dev/namespaces.yaml":    {Data: []byte("other: true\n")},
		"zones/prod.json":                 {Data: []byte("{\n  \"spec\": {\n    \"zones\": {\n      \"shop.example.com\": { \"ttl\": 300 },\n      \"api.example.com\": { \"ttl\": 60 }\n    }\n  }\n}\n")},
		"dns/prod.tfvars":                 {Data: []byte("records = {\n  www = { type = \"CNAME\" }\n  api = { type = \"A\" }\n}\n")},
	}
}

// TestEntries: the entries of a list keyed by a field, of a map in JSON under a nested path, and
// of an HCL map; a file whose keys repeat lists them with its problem; a file without the
// collection has none.
func TestEntries(t *testing.T) {
	fsys := entryFS()
	ns, err := NewEntry("clusters/{cluster}/namespaces.yaml", "namespaces", "name", "name")
	if err != nil {
		t.Fatal(err)
	}
	regions, err := ns.Scan(fsys)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range regions {
		got = append(got, r.Key+" "+r.Params["cluster"]+"/"+r.Params["name"]+" "+r.Problem)
	}
	want := []string{"prod-1/orders prod-1/orders ", "prod-1/shop prod-1/shop ", `prod-2/shop prod-2/shop the key "shop" appears twice in namespaces`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Scan() = %q, want %q", got, want)
	}

	zones, _ := NewEntry("zones/{env}.json", "spec.zones", "", "zone")
	if regions, _ := zones.Scan(fsys); len(regions) != 2 || regions[0].Key != "prod/api.example.com" || regions[0].Collection.String() != "$.spec.zones" {
		t.Errorf("JSON map entries = %+v", regions)
	}
	dns, _ := NewEntry("dns/{env}.tfvars", "records", "", "name")
	if regions, _ := dns.Scan(fsys); len(regions) != 2 || regions[1].Key != "prod/www" {
		t.Errorf("HCL map entries = %+v", regions)
	}
	if _, problem := ns.Elements("x.yaml", []byte("namespaces: { a: 1 }\n")); !strings.Contains(problem, "is not a list") {
		t.Errorf("a map where a list is expected: %q", problem)
	}
	if _, problem := ns.Elements("x.yaml", []byte("namespaces:\n  - quota: 1\n")); !strings.Contains(problem, "has no valid name") {
		t.Errorf("an item without its key: %q", problem)
	}
	if _, problem := ns.Elements("x.yaml", []byte("namespaces: [\n")); !strings.Contains(problem, "cannot be read") {
		t.Errorf("a broken file: %q", problem)
	}
}

// TestEntryRegions: an entry's key adds its key param to its file's; its fingerprint is its own
// bytes, which a change to another entry leaves alone.
func TestEntryRegions(t *testing.T) {
	fsys := entryFS()
	ns, _ := NewEntry("clusters/{cluster}/namespaces.yaml", "namespaces", "name", "name")
	params, err := ns.ParseKey("prod-1/orders")
	if err != nil || !reflect.DeepEqual(params, map[string]string{"cluster": "prod-1", "name": "orders"}) {
		t.Fatalf("ParseKey() = %v, %v", params, err)
	}
	r, err := ns.Region(params)
	if err != nil || r.Path != "clusters/prod-1/namespaces.yaml" || r.Item != "orders" || r.Key != "prod-1/orders" {
		t.Fatalf("Region() = %+v, %v", r, err)
	}
	b, err := Bytes(fsys, r)
	if err != nil || string(b) != "- name: orders\n    quota: { cpu: \"4\", memory: 8Gi }" {
		t.Errorf("Bytes() = %q, %v", b, err)
	}
	before, _ := Fingerprint(fsys, r)
	fsys["clusters/prod-1/namespaces.yaml"].Data = []byte(strings.Replace(namespaces, `cpu: "8"`, `cpu: "16"`, 1))
	if after, _ := Fingerprint(fsys, r); after != before || before == Absent {
		t.Errorf("a change to shop changed orders: %s, %s", before, after)
	}
	fsys["clusters/prod-1/namespaces.yaml"].Data = []byte(strings.Replace(namespaces, `cpu: "4"`, `cpu: "6"`, 1))
	if after, _ := Fingerprint(fsys, r); after == before {
		t.Error("a change to orders left its fingerprint")
	}
	gone, _ := ns.Region(map[string]string{"cluster": "prod-1", "name": "billing"})
	if fp, err := Fingerprint(fsys, gone); fp != Absent || err != nil {
		t.Errorf("an entry that does not exist = %s, %v", fp, err)
	}
	dup, _ := ns.Region(map[string]string{"cluster": "prod-2", "name": "shop"})
	if _, err := Fingerprint(fsys, dup); err == nil || !strings.Contains(err.Error(), "appears twice") {
		t.Errorf("an entry whose key repeats: %v", err)
	}
	if _, err := NewEntry("clusters/{name}/ns.yaml", "namespaces", "name", "name"); err == nil {
		t.Error("the key param in the path")
	}
}
