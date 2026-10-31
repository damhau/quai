package generate

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damhau/quai/backend/internal/engine/definitions"
)

// vmType is a VM type whose form asks a t-shirt size: the template turns it into cpu and memory,
// derives the hostname from the params, and hides defaults the user never sees.
func vmType(t *testing.T) *definitions.ResourceType {
	t.Helper()
	repo, problems := definitions.Load(fstest.MapFS{
		".quai/types/vm.yaml": {Data: []byte(`apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: vm, title: VM }
spec:
  locate: { strategy: file, path: "envs/{env}/vms/{name}.yaml" }
  params: [{ name: env }, { name: name }]
  schema: vm.schema.json
  inputs: inputs.schema.json
  ownership: { field: owner }
  generate:
    kind: mapping
    template:
      cpu: "${{ inputs.size == 'small' ? 2 : 8 }}"
      memory_gb: "${{ inputs.size == 'small' ? 4 : 32 }}"
      hostname: "${{ params.name }}.${{ params.env }}.example.com"
      backup: { enabled: true, policy: daily }
      description: "${{ inputs.?description }}"
      note: "${{ has(inputs.note) ? inputs.note : null }}"
    read:
      size: "${{ doc.cpu <= 2 ? 'small' : 'large' }}"
      note: "${{ doc.?note }}"
`)},
		"vm.schema.json": {Data: []byte(`{"type": "object", "properties": {"owner": {"type": "string"}, "cpu": {"type": "integer", "maximum": 16},
  "memory_gb": {"type": "integer"}, "hostname": {"type": "string"}, "description": {"type": "string"}, "note": {"type": "string"},
  "backup": {"type": "object"}, "tags": {"type": "object"}}}`)},
		"inputs.schema.json": {Data: []byte(`{"type": "object", "required": ["size"], "properties": {"size": {"enum": ["small", "large"], "default": "small"},
  "description": {"type": "string"}, "note": {"type": "string"}}}`)},
	})
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	return repo.Types["vm"]
}

func TestMappingCreate(t *testing.T) {
	vm := vmType(t)
	params := map[string]string{"env": "prod", "name": "web01"}
	doc, err := Document(vm, Request{Kind: Create, Params: params, Values: map[string]any{}, Owner: "team:shop"})
	want := map[string]any{"owner": "team:shop", "cpu": int64(2), "memory_gb": int64(4), "hostname": "web01.prod.example.com",
		"backup": map[string]any{"enabled": true, "policy": "daily"}}
	if err != nil || !reflect.DeepEqual(doc, want) {
		t.Errorf("a small VM by default:\n%#v, %v", doc, err)
	}
	doc, err = Document(vm, Request{Kind: Create, Params: params, Values: map[string]any{"size": "large", "description": "web", "note": "n"}})
	if err != nil || doc["cpu"] != int64(8) || doc["memory_gb"] != int64(32) || doc["description"] != "web" || doc["note"] != "n" {
		t.Errorf("a large VM: %#v, %v", doc, err)
	}
	if _, err := Document(vm, Request{Kind: Create, Params: params, Values: map[string]any{"size": "huge"}}); err == nil || !strings.Contains(err.Error(), "size") {
		t.Errorf("a size the inputs do not allow: %v", err)
	}
}

// TestMappingUpdate: only the template's paths are managed; a null binding removes its key, an
// absent one leaves it.
func TestMappingUpdate(t *testing.T) {
	vm := vmType(t)
	current := map[string]any{"owner": "team:shop", "cpu": int64(2), "memory_gb": int64(4), "hostname": "web01.prod.example.com",
		"backup": map[string]any{"enabled": false, "policy": "daily", "retention": "7d"}, "description": "kept", "note": "dropped",
		"tags": map[string]any{"cost-center": "42"}}
	doc, err := Document(vm, Request{Kind: Update, Params: map[string]string{"env": "prod", "name": "web01"}, Current: current, Values: map[string]any{"size": "large"}})
	want := map[string]any{"owner": "team:shop", "cpu": int64(8), "memory_gb": int64(32), "hostname": "web01.prod.example.com",
		"backup": map[string]any{"enabled": true, "policy": "daily", "retention": "7d"}, "description": "kept", "tags": map[string]any{"cost-center": "42"}}
	if err != nil || !reflect.DeepEqual(doc, want) {
		t.Errorf("update:\n%#v, %v", doc, err)
	}
	if current["note"] != "dropped" || current["cpu"] != int64(2) {
		t.Error("the update changed the current document")
	}
}

// TestMappingRead: the inputs bound directly are read from where the template writes them, the
// others through the read expressions; writing what was read changes nothing.
func TestMappingRead(t *testing.T) {
	vm := vmType(t)
	params := map[string]string{"env": "prod", "name": "web01"}
	doc := map[string]any{"owner": "team:shop", "cpu": int64(8), "memory_gb": int64(32), "hostname": "web01.prod.example.com",
		"backup": map[string]any{"enabled": true, "policy": "daily"}, "description": "web", "tags": map[string]any{"a": "b"}}
	values, err := Read(vm, doc, params)
	if err != nil || !reflect.DeepEqual(values, map[string]any{"size": "large", "description": "web"}) {
		t.Fatalf("Read() = %#v, %v", values, err)
	}
	again, err := Document(vm, Request{Kind: Update, Params: params, Current: doc, Values: values})
	if err != nil || !reflect.DeepEqual(again, doc) {
		t.Errorf("write(read(doc)) = %#v, %v", again, err)
	}
	if got, _ := Read(appType(t, ""), map[string]any{"replicas": 2}, nil); !reflect.DeepEqual(got, map[string]any{"replicas": 2}) {
		t.Errorf("identity: %v", got)
	}
}
