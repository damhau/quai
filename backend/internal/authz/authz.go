// Package authz answers every access question with one function, Authorize
// (docs/design/08-identity-access-tenancy.md#authorization). Roles are granted to IdP groups at a
// scope: global, a team, a repository or a catalog item; the caller resolves a person's grants
// and passes them in the Principal. Whether someone may approve a request is not a role: approval
// rules decide it (internal/engine/policy).
//
// Every API handler and job calls Authorize; a test in internal/api fails the build when a
// handler does not.
package authz

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
)

// Roles.
type Role string

// The roles of docs/design/08.
const (
	Viewer          Role = "viewer"
	Requester       Role = "requester"
	Operator        Role = "operator"
	CatalogAuthor   Role = "catalog-author"
	RepositoryAdmin Role = "repository-admin"
	PlatformAdmin   Role = "platform-admin"
	Auditor         Role = "auditor"
	BreakGlass      Role = "break-glass"
)

// Scope kinds.
const (
	ScopeGlobal      = "global"
	ScopeTeam        = "team"
	ScopeRepository  = "repository"
	ScopeCatalogItem = "catalog-item"
)

// Scope is where a role applies. Name is empty for the global scope.
type Scope struct {
	Kind string
	Name string
}

// Global is the scope of everything.
var Global = Scope{Kind: ScopeGlobal}

// Grant gives a role at a scope.
type Grant struct {
	Role  Role
	Scope Scope
}

// Principal is who asks: a person, or a token acting for one.
type Principal struct {
	Name   string
	Groups []string // IdP groups; a team is the group of the same name
	Grants []Grant
	// Scopes narrow what an API token may do (TokenScopes); nil for a browser session.
	Scopes []string
}

// TokenScopes are the scopes an API token can have, and the actions each allows.
var TokenScopes = map[string][]Action{
	"catalog:read":   {ReadCatalog},
	"inventory:read": {ReadInventory},
	"requests:read":  {ReadRequest},
	"requests:write": {ReadRequest, CreateRequest, CancelRequest, RetryRequest, ReviewRequest, OperateResource},
}

// Action is what the principal wants to do.
type Action string

// Actions.
const (
	ReadCatalog        Action = "catalog.read"
	ReadInventory      Action = "inventory.read"
	ReadRequest        Action = "requests.read"
	CreateRequest      Action = "requests.create"
	ReviewRequest      Action = "requests.review"
	CancelRequest      Action = "requests.cancel"
	RetryRequest       Action = "requests.retry"
	OperateResource    Action = "resources.operate"
	PreviewDefinitions Action = "definitions.preview"
	ManageRepository   Action = "repositories.manage"
	ManageSettings     Action = "settings.manage"
	ReadAudit          Action = "audit.read"
	UseBreakGlass      Action = "requests.break-glass"
	ManageOwnTokens    Action = "tokens.manage"
)

// Object is what the action is on. Fields that do not apply are empty.
type Object struct {
	Repository  string
	CatalogItem string
	// Team and Owner own the object: a resource's owning team (team:<name> owners), or user.
	Team  string
	Owner string
	// Requester made the request the object is, or belongs to.
	Requester string
	// Entitlement is who a catalog item lets see it and request it.
	Entitlement Entitlement
}

// Entitlement lists, as group patterns (group:developers, group:data-*), who may see a catalog
// item and who may request it. An empty list does not restrict.
type Entitlement struct {
	Visibility []string
	Requesters []string
}

// Decision is the answer, with the reason when it is no.
type Decision struct {
	Allowed bool
	Reason  string
}

func allow() Decision { return Decision{Allowed: true} }

func deny(format string, args ...any) Decision {
	return Decision{Reason: fmt.Sprintf(format, args...)}
}

// Authorize decides whether p may do a on o. The error is for failures to decide, never for a
// denial, which is a Decision with a reason.
func Authorize(_ context.Context, p Principal, a Action, o Object) (Decision, error) {
	if p.Name == "" {
		return deny("not signed in"), nil
	}
	if p.Scopes != nil && !slices.ContainsFunc(p.Scopes, func(s string) bool { return slices.Contains(TokenScopes[s], a) }) {
		return deny("the token's scopes do not allow %s", a), nil
	}
	if p.has(PlatformAdmin, o) && a != UseBreakGlass {
		return allow(), nil
	}
	switch a {
	case ReadCatalog:
		if !p.has(Viewer, o) && !p.has(Requester, o) && !p.has(Auditor, o) {
			return deny("viewing the catalog needs the viewer or requester role"), nil
		}
		if !p.inAny(o.Entitlement.Visibility) && !p.has(Auditor, o) {
			return deny("this catalog item is not visible to your groups"), nil
		}
	case ReadInventory:
		if !p.has(Viewer, o) && !p.has(Operator, o) && !p.has(Auditor, o) && !p.owns(o) {
			return deny("viewing this needs the viewer role or ownership"), nil
		}
	case ReadRequest:
		if o.Requester != p.Name && !p.has(Viewer, o) && !p.has(Auditor, o) && !p.owns(o) {
			return deny("viewing this request needs the viewer role, or to have made it"), nil
		}
	case CreateRequest:
		if !p.has(Requester, o) {
			return deny("requesting needs the requester role here"), nil
		}
		if !p.inAny(o.Entitlement.Requesters) || !p.inAny(o.Entitlement.Visibility) {
			return deny("this catalog item is not open to your groups"), nil
		}
	case ReviewRequest:
		// Eligibility comes from the request's approval rules; here only who may see it.
		if !p.has(Viewer, o) && !p.has(Requester, o) && !p.has(Operator, o) && !p.has(Auditor, o) {
			return deny("reviewing needs access to the request"), nil
		}
	case CancelRequest, RetryRequest:
		if o.Requester != p.Name {
			return deny("only the requester or a platform admin can do this"), nil
		}
	case OperateResource:
		if !p.has(Operator, o) {
			return deny("changing resources needs the operator role here"), nil
		}
		if !p.owns(o) {
			return deny("only the owner, their team or a platform admin can change this resource"), nil
		}
	case PreviewDefinitions:
		if !p.has(CatalogAuthor, o) && !p.has(RepositoryAdmin, o) {
			return deny("previewing definitions needs the catalog author role"), nil
		}
	case ManageRepository:
		if !p.has(RepositoryAdmin, o) {
			return deny("managing the repository needs the repository admin role"), nil
		}
	case ManageSettings:
		return deny("managing settings needs the platform admin role"), nil
	case ReadAudit:
		if !p.has(Auditor, o) {
			return deny("reading the audit log needs the auditor role"), nil
		}
	case ManageOwnTokens:
		// Anyone signed in manages their own tokens; tokens cannot make tokens (scopes above).
	case UseBreakGlass:
		if !p.has(BreakGlass, o) {
			return deny("break-glass needs the break-glass role"), nil
		}
		if o.Requester == p.Name {
			return deny("break-glass cannot be used on your own request"), nil
		}
	default:
		return Decision{}, fmt.Errorf("authz: unknown action %q", a)
	}
	return allow(), nil
}

// has reports whether p holds role at a scope that covers o.
func (p Principal) has(role Role, o Object) bool {
	return slices.ContainsFunc(p.Grants, func(g Grant) bool {
		if g.Role != role {
			return false
		}
		switch g.Scope.Kind {
		case ScopeGlobal:
			return true
		case ScopeRepository:
			return g.Scope.Name != "" && g.Scope.Name == o.Repository
		case ScopeTeam:
			return g.Scope.Name != "" && g.Scope.Name == o.Team
		case ScopeCatalogItem:
			return g.Scope.Name != "" && g.Scope.Name == o.CatalogItem
		}
		return false
	})
}

// owns reports whether p owns o, directly or through their team.
func (p Principal) owns(o Object) bool {
	return o.Owner != "" && o.Owner == p.Name || o.Team != "" && slices.Contains(p.Groups, o.Team)
}

// inAny reports whether p is in one of the group patterns; an empty list allows everyone.
func (p Principal) inAny(patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pat := range patterns {
		g, ok := strings.CutPrefix(pat, "group:")
		if !ok {
			if u, isUser := strings.CutPrefix(pat, "user:"); isUser && u == p.Name {
				return true
			}
			continue
		}
		for _, mine := range p.Groups {
			if ok, _ := path.Match(g, mine); ok {
				return true
			}
		}
	}
	return false
}
