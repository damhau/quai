package plan

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/locate"
)

// TestEntryFormatChanges: create, update and delete on entries of JSON arrays and objects, of an
// HCL map in a *.tfvars file and of a map in the locals block of a *.tf file; each change made
// inside its entry, the rest of the file as it was.
func TestEntryFormatChanges(t *testing.T) {
	jsonList := "{\n  \"records\": [\n    { \"name\": \"api\", \"ttl\": 60 },\n    { \"name\": \"www\", \"ttl\": 300 }\n  ]\n}\n"
	jsonMap := "{\"zones\": {\"api\": {\"ttl\": 60}, \"www\": {\"ttl\": 300}}}\n"
	tfvars := "# DNS records of prod.\nrecords = {\n  api = {\n    type = \"A\"\n    ttl  = var.default_ttl\n  }\n  www = { type = \"CNAME\", ttl = 300 }\n}\n"
	locals := "variable \"env\" {}\n\nlocals {\n  prefix = \"${var.env}-dns\"\n  records = {\n    api = { type = \"A\" }\n  }\n}\n"
	tests := []struct {
		name, path, collection, key, file string
		r                                 generate.Request
		want                              string
	}{
		{"JSON list create", "dns/{cluster}.json", "records", "name", jsonList,
			generate.Request{Kind: generate.Create, Params: params("prod", "mail"), Values: map[string]any{"ttl": 30}},
			strings.Replace(jsonList, "300 }\n", "300 },\n    { \"name\": \"mail\", \"ttl\": 30 }\n", 1)},
		{"JSON list update", "dns/{cluster}.json", "records", "name", jsonList,
			generate.Request{Kind: generate.Update, Params: params("prod", "api"), Values: map[string]any{"ttl": 120}},
			strings.Replace(jsonList, "60", "120", 1)},
		{"JSON list delete", "dns/{cluster}.json", "records", "name", jsonList,
			generate.Request{Kind: generate.Delete, Params: params("prod", "api")},
			"{\n  \"records\": [\n    { \"name\": \"www\", \"ttl\": 300 }\n  ]\n}\n"},
		{"JSON map update", "dns/{cluster}.json", "zones", "", jsonMap,
			generate.Request{Kind: generate.Update, Params: params("prod", "www"), Values: map[string]any{"ttl": 30}},
			strings.Replace(jsonMap, "300", "30", 1)},
		{"JSON map delete", "dns/{cluster}.json", "zones", "", jsonMap,
			generate.Request{Kind: generate.Delete, Params: params("prod", "api")},
			"{\"zones\": {\"www\": {\"ttl\": 300}}}\n"},
		{"tfvars update around code", "dns/{cluster}.tfvars", "records", "", tfvars,
			generate.Request{Kind: generate.Update, Params: params("prod", "api"), Values: map[string]any{"type": "AAAA"}},
			strings.Replace(tfvars, `"A"`, `"AAAA"`, 1)},
		{"tfvars delete", "dns/{cluster}.tfvars", "records", "", tfvars,
			generate.Request{Kind: generate.Delete, Params: params("prod", "www")},
			strings.Replace(tfvars, "  www = { type = \"CNAME\", ttl = 300 }\n", "", 1)},
		{"locals create", "dns/{cluster}.tf", "records", "", locals,
			generate.Request{Kind: generate.Create, Params: params("prod", "www"), Values: map[string]any{"type": "CNAME"}},
			strings.Replace(locals, "    api = { type = \"A\" }\n", "    api = { type = \"A\" }\n    www = { type = \"CNAME\" }\n", 1)},
		{"locals update", "dns/{cluster}.tf", "records", "", locals,
			generate.Request{Kind: generate.Update, Params: params("prod", "api"), Values: map[string]any{"type": "AAAA", "ttl": 60}},
			strings.Replace(locals, "{ type = \"A\" }", "{ type = \"AAAA\", ttl = 60 }", 1)},
		{"locals delete", "dns/{cluster}.tf", "records", "", locals,
			generate.Request{Kind: generate.Delete, Params: params("prod", "api")},
			// records fits on one line now: terraform fmt aligns it with prefix.
			"variable \"env\" {}\n\nlocals {\n  prefix  = \"${var.env}-dns\"\n  records = {}\n}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			typ, fsys := entryType(t, tt.path, tt.collection, tt.key, "")
			file := strings.Replace(tt.path, "{cluster}", "prod", 1)
			fsys[file] = &fstest.MapFile{Data: []byte(tt.file)}
			cs, err := entryPlan(t, typ, fsys, tt.r)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(cs.Changes[0].After); got != tt.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tt.want)
			}
			region := locate.EntryRegion(file, tt.collection, tt.key, tt.r.Params["name"])
			if fp, _ := locate.Fingerprint(fsys, region); cs.Fingerprints[region.Ref()] != fp {
				t.Errorf("fingerprints = %v, want %s at %s", cs.Fingerprints, fp, region.Ref())
			}
		})
	}
}

// TestEntryNewTerraformFile: a new entry in a *.tf file that does not exist writes a locals
// block; a *.tf file without one cannot hold entries.
func TestEntryNewTerraformFile(t *testing.T) {
	typ, fsys := entryType(t, "dns/{cluster}.tf", "records", "", "")
	cs, err := entryPlan(t, typ, fsys, generate.Request{Kind: generate.Create, Params: params("prod", "www"), Values: map[string]any{"type": "CNAME", "ttl": 300}})
	if want := "locals {\n  records = {\n    www = {\n      type = \"CNAME\"\n      ttl  = 300\n    }\n  }\n}\n"; err != nil || string(cs.Changes[0].After) != want {
		t.Errorf("a new file: %v\n%s", err, cs.Changes[0].After)
	}
	fsys["dns/dev.tf"] = &fstest.MapFile{Data: []byte("variable \"env\" {}\n")}
	if _, err := entryPlan(t, typ, fsys, generate.Request{Kind: generate.Create, Params: params("dev", "www")}); err == nil || !strings.Contains(err.Error(), "no locals block") {
		t.Errorf("a file without locals: %v", err)
	}
}
