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
	request  = Object{Repository: "infra-live", Requester: "bob"}
)

func who(name string, groups []string, grants ...Grant) Principal {
	return Principal{Name: name, Groups: groups, Grants: grants}
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

		// Reviewing: access only; eligibility is the policy's.
		{"a viewer may review", who("vic", nil, at(Viewer, ScopeGlobal, "")), ReviewRequest, request, true, ""},
		{"no role, no review", who("eve", nil), ReviewRequest, request, false, "access"},

		// Operator: owned resources only.
		{"operator, owner", who("carol", nil, at(Operator, ScopeGlobal, "")), OperateResource, resource, true, ""},
		{"operator, team member", who("sam", []string{"shop"}, at(Operator, ScopeTeam, "shop")), OperateResource, resource, true, ""},
		{"operator, not an owner", who("sam", []string{"blog"}, at(Operator, ScopeGlobal, "")), OperateResource, resource, false, "only the owner"},
		{"owner without the role", who("carol", nil), OperateResource, resource, false, "operator role"},

		// Catalog author, repository admin, platform admin.
		{"catalog author previews", who("cat", nil, at(CatalogAuthor, ScopeRepository, "infra-live")), PreviewDefinitions, repo, true, ""},
		{"repository admin previews", who("ray", nil, at(RepositoryAdmin, ScopeRepository, "infra-live")), PreviewDefinitions, repo, true, ""},
		{"viewer cannot preview", who("vic", nil, at(Viewer, ScopeGlobal, "")), PreviewDefinitions, repo, false, "catalog author"},
		{"repository admin manages it", who("ray", nil, at(RepositoryAdmin, ScopeRepository, "infra-live")), ManageRepository, repo, true, ""},
		{"repository admin, other repository", who("ray", nil, at(RepositoryAdmin, ScopeRepository, "dns")), ManageRepository, repo, false, "repository admin"},
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

		// Break-glass.
		{"break-glass on someone else's request", who("bg", nil, at(BreakGlass, ScopeGlobal, "")), UseBreakGlass, request, true, ""},
		{"break-glass on one's own request", who("bob", nil, at(BreakGlass, ScopeGlobal, "")), UseBreakGlass, request, false, "your own request"},
		{"platform admin is not break-glass", who("ada", nil, at(PlatformAdmin, ScopeGlobal, "")), UseBreakGlass, request, false, "break-glass role"},
		{"unknown scope kind", who("x", nil, at(Viewer, "planet", "earth")), ReadInventory, resource, false, "viewer role"},
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
