package scim

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/store/db"
)

// groupView is a provisioned group as SCIM shows it; members is left out when asked
// (excludedAttributes=members), as Microsoft Entra ID asks.
type groupView struct {
	Schemas     []string       `json:"schemas"`
	ID          string         `json:"id"`
	ExternalID  string         `json:"externalId,omitempty"`
	DisplayName string         `json:"displayName"`
	Members     *[]resourceRef `json:"members,omitempty"`
	Meta        meta           `json:"meta"`
}

func (h *Handler) groupView(g db.ScimGroup, members []db.SCIMGroupMembersRow, withMembers bool) groupView {
	v := groupView{Schemas: []string{schemaGroup}, ID: g.ID.String(), ExternalID: g.ExternalID, DisplayName: g.DisplayName,
		Meta: meta{ResourceType: "Group", Created: g.CreatedAt.Time.UTC(), LastModified: g.UpdatedAt.Time.UTC(), Location: h.base + "/Groups/" + g.ID.String()}}
	if withMembers {
		refs := []resourceRef{}
		for _, m := range members {
			if m.GroupID != g.ID {
				continue
			}
			id := uuid.UUID(m.ScimID.Bytes).String()
			refs = append(refs, resourceRef{Value: id, Ref: h.base + "/Users/" + id, Display: m.ScimUserName})
		}
		v.Members = &refs
	}
	return v
}

// withMembers reports whether a request wants the groups' members: not when excludedAttributes
// names members.
func withMembers(r *http.Request) bool {
	for _, a := range strings.Split(r.URL.Query().Get("excludedAttributes"), ",") {
		if strings.EqualFold(strings.TrimSpace(a), "members") {
			return false
		}
	}
	return true
}

// quaiName is the name a group has in Quai, as the ID token's groups claim names it
// (auth.scim.groupName): its externalId or its displayName; "" names none.
func (h *Handler) quaiName(externalID, displayName string) string {
	if h.cfg.GroupName == config.SCIMGroupDisplayName {
		return displayName
	}
	return externalID
}

func (h *Handler) getGroup(ctx context.Context, r *http.Request, raw string) (int, any, error) {
	id, ok := parseID(raw)
	if !ok {
		return 0, nil, errNotFound
	}
	g, err := h.store.SCIMGroup(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, nil, errNotFound
	case err != nil:
		return 0, nil, err
	}
	var members []db.SCIMGroupMembersRow
	all := withMembers(r)
	if all {
		if members, err = h.store.SCIMGroupMembers(ctx, []uuid.UUID{g.ID}); err != nil {
			return 0, nil, err
		}
	}
	return http.StatusOK, h.groupView(g, members, all), nil
}

// listGroups answers GET /Groups, filtered by displayName, externalId, id and members.
func (h *Handler) listGroups(ctx context.Context, r *http.Request) (int, any, error) {
	var p db.ListSCIMGroupsParams
	none := false
	if f := r.URL.Query().Get("filter"); f != "" {
		clauses, err := parseFilter(f, "displayname", "externalid", "id", "members")
		if err != nil {
			return 0, nil, invalid("invalidFilter", err)
		}
		for _, c := range clauses {
			switch c.attr {
			case "displayname":
				p.ByDisplayName, p.DisplayName = true, c.value
			case "externalid":
				p.ByExternalID, p.ExternalID = true, c.value
			case "id":
				id, ok := parseID(c.value)
				p.ByID, p.ID, none = true, id, none || !ok
			case "members":
				id, ok := parseID(c.value)
				p.ByMember, p.Member, none = true, id, none || !ok
			}
		}
	}
	skip, take, err := page(r)
	if err != nil {
		return 0, nil, err
	}
	if none {
		return http.StatusOK, list[groupView](0, skip, nil), nil
	}
	total, err := h.store.CountSCIMGroups(ctx, db.CountSCIMGroupsParams{ByDisplayName: p.ByDisplayName, DisplayName: p.DisplayName,
		ByExternalID: p.ByExternalID, ExternalID: p.ExternalID, ByID: p.ByID, ID: p.ID, ByMember: p.ByMember, Member: p.Member})
	if err != nil {
		return 0, nil, err
	}
	p.Skip, p.Take = skip, take
	var views []groupView
	if take > 0 {
		groups, err := h.store.ListSCIMGroups(ctx, p)
		if err != nil {
			return 0, nil, err
		}
		all := withMembers(r)
		var members []db.SCIMGroupMembersRow
		if all && len(groups) > 0 {
			ids := make([]uuid.UUID, 0, len(groups))
			for _, g := range groups {
				ids = append(ids, g.ID)
			}
			if members, err = h.store.SCIMGroupMembers(ctx, ids); err != nil {
				return 0, nil, err
			}
		}
		for _, g := range groups {
			views = append(views, h.groupView(g, members, all))
		}
	}
	return http.StatusOK, list(total, skip, views), nil
}

// groupState is what SCIM changes of a group.
type groupState struct {
	id          string
	displayName string
	externalID  string
}

// createGroup answers POST /Groups: displayName (unique in any case, as Microsoft Entra ID
// requires), externalId (unique), and members, if any. From then on, the name it has in Quai
// (quaiName) is SCIM's: provisioned people hold it only while the group lists them.
func (h *Handler) createGroup(ctx context.Context, r *http.Request, body object) (int, any, error) {
	dn, err := text("displayName", body["displayname"])
	if err != nil || dn == "" {
		return 0, nil, fail(http.StatusBadRequest, "invalidValue", "displayName is required")
	}
	var ext string
	if v := body["externalid"]; !null(v) {
		if ext, err = text("externalId", v); err != nil {
			return 0, nil, invalid("invalidValue", err)
		}
	}
	var ids []string
	if v := body["members"]; !null(v) {
		if ids, err = references("members", v); err != nil {
			return 0, nil, invalid("invalidValue", err)
		}
	}
	var g db.ScimGroup
	err = h.tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := h.unique(ctx, q, uuid.Nil, dn, ext); err != nil {
			return err
		}
		if g, err = q.CreateSCIMGroup(ctx, db.CreateSCIMGroupParams{ID: uuid.New(), ExternalID: ext, DisplayName: dn}); err != nil {
			return err
		}
		if err := audit.Add(ctx, tx, audit.Event{Action: "scim.group_created", Object: audit.Object{Type: "group", ID: g.ID.String()},
			Details: map[string]any{"displayName": dn, "externalId": ext}}); err != nil {
			return err
		}
		affected, err := h.know(ctx, q, h.quaiName(ext, dn))
		if err != nil {
			return err
		}
		added, err := h.members(ctx, tx, q, g, ids, nil)
		if err != nil {
			return err
		}
		return h.refresh(ctx, tx, q, append(affected, added...))
	})
	if err != nil {
		return 0, nil, err
	}
	members, err := h.store.SCIMGroupMembers(ctx, []uuid.UUID{g.ID})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, h.groupView(g, members, withMembers(r)), nil
}

// unique refuses a displayName (in any case) or a non-empty externalId another group has.
func (h *Handler) unique(ctx context.Context, q *db.Queries, self uuid.UUID, displayName, externalID string) error {
	others, err := q.ListSCIMGroups(ctx, db.ListSCIMGroupsParams{ByDisplayName: true, DisplayName: displayName, Take: 2})
	if err != nil {
		return err
	}
	if slices.ContainsFunc(others, func(g db.ScimGroup) bool { return g.ID != self }) {
		return fail(http.StatusConflict, "uniqueness", "a group with this displayName is provisioned already")
	}
	if externalID == "" {
		return nil
	}
	if others, err = q.ListSCIMGroups(ctx, db.ListSCIMGroupsParams{ByExternalID: true, ExternalID: externalID, Take: 2}); err != nil {
		return err
	}
	if slices.ContainsFunc(others, func(g db.ScimGroup) bool { return g.ID != self }) {
		return fail(http.StatusConflict, "uniqueness", "a group with this externalId is provisioned already")
	}
	return nil
}

// know makes a group name SCIM's for good, and returns the provisioned people whose last sign-in
// gave it to them: their groups are to work out again.
func (h *Handler) know(ctx context.Context, q *db.Queries, names ...string) ([]string, error) {
	var out []string
	for _, n := range names {
		if n == "" {
			continue
		}
		if err := q.AddKnownSCIMGroup(ctx, db.AddKnownSCIMGroupParams{Kind: h.cfg.GroupName, Name: n}); err != nil {
			return nil, err
		}
		holders, err := q.ProvisionedHolding(ctx, []string{n})
		if err != nil {
			return nil, err
		}
		out = append(out, holders...)
	}
	return out, nil
}

// patchGroup answers PATCH /Groups/{id}: displayName, externalId (set once), and members added
// or removed, as Microsoft Entra ID sends them: {"op": "Add", "path": "members", "value":
// [{"value": "id"}]}, the same with Remove, and {"op": "Remove", "path": "members[value eq
// \"id\"]"}. It answers 204, as Microsoft Entra ID advises.
func (h *Handler) patchGroup(ctx context.Context, raw string, body object) (int, any, error) {
	ops, err := parsePatch(body)
	if err != nil {
		return 0, nil, invalid("invalidSyntax", err)
	}
	id, ok := parseID(raw)
	if !ok {
		return 0, nil, errNotFound
	}
	err = h.tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		g, err := q.LockSCIMGroup(ctx, id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return errNotFound
		case err != nil:
			return err
		}
		cur := groupState{id: raw, displayName: g.DisplayName, externalID: g.ExternalID}
		next := cur
		var add, remove []string
		removeAll := false
		for _, op := range ops {
			if err := applyGroup(&next, op, &add, &remove, &removeAll); err != nil {
				return err
			}
		}
		var affected []string
		if next != cur {
			switch {
			case next.displayName == "":
				return fail(http.StatusBadRequest, "invalidValue", "displayName cannot be empty")
			case next.externalID != cur.externalID && cur.externalID != "":
				return fail(http.StatusBadRequest, "mutability", "externalId cannot change once set")
			}
			if err := h.unique(ctx, q, g.ID, next.displayName, next.externalID); err != nil {
				return err
			}
			updated, err := q.UpdateSCIMGroup(ctx, db.UpdateSCIMGroupParams{ID: g.ID, ExternalID: next.externalID, DisplayName: next.displayName})
			if err != nil {
				return err
			}
			if err := audit.Add(ctx, tx, audit.Event{Action: "scim.group_changed", Object: audit.Object{Type: "group", ID: g.ID.String()},
				Details: map[string]any{"before": map[string]any{"displayName": cur.displayName, "externalId": cur.externalID},
					"after": map[string]any{"displayName": next.displayName, "externalId": next.externalID}}}); err != nil {
				return err
			}
			// A new name in Quai: SCIM's from now on, and the old one stays SCIM's.
			if old, now := h.quaiName(cur.externalID, cur.displayName), h.quaiName(next.externalID, next.displayName); old != now {
				if affected, err = h.know(ctx, q, old, now); err != nil {
					return err
				}
				members, err := q.SCIMMemberNames(ctx, g.ID)
				if err != nil {
					return err
				}
				affected = append(affected, members...)
			}
			g = updated
		}
		if removeAll {
			members, err := q.SCIMMemberNames(ctx, g.ID)
			if err != nil {
				return err
			}
			changed, err := h.memberNames(ctx, tx, q, g, members)
			if err != nil {
				return err
			}
			affected = append(affected, changed...)
		}
		changed, err := h.members(ctx, tx, q, g, add, remove)
		if err != nil {
			return err
		}
		return h.refresh(ctx, tx, q, append(affected, changed...))
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// applyGroup applies one operation of a PATCH to a group: its attributes to s, the members it
// adds and removes to add and remove. Quai keeps displayName, externalId and members; the other
// attributes are accepted and not kept.
func applyGroup(s *groupState, op operation, add, remove *[]string, removeAll *bool) error {
	if op.path == "" {
		if op.op == "remove" {
			return fail(http.StatusBadRequest, "noTarget", "remove needs a path")
		}
		ops, err := pathless(op)
		if err != nil {
			return invalid("invalidValue", err)
		}
		for _, o := range ops {
			if err := applyGroup(s, o, add, remove, removeAll); err != nil {
				return err
			}
		}
		return nil
	}
	if id, ok, err := memberFilter(op.path); ok {
		switch {
		case err != nil:
			return invalid("invalidPath", err)
		case op.op != "remove":
			return fail(http.StatusBadRequest, "invalidPath", "a filtered members path is for remove")
		}
		*remove = append(*remove, id)
		return nil
	}
	var err error
	switch attribute(op.path, schemaGroup) {
	case "displayname":
		if op.op == "remove" {
			return fail(http.StatusBadRequest, "mutability", "displayName cannot be removed")
		}
		s.displayName, err = text("displayName", op.value)
	case "externalid":
		if op.op == "remove" {
			return fail(http.StatusBadRequest, "mutability", "externalId cannot be removed")
		}
		s.externalID, err = text("externalId", op.value)
	case "members":
		var ids []string
		switch op.op {
		case "add":
			ids, err = references("members", op.value)
			*add = append(*add, ids...)
		case "remove":
			if op.value == nil {
				*removeAll = true
				return nil
			}
			ids, err = references("members", op.value)
			*remove = append(*remove, ids...)
		default:
			return fail(http.StatusBadRequest, "invalidValue", "members are added and removed, not replaced")
		}
		if len(*add)+len(*remove) > maxMembers {
			return fail(http.StatusBadRequest, "invalidValue", "a PATCH changes at most %d members", maxMembers)
		}
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

// members removes then adds the members of a group by their ids, and returns the names of the
// people whose membership changed. A member added must be a provisioned user; one removed that
// is none, or not a member, is no change.
func (h *Handler) members(ctx context.Context, tx pgx.Tx, q *db.Queries, g db.ScimGroup, add, remove []string) ([]string, error) {
	resolve := func(ids []string, strict bool) ([]string, error) {
		var uuids []uuid.UUID
		for _, s := range ids {
			id, ok := parseID(s)
			if !ok {
				if strict {
					return nil, fail(http.StatusBadRequest, "invalidValue", "members must be ids of provisioned users")
				}
				continue
			}
			if !slices.Contains(uuids, id) {
				uuids = append(uuids, id)
			}
		}
		if len(uuids) == 0 {
			return nil, nil
		}
		users, err := q.SCIMUsersByID(ctx, uuids)
		if err != nil {
			return nil, err
		}
		if strict && len(users) != len(uuids) {
			return nil, fail(http.StatusBadRequest, "invalidValue", "members must be ids of provisioned users")
		}
		names := make([]string, 0, len(users))
		for _, u := range users {
			names = append(names, u.Name)
		}
		return names, nil
	}
	out, err := resolve(remove, false)
	if err != nil {
		return nil, err
	}
	removed, err := h.memberNames(ctx, tx, q, g, out)
	if err != nil {
		return nil, err
	}
	in, err := resolve(add, true)
	if err != nil {
		return nil, err
	}
	var added []audit.Personal
	for _, n := range in {
		c, err := q.AddSCIMMember(ctx, db.AddSCIMMemberParams{GroupID: g.ID, UserName: n})
		if err != nil {
			return nil, err
		}
		if c > 0 {
			added = append(added, audit.Personal(n))
		}
	}
	changed := removed
	for _, a := range added {
		changed = append(changed, string(a))
	}
	if len(added) > 0 {
		if err := audit.Add(ctx, tx, audit.Event{Action: "scim.members_changed", Object: audit.Object{Type: "group", ID: g.ID.String()},
			Details: map[string]any{"added": added, "removed": []audit.Personal{}}}); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

// memberNames removes people, by name, from a group, and returns those who were members.
func (h *Handler) memberNames(ctx context.Context, tx pgx.Tx, q *db.Queries, g db.ScimGroup, names []string) ([]string, error) {
	var removed []string
	for _, n := range names {
		c, err := q.RemoveSCIMMember(ctx, db.RemoveSCIMMemberParams{GroupID: g.ID, UserName: n})
		if err != nil {
			return nil, err
		}
		if c > 0 {
			removed = append(removed, n)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	personal := make([]audit.Personal, 0, len(removed))
	for _, n := range removed {
		personal = append(personal, audit.Personal(n))
	}
	return removed, audit.Add(ctx, tx, audit.Event{Action: "scim.members_changed", Object: audit.Object{Type: "group", ID: g.ID.String()},
		Details: map[string]any{"added": []audit.Personal{}, "removed": personal}})
}

// deleteGroup answers DELETE /Groups/{id}: the group and its members go; its name in Quai stays
// SCIM's, so that nobody provisioned holds it any longer.
func (h *Handler) deleteGroup(ctx context.Context, raw string) (int, any, error) {
	id, ok := parseID(raw)
	if !ok {
		return 0, nil, errNotFound
	}
	err := h.tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		g, err := q.LockSCIMGroup(ctx, id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return errNotFound
		case err != nil:
			return err
		}
		members, err := q.SCIMMemberNames(ctx, g.ID)
		if err != nil {
			return err
		}
		if _, err := q.DeleteSCIMGroup(ctx, g.ID); err != nil {
			return err
		}
		personal := make([]audit.Personal, 0, len(members))
		for _, n := range members {
			personal = append(personal, audit.Personal(n))
		}
		if err := audit.Add(ctx, tx, audit.Event{Action: "scim.group_deleted", Object: audit.Object{Type: "group", ID: g.ID.String()},
			Details: map[string]any{"displayName": g.DisplayName, "externalId": g.ExternalID, "removed": personal}}); err != nil {
			return err
		}
		holders, err := h.know(ctx, q, h.quaiName(g.ExternalID, g.DisplayName))
		if err != nil {
			return err
		}
		return h.refresh(ctx, tx, q, append(members, holders...))
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusNoContent, nil, nil
}

// refresh works out again the groups of the provisioned people named (RefreshSCIMGroups), and
// writes each change in the audit log, with the groups and the grants before and after: SCIM
// taking away the platform-admins group shows as platform-admin taken away.
func (h *Handler) refresh(ctx context.Context, tx pgx.Tx, q *db.Queries, names []string) error {
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) == 0 {
		return nil
	}
	rows, err := q.RefreshSCIMGroups(ctx, db.RefreshSCIMGroupsParams{GroupKind: h.cfg.GroupName, Names: names})
	if err != nil {
		return err
	}
	var events []audit.Event
	for _, r := range rows {
		if slices.Equal(r.GroupsBefore, r.GroupsAfter) {
			continue
		}
		events = append(events, audit.Event{Action: "auth.groups_changed", Object: audit.Object{Type: "user", ID: r.Name},
			Details: map[string]any{
				"before": nonNil(r.GroupsBefore), "after": nonNil(r.GroupsAfter),
				"grantsBefore": h.dir.GrantNames(r.GroupsBefore), "grantsAfter": h.dir.GrantNames(r.GroupsAfter)}})
	}
	return audit.Add(ctx, tx, events...)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
