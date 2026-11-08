package repositories

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/connectors"
)

// prefix covers the paths under dir.
func prefix(dir string) func(string) bool {
	return func(p string) bool { return strings.HasPrefix(p, dir) }
}

// countingOwners counts the reads of the platform's owner rules.
type countingOwners struct {
	trustConnector
	reads *int
	err   error
}

func (c countingOwners) Owners() connectors.Owners { return c }
func (c countingOwners) Rules(ctx context.Context, branch string) ([]connectors.OwnerRule, error) {
	*c.reads++
	if c.err != nil {
		return nil, c.err
	}
	return c.trustConnector.Rules(ctx, branch)
}

// TestCodeowners: owners match Quai through the repository's mapping first, then as an email
// address, then through the email the platform gives; the others match no one and are listed as
// the platform writes them.
func TestCodeowners(t *testing.T) {
	d := doc(t, `apiVersion: quai.dev/v1beta1
kind: Repository
metadata: { name: infra }
spec:
  connector: github
  url: https://github.com/acme/infra
  codeowners:
    mapping:
      "@Acme/Platform": group:platform-admins
      bob: user:bob
      erin@acme.ch: user:erin
`)
	d.ApplyDefaults()
	rules := []connectors.OwnerRule{
		{Pattern: "*", Owners: []string{"acme/everyone"}, Match: func(string) bool { return true }},
		{Pattern: "/envs/", Owners: []string{"acme/platform", "@bob", "carol", "Dave@Acme.ch", "erin@acme.ch"}, Match: prefix("envs/")},
		{Pattern: "/envs/prod/", Owners: []string{"ghost", "acme/dba"}, Match: prefix("envs/prod/")},
		{Pattern: "/docs/", Match: prefix("docs/")},
	}
	reads := 0
	platform := countingOwners{trustConnector: trustConnector{platform: "github", rules: rules,
		emails: map[string][]string{"carol": {"Carol@acme.ch", "carol@old.acme.ch"}, "acme/dba": {"dba@acme.ch"}}}, reads: &reads}
	repo := &Repo{Name: "infra", Branch: "main", Document: d, Connector: withCap{platform}}

	c, err := repo.Codeowners(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		paths             []string
		owners, unmatched string
	}{
		{[]string{"README.md"}, "[]", "[@acme/everyone]"},
		{[]string{"envs/dev/vm.yaml"}, "[email:carol@acme.ch email:carol@old.acme.ch email:dave@acme.ch group:platform-admins user:bob user:erin]", "[@acme/everyone]"},
		{[]string{"/envs/prod/vm.yaml"}, "[email:carol@acme.ch email:carol@old.acme.ch email:dave@acme.ch group:platform-admins user:bob user:erin]", "[@acme/dba @acme/everyone @ghost]"},
		{[]string{"docs/a.md", "README.md"}, "[]", "[@acme/everyone]"},
	} {
		got := c.Of(tt.paths...)
		if fmt.Sprint(got.Entries) != tt.owners || fmt.Sprint(got.Unmatched) != tt.unmatched {
			t.Errorf("%v: owners %v, unmatched %v; want %s, %s", tt.paths, got.Entries, got.Unmatched, tt.owners, tt.unmatched)
		}
	}
	// A team's email is not a user's: the platform gives none, and one given is not used.
	if got := c.Of("envs/prod/x"); strings.Contains(fmt.Sprint(got.Entries), "dba@") {
		t.Errorf("a team matched by email: %v", got.Entries)
	}

	// Kept for the same commit, read again for another.
	if _, err := repo.Codeowners(context.Background(), "c1"); err != nil || reads != 1 {
		t.Errorf("same commit: %d reads, %v", reads, err)
	}
	if _, err := repo.Codeowners(context.Background(), "c2"); err != nil || reads != 2 {
		t.Errorf("new commit: %d reads, %v", reads, err)
	}
	// A platform that cannot answer is an error the caller sees.
	repo.owners = nil
	repo.Connector = withCap{countingOwners{trustConnector: trustConnector{platform: "github"}, reads: &reads, err: errors.New("boom")}}
	if _, err := repo.Codeowners(context.Background(), "c3"); !errors.Is(err, ErrOwnersUnavailable) {
		t.Errorf("platform error: %v", err)
	}
	// A nil table names no one.
	var none *Codeowners
	if got := none.Of("a"); got.Entries == nil || got.Unmatched == nil || len(got.Entries)+len(got.Unmatched) != 0 {
		t.Errorf("nil table: %+v", got)
	}
}

// withCap says the platform writes owners in a CODEOWNERS file, with a leading @.
type withCap struct{ connectors.Connector }

func (c withCap) Info() connectors.Info {
	i := c.Connector.Info()
	i.Capabilities = append(i.Capabilities, connectors.CapCodeOwnersFile)
	return i
}

func TestCodeownersMapping(t *testing.T) {
	for name, tt := range map[string]struct{ mapping, want string }{
		"value":     {`{ "@acme/x": "acme-x" }`, `spec.codeowners.mapping["@acme/x"] must be group:<name> or user:<name>`},
		"email":     {`{ "@acme/x": "email:x@acme.ch" }`, "must be group:<name> or user:<name>"},
		"empty":     {`{ "@": "group:x" }`, "an owner's name is empty"},
		"twice":     {`{ "@acme/x": "group:x", "ACME/X": "group:y" }`, "names one owner twice"},
		"no target": {`{ "bob": "user:" }`, "must be group:<name> or user:<name>"},
	} {
		src := "apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: a }\nspec: { connector: gitea, url: x, codeowners: { mapping: " + tt.mapping + " } }\n"
		if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
	d := doc(t, "apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: a }\nspec: { connector: azure-devops, url: x, codeowners: { mapping: { '[Infra]\\Platform': group:platform-admins } } }\n")
	if d.Spec.Codeowners.Mapping[`[Infra]\Platform`] != "group:platform-admins" {
		t.Errorf("mapping = %v", d.Spec.Codeowners.Mapping)
	}
}
