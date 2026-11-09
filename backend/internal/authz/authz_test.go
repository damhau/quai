package authz

import (
	"context"
	"strings"
	"testing"
)

var (
	repo     = Object{Repository: "infra-live"}
	item     = Object{Repository: "infra-live", CatalogItem: "linux-vm", Entitlement: Entitlement{Visibility: []string{"group:dev*"}, Requesters: []string{"group:developers", "user:zoe"}}}
	resource = Object{Repository: "infra-live", Team: "shop", Owner: "carol"}
	// codeowned is owned through CODEOWNERS (ownership.codeowners).
	codeowned = Object{Repository: "infra-live", Owners: []string{"user:erin", "group:dba", "email:ivy@acme.ch"}}
	request   = Object{Repository: "infra-live", Requester: "bob"}
	// teamOwned is owned through team: entries (ownership.codeowners mapped to a team).
	teamOwned = Object{Repository: "infra-live", Owners: []string{"team:platform"}}
)

// who is a person whose teams are their groups, as without auth.teams.
func who(name string, groups []string, grants ...Grant) Principal {
	return Principal{Name: name, Groups: groups, Teams: groups, Grants: grants}
}

// mapped is a person whose groups auth.teams maps to other teams.
func mapped(name string, groups, teams []string, grants ...Grant) Principal {
	return Principal{Name: name, Groups: groups, Teams: teams, Grants: grants}
}

func at(role Role, kind, name string) Grant {
	return Grant{Role: role, Scope: Scope{Kind: kind, Name: name}}
}

func TestAuthorize(t *testing.T) {
	tests := []struct {
		name   string
		p      Principal
		action Action
		obj    Object
		want   bool
		reason string
	}{
		// Not signed in.
		{"anonymous", Principal{}, ReadCatalog, item, false, "not signed in"},

		// Viewer, by scope.
		{"global viewer reads the catalog", who("vic", []string{"devs"}, at(Viewer, ScopeGlobal, "")), ReadCatalog, item, true, ""},
		{"viewer outside the item's visibility", who("vic", []string{"ops"}, at(Viewer, ScopeGlobal, "")), ReadCatalog, item, false, "not visible"},
		{"repository viewer reads its inventory", who("vic", nil, at(Viewer, ScopeRepository, "infra-live")), ReadInventory, resource, true, ""},
		{"repository viewer, other repository", who("vic", nil, at(Viewer, ScopeRepository, "dns")), ReadInventory, resource, false, "viewer role"},
		{"team viewer reads the team's resources", who("vic", nil, at(Viewer, ScopeTeam, "shop")), ReadInventory, resource, true, ""},
		{"catalog-item viewer", who("vic", []string{"dev"}, at(Viewer, ScopeCatalogItem, "linux-vm")), ReadCatalog, item, true, ""},
		{"nobody without a role", who("nina", []string{"devs"}), ReadCatalog, item, false, "viewer or requester"},
		{"an owner sees their resource", who("carol", nil), ReadInventory, resource, true, ""},
		{"a viewer reads requests", who("vic", nil, at(Viewer, ScopeGlobal, "")), ReadRequest, request, true, ""},
		{"a requester reads their request", who("bob", nil), ReadRequest, request, true, ""},
		{"someone else's request", who("eve", nil), ReadRequest, request, false, "viewer role"},

		// Requester and entitlements.
		{"entitled requester", who("dan", []string{"developers"}, at(Requester, ScopeGlobal, "")), CreateRequest, item, true, ""},
		{"entitled by name", who("zoe", []string{"dev"}, at(Requester, ScopeRepository, "infra-live")), CreateRequest, item, true, ""},
		{"requester not entitled", who("dan", []string{"devops"}, at(Requester, ScopeGlobal, "")), CreateRequest, item, false, "not open"},
		{"entitled but no role", who("dan", []string{"developers"}, at(Viewer, ScopeGlobal, "")), CreateRequest, item, false, "requester role"},
		{"requester reads the catalog", who("dan", []string{"developers"}, at(Requester, ScopeGlobal, "")), ReadCatalog, item, true, ""},
		{"a requester cancels their request", who("bob", nil), CancelRequest, request, true, ""},
		{"another person cannot cancel it", who("eve", nil, at(Operator, ScopeGlobal, "")), CancelRequest, request, false, "only the requester"},
		{"the requester retries", who("bob", nil), RetryRequest, request, true, ""},

		// Reviewing: access only; eligibility is the policy's.
		{"a viewer may review", who("vic", nil, at(Viewer, ScopeGlobal, "")), ReviewRequest, request, true, ""},
		{"no role, no review", who("eve", nil), ReviewRequest, request, false, "access"},

		// Operator: owned resources only.
		{"operator, owner", who("carol", nil, at(Operator, ScopeGlobal, "")), OperateResource, resource, true, ""},
		{"operator, team member", who("sam", []string{"shop"}, at(Operator, ScopeTeam, "shop")), OperateResource, resource, true, ""},
		{"operator, not an owner", who("sam", []string{"blog"}, at(Operator, ScopeGlobal, "")), OperateResource, resource, false, "only the owner"},
		{"owner without the role", who("carol", nil), OperateResource, resource, false, "operator role"},
		{"operator, a codeowner by name", who("erin", nil, at(Operator, ScopeGlobal, "")), OperateResource, codeowned, true, ""},
		{"operator, a codeowner by group", who("sam", []string{"dba"}, at(Operator, ScopeGlobal, "")), OperateResource, codeowned, true, ""},
		{"operator, a codeowner by email", Principal{Name: "ivy", Email: "IVY@acme.ch", Grants: []Grant{at(Operator, ScopeGlobal, "")}}, OperateResource, codeowned, true, ""},
		{"operator, another email", Principal{Name: "ivy", Email: "ivy@acme.com", Grants: []Grant{at(Operator, ScopeGlobal, "")}}, OperateResource, codeowned, false, "only the owner"},
		{"operator, no email", who("ivy", nil, at(Operator, ScopeGlobal, "")), OperateResource, codeowned, false, "only the owner"},
		{"a codeowner sees their resource", who("erin", nil), ReadInventory, codeowned, true, ""},

		// Teams come from auth.teams: a group maps to its team, not to the team of its name.
		{"operator, in the team through auth.teams", mapped("sam", []string{"sg-quai-shop"}, []string{"shop"}, at(Operator, ScopeTeam, "shop")), OperateResource, resource, true, ""},
		{"a group named after a team it is not mapped to", mapped("sam", []string{"shop"}, []string{"retail"}, at(Operator, ScopeGlobal, "")), OperateResource, resource, false, "only the owner"},
		{"operator, a team: owner through auth.teams", mapped("pat", []string{"sg-platform"}, []string{"platform"}, at(Operator, ScopeGlobal, "")), OperateResource, teamOwned, true, ""},
		{"a group: entry is not a team: entry", mapped("pat", []string{"platform"}, []string{"ops"}, at(Operator, ScopeGlobal, "")), OperateResource, teamOwned, false, "only the owner"},

		// Catalog author, repository admin, platform admin.
		{"catalog author previews", who("cat", nil, at(CatalogAuthor, ScopeRepository, "infra-live")), PreviewDefinitions, repo, true, ""},
		{"repository admin previews", who("ray", nil, at(RepositoryAdmin, ScopeRepository, "infra-live")), PreviewDefinitions, repo, true, ""},
		{"viewer cannot preview", who("vic", nil, at(Viewer, ScopeGlobal, "")), PreviewDefinitions, repo, false, "catalog author"},
		{"repository admin manages it", who("ray", nil, at(RepositoryAdmin, ScopeRepository, "infra-live")), ManageRepository, repo, true, ""},
		{"repository admin, other repository", who("ray", nil, at(RepositoryAdmin, ScopeRepository, "dns")), ManageRepository, repo, false, "repository admin"},
		{"repository admin publishes", who("ray", nil, at(RepositoryAdmin, ScopeRepository, "infra-live")), PublishBlueprint, repo, true, ""},
		{"catalog author cannot publish directly", who("cat", nil, at(CatalogAuthor, ScopeGlobal, "")), PublishBlueprint, repo, false, "repository admin"},
		{"only platform admins manage settings", who("ray", nil, at(RepositoryAdmin, ScopeGlobal, "")), ManageSettings, Object{}, false, "platform admin"},
		{"platform admin manages settings", who("ada", nil, at(PlatformAdmin, ScopeGlobal, "")), ManageSettings, Object{}, true, ""},
		{"platform admin operates any resource", who("ada", nil, at(PlatformAdmin, ScopeGlobal, "")), OperateResource, resource, true, ""},
		{"platform admin cancels any request", who("ada", nil, at(PlatformAdmin, ScopeGlobal, "")), CancelRequest, request, true, ""},
		{"repository-scoped platform admin elsewhere", who("ada", nil, at(PlatformAdmin, ScopeRepository, "dns")), ManageRepository, repo, false, "repository admin"},

		// Auditor: reads everything, writes nothing.
		{"auditor reads the audit log", who("aud", nil, at(Auditor, ScopeGlobal, "")), ReadAudit, Object{}, true, ""},
		{"auditor reads any request", who("aud", nil, at(Auditor, ScopeGlobal, "")), ReadRequest, request, true, ""},
		{"auditor sees hidden catalog items", who("aud", []string{"ops"}, at(Auditor, ScopeGlobal, "")), ReadCatalog, item, true, ""},
		{"auditor cannot request", who("aud", []string{"developers"}, at(Auditor, ScopeGlobal, "")), CreateRequest, item, false, "requester role"},
		{"viewer cannot read the audit log", who("vic", nil, at(Viewer, ScopeGlobal, "")), ReadAudit, Object{}, false, "auditor"},
		{"auditor reads the access report", who("aud", nil, at(Auditor, ScopeGlobal, "")), ReadAccessReport, Object{}, true, ""},
		{"platform admin reads the access report", who("ada", nil, at(PlatformAdmin, ScopeGlobal, "")), ReadAccessReport, Object{}, true, ""},
		{"repository admin cannot read the access report", who("ray", nil, at(RepositoryAdmin, ScopeGlobal, "")), ReadAccessReport, Object{}, false, "auditor"},
		{"a team's auditor cannot read the access report", who("aud", nil, at(Auditor, ScopeTeam, "shop")), ReadAccessReport, Object{}, false, "auditor"},

		// Break-glass.
		{"break-glass on someone else's request", who("bg", nil, at(BreakGlass, ScopeGlobal, "")), UseBreakGlass, request, true, ""},
		{"break-glass on one's own request", who("bob", nil, at(BreakGlass, ScopeGlobal, "")), UseBreakGlass, request, false, "your own request"},
		{"platform admin is not break-glass", who("ada", nil, at(PlatformAdmin, ScopeGlobal, "")), UseBreakGlass, request, false, "break-glass role"},
		{"unknown scope kind", who("x", nil, at(Viewer, "planet", "earth")), ReadInventory, resource, false, "viewer role"},

		// API tokens: the owner's roles, narrowed by the token's scopes.
		{"token reads with catalog:read", Principal{Name: "dan", Groups: []string{"developers"}, Grants: []Grant{at(Requester, ScopeGlobal, "")}, Scopes: []string{"catalog:read"}}, ReadCatalog, item, true, ""},
		{"token cannot request without requests:write", Principal{Name: "dan", Groups: []string{"developers"}, Grants: []Grant{at(Requester, ScopeGlobal, "")}, Scopes: []string{"catalog:read"}}, CreateRequest, item, false, "scopes do not allow requests.create"},
		{"token with requests:write requests", Principal{Name: "dan", Groups: []string{"developers"}, Grants: []Grant{at(Requester, ScopeGlobal, "")}, Scopes: []string{"requests:write"}}, CreateRequest, item, true, ""},
		{"a scope does not add a role", Principal{Name: "dan", Groups: []string{"developers"}, Scopes: []string{"requests:write"}}, CreateRequest, item, false, "requester role"},
		{"an admin's token is still scoped", Principal{Name: "ada", Grants: []Grant{at(PlatformAdmin, ScopeGlobal, "")}, Scopes: []string{"catalog:read"}}, ManageSettings, Object{}, false, "scopes"},
		{"tokens cannot make tokens", Principal{Name: "dan", Scopes: []string{"requests:write"}}, ManageOwnTokens, Object{}, false, "scopes"},
		{"an auditor's token cannot read the access report", Principal{Name: "aud", Grants: []Grant{at(Auditor, ScopeGlobal, "")}, Scopes: []string{"requests:read"}}, ReadAccessReport, Object{}, false, "scopes"},
		{"a session manages its tokens", who("dan", nil), ManageOwnTokens, Object{}, true, ""},
	}
	for _, tt := range tests {
		d, err := Authorize(context.Background(), tt.p, tt.action, tt.obj)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if d.Allowed != tt.want || !strings.Contains(d.Reason, tt.reason) || d.Allowed && d.Reason != "" {
			t.Errorf("%s: %+v, want allowed=%v with a reason containing %q", tt.name, d, tt.want, tt.reason)
		}
	}
	if _, err := Authorize(context.Background(), who("x", nil), "fly", Object{}); err == nil {
		t.Error("an unknown action was decided")
	}
}
