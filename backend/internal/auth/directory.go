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
//
// Without auth.teams, each group is the team of its own name. With it, only the groups an entry
// matches are teams (open question 20): a group named like a team (a self-service group, another
// department's) does not make its members that team. A last entry {group: "*", team: "*"} brings
// the fallback back.
type Directory struct {
	teams []config.TeamMapping
	roles []config.RoleMapping
}

// NewDirectory returns the directory of a configuration.
func NewDirectory(cfg config.Auth) *Directory {
	return &Directory{teams: cfg.Teams, roles: cfg.Roles}
}

// Team returns the team of a group: without auth.teams, the group's own name; with it, the team of
// the first entry that matches the group, and none (false) when no entry does.
func (d *Directory) Team(group string) (string, bool) {
	if len(d.teams) == 0 {
		return group, true
	}
	for _, m := range d.teams {
		if team, ok := teamOf(m, group); ok {
			return team, true
		}
	}
	return "", false
}

// teamOf returns the team m maps group to, if m matches it: its group is the group's name, or a
// pattern whose * matches one or more characters, which a * in its team stands for ("*" alone
// matches every group, and a team "*" is then the group's name).
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

// Teams returns the teams of groups, sorted, each once; nil when they make no team.
func (d *Directory) Teams(groups []string) []string {
	var out []string
	for _, g := range groups {
		if team, ok := d.Team(g); ok {
			out = append(out, team)
		}
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
