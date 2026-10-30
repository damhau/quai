package edit

import (
	"reflect"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

func TestFormatOf(t *testing.T) {
	for name, want := range map[string]Format{"a/b.yaml": YAML, "b.YML": YAML, "c.tfvars.json": JSON, "d.json": JSON, "e.tfvars": HCL, "f.hcl": HCL} {
		if got, ok := FormatOf(name); !ok || got != want {
			t.Errorf("FormatOf(%q) = %q, %v", name, got, ok)
		}
	}
	if _, ok := FormatOf("g.toml"); ok {
		t.Error("TOML is not edited")
	}
}

// TestEachFormat: one file per format goes through Decode, Edit, Create and Keys by its name;
// errors and refusals come back as such.
func TestEachFormat(t *testing.T) {
	for _, tt := range []struct{ name, src, broken string }{
		{"a.yaml", "cpu: 2\nname: a\n", "cpu: [4\n"},
		{"a.tfvars.json", "{\n  \"cpu\": 2,\n  \"name\": \"a\"\n}\n", `{"cpu": `},
		{"a.tfvars", "cpu  = 2\nname = \"a\"\n", "cpu = \n"},
	} {
		doc, _, err := Decode(tt.name, []byte(tt.src))
		if err != nil || !reflect.DeepEqual(doc, map[string]any{"cpu": int64(2), "name": "a"}) {
			t.Errorf("%s: Decode() = %#v, %v", tt.name, doc, err)
		}
		if _, _, err := Decode(tt.name, []byte(tt.broken)); err == nil {
			t.Errorf("%s: a broken file decodes", tt.name)
		}
		out, err := Edit(tt.name, []byte(tt.src), map[string]any{"cpu": 4, "name": "a"})
		if err != nil || strings.Count(string(out), "4") != 1 || len(out) != len(tt.src) {
			t.Errorf("%s: Edit() = %q, %v", tt.name, out, err)
		}
		created, err := Create(tt.name, map[string]any{"name": "a", "cpu": 2}, []string{"name"})
		if err != nil || !strings.HasPrefix(strings.TrimLeft(string(created), "{\n "), map[string]string{"a.yaml": "name", "a.tfvars.json": `"name"`, "a.tfvars": "name"}[tt.name]) {
			t.Errorf("%s: Create() = %q, %v", tt.name, created, err)
		}
		if keys, err := Keys(tt.name, []byte(tt.src)); err != nil || !reflect.DeepEqual(keys[""], []string{"cpu", "name"}) {
			t.Errorf("%s: Keys() = %v, %v", tt.name, keys, err)
		}
	}
	if _, _, err := Decode("a.toml", nil); err == nil {
		t.Error("an unknown format decodes")
	}
}

func TestCodeAndRefusals(t *testing.T) {
	src := []byte("cpu  = var.cpu\nname = \"a\"\n")
	doc, code, err := Decode("a.tfvars", src)
	if err != nil || len(code) != 1 || Field(code[0].Path) != "cpu" || code[0].Text != "var.cpu" || !reflect.DeepEqual(doc, map[string]any{"name": "a"}) {
		t.Fatalf("Decode() = %#v, %+v, %v", doc, code, err)
	}
	if got := OutsideCode(map[string]string{"cpu": "is required", "name": "too short"}, code); !reflect.DeepEqual(got, map[string]string{"name": "too short"}) {
		t.Errorf("OutsideCode() = %v", got)
	}
	_, err = Edit("a.tfvars", src, map[string]any{"cpu": 4, "name": "a"})
	if at, reason, ok := Refused(err); !ok || Field(at) != "cpu" || !strings.Contains(reason, "defined in code") {
		t.Errorf("Refused() = %v, %q, %v", at, reason, ok)
	}
	if _, _, ok := Refused(nil); ok {
		t.Error("no error is no refusal")
	}
	if f := Field(document.Path{"disks", 0, "size_gb"}); f != "disks.0.size_gb" {
		t.Errorf("Field() = %q", f)
	}
}
