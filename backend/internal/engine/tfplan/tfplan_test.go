package tfplan

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/engine/definitions"
)

// The plans in testdata are recorded with testdata/generate.sh.
func plan(t *testing.T, name string, opts Options) []Change {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := Parse(b, opts)
	if err != nil {
		t.Fatal(err)
	}
	return changes
}

func TestParse(t *testing.T) {
	changes := plan(t, "prod.json", Options{})
	want := []Change{
		{Address: `module.vm["api01"].terraform_data.vm`, Action: Create},
		{Address: `module.vm["db01"].terraform_data.vm`, Action: Delete},
		{Address: `module.vm["legacy-ftp"].terraform_data.vm`, Action: Replace, Attributes: []string{"id", "output"}},
		{Address: `module.vm["web01"].terraform_data.vm`, Action: Update, Attributes: []string{"input.cpu", "output"}},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Errorf("changes = %+v", changes)
	}
	if add, change, destroy := Summary(changes); add != 2 || change != 1 || destroy != 2 {
		t.Errorf("summary = +%d ~%d -%d", add, change, destroy)
	}

	// Values on request: the old and new size, and the output known after the apply.
	changes = plan(t, "prod.json", Options{Values: true})
	web := changes[3]
	if v := web.Values["input.cpu"]; v.Before != 4.0 || v.After != 8.0 || v.Unknown {
		t.Errorf("input.cpu = %+v", v)
	}
	if v := web.Values["output"]; !v.Unknown || v.After != nil {
		t.Errorf("output = %+v", v)
	}
	if changes[0].Values != nil {
		t.Errorf("a create keeps values: %+v", changes[0])
	}

	// No-ops are left out; nothing but plans is read.
	if changes := plan(t, "nested-update.json", Options{}); len(changes) != 2 {
		t.Errorf("nested update = %+v", changes)
	}
	for _, bad := range []string{`{}`, `not json`, `{"format_version": "2.0"}`} {
		if _, err := Parse([]byte(bad), Options{}); err == nil {
			t.Errorf("Parse(%s) passed", bad)
		}
	}
}

// TestSensitive: a sensitive attribute is named as changed, but its value is never kept, even
// with values on.
func TestSensitive(t *testing.T) {
	changes := plan(t, "nested-update.json", Options{Values: true})
	db := changes[0]
	if db.Address != `module.app["shop"].module.vm["db"].terraform_data.vm` || !reflect.DeepEqual(db.Attributes, []string{"input.password", "output"}) {
		t.Fatalf("db = %+v", db)
	}
	if _, ok := db.Values["input.password"]; ok {
		t.Errorf("the password's value is kept: %+v", db.Values)
	}
	b, _ := json.Marshal(changes)
	for _, secret := range []string{"fixture-secret-value", "fixture-new-secret"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("%s is in %s", secret, b)
		}
	}
}

func TestMap(t *testing.T) {
	repo, problems := definitions.Load(os.DirFS("../../../../examples/gitops-repo"))
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	claims := Claims(repo.Types)
	m := Map(plan(t, "prod.json", Options{}), "envs/prod", claims)
	var got []string
	for _, r := range m.Resources {
		got = append(got, r.Type+"/"+r.Key+" "+r.Changes[0].Action)
	}
	if want := []string{"vm/prod/api01 create", "vm/prod/db01 delete", "vm/prod/legacy-ftp replace", "vm/prod/web01 update"}; !reflect.DeepEqual(got, want) {
		t.Errorf("resources = %v", got)
	}
	if len(m.Unmapped) > 0 || len(m.Ambiguous) > 0 {
		t.Errorf("unmapped %+v, ambiguous %+v", m.Unmapped, m.Ambiguous)
	}
	// A stack no type plans: nothing is claimed.
	if m := Map(plan(t, "prod.json", Options{}), "platform/dns", claims); len(m.Resources) > 0 || len(m.Unmapped) != 4 {
		t.Errorf("another stack = %+v", m)
	}
}

func resourceType(stack, address, path string) *definitions.ResourceType {
	t := &definitions.ResourceType{}
	t.Spec.Stack, t.Spec.Address, t.Spec.Locate.Path = stack, address, path
	return t
}

// TestMapNested: a module inside a module with for_each keys at both levels; a resource no type
// claims; a change two resources claim.
func TestMapNested(t *testing.T) {
	changes := plan(t, "nested.json", Options{})
	types := map[string]*definitions.ResourceType{
		"app-vm": resourceType("apps/{env}", `module.app["{app}"].module.vm["{vm}"]`, "apps/{env}/{app}/{vm}.yaml"),
		"other":  resourceType("platform", `module.app["{app}"]`, "platform/{app}.yaml"),
	}
	m := Map(changes, "apps/prod", Claims(types))
	if len(m.Resources) != 2 || m.Resources[0].Key != "prod/shop/db" || m.Resources[1].Key != "prod/shop/web" || m.Resources[0].Changes[0].Action != Create {
		t.Errorf("resources = %+v", m.Resources)
	}
	if len(m.Unmapped) != 1 || m.Unmapped[0].Address != "terraform_data.unclaimed" {
		t.Errorf("unmapped = %+v", m.Unmapped)
	}

	// Claimed by the application and by its VM: ambiguous.
	types["app"] = resourceType("apps/{env}", `module.app["{app}"]`, "apps/{env}/{app}.yaml")
	m = Map(changes, "apps/prod", Claims(types))
	if len(m.Resources) != 0 || len(m.Ambiguous) != 2 || !reflect.DeepEqual(m.Ambiguous[0].Resources, []string{"app/prod/shop", "app-vm/prod/shop/db"}) {
		t.Errorf("ambiguous = %+v, resources %+v", m.Ambiguous, m.Resources)
	}
	// A type without an address claims nothing; a param missing from both patterns, nothing either.
	types = map[string]*definitions.ResourceType{
		"no-address": resourceType("apps/{env}", "", "apps/{env}/{app}.yaml"),
		"partial":    resourceType("apps/{env}", `module.app["{app}"]`, "apps/{env}/{app}/{vm}.yaml"),
	}
	if m := Map(changes, "apps/prod", Claims(types)); len(m.Resources) != 0 || len(m.Unmapped) != 3 {
		t.Errorf("unclaimed = %+v", m)
	}
}
