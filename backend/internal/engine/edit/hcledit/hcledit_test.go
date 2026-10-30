package hcledit

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/engine/edit/document"
)

type p = document.Path

func set(pairs ...any) func(any) any {
	return func(doc any) any {
		for i := 0; i < len(pairs); i += 2 {
			v, err := document.Normalize(pairs[i+1])
			if err != nil {
				panic(err)
			}
			doc = document.With(doc, pairs[i].(p), v)
		}
		return doc
	}
}

func del(paths ...p) func(any) any {
	return func(doc any) any {
		for _, path := range paths {
			doc = document.Without(doc, path)
		}
		return doc
	}
}

const tfvars = `# Production VMs. Reviewed by the platform team.
region = "westeurope"

vms = {
  web01 = {
    cpu       = 4 # bumped in CHG-2291
    memory_gb = 16
    tags      = ["web", "booking"]
  }
  db01 = {
    cpu       = 8
    memory_gb = 64
    disks = [
      { size_gb = 100 },
      { size_gb = 500 },
    ]
  }
}
`

func TestEdit(t *testing.T) {
	for _, tt := range []struct {
		name, src, block string
		edit             func(any) any
		want             string
	}{
		{"a value, its comment kept", tfvars, "", set(p{"vms", "web01", "cpu"}, 8),
			strings.Replace(tfvars, "cpu       = 4 #", "cpu       = 8 #", 1)},
		{"a new key, aligned as terraform fmt does", tfvars, "", set(p{"vms", "web01", "os"}, "rhel-9"),
			strings.Replace(tfvars, "    tags      = [\"web\", \"booking\"]\n", "    tags      = [\"web\", \"booking\"]\n    os        = \"rhel-9\"\n", 1)},
		{"a longer new key realigns its group", tfvars, "", set(p{"vms", "web01", "backup_policy"}, "daily"),
			strings.Replace(tfvars, "    cpu       = 4 # bumped in CHG-2291\n    memory_gb = 16\n    tags      = [\"web\", \"booking\"]\n",
				"    cpu           = 4 # bumped in CHG-2291\n    memory_gb     = 16\n    tags          = [\"web\", \"booking\"]\n    backup_policy = \"daily\"\n", 1)},
		{"a new object entry, on several lines", tfvars, "", set(p{"vms", "app01"}, map[string]any{"cpu": 2, "memory_gb": 8}),
			strings.Replace(tfvars, "      { size_gb = 500 },\n    ]\n  }\n}\n",
				"      { size_gb = 500 },\n    ]\n  }\n  app01 = {\n    cpu       = 2\n    memory_gb = 8\n  }\n}\n", 1)},
		{"an entry removed with its lines, the group realigned", tfvars, "", del(p{"vms", "web01", "memory_gb"}),
			strings.Replace(tfvars, "    cpu       = 4 # bumped in CHG-2291\n    memory_gb = 16\n    tags      = [\"web\", \"booking\"]\n",
				"    cpu  = 4 # bumped in CHG-2291\n    tags = [\"web\", \"booking\"]\n", 1)},
		{"an item appended to a tuple with trailing commas", tfvars, "", set(p{"vms", "db01", "disks", 2}, map[string]any{"size_gb": 50}),
			strings.Replace(tfvars, "      { size_gb = 500 },\n", "      { size_gb = 500 },\n      { size_gb = 50 },\n", 1)},
		{"an item appended to a one-line tuple", tfvars, "", set(p{"vms", "web01", "tags", 2}, "prod"),
			strings.Replace(tfvars, `["web", "booking"]`, `["web", "booking", "prod"]`, 1)},
		{"an item removed from a one-line tuple", tfvars, "", del(p{"vms", "web01", "tags", 0}),
			strings.Replace(tfvars, `["web", "booking"]`, `["booking"]`, 1)},
		{"a key in a one-line object", tfvars, "", set(p{"vms", "db01", "disks", 0, "tier"}, "ssd"),
			strings.Replace(tfvars, "{ size_gb = 100 }", `{ size_gb = 100, tier = "ssd" }`, 1)},
		{"a new top-level attribute, after the last and a blank line like the others", tfvars, "", set(p{"environment"}, "prod"),
			tfvars + "\nenvironment = \"prod\"\n"},
		{"an attribute removed with its own comment", "a = 1\n# about b\nb = 2\nc = 3\n", "", del(p{"b"}), "a = 1\nc = 3\n"},
		{"the first attribute's comment stays", "# header\na = 1\nb = 2\n", "", del(p{"a"}), "# header\nb = 2\n"},
		{"strings that HCL would interpolate", "s = \"x\"\n", "", set(p{"s"}, "${var.y} and %{if}"), "s = \"$${var.y} and %%{if}\"\n"},
		{"negative numbers", "n = -4\n", "", set(p{"n"}, -8), "n = -8\n"},
		{"a value next to one defined in code", "cpu = var.default_cpu\nmem = 4\n", "", set(p{"mem"}, 8),
			"cpu = var.default_cpu\nmem = 8\n"},
		{"a key added to an object holding code", "vm = {\n  cpu = var.cpu\n}\n", "", set(p{"vm", "mem"}, 8),
			"vm = {\n  cpu = var.cpu\n  mem = 8\n}\n"},
		{"a locals block", "variable \"x\" {}\n\nlocals {\n  prefix = \"shop\"\n  name   = \"${local.prefix}-web\"\n}\n", "locals",
			set(p{"prefix"}, "booking", p{"zone"}, "b"),
			"variable \"x\" {}\n\nlocals {\n  prefix = \"booking\"\n  name   = \"${local.prefix}-web\"\n  zone   = \"b\"\n}\n"},
		{"an empty locals block", "locals {}\n", "locals", set(p{"a"}, 1), "locals {\n  a = 1\n}\n"},
		{"an empty file", "", "", set(p{"vms"}, map[string]any{"web01": map[string]any{"cpu": 4}}), "vms = {\n  web01 = {\n    cpu = 4\n  }\n}\n"},
		{"an unformatted file stays as it is", "a   =   1\nb=2\n", "", set(p{"b"}, 3, p{"c"}, 4), "a   =   1\nb=3\nc = 4\n"},
		{"CRLF line endings", "a = 1\r\nb = 2\r\n", "", set(p{"c"}, 3), "a = 1\r\nb = 2\r\nc = 3\r\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			doc, _, err := Decode([]byte(tt.src), tt.block)
			if err != nil {
				t.Fatal(err)
			}
			out, err := Edit([]byte(tt.src), tt.block, tt.edit(doc))
			if err != nil {
				t.Fatalf("Edit() error = %v", err)
			}
			if string(out) != tt.want {
				t.Errorf("Edit() =\n%s\nwant\n%s", out, tt.want)
			}
		})
	}
}

func TestDecode(t *testing.T) {
	src := "a = 1\nb = \"x\"\nc = var.c\nd = { e = true, f = local.f, \"k.k\" = 1.5 }\ng = [1, var.g]\nh = <<EOT\nhi\nEOT\nn = null\n"
	doc, code, err := Decode([]byte(src), "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": int64(1), "b": "x", "d": map[string]any{"e": true, "k.k": 1.5}, "n": nil}
	if !reflect.DeepEqual(doc, want) {
		t.Errorf("Decode() = %#v, want %#v", doc, want)
	}
	var paths []string
	for _, c := range code {
		paths = append(paths, c.String())
	}
	if !reflect.DeepEqual(paths, []string{"$.c", "$.d.f", "$.g", "$.h"}) {
		t.Errorf("code = %v", paths)
	}
}

func TestRefusals(t *testing.T) {
	for _, tt := range []struct {
		name, src, block string
		want             any
		fragment         string
	}{
		{"writing a value defined in code", "cpu = var.cpu\n", "", map[string]any{"cpu": 4}, DefinedInCode},
		{"writing a heredoc", "h = <<EOT\nhi\nEOT\n", "", map[string]any{"h": "ho"}, DefinedInCode},
		{"an object holding code becoming a string", "vm = { cpu = var.cpu, mem = 4 }\n", "", map[string]any{"vm": "x"}, "holds values defined in code"},
		{"a duplicate attribute", "a = 1\na = 2\n", "", map[string]any{}, "defined twice"},
		{"a duplicate object key", "o = { a = 1, a = 2 }\n", "", map[string]any{}, "appears twice"},
		{"no locals block", "a = 1\n", "locals", map[string]any{}, "no locals block"},
		{"two locals blocks", "locals {\n}\nlocals {\n}\n", "locals", map[string]any{}, "2 locals blocks"},
		{"a byte-order mark", "\xef\xbb\xbfa = 1\n", "", map[string]any{}, "byte-order mark"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Edit([]byte(tt.src), tt.block, tt.want)
			var refusal *RefusalError
			if !errors.As(err, &refusal) || !strings.Contains(err.Error(), tt.fragment) {
				t.Errorf("Edit() error = %v, want a refusal containing %q", err, tt.fragment)
			}
		})
	}
	if _, err := Edit([]byte("a = \n"), "", map[string]any{}); err == nil {
		t.Error("invalid HCL")
	}
}

func TestCreate(t *testing.T) {
	out, err := Create(map[string]any{"vms": map[string]any{"web01": map[string]any{"cpu": 4, "tags": []any{"a"}}}, "region": "westeurope"},
		[]string{"region"})
	if err != nil {
		t.Fatal(err)
	}
	want := "region = \"westeurope\"\nvms = {\n  web01 = {\n    cpu  = 4\n    tags = [\"a\"]\n  }\n}\n"
	if string(out) != want {
		t.Errorf("Create() =\n%s\nwant\n%s", out, want)
	}
}

func TestKeys(t *testing.T) {
	got, err := Keys([]byte("b = 1\na = { z = 1, y = { k = 1 } }\nc = var.c\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"": {"b", "a", "c"}, "/a": {"z", "y"}, "/a/y": {"k"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Keys() = %v, want %v", got, want)
	}
}
