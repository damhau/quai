package plan

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/generate"
	"github.com/damhau/quai/backend/internal/engine/locate"
	"github.com/damhau/quai/backend/internal/engine/schema"
)

const namespaceSchema = `{
  "type": "object",
  "required": ["team"],
  "properties": {
    "name": { "type": "string", "readOnly": true },
    "owner": { "type": "string" },
    "team": { "type": "string", "pattern": "^[a-z]+$" },
    "cpu": { "type": "string" },
    "memory": { "type": "string" }
  }
}`

const namespaceType = `apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: namespace, title: Namespace }
spec:
  locate: { strategy: dir, path: "teams/{name}" }
  params: [{ name: name, pattern: "[a-z][a-z0-9-]{1,30}" }]
  schema: s/namespace.schema.json
  generate:
    kind: component
    component: k8s.namespace
    with:
      name: ${{ params.name }}
      labels: { team: "${{ inputs.team }}" }
      quota:
        requests.cpu: ${{ inputs.?cpu }}
        requests.memory: ${{ inputs.?memory }}
  ownership: { field: owner }
  paramFields: { name: name }
  actions: [{ name: grow, set: { cpu: "16" } }]
`

const (
	shopRecord = "name: shop\nowner: alice\nteam: shop\ncpu: \"8\"\nmemory: 16Gi\n"
	shopKust   = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - manifests.yaml\n"
	shopNS     = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: shop\n  labels:\n    team: shop\n"
	shopQuota  = "apiVersion: v1\nkind: ResourceQuota\nmetadata:\n  name: quota\n  namespace: shop\nspec:\n  hard:\n    requests.cpu: \"8\"\n    requests.memory: 16Gi\n"
)

func namespaceRepo(t *testing.T, files map[string]string) (fstest.MapFS, *definitions.ResourceType) {
	t.Helper()
	fsys := fstest.MapFS{
		"s/namespace.schema.json":    {Data: []byte(namespaceSchema)},
		".quai/types/namespace.yaml": {Data: []byte(namespaceType)},
	}
	for p, s := range files {
		fsys[p] = &fstest.MapFile{Data: []byte(s)}
	}
	repo, problems := definitions.Load(fsys)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	return fsys, repo.Types["namespace"]
}

// shop is the directory the create below writes, as files of a repository.
func shop() map[string]string {
	return map[string]string{
		"teams/shop/quai.yaml":          shopRecord,
		"teams/shop/kustomization.yaml": shopKust,
		"teams/shop/manifests.yaml":     shopNS + "---\n" + shopQuota,
	}
}

func changes(cs *ChangeSet) map[string]string {
	out := map[string]string{}
	for _, c := range cs.Changes {
		out[c.Path] = c.Op + "\n" + string(c.After)
	}
	return out
}

func TestDirCreate(t *testing.T) {
	fsys, typ := namespaceRepo(t, nil)
	cs, err := Type(fsys, typ, generate.Request{Kind: generate.Create, Params: map[string]string{"name": "shop"}, Owner: "alice",
		Values: map[string]any{"team": "shop", "cpu": "8", "memory": "16Gi"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for p, s := range shop() {
		want[p] = "create\n" + s
	}
	if got := changes(cs); len(got) != 3 || got["teams/shop/quai.yaml"] != want["teams/shop/quai.yaml"] ||
		got["teams/shop/manifests.yaml"] != want["teams/shop/manifests.yaml"] || got["teams/shop/kustomization.yaml"] != want["teams/shop/kustomization.yaml"] {
		t.Errorf("changes =\n%v\nwant\n%v", got, want)
	}
	if cs.Fingerprints["teams/shop"] != locate.Absent || len(cs.Fingerprints) != 1 {
		t.Errorf("fingerprints = %v", cs.Fingerprints)
	}
	if cs.Changes[0].Path != "teams/shop/kustomization.yaml" {
		t.Errorf("changes are not sorted by path: %s first", cs.Changes[0].Path)
	}

	// Without a quota, the namespace alone.
	cs, err = Type(fsys, typ, generate.Request{Kind: generate.Create, Params: map[string]string{"name": "web"}, Values: map[string]any{"team": "web"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := changes(cs)["teams/web/manifests.yaml"]; got != "create\n"+strings.ReplaceAll(shopNS, "shop", "web") {
		t.Errorf("manifests without a quota =\n%s", got)
	}
}

func TestDirUpdate(t *testing.T) {
	files := shop()
	// A comment in the manifests, a resource someone added to the kustomization and a file of
	// their own stay.
	files["teams/shop/manifests.yaml"] = "# Shop's namespace.\n" + shopNS + "---\n" + strings.Replace(shopQuota, "requests.cpu: \"8\"", "requests.cpu: \"8\" # cores", 1)
	files["teams/shop/kustomization.yaml"] = shopKust + "  - extra.yaml\nnamespace: shop\n"
	files["teams/shop/README.md"] = "Shop.\n"
	fsys, typ := namespaceRepo(t, files)
	values := map[string]any{"name": "shop", "owner": "alice", "team": "shop", "cpu": "12", "memory": "16Gi"}
	current := map[string]any{"name": "shop", "owner": "alice", "team": "shop", "cpu": "8", "memory": "16Gi"}
	cs, err := Type(fsys, typ, generate.Request{Kind: generate.Update, Params: map[string]string{"name": "shop"}, Values: values, Current: current})
	if err != nil {
		t.Fatal(err)
	}
	got := changes(cs)
	want := map[string]string{
		"teams/shop/quai.yaml":      "update\n" + strings.Replace(shopRecord, `cpu: "8"`, `cpu: "12"`, 1),
		"teams/shop/manifests.yaml": "update\n" + strings.Replace(files["teams/shop/manifests.yaml"], `requests.cpu: "8"`, `requests.cpu: "12"`, 1),
	}
	if len(got) != 2 || got["teams/shop/quai.yaml"] != want["teams/shop/quai.yaml"] || got["teams/shop/manifests.yaml"] != want["teams/shop/manifests.yaml"] {
		t.Errorf("changes =\n%v\nwant\n%v", got, want)
	}
	fp, _ := locate.Fingerprint(fsys, locate.Region{Strategy: locate.Dir, Path: "teams/shop"})
	if cs.Fingerprints["teams/shop"] != fp || fp == locate.Absent {
		t.Errorf("fingerprints = %v, want the directory's %s", cs.Fingerprints, fp)
	}

	// The values as they are: nothing to change.
	if _, err := Type(fsys, typ, generate.Request{Kind: generate.Update, Params: map[string]string{"name": "shop"}, Values: current}); !errors.Is(err, ErrNoChange) {
		t.Errorf("an update without changes = %v", err)
	}

	// An action sets fields of the record; the manifests follow.
	cs, err = Type(fsys, typ, generate.Request{Kind: generate.Action, ActionName: "grow", Params: map[string]string{"name": "shop"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := changes(cs)["teams/shop/manifests.yaml"]; !strings.Contains(got, `requests.cpu: "16" # cores`) {
		t.Errorf("the action's manifests =\n%s", got)
	}

	// Without the quota's values, the quota goes, with its marker.
	cs, err = Type(fsys, typ, generate.Request{Kind: generate.Update, Params: map[string]string{"name": "shop"},
		Values: map[string]any{"name": "shop", "owner": "alice", "team": "shop"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := changes(cs)["teams/shop/manifests.yaml"]; got != "update\n# Shop's namespace.\n"+shopNS {
		t.Errorf("manifests without a quota =\n%s", got)
	}

	// A kustomization without the manifests lists them again, a missing one is written.
	files = shop()
	files["teams/shop/kustomization.yaml"] = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - other.yaml\n"
	fsys, typ = namespaceRepo(t, files)
	cs, err = Type(fsys, typ, generate.Request{Kind: generate.Update, Params: map[string]string{"name": "shop"}, Values: current})
	if err != nil {
		t.Fatal(err)
	}
	if got := changes(cs); len(got) != 1 || got["teams/shop/kustomization.yaml"] != "update\n"+files["teams/shop/kustomization.yaml"]+"  - manifests.yaml\n" {
		t.Errorf("changes = %v", got)
	}
	files = shop()
	delete(files, "teams/shop/kustomization.yaml")
	fsys, typ = namespaceRepo(t, files)
	cs, err = Type(fsys, typ, generate.Request{Kind: generate.Update, Params: map[string]string{"name": "shop"}, Values: current})
	if err != nil {
		t.Fatal(err)
	}
	if got := changes(cs); len(got) != 1 || got["teams/shop/kustomization.yaml"] != "create\n"+shopKust {
		t.Errorf("changes = %v", got)
	}
}

func TestDirDelete(t *testing.T) {
	files := shop()
	files["teams/shop/docs/README.md"] = "Shop.\n"
	fsys, typ := namespaceRepo(t, files)
	cs, err := Type(fsys, typ, generate.Request{Kind: generate.Delete, Params: map[string]string{"name": "shop"}})
	if err != nil {
		t.Fatal(err)
	}
	var deleted []string
	for _, c := range cs.Changes {
		if c.Op != OpDelete || string(c.Before) != files[c.Path] {
			t.Errorf("change %s %s", c.Op, c.Path)
		}
		deleted = append(deleted, c.Path)
	}
	if strings.Join(deleted, " ") != "teams/shop/docs/README.md teams/shop/kustomization.yaml teams/shop/manifests.yaml teams/shop/quai.yaml" {
		t.Errorf("deleted = %v", deleted)
	}
}

func TestDirRefusals(t *testing.T) {
	files := shop()
	files["teams/bare/manifests.yaml"] = shopNS
	files["teams/broken/quai.yaml"] = "team: [shop\n"
	files["teams/twice/quai.yaml"] = "team: shop\n"
	files["teams/twice/manifests.yaml"] = shopNS + "---\n" + shopNS
	fsys, typ := namespaceRepo(t, files)
	name := func(n string) map[string]string { return map[string]string{"name": n} }
	for _, tt := range []struct {
		name string
		req  generate.Request
		want string
	}{
		{"create over a directory", generate.Request{Kind: generate.Create, Params: name("shop"), Values: map[string]any{"team": "shop"}}, "already exists"},
		{"update of nothing", generate.Request{Kind: generate.Update, Params: name("ghost"), Values: map[string]any{"team": "x"}}, "does not exist"},
		{"no record", generate.Request{Kind: generate.Update, Params: name("bare"), Values: map[string]any{"team": "x"}}, "teams/bare has no quai.yaml, the record of its values"},
		{"a record that does not parse", generate.Request{Kind: generate.Update, Params: name("broken"), Values: map[string]any{"team": "x"}}, "teams/broken/quai.yaml cannot be read"},
		{"a manifest twice", generate.Request{Kind: generate.Update, Params: name("twice"), Values: map[string]any{"team": "x"}}, "teams/twice/manifests.yaml: yamledit: refused: two documents of the file are Namespace shop"},
	} {
		if _, err := Type(fsys, typ, tt.req); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
	// The form's mistakes are the form's, by field.
	_, err := Type(fsys, typ, generate.Request{Kind: generate.Create, Params: name("web"), Values: map[string]any{"team": "Web"}})
	var ve *schema.ValidationError
	if !errors.As(err, &ve) || ve.Fields["team"] == "" {
		t.Errorf("a value the schema refuses = %v", err)
	}
	// What the component refuses is the type's.
	bad := strings.Replace(namespaceType, "name: ${{ params.name }}", "name: ${{ params.name + '_x' }}", 1)
	fsys[".quai/types/namespace.yaml"] = &fstest.MapFile{Data: []byte(bad)}
	repo, problems := definitions.Load(fsys)
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	_, err = Type(fsys, repo.Types["namespace"], generate.Request{Kind: generate.Create, Params: name("web"), Values: map[string]any{"team": "web"}})
	if err == nil || errors.As(err, &ve) || !strings.Contains(err.Error(), `type namespace: component k8s.namespace: with.name must be a DNS label`) {
		t.Errorf("a name the component refuses = %v", err)
	}
}
