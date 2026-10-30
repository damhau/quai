package locate

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// TestFieldRegions: a field region is the listed fields of an entry, or of a whole file; its
// fingerprint changes with those fields only; its reference names them.
func TestFieldRegions(t *testing.T) {
	fsys := entryFS()
	quota, err := NewField("clusters/{cluster}/namespaces.yaml", "namespaces", "name", "name", []string{"quota"})
	if err != nil {
		t.Fatal(err)
	}
	regions, _ := quota.Scan(fsys)
	if len(regions) != 3 || regions[0].Strategy != Field || regions[0].Key != "prod-1/orders" || !reflect.DeepEqual(regions[0].Fields, []string{"quota"}) {
		t.Fatalf("Scan() = %+v", regions)
	}
	orders := regions[0]
	if ref := orders.Ref(); ref != "clusters/prod-1/namespaces.yaml#namespaces[name=orders]{quota}" {
		t.Errorf("Ref() = %s", ref)
	}
	if r := ParseRef(fsys, orders.Ref()); r.Strategy != Field || r.Item != "orders" || r.KeyField() != "name" || !reflect.DeepEqual(r.Fields, []string{"quota"}) {
		t.Errorf("ParseRef() = %+v", r)
	}
	if p := RefPath(orders.Ref()); p != "clusters/prod-1/namespaces.yaml" {
		t.Errorf("RefPath() = %s", p)
	}
	before, err := FingerprintRef(fsys, orders.Ref())
	if err != nil || before == Absent {
		t.Fatalf("fingerprint = %s, %v", before, err)
	}
	file := fsys["clusters/prod-1/namespaces.yaml"]
	file.Data = []byte(strings.Replace(namespaces, "  - name: orders\n", "  - name: orders\n    labels: { team: orders }\n", 1))
	if now, _ := Fingerprint(fsys, orders); now != before {
		t.Error("a new field beside quota changed the fingerprint")
	}
	file.Data = []byte(strings.Replace(namespaces, `cpu: "4"`, `cpu: "6"`, 1))
	if now, _ := Fingerprint(fsys, orders); now == before {
		t.Error("a change to quota left the fingerprint")
	}
	file.Data = []byte(strings.Replace(namespaces, "    quota: { cpu: \"4\", memory: 8Gi }\n", "", 1))
	if now, _ := Fingerprint(fsys, orders); now == before || now == Absent {
		t.Errorf("without quota: %s", now)
	}
	file.Data = []byte(namespaces)

	doc := map[string]any{"namespaces": []any{map[string]any{"name": "orders", "quota": map[string]any{"cpu": "4"}, "labels": map[string]any{"a": "b"}}}}
	got, _, err := Document(doc, nil, orders)
	if err != nil || !reflect.DeepEqual(got, map[string]any{"quota": map[string]any{"cpu": "4"}}) {
		t.Errorf("Document() = %v, %v", got, err)
	}
	put, err := Put(doc, orders, map[string]any{})
	if want := map[string]any{"namespaces": []any{map[string]any{"name": "orders", "labels": map[string]any{"a": "b"}}}}; err != nil || !reflect.DeepEqual(put, want) {
		t.Errorf("Put() = %v, %v", put, err)
	}
	if u := Unlisted([]string{"quota", "spec.backup"}, map[string]any{"quota": 1, "spec": map[string]any{"backup": true, "size": 2}, "labels": 1}); !reflect.DeepEqual(u, []string{"labels", "spec.size"}) {
		t.Errorf("Unlisted() = %v", u)
	}

	// The fields of whole files.
	fsys["vms/web01.yaml"] = &fstest.MapFile{Data: []byte("cpu: 4\nbackup: { enabled: false }\n")}
	backup, err := NewField("vms/{name}.yaml", "", "", "", []string{"backup.enabled"})
	if err != nil {
		t.Fatal(err)
	}
	regions, _ = backup.Scan(fsys)
	if len(regions) != 1 || regions[0].Key != "web01" || regions[0].Ref() != "vms/web01.yaml{backup.enabled}" {
		t.Fatalf("whole files = %+v", regions)
	}
	before, _ = Fingerprint(fsys, regions[0])
	fsys["vms/web01.yaml"].Data = []byte("cpu: 8\nbackup: { enabled: false }\n")
	if now, _ := FingerprintRef(fsys, regions[0].Ref()); now != before {
		t.Error("a change to cpu changed the fingerprint of backup.enabled")
	}
	if _, err := NewField("vms/{name}.yaml", "", "", "name", []string{"x"}); err == nil {
		t.Error("a key param without a collection")
	}
}
