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
	Email  string   // from the IdP, when it gives a verified one; empty for an API token
	Groups []string // IdP groups
	// Teams are the Quai teams of the groups (auth.teams; a group no entry maps is the team of its
	// own name): ownership by team:<name> and team: owner entries match them.
	Teams  []string
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
	ReadAccessReport   Action = "access.read"
	UseBreakGlass      Action = "requests.break-glass"
	ManageOwnTokens    Action = "tokens.manage"
	PublishBlueprint   Action = "blueprints.publish"
	// ManageOwnPreferences reads and changes one's notification preferences.
	ManageOwnPreferences Action = "preferences.manage"
)

// Object is what the action is on. Fields that do not apply are empty.
type Object struct {
	Repository  string
	CatalogItem string
	// Team and Owner own the object: a resource's owning team (team:<name> owners), or user.
	Team  string
	Owner string
	// Owners own the object too: the owners the Git platform names for its path
	// (ownership.codeowners), as user:<name>, group:<name> or email:<address> entries.
	Owners []string
	// Requester made the request the object is, or belongs to.
	Requester string
	// Entitlement is who a catalog item lets see it and request it.
	Entitlement Entitlement
}

// Entitlement lists, as group patterns (group:developers, group:data-*) or people (user:zoe), who
// may see a catalog item and who may request it (docs/design/08-identity-access-tenancy.md#entitlements).
// An empty list does not restrict. Requesting needs to be in Visibility and in Requesters, and, for
// an environment the item narrows, in that environment's list too: it never widens Requesters.
type Entitlement struct {
	Visibility []string
	Requesters []string
	// Environment is the environment a request is for when the item narrows its requesters there,
	// and EnvironmentRequesters that narrower list; empty otherwise.
	Environment           string
	EnvironmentRequesters []string
	// Item names what the lists belong to in a denial when it is not the object's catalog item:
	// "type vm" for a resource a blueprint creates.
	Item string
}

// subject is what a denial names.
func (e Entitlement) subject() string {
	if e.Item != "" {
		return e.Item
	}
	return "this catalog item"
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
			return deny("%s is not visible to your groups", o.Entitlement.subject()), nil
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
		e := o.Entitlement
		switch {
		case !p.inAny(e.Visibility):
			return deny("%s is not visible to your groups", e.subject()), nil
		case !p.inAny(e.Requesters):
			return deny("%s is not open to your groups", e.subject()), nil
		case e.Environment != "" && (len(e.EnvironmentRequesters) == 0 || !p.inAny(e.EnvironmentRequesters)):
			// An environment's list only narrows; definitions never have an empty one (quai lint),
			// and none would let anyone in.
			return deny("%s is not open to your groups in %s", e.subject(), e.Environment), nil
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
	case ManageRepository, PublishBlueprint:
		// Publishing commits a definition straight to the branch, so it takes a repository admin
		// until definitions go through pull requests.
		if !p.has(RepositoryAdmin, o) {
			return deny("managing the repository needs the repository admin role"), nil
		}
	case ManageSettings:
		return deny("managing settings needs the platform admin role"), nil
	case ReadAudit:
		if !p.has(Auditor, o) {
			return deny("reading the audit log needs the auditor role"), nil
		}
	case ReadAccessReport:
		if !p.has(Auditor, o) {
			return deny("reading the access report needs the auditor role"), nil
		}
	case ManageOwnTokens:
		// Anyone signed in manages their own tokens; tokens cannot make tokens (scopes above).
	case ManageOwnPreferences:
		// Anyone signed in manages their own notification preferences, in a browser session: no
		// scope allows it to a token (open question 58).
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
	return o.Owner != "" && o.Owner == p.Name || o.Team != "" && slices.Contains(p.Teams, o.Team) ||
		slices.ContainsFunc(o.Owners, p.is)
}

// is reports whether an owner entry names p: user:<name>, group:<name>, team:<name> or
// email:<address>.
func (p Principal) is(entry string) bool {
	kind, v, _ := strings.Cut(entry, ":")
	switch kind {
	case "user":
		return v != "" && v == p.Name
	case "group":
		return v != "" && slices.Contains(p.Groups, v)
	case "team":
		return v != "" && slices.Contains(p.Teams, v)
	case "email":
		return v != "" && p.Email != "" && strings.EqualFold(v, p.Email)
	}
	return false
}

// inAny reports whether one of the entries names p: a group pattern (group:data-*) matching one of
// their groups, never a team, or user:<their name>. Any other entry names no one; an empty list
// allows everyone.
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
