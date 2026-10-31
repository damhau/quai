package lint

import (
	"strings"
	"testing"
	"testing/fstest"
)

// The team type of docs/design/02's schema evolution, before and after: version 1 has size;
// version 2 adds an optional region, renames size to tier and requires it.
const (
	teamV1 = `apiVersion: quai.dev/v1beta1
kind: ResourceType
metadata: { name: team, title: Team, version: 1.0.0 }
spec:
  locate: { strategy: file, path: "teams/{name}.yaml" }
  params: [{ name: name, pattern: "[a-z]+" }]
  paramFields: { name: name }
  schema: schemas/team.schema.json
`
	schemaV1 = `{"type": "object", "additionalProperties": false, "required": ["name"],
  "properties": {"name": {"type": "string"}, "size": {"type": "string", "enum": ["small", "large"]}}}`
	// A new optional field.
	schemaOptional = `{"type": "object", "additionalProperties": false, "required": ["name"],
  "properties": {"name": {"type": "string"}, "size": {"type": "string", "enum": ["small", "large"]}, "region": {"type": "string"}}}`
	// A field renamed, and required.
	schemaV2 = `{"type": "object", "additionalProperties": false, "required": ["name", "tier"],
  "properties": {"name": {"type": "string"}, "tier": {"type": "string", "enum": ["small", "large"]}, "region": {"type": "string"}}}`
	migration = `  migrate: "${{ has(doc.size) ? {'tier': doc.size, 'size': null} : {} }}"
`
)

func teams(typ, schema string, extra map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{
		".quai/types/team.yaml":    {Data: []byte(typ)},
		"schemas/team.schema.json": {Data: []byte(schema)},
		"teams/web.yaml":           {Data: []byte("# The web team.\nname: web\nsize: small\n")},
		"teams/shop.yaml":          {Data: []byte("name: shop\n")},
	}
	for p, s := range extra {
		fsys[p] = &fstest.MapFile{Data: []byte(s)}
	}
	return fsys
}

// TestSchemaEvolution: each row of docs/design/02's schema evolution table, as quai lint sees it
// before the type change merges.
func TestSchemaEvolution(t *testing.T) {
	// The type as it is: every resource conforms.
	if r := Run(teams(teamV1, schemaV1, nil)); len(r.Problems) != 0 || len(r.NonConforming) != 0 || r.Resources != 2 {
		t.Fatalf("v1 = %+v", r)
	}

	// A new optional field: the files stay valid.
	if r := Run(teams(teamV1, schemaOptional, nil)); len(r.Problems) != 0 || len(r.NonConforming) != 0 {
		t.Errorf("an optional field = %+v", r)
	}

	// A new required field (tier), the old one refused: both files become non-conforming, counted
	// per type, each with its problems.
	r := Run(teams(teamV1, schemaV2, nil))
	if r.NonConforming["team"] != 2 || r.Migratable["team"] != 0 || len(r.Problems) != 3 {
		t.Errorf("a required field = %+v", r)
	}
	if got := r.Problems.Error(); !strings.Contains(got, "teams/web.yaml: size") || !strings.Contains(got, "teams/web.yaml: tier") || !strings.Contains(got, "teams/shop.yaml: tier") {
		t.Errorf("problems:\n%s", got)
	}

	// The field renamed, with a migration: the file it fixes is counted, not a problem; the one it
	// does not fix (shop has no size) still is. A golden case checks what the migration writes.
	golden := map[string]string{
		".quai/tests/team/migrate/request.yaml":         "kind: migrate\nkey: web\n",
		".quai/tests/team/migrate/after/teams/web.yaml": "# The web team.\nname: web\ntier: small\n",
	}
	r = Run(teams(teamV1+migration, schemaV2, golden))
	if r.NonConforming["team"] != 2 || r.Migratable["team"] != 1 || len(r.Problems) != 1 || r.Problems[0].Path != "teams/shop.yaml" || r.Tests != 1 {
		t.Errorf("a renamed field = %+v", r)
	}
	golden[".quai/tests/team/migrate/after/teams/web.yaml"] = "name: web\ntier: small\n"
	if r := Run(teams(teamV1+migration, schemaV2, golden)); len(r.Problems) != 2 || !strings.Contains(r.Problems.Error(), ".quai/tests/team/migrate") {
		t.Errorf("a golden case the migration does not match = %v", r.Problems)
	}

	// A major version: a blueprint that pins the old range is a problem until it is updated.
	squad := `apiVersion: quai.dev/v1beta1
kind: Blueprint
metadata: { name: squad, title: Squad }
spec:
  inputs: { type: object }
  resources:
    - { id: team, type: { name: team, version: "^1" }, params: { name: red }, values: { size: small } }
`
	r = Run(teams(strings.Replace(teamV1, "1.0.0", "2.0.0", 1), schemaV1, map[string]string{"blueprints/squad.yaml": squad}))
	if len(r.Problems) != 1 || r.Problems[0].Path != "blueprints/squad.yaml" || !strings.Contains(r.Problems[0].Message, "team: pins type team ^1, and the type is at 2.0.0") {
		t.Errorf("a major version = %v", r.Problems)
	}
	if r := Run(teams(teamV1, schemaV1, map[string]string{"blueprints/squad.yaml": squad})); len(r.Problems) != 0 {
		t.Errorf("in the range = %v", r.Problems)
	}
}
