package auth

import (
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/config"
)

// Directory resolves what a person's IdP groups make them in Quai
// (docs/design/08-identity-access-tenancy.md#directory): their teams, through auth.teams, and their
// grants, through auth.roles, each with the group that gives it. It reads the configuration only,
// so a change of the mapping applies at the next request. The zero Directory maps nothing: each
// group is the team of its name and gives no role.
type Directory struct {
	teams []config.TeamMapping
	roles []config.RoleMapping
}

// NewDirectory returns the directory of a configuration.
func NewDirectory(cfg config.Auth) *Directory {
	return &Directory{teams: cfg.Teams, roles: cfg.Roles}
}

// Team returns the team of a group: the team of the first auth.teams entry that matches it, or,
// when none does, the group's own name.
func (d *Directory) Team(group string) string {
	for _, m := range d.teams {
		if team, ok := teamOf(m, group); ok {
			return team
		}
	}
	return group
}

// teamOf returns the team m maps group to, if m matches it: its group is the group's name, or a
// pattern whose * matches one or more characters, which a * in its team stands for.
func teamOf(m config.TeamMapping, group string) (string, bool) {
	before, after, star := strings.Cut(m.Group, "*")
	if !star {
		return m.Team, group == m.Group
	}
	if len(group) <= len(before)+len(after) || !strings.HasPrefix(group, before) || !strings.HasSuffix(group, after) {
		return "", false
	}
	return strings.Replace(m.Team, "*", group[len(before):len(group)-len(after)], 1), true
}

// Teams returns the teams of groups, sorted, each once.
func (d *Directory) Teams(groups []string) []string {
	if len(groups) == 0 {
		return nil
	}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, d.Team(g))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Granted is a grant and the group that gives it.
type Granted struct {
	authz.Grant
	Group string
}

// Grants returns what groups give, in the order of auth.roles: each grant with each group of
// groups that gives it.
func (d *Directory) Grants(groups []string) []Granted {
	var out []Granted
	for _, m := range d.roles {
		if !slices.Contains(groups, m.Group) {
			continue
		}
		scope := authz.Global
		if kind, n, ok := strings.Cut(m.Scope, ":"); ok {
			scope = authz.Scope{Kind: kind, Name: n}
		}
		g := Granted{Grant: authz.Grant{Role: authz.Role(m.Role), Scope: scope}, Group: m.Group}
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

// ScopeName writes a scope as configuration and the API do: global, or its kind and name.
func ScopeName(s authz.Scope) string {
	if s.Name == "" {
		return s.Kind
	}
	return s.Kind + ":" + s.Name
}
