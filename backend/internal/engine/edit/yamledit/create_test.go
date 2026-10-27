package yamledit

import (
	"reflect"
	"strings"
	"testing"
)

func TestCreate(t *testing.T) {
	doc := map[string]any{
		"tags":    map[string]any{"cost_center": "4711", "app": "shop"},
		"network": map[string]any{"ip": "10.0.20.15", "vlan": 120},
		"disks":   []any{map[string]any{"tier": "ssd", "size_gb": 80}},
		"cpu":     4,
		"owner":   "alice",
		"note":    "a: b",
		"empty":   map[string]any{},
		"flow":    []any{},
	}
	out, err := Create(doc, []string{"owner", "cpu", "disks", "network", "tags", "vlan", "ip", "size_gb", "tier", "cpu"})
	if err != nil {
		t.Fatal(err)
	}
	want := `owner: alice
cpu: 4
disks:
  - size_gb: 80
    tier: ssd
network:
  vlan: 120
  ip: 10.0.20.15
tags:
  app: shop
  cost_center: "4711"
empty: {}
flow: []
note: "a: b"
`
	if string(out) != want {
		t.Errorf("Create =\n%s\nwant\n%s", out, want)
	}
	sorted, err := Create(map[string]any{"b": 1, "a": 2}, nil)
	if err != nil || string(sorted) != "a: 2\nb: 1\n" {
		t.Errorf("Create without an order = %q, %v", sorted, err)
	}
	if out, err := Create("scalar", nil); err != nil || string(out) != "scalar\n" {
		t.Errorf("Create(scalar) = %q, %v", out, err)
	}
	if _, err := Create(map[string]any{"f": func() {}}, nil); err == nil {
		t.Error("Create accepted a value outside the data model")
	}
}

func TestKeys(t *testing.T) {
	src := `# a schema
type: object
properties:
  owner: { type: string }
  disks:
    type: array
    items: { properties: { size_gb: {}, tier: {} } }
  "a/b~c": {}
base: &b { x: 1 }
merged:
  <<: *b
  y: 2
list: [{ k: 1, j: 2 }]
`
	got, err := Keys([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"":                                   {"type", "properties", "base", "merged", "list"},
		"/properties":                        {"owner", "disks", "a/b~c"},
		"/properties/owner":                  {"type"},
		"/properties/disks":                  {"type", "items"},
		"/properties/disks/items":            {"properties"},
		"/properties/disks/items/properties": {"size_gb", "tier"},
		"/properties/disks/items/properties/size_gb": {},
		"/properties/disks/items/properties/tier":    {},
		"/properties/a~1b~0c":                        {},
		"/base":                                      {"x"},
		"/merged":                                    {"y"},
		"/list/0":                                    {"k", "j"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Keys =\n%v\nwant\n%v", got, want)
	}
	if got, err := Keys(nil); err != nil || len(got) != 0 {
		t.Errorf("Keys(empty) = %v, %v", got, err)
	}
	if _, err := Keys([]byte("a: [1\n")); err == nil || !strings.Contains(err.Error(), "not valid YAML") {
		t.Errorf("Keys(invalid) error = %v", err)
	}
}
