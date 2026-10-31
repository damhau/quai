package plan

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/damhau/quai/backend/internal/engine/definitions"
	"github.com/damhau/quai/backend/internal/engine/generate"
)

// The team type of the schema evolution tests: version 2 renamed size to tier, which it requires,
// and ships a migration from the old name.
const (
	teamType = `apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: team, title: Team, version: 2.0.0 }
spec:
  locate: { strategy: file, path: "teams/{name}.yaml" }
  params: [{ name: name, pattern: "[a-z]+" }]
  paramFields: { name: name }
  schema: schemas/team.schema.json
  migrate: "${{ has(doc.size) ? {'tier': doc.size, 'size': null} : {} }}"
`
	teamSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "tier"],
  "properties": {
    "name": { "type": "string" },
    "tier": { "type": "string", "enum": ["small", "large"] },
    "region": { "type": "string" }
  }
}`
	squad = `apiVersion: quai.dev/v1beta1
kind: Blueprint
metadata: { name: squad, title: Squad, version: 1.0.0 }
spec:
  inputs: { type: object, properties: { name: { type: string } } }
  resources:
    - { id: team, type: { name: team, version: ">=1.0 <2" }, params: { name: "${{ inputs.name }}" }, values: { tier: small } }
`
)

func evolutionRepo(t *testing.T, files map[string]string) (fstest.MapFS, *definitions.Repository) {
	t.Helper()
	fsys := fstest.MapFS{
		".quai/types/team.yaml":    {Data: []byte(teamType)},
		"schemas/team.schema.json": {Data: []byte(teamSchema)},
		// web uses the old name, shop an old value the new schema refuses, ops is already on the
		// new schema.
		"teams/web.yaml":  {Data: []byte("# The web team.\nname: web\nsize: small\n")},
		"teams/shop.yaml": {Data: []byte("name: shop\nsize: huge\n")},
		"teams/ops.yaml":  {Data: []byte("name: ops\ntier: large\n")},
	}
	for p, s := range files {
		fsys[p] = &fstest.MapFile{Data: []byte(s)}
	}
	repo, problems := definitions.Load(fsys)
	if len(problems) > 0 {
		t.Fatalf("problems:\n%v", problems)
	}
	return fsys, repo
}

// TestMigrate: a migrate request applies the type's migration to its non-conforming resources, in
// one change set; those it does not fix stay out of it, with their reasons (docs/design/02,
// schema evolution).
func TestMigrate(t *testing.T) {
	fsys, repo := evolutionRepo(t, nil)
	team := repo.Types["team"]
	cs, left, err := Migrate(fsys, team, map[string]map[string]string{
		"web": {"name": "web"}, "shop": {"name": "shop"}, "ops": {"name": "ops"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Changes) != 1 || cs.Changes[0].Path != "teams/web.yaml" || cs.Changes[0].Op != OpUpdate {
		t.Fatalf("changes = %+v", cs.Changes)
	}
	if got := string(cs.Changes[0].After); got != "# The web team.\nname: web\ntier: small\n" {
		t.Errorf("the migrated file = %q", got)
	}
	if _, ok := cs.Fingerprints["teams/shop.yaml"]; ok || len(cs.Fingerprints) != 1 || cs.Hash == "" {
		t.Errorf("fingerprints = %v, hash %q", cs.Fingerprints, cs.Hash)
	}
	if len(left) != 2 || left[0].Key != "ops" || left[0].Reason != "the migration does not change it" ||
		left[1].Key != "shop" || !strings.Contains(left[1].Reason, "tier") {
		t.Errorf("left = %+v", left)
	}

	// A migration that fixes nothing has nothing to request.
	if _, left, err := Migrate(fsys, team, map[string]map[string]string{"shop": {"name": "shop"}}); !errors.Is(err, ErrNoChange) || len(left) != 1 {
		t.Errorf("only shop: %v, %+v", err, left)
	}
	// A type without a migration has none to apply.
	plain := *team
	plain.Spec.MigrateTemplate = nil
	if _, _, err := Migrate(fsys, &plain, map[string]map[string]string{"web": {"name": "web"}}); err == nil || !strings.Contains(err.Error(), "no migration") {
		t.Errorf("without a migration: %v", err)
	}
	// What the migration gives must be a merge patch.
	if _, err := generate.Migrated(withMigrate(t, `${{ 'tier' }}`), map[string]any{"name": "web"}, nil); err == nil || !strings.Contains(err.Error(), "not a merge patch") {
		t.Errorf("a string: %v", err)
	}
}

func withMigrate(t *testing.T, migrate string) *definitions.ResourceType {
	t.Helper()
	_, repo := evolutionRepo(t, map[string]string{".quai/types/team.yaml": strings.Replace(teamType,
		`"${{ has(doc.size) ? {'tier': doc.size, 'size': null} : {} }}"`, `"`+migrate+`"`, 1)})
	return repo.Types["team"]
}

// TestPins: a blueprint that pins a range of a type's versions is refused when the type leaves it,
// a new major version, with the pin and the type's version; one in the range plans.
func TestPins(t *testing.T) {
	fsys, repo := evolutionRepo(t, map[string]string{"blueprints/squad.yaml": squad})
	b := repo.Blueprints["squad"]
	if got := b.Pins(repo); len(got) != 1 || got[0] != "team: pins type team >=1.0 <2, and the type is at 2.0.0" {
		t.Errorf("pins = %q", got)
	}
	_, err := Blueprint(fsys, repo, b, BlueprintRequest{Name: "red", Inputs: map[string]any{"name": "red"}, Owner: "alice", Repository: "infra-live"})
	var re *ResourceError
	if !errors.As(err, &re) || re.ID != "team" || !strings.Contains(err.Error(), "the type is at 2.0.0: the blueprint must be updated for it") {
		t.Errorf("a request outside the pin = %v", err)
	}

	fsys, repo = evolutionRepo(t, map[string]string{"blueprints/squad.yaml": strings.Replace(squad, `">=1.0 <2"`, `"^2"`, 1)})
	b = repo.Blueprints["squad"]
	if got := b.Pins(repo); len(got) != 0 {
		t.Errorf("pins = %q", got)
	}
	if _, err := Blueprint(fsys, repo, b, BlueprintRequest{Name: "red", Inputs: map[string]any{"name": "red"}, Owner: "alice", Repository: "infra-live"}); err != nil {
		t.Errorf("a request in the pin = %v", err)
	}
}
