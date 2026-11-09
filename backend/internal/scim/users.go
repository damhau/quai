package scim

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/store/db"
)

// userView is a provisioned person as SCIM shows them: what Quai keeps of them, nothing more.
type userView struct {
	Schemas    []string    `json:"schemas"`
	ID         string      `json:"id"`
	ExternalID string      `json:"externalId"`
	UserName   string      `json:"userName"`
	Active     bool        `json:"active"`
	Enterprise *enterprise `json:"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User,omitempty"`
	Meta       meta        `json:"meta"`
}

type enterprise struct {
	Manager *resourceRef `json:"manager,omitempty"`
}

// resourceRef is a reference to another resource.
type resourceRef struct {
	Value   string `json:"value"`
	Ref     string `json:"$ref,omitempty"`
	Display string `json:"display,omitempty"`
}

func (h *Handler) userView(u db.User) userView {
	id := uuid.UUID(u.ScimID.Bytes).String()
	v := userView{Schemas: []string{schemaUser}, ID: id, ExternalID: u.ScimExternalID, UserName: u.ScimUserName, Active: !u.DeactivatedAt.Valid,
		Meta: meta{ResourceType: "User", Created: u.CreatedAt.Time.UTC(), LastModified: u.UpdatedAt.Time.UTC(), Location: h.base + "/Users/" + id}}
	if m := scimManager(u); m != "" {
		v.Schemas = append(v.Schemas, schemaEnterprise)
		v.Enterprise = &enterprise{Manager: &resourceRef{Value: m, Ref: h.base + "/Users/" + m}}
	}
	return v
}

// scimManager is the manager SCIM gave a person: their SCIM id, or "".
func scimManager(u db.User) string {
	if u.ManagerSource != auth.ManagerFromSCIM {
		return ""
	}
	return u.ManagerID
}

// noManager is why a provisioned person has no manager.
const noManager = "the identity provider's provisioning names no manager"

func managerReason(manager string) string {
	if manager == "" {
		return noManager
	}
	return ""
}

// userNameRe is what a userName may be: Microsoft Entra ID's user principal names, and email
// addresses. It becomes the person's name in Quai.
var userNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+'!#^~-]{0,255}$`)

// userState is what SCIM changes of a person.
type userState struct {
	id         string
	userName   string
	externalID string
	active     bool
	activeSet  bool // an operation set active
	manager    string
}

// parseUser reads a POST /Users: userName, externalId (the identity provider's immutable
// identifier of the person, which binds them), active, and the enterprise extension's manager.
// The other attributes (name, emails, ...) are accepted and not kept.
func parseUser(body object) (userState, error) {
	s := userState{active: true}
	var err error
	if s.userName, err = text("userName", body["username"]); err != nil {
		return s, invalid("invalidValue", err)
	}
	if !userNameRe.MatchString(s.userName) {
		return s, fail(http.StatusBadRequest, "invalidValue", "userName must be letters, digits and ._@+'!#^~-, starting with a letter or a digit")
	}
	if s.externalID, err = text("externalId", body["externalid"]); err != nil || s.externalID == "" {
		return s, fail(http.StatusBadRequest, "invalidValue",
			"externalId is required: the identity provider's immutable identifier of the user (Microsoft Entra ID: objectId)")
	}
	if v := body["active"]; !null(v) {
		if s.active, err = boolean("active", v); err != nil {
			return s, invalid("invalidValue", err)
		}
	}
	if v := body[strings.ToLower(schemaEnterprise)]; !null(v) {
		ext, err := asObject(v)
		if err != nil {
			return s, fail(http.StatusBadRequest, "invalidValue", "%s must be an object", schemaEnterprise)
		}
		if s.manager, err = reference("manager", ext["manager"]); err != nil {
			return s, invalid("invalidValue", err)
		}
	}
	return s, checkManager(s.manager)
}

// checkManager checks a manager is a user's id, which they may not have yet: the identity
// provider can name a manager it provisions later.
func checkManager(m string) error {
	if m == "" {
		return nil
	}
	if _, ok := parseID(m); !ok {
		return fail(http.StatusBadRequest, "invalidValue", "manager must be the id of a user")
	}
	return nil
}

// applyUser applies one operation of a PATCH to s. Quai keeps userName, externalId, active and
// the enterprise extension's manager; the other attributes are accepted and not kept.
func applyUser(s *userState, op operation) error {
	if op.path == "" {
		if op.op == "remove" {
			return fail(http.StatusBadRequest, "noTarget", "remove needs a path")
		}
		ops, err := pathless(op)
		if err != nil {
			return invalid("invalidValue", err)
		}
		for _, o := range ops {
			if err := applyUser(s, o); err != nil {
				return err
			}
		}
		return nil
	}
	var err error
	switch attribute(op.path, schemaUser) {
	case "username":
		if op.op == "remove" {
			return fail(http.StatusBadRequest, "mutability", "userName cannot be removed")
		}
		s.userName, err = text("userName", op.value)
	case "externalid":
		if op.op == "remove" {
			return fail(http.StatusBadRequest, "mutability", "externalId cannot be removed")
		}
		s.externalID, err = text("externalId", op.value)
	case "active":
		if op.op == "remove" {
			return fail(http.StatusBadRequest, "mutability", "active cannot be removed")
		}
		s.active, err = boolean("active", op.value)
		s.activeSet = true
	case "manager":
		if op.op == "remove" {
			s.manager = ""
			return nil
		}
		s.manager, err = reference("manager", op.value)
	case "id":
		var id string
		if id, err = text("id", op.value); err == nil && id != s.id {
			return fail(http.StatusBadRequest, "mutability", "id cannot change")
		}
	}
	if err != nil {
		return invalid("invalidValue", err)
	}
	return nil
}

// parseID reads a resource's id: a UUID, as Quai writes it.
func parseID(s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	return id, err == nil && id.String() == s
}

func pgID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// sameAccount reports whether a person of the directory is the identity provider's account
// externalID names: by the externalId SCIM bound them to before, if any, and by their Entra ID
// oid, or else their sub. A name an administrator released (no identifier left) is taken too.
func sameAccount(u db.User, externalID string) bool {
	switch {
	case u.ScimExternalID != "" && !strings.EqualFold(u.ScimExternalID, externalID):
		return false
	case u.ObjectID != "":
		return strings.EqualFold(u.ObjectID, externalID)
	case u.Subject != "":
		return u.Subject == externalID
	}
	return true
}

func (h *Handler) getUser(ctx context.Context, raw string) (int, any, error) {
	id, ok := parseID(raw)
	if !ok {
		return 0, nil, errNotFound
	}
	u, err := h.store.SCIMUser(ctx, pgID(id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, nil, errNotFound
	case err != nil:
		return 0, nil, err
	}
	return http.StatusOK, h.userView(u), nil
}

// listUsers answers GET /Users, filtered by userName, externalId, id and manager.
func (h *Handler) listUsers(ctx context.Context, r *http.Request) (int, any, error) {
	var p db.ListSCIMUsersParams
	none := false
	if f := r.URL.Query().Get("filter"); f != "" {
		clauses, err := parseFilter(f, "username", "externalid", "id", "manager")
		if err != nil {
			return 0, nil, invalid("invalidFilter", err)
		}
		for _, c := range clauses {
			switch c.attr {
			case "username":
				p.ByUserName, p.UserName = true, c.value
			case "externalid":
				p.ByExternalID, p.ExternalID = true, c.value
			case "id":
				id, ok := parseID(c.value)
				p.ByID, p.ID, none = true, id, none || !ok
			case "manager":
				p.ByManager, p.Manager = true, c.value
			}
		}
	}
	skip, take, err := page(r)
	if err != nil {
		return 0, nil, err
	}
	if none {
		return http.StatusOK, list[userView](0, skip, nil), nil
	}
	total, err := h.store.CountSCIMUsers(ctx, db.CountSCIMUsersParams{ByUserName: p.ByUserName, UserName: p.UserName,
		ByExternalID: p.ByExternalID, ExternalID: p.ExternalID, ByID: p.ByID, ID: p.ID, ByManager: p.ByManager, Manager: p.Manager})
	if err != nil {
		return 0, nil, err
	}
	p.Skip, p.Take = skip, take
	var views []userView
	if take > 0 {
		users, err := h.store.ListSCIMUsers(ctx, p)
		if err != nil {
			return 0, nil, err
		}
		for _, u := range users {
			views = append(views, h.userView(u))
		}
	}
	return http.StatusOK, list(total, skip, views), nil
}

// createUser answers POST /Users. The person's name in Quai is the userName, for good: a rename
// changes the userName SCIM shows, never the name. A person of the directory with that very name
// is bound (adopted) when they are the same account of the identity provider, and refused
// otherwise, as is a name equal but for case to one Quai holds, a name reserved, and a userName or
// externalId provisioned already: a new person never inherits another's name, nor what it holds.
func (h *Handler) createUser(ctx context.Context, body object) (int, any, error) {
	in, err := parseUser(body)
	if err != nil {
		return 0, nil, err
	}
	if h.isReserved(in.userName) {
		return 0, nil, fail(http.StatusConflict, "uniqueness", "this userName is reserved: Quai does not provision it")
	}
	var out db.User
	adopted, deactivated := false, false
	err = h.tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		switch _, err := q.SCIMUserByUserName(ctx, in.userName); {
		case err == nil:
			return fail(http.StatusConflict, "uniqueness", "a user with this userName is provisioned already")
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		switch _, err := q.UserByBinding(ctx, in.externalID); {
		case err == nil:
			return fail(http.StatusConflict, "uniqueness", "a user with this externalId is provisioned already")
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		names, err := q.UserNamesFolded(ctx, in.userName)
		if err != nil {
			return err
		}
		for _, n := range names {
			if n != in.userName {
				return fail(http.StatusConflict, "uniqueness", "this userName differs only in case from a name Quai holds")
			}
		}
		id := pgID(uuid.New())
		if len(names) == 0 {
			p := db.CreateSCIMUserParams{Name: in.userName, ScimID: id, ExternalID: in.externalID, UserName: in.userName,
				ManagerID: in.manager, ManagerReason: managerReason(in.manager)}
			if !in.active {
				p.DeactivatedAt = pgtype.Timestamptz{Time: h.now(), Valid: true}
			}
			if out, err = q.CreateSCIMUser(ctx, p); err != nil {
				return err
			}
		} else {
			known, err := q.LockUser(ctx, in.userName)
			switch {
			case err != nil:
				return err
			case known.ScimID.Valid:
				return fail(http.StatusConflict, "uniqueness", "this userName is the name of another provisioned user in Quai")
			case !sameAccount(known, in.externalID):
				return fail(http.StatusConflict, "uniqueness", "this userName is the name of another account of the identity provider in Quai")
			}
			if out, err = q.AdoptSCIMUser(ctx, db.AdoptSCIMUserParams{ScimID: id, ExternalID: in.externalID, UserName: in.userName,
				ManagerID: in.manager, ManagerReason: managerReason(in.manager), Name: in.userName}); err != nil {
				return err
			}
			adopted = true
		}
		details := map[string]any{"adopted": adopted, "active": in.active}
		if in.manager != "" {
			details["manager"] = audit.Personal(in.manager)
		}
		if err := audit.Add(ctx, tx, audit.Event{Action: "scim.user_created", Object: audit.Object{Type: "user", ID: out.Name}, Details: details}); err != nil {
			return err
		}
		if !adopted {
			return nil
		}
		switch {
		case !in.active:
			out, err = h.deactivate(ctx, tx, q, out)
			deactivated = true
		case out.DeactivatedAt.Valid:
			out, err = h.reactivate(ctx, tx, q, out)
		}
		if err != nil {
			return err
		}
		// What their last sign-in gave them, less the groups SCIM provisions without them.
		return h.refresh(ctx, tx, q, []string{out.Name})
	})
	if err != nil {
		return 0, nil, err
	}
	if deactivated {
		if err := h.deprovision(ctx, out.Name); err != nil {
			return 0, nil, err
		}
	}
	return http.StatusCreated, h.userView(out), nil
}

// patchUser answers PATCH /Users/{id}: a rename (userName), the manager, and active. externalId
// never changes: it binds the person to their account of the identity provider.
func (h *Handler) patchUser(ctx context.Context, raw string, body object) (int, any, error) {
	ops, err := parsePatch(body)
	if err != nil {
		return 0, nil, invalid("invalidSyntax", err)
	}
	id, ok := parseID(raw)
	if !ok {
		return 0, nil, errNotFound
	}
	var out db.User
	deactivated := false
	err = h.tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		u, err := q.LockSCIMUser(ctx, pgID(id))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return errNotFound
		case err != nil:
			return err
		case h.isReserved(u.Name):
			return fail(http.StatusForbidden, "", "this user is reserved: Quai does not provision it")
		}
		cur := userState{id: raw, userName: u.ScimUserName, externalID: u.ScimExternalID, active: !u.DeactivatedAt.Valid, manager: scimManager(u)}
		next := cur
		for _, op := range ops {
			if err := applyUser(&next, op); err != nil {
				return err
			}
		}
		if !strings.EqualFold(next.externalID, cur.externalID) {
			return fail(http.StatusBadRequest, "mutability", "externalId cannot change: it binds the user to their account of the identity provider")
		}
		if err := checkManager(next.manager); err != nil {
			return err
		}
		out = u
		if next.userName != cur.userName || next.manager != cur.manager {
			if out, err = h.update(ctx, tx, q, u, cur, next); err != nil {
				return err
			}
		}
		switch {
		case !next.active && (cur.active || next.activeSet):
			// Deactivated again too: what a failure left is finished.
			out, err = h.deactivate(ctx, tx, q, out)
			deactivated = true
		case next.active && !cur.active:
			out, err = h.reactivate(ctx, tx, q, out)
		}
		return err
	})
	if err != nil {
		return 0, nil, err
	}
	if deactivated {
		if err := h.deprovision(ctx, out.Name); err != nil {
			return 0, nil, err
		}
	}
	return http.StatusOK, h.userView(out), nil
}

// update writes a rename and a new manager, and their audit event.
func (h *Handler) update(ctx context.Context, tx pgx.Tx, q *db.Queries, u db.User, cur, next userState) (db.User, error) {
	before, after := map[string]any{}, map[string]any{}
	if next.userName != cur.userName {
		if !userNameRe.MatchString(next.userName) {
			return u, fail(http.StatusBadRequest, "invalidValue", "userName must be letters, digits and ._@+'!#^~-, starting with a letter or a digit")
		}
		if h.isReserved(next.userName) {
			return u, fail(http.StatusConflict, "uniqueness", "this userName is reserved: Quai does not provision it")
		}
		switch other, err := q.SCIMUserByUserName(ctx, next.userName); {
		case err == nil && other.Name != u.Name:
			return u, fail(http.StatusConflict, "uniqueness", "a user with this userName is provisioned already")
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return u, err
		}
		// Nor the name of another person of Quai, which would read as theirs.
		names, err := q.UserNamesFolded(ctx, next.userName)
		if err != nil {
			return u, err
		}
		if slices.ContainsFunc(names, func(n string) bool { return n != u.Name }) {
			return u, fail(http.StatusConflict, "uniqueness", "this userName is the name of another person of Quai")
		}
		before["userName"], after["userName"] = audit.Personal(cur.userName), audit.Personal(next.userName)
	}
	if next.manager != cur.manager {
		before["manager"], after["manager"] = audit.Personal(cur.manager), audit.Personal(next.manager)
	}
	out, err := q.UpdateSCIMUser(ctx, db.UpdateSCIMUserParams{UserName: next.userName, ExternalID: cur.externalID,
		ManagerID: next.manager, ManagerReason: managerReason(next.manager), ScimID: u.ScimID})
	if err != nil {
		return u, err
	}
	return out, audit.Add(ctx, tx, audit.Event{Action: "scim.user_changed", Object: audit.Object{Type: "user", ID: u.Name},
		Details: map[string]any{"before": before, "after": after}})
}

// deleteUser answers DELETE /Users/{id}: the person is deactivated, leaves the groups SCIM
// provisions, and is no longer provisioned (a GET answers 404). Quai keeps their name and their
// history; a POST of the same externalId provisions them again, still deactivated if it says so.
func (h *Handler) deleteUser(ctx context.Context, raw string) (int, any, error) {
	id, ok := parseID(raw)
	if !ok {
		return 0, nil, errNotFound
	}
	var name string
	err := h.tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		u, err := q.LockSCIMUser(ctx, pgID(id))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return errNotFound
		case err != nil:
			return err
		case h.isReserved(u.Name):
			return fail(http.StatusForbidden, "", "this user is reserved: Quai does not provision it")
		}
		name = u.Name
		if u, err = h.deactivate(ctx, tx, q, u); err != nil {
			return err
		}
		groups, err := q.RemoveSCIMMemberships(ctx, u.Name)
		if err != nil {
			return err
		}
		for _, g := range groups {
			if err := audit.Add(ctx, tx, audit.Event{Action: "scim.members_changed", Object: audit.Object{Type: "group", ID: g.String()},
				Details: map[string]any{"added": []audit.Personal{}, "removed": []audit.Personal{audit.Personal(u.Name)}}}); err != nil {
				return err
			}
		}
		if err := h.refresh(ctx, tx, q, []string{u.Name}); err != nil {
			return err
		}
		if err := q.UnbindSCIMUser(ctx, u.ScimID); err != nil {
			return err
		}
		return audit.Add(ctx, tx, audit.Event{Action: "scim.user_deleted", Object: audit.Object{Type: "user", ID: u.Name}})
	})
	if err != nil {
		return 0, nil, err
	}
	if err := h.deprovision(ctx, name); err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// deactivate ends a provisioned person's access, in tx: deactivated with a new session epoch, so
// that their sessions end at their next request, and their API tokens revoked. Their requests,
// reviews and delegations are the caller's to end after the transaction (deprovision). A person
// deactivated already keeps their deactivation; tokens are revoked all the same.
func (h *Handler) deactivate(ctx context.Context, tx pgx.Tx, q *db.Queries, u db.User) (db.User, error) {
	user := audit.Object{Type: "user", ID: u.Name}
	d, err := q.DeactivateSCIMUser(ctx, u.ScimID)
	fresh := err == nil
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return u, err
	default:
		u = d
	}
	tokens, err := q.RevokeOwnerAPITokens(ctx, u.Name)
	if err != nil {
		return u, err
	}
	var events []audit.Event
	if fresh {
		events = append(events, audit.Event{Action: "scim.user_deactivated", Object: user,
			Details: map[string]any{"sessions": "ended", "tokens": len(tokens)}})
	}
	for _, t := range tokens {
		events = append(events, audit.Event{Action: "token.revoked", Object: audit.Object{Type: "token", ID: t.ID.String()},
			Details: map[string]any{"reason": "its owner was deactivated", "owner": audit.Personal(u.Name)}})
	}
	return u, audit.Add(ctx, tx, events...)
}

// reactivate lets a provisioned person sign in again: nothing their deactivation ended comes back.
func (h *Handler) reactivate(ctx context.Context, tx pgx.Tx, q *db.Queries, u db.User) (db.User, error) {
	r, err := q.ReactivateSCIMUser(ctx, u.ScimID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return u, nil
	case err != nil:
		return u, err
	}
	return r, audit.Add(ctx, tx, audit.Event{Action: "scim.user_reactivated", Object: audit.Object{Type: "user", ID: u.Name}})
}

// deprovision ends what a deactivated person leaves on requests: their delegations, their
// requests not delivered yet, their approvals not counted yet (requests.Deprovision).
func (h *Handler) deprovision(ctx context.Context, name string) error {
	if h.requests == nil {
		return nil
	}
	if _, err := h.requests.Deprovision(ctx, name); err != nil {
		return fmt.Errorf("deprovisioning %s: %w", name, err)
	}
	return nil
}
