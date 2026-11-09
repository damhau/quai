package scim

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// TestParseFilter: the filters Microsoft Entra ID sends, and nothing else.
func TestParseFilter(t *testing.T) {
	t.Parallel()
	ok := []struct {
		filter string
		want   []clause
	}{
		{`userName eq "alice@contoso.com"`, []clause{{"username", "alice@contoso.com"}}},
		{`USERNAME EQ "Alice@Contoso.com"`, []clause{{"username", "Alice@Contoso.com"}}},
		{`externalId eq "5a1c\"x"`, []clause{{"externalid", `5a1c"x`}}},
		{`  id eq "1" and manager eq "2"  `, []clause{{"id", "1"}, {"manager", "2"}}},
		{`id eq "1"  AND  manager eq ""`, []clause{{"id", "1"}, {"manager", ""}}},
		{`userName eq "a b"`, []clause{{"username", "a b"}}},
	}
	for _, tt := range ok {
		got, err := parseFilter(tt.filter, "username", "externalid", "id", "manager")
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s = %+v %v, want %+v", tt.filter, got, err, tt.want)
		}
	}
	for _, f := range []string{
		``, ` `, `userName`, `userName eq`, `userName eq alice`, `userName ne "a"`, `userName co "a"`, `userName sw "a"`,
		`userName pr`, `title eq "a"`, `name.familyName eq "a"`, `emails[type eq "work"].value eq "a"`,
		`userName eq "a" or id eq "1"`, `not (userName eq "a")`, `(userName eq "a")`, `userName eq "a" and userName eq "b"`,
		`userName eq "a"and id eq "1"`, `userName eq "a" and`, `userName eq "a" id eq "1"`, `userName eq "a`, `userName eq "\x"`,
		`userName eq "a\u0000"`, "userName eq \"a\tb\"", `userName eq 'a'`, `userName eq 1`, `userName eq true`, `userName eq null`,
		"userName\teq \"a\"",
		`id eq "1" and manager eq "2" and userName eq "3" and externalId eq "4" and displayName eq "5"`,
		strings.Repeat(" ", maxFilter) + `id eq "1"`,
	} {
		if got, err := parseFilter(f, "username", "externalid", "id", "manager"); err == nil {
			t.Errorf("%q = %+v, want an error", f, got)
		}
	}
	if _, err := parseFilter(`a eq "1" and b eq "2" and c eq "3" and d eq "4" and e eq "5"`, "a", "b", "c", "d", "e"); err == nil {
		t.Error("five comparisons")
	}
}

// FuzzParseFilter: whatever the filter, the parser does not panic, and what it accepts compares
// allowed attributes, each once, to values without control characters.
func FuzzParseFilter(f *testing.F) {
	for _, s := range []string{`userName eq "a"`, `id eq "1" and members eq "2"`, `externalId eq "é\"\\"`, `x`, `userName eq "`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := parseFilter(s, "username", "id", "members")
		if err != nil {
			return
		}
		if len(got) == 0 || len(got) > maxClauses || len(s) > maxFilter {
			t.Fatalf("%q = %+v", s, got)
		}
		seen := map[string]bool{}
		for _, c := range got {
			if seen[c.attr] || (c.attr != "username" && c.attr != "id" && c.attr != "members") || strings.ContainsFunc(c.value, unicode.IsControl) {
				t.Fatalf("%q = %+v", s, got)
			}
			seen[c.attr] = true
		}
	})
}

// TestDecodeObject: one object, its keys folded, none twice, a bounded number of them.
func TestDecodeObject(t *testing.T) {
	t.Parallel()
	o, err := decodeObject([]byte(` {"userName": "a", "Name": {"givenName": "b"}, "ACTIVE": "False"} `))
	if err != nil || string(o["username"]) != `"a"` || string(o["active"]) != `"False"` || o["name"] == nil {
		t.Fatalf("object = %v %v", o, err)
	}
	keys := make([]string, maxKeys+1)
	for i := range keys {
		keys[i] = `"k` + strings.Repeat("x", i) + `": 1`
	}
	for _, raw := range []string{``, `[]`, `"a"`, `{`, `{"a": 1} {}`, `{"a": 1}]`, `{"a": 1, "A": 2}`, `{"a": }`, `{1: 2}`,
		`{` + strings.Join(keys, ",") + `}`} {
		if o, err := decodeObject([]byte(raw)); err == nil {
			t.Errorf("%.40s = %v, want an error", raw, o)
		}
	}
}

// TestValues: the attributes' values, as Entra ID sends them.
func TestValues(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{`true`: true, `false`: false, `"True"`: true, `"False"`: false, `"FALSE"`: false} {
		if got, err := boolean("active", json.RawMessage(raw)); err != nil || got != want {
			t.Errorf("boolean %s = %v %v", raw, got, err)
		}
	}
	for _, raw := range []string{`"yes"`, `1`, `null`, `"t"`, `["true"]`} {
		if _, err := boolean("active", json.RawMessage(raw)); err == nil {
			t.Errorf("boolean %s: no error", raw)
		}
	}
	for raw, want := range map[string]string{`"m"`: "m", `{"value": "m"}`: "m", `[{"$ref": null, "value": "m"}]`: "m", `{"VALUE": "m"}`: "m",
		`null`: "", `""`: "", `[]`: "", `{"value": null}`: ""} {
		if got, err := reference("manager", json.RawMessage(raw)); err != nil || got != want {
			t.Errorf("reference %s = %q %v", raw, got, err)
		}
	}
	for _, raw := range []string{`[{"value": "a"}, {"value": "b"}]`, `1`, `{"value": 1}`, `"` + strings.Repeat("m", maxString+1) + `"`, `"a\nb"`} {
		if got, err := reference("manager", json.RawMessage(raw)); err == nil {
			t.Errorf("reference %.40s = %q", raw, got)
		}
	}
	if got, err := text("userName", json.RawMessage(`"`+strings.Repeat("é", maxString)+`"`)); err != nil || utf8.RuneCountInString(got) != maxString {
		t.Errorf("a long name = %v", err)
	}
	if got, err := references("members", json.RawMessage(`[{"$ref": null, "value": "a"}, {"value": "b"}]`)); err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("references = %v %v", got, err)
	}
	many := "[" + strings.TrimSuffix(strings.Repeat(`{"value": "a"},`, maxMembers+1), ",") + "]"
	for _, raw := range []string{many, `{"value": "a"}`, `["a"]`, `[{"value": 1}]`, `[{}]`} {
		if got, err := references("members", json.RawMessage(raw)); err == nil {
			t.Errorf("references %.40s = %v", raw, got)
		}
	}
}

// TestParsePatch: Entra ID's PATCH bodies, op in any case; the rest refused.
func TestParsePatch(t *testing.T) {
	t.Parallel()
	parse := func(raw string) ([]operation, error) {
		o, err := decodeObject([]byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		return parsePatch(o)
	}
	ops, err := parse(`{"schemas": ["` + schemaPatch + `"], "Operations": [
		{"op": "Replace", "path": "active", "value": "False"},
		{"op": "Add", "path": "members", "value": [{"$ref": null, "value": "1"}]},
		{"op": "remove", "path": "members[value eq \"1\"]"},
		{"op": "replace", "value": {"displayName": "x"}}]}`)
	if err != nil || len(ops) != 4 || ops[0].op != "replace" || ops[1].op != "add" || ops[2].value != nil || ops[3].path != "" {
		t.Fatalf("ops = %+v %v", ops, err)
	}
	many := strings.TrimSuffix(strings.Repeat(`{"op": "remove", "path": "members"},`, maxOperations+1), ",")
	for _, raw := range []string{
		`{"Operations": [{"op": "remove", "path": "members"}]}`,
		`{"schemas": ["urn:x"], "Operations": [{"op": "remove", "path": "members"}]}`,
		`{"schemas": ["` + schemaPatch + `"], "Operations": []}`,
		`{"schemas": ["` + schemaPatch + `"]}`,
		`{"schemas": ["` + schemaPatch + `"], "Operations": [` + many + `]}`,
		`{"schemas": ["` + schemaPatch + `"], "Operations": [{"op": "copy", "path": "a"}]}`,
		`{"schemas": ["` + schemaPatch + `"], "Operations": [{"op": "add", "path": "a"}]}`,
		`{"schemas": ["` + schemaPatch + `"], "Operations": [{"op": "replace", "path": "a", "value": null}]}`,
		`{"schemas": ["` + schemaPatch + `"], "Operations": [{"op": "add", "path": "a", "value": 1, "from": "b"}]}`,
		`{"schemas": ["` + schemaPatch + `"], "Operations": [{"op": "add", "path": "a\nb", "value": 1}]}`,
		`{"schemas": ["` + schemaPatch + `"], "Operations": [{"op": "add", "path": "` + strings.Repeat("a", maxPath+1) + `", "value": 1}]}`,
		`{"schemas": ["` + schemaPatch + `"], "Operations": ["add"]}`,
		`{"schemas": ["` + schemaPatch + `"], "Operations": [], "id": "1"}`,
	} {
		if ops, err := parse(raw); err == nil {
			t.Errorf("%.80s = %+v, want an error", raw, ops)
		}
	}
}

// TestPaths: attribute paths without their schema, operations without a path spread, the
// members a path's filter names.
func TestPaths(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{"userName": "username", schemaUser + ":Active": "active",
		strings.ToUpper(schemaEnterprise) + ":manager": "manager", `emails[type eq "work"].value`: `emails[type eq "work"].value`} {
		if got := attribute(path, schemaUser); got != want {
			t.Errorf("attribute %s = %q, want %q", path, got, want)
		}
	}
	spread, err := pathless(operation{op: "replace", value: json.RawMessage(`{"Active": false, "` + schemaEnterprise + `": {"Manager": "m"}, "displayName": "x"}`)})
	if err != nil || len(spread) != 3 {
		t.Fatalf("pathless = %+v %v", spread, err)
	}
	paths := map[string]string{}
	for _, o := range spread {
		paths[attribute(o.path, schemaUser)] = string(o.value)
	}
	if !reflect.DeepEqual(paths, map[string]string{"active": "false", "manager": `"m"`, "displayname": `"x"`}) {
		t.Errorf("pathless = %v", paths)
	}
	if _, err := pathless(operation{op: "replace", value: json.RawMessage(`"x"`)}); err == nil {
		t.Error("pathless with a string")
	}
	for path, want := range map[string]string{`members[value eq "1"]`: "1", `Members[VALUE EQ "2"]`: "2"} {
		if id, is, err := memberFilter(path); err != nil || !is || id != want {
			t.Errorf("memberFilter %s = %q %v %v", path, id, is, err)
		}
	}
	if _, is, err := memberFilter("members"); is || err != nil {
		t.Error("members is not a filter")
	}
	for _, path := range []string{`members[value eq "1"`, `members[display eq "1"]`, `members[value eq "1" and value eq "2"]`, `members[value co "1"]`} {
		if _, is, err := memberFilter(path); !is || err == nil {
			t.Errorf("memberFilter %s: no error", path)
		}
	}
}
