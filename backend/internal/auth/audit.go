package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/store/db"
)

// ActorOf returns the audit log's actor for an identity, from the address of the request ctx
// serves: a user with their authentication time, or an API token with its ID.
func ActorOf(ctx context.Context, id Identity) audit.Actor {
	a := audit.Actor{Type: audit.User, ID: id.Principal.Name, IP: audit.IPFrom(ctx), AuthTime: id.AuthTime}
	if id.TokenID != "" {
		a.Type, a.Token, a.AuthTime = audit.Token, id.TokenID, time.Time{}
	}
	return a
}

// RemoteIP returns the address the request comes from: the connection's peer. Headers such as
// X-Forwarded-For are not trusted (open question 32).
func RemoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// GrantNames lists grants as the audit log records them, sorted: role@scope.
func GrantNames(grants []authz.Grant) []string {
	out := make([]string, 0, len(grants))
	for _, g := range grants {
		out = append(out, string(g.Role)+"@"+ScopeName(g.Scope))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// refusal is a login Quai refuses for what it knows of the person: its message is for them.
type refusal struct{ msg string }

func (r refusal) Error() string { return r.msg }

// recordLogin keeps the person in the directory as this login describes them, and writes the
// login in the audit log, with the grants the person has now and their change since the person's
// last login. It is written before the session starts: a login the log cannot record does not
// happen. It refuses a deactivated person, and a name the directory holds for another subject of
// the identity provider (open question 45). The person's manager is the login's, and a change of it
// is written too (auth.manager_changed).
//
// A person SCIM provisions is found by the login's immutable identifier (Entra ID's oid, else its
// sub), never by its name claim: their name in Quai is the one SCIM gave them, a rename at the
// identity provider notwithstanding. Their first sign-in must carry that very name, so that SCIM
// alone cannot hand a name to someone else; a name SCIM bound to another identifier is refused
// (open question 84). It sets u's name, groups and session epoch to the directory's.
func (a *Authenticator) recordLogin(ctx context.Context, u *login) error {
	binding := u.ObjectID
	if binding == "" {
		binding = u.Subject
	}
	kind := config.SCIMGroupExternalID
	if a.cfg.SCIM != nil {
		kind = a.cfg.SCIM.GroupName
	}
	return a.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if u.Method == config.AuthOIDC {
			switch bound, err := q.UserByBinding(ctx, binding); {
			case errors.Is(err, pgx.ErrNoRows):
			case err != nil:
				return err
			case bound.DeactivatedAt.Valid:
				return refusal{"your Quai account is deactivated; ask an administrator"}
			case bound.Subject != "" && bound.Subject != u.Subject:
				return refusal{"your account of the identity provider is not the one Quai provisioned; ask an administrator"}
			case bound.Subject == "" && !strings.EqualFold(bound.Name, u.Name) && !strings.EqualFold(bound.ScimUserName, u.Name):
				return refusal{fmt.Sprintf("Quai's provisioning knows you as %q, and the identity provider names you %q; "+
					"ask an administrator to make them agree", bound.ScimUserName, u.Name)}
			default:
				u.Name = bound.Name
			}
		}
		actor := audit.Actor{Type: audit.User, ID: u.Name, IP: audit.IPFrom(ctx), AuthTime: u.AuthTime}
		user := audit.Object{Type: "user", ID: u.Name}
		var manager, source string // as the directory had them
		switch known, err := q.GetUser(ctx, u.Name); {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		case known.DeactivatedAt.Valid:
			return refusal{"your Quai account is deactivated; ask an administrator"}
		case u.Subject != "" && known.Subject != "" && known.Subject != u.Subject:
			return refusal{fmt.Sprintf("the name %q belongs to another account of the identity provider; "+
				"ask an administrator to check the name claim, or to release the name", u.Name)}
		case known.ScimID.Valid && (binding == "" || !strings.EqualFold(known.ScimExternalID, binding)):
			return refusal{fmt.Sprintf("Quai's provisioning gave the name %q to another account of the identity provider; "+
				"ask an administrator to check that the provisioning maps the object ID to externalId", u.Name)}
		default:
			manager, source = known.ManagerID, known.ManagerSource
			if known.ScimID.Valid { // the manager is SCIM's (open question 87)
				u.ManagerID, u.ManagerSource, u.ManagerReason = known.ManagerID, known.ManagerSource, known.ManagerReason
			}
		}
		row, err := q.RecordUserLogin(ctx, db.RecordUserLoginParams{
			Name: u.Name, Email: u.Email, Groups: nonNil(u.Groups), Subject: u.Subject, ObjectID: u.ObjectID,
			AuthTime:  pgtype.Timestamptz{Time: u.AuthTime, Valid: !u.AuthTime.IsZero()},
			ManagerID: u.ManagerID, ManagerSource: u.ManagerSource, ManagerReason: u.ManagerReason,
			GroupKind: kind, Binding: binding,
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows): // deactivated, or the name taken by another subject, since GetUser
			return refusal{"your Quai account changed while you signed in; sign in again, or ask an administrator"}
		case err != nil:
			return err
		}
		u.Groups, u.Epoch = row.Groups, row.SessionEpoch
		grants := GrantNames(a.principal(u.Name, u.Groups, nil).Grants)
		details := map[string]any{"method": u.Method, "grants": grants}
		if u.StepUp {
			details["stepUp"] = true
		}
		if u.ACR != "" {
			details["acr"] = u.ACR
		}
		if len(u.ACRS) > 0 {
			details["acrs"] = u.ACRS
		}
		events := []audit.Event{{Actor: actor, Action: "auth.login", Object: user, Details: details}}
		if manager != u.ManagerID || source != u.ManagerSource {
			// The identifiers are personal data; why there is none is Quai's words.
			after := map[string]any{"manager": audit.Personal(u.ManagerID), "source": u.ManagerSource}
			if u.ManagerReason != "" {
				after["reason"] = u.ManagerReason
			}
			events = append(events, audit.Event{Actor: actor, Action: "auth.manager_changed", Object: user,
				Details: map[string]any{"before": map[string]any{"manager": audit.Personal(manager), "source": source}, "after": after}})
		}
		raw, err := q.LastLoginGrants(ctx, u.Name)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			var before struct {
				Grants []string `json:"grants"`
			}
			if json.Unmarshal(raw, &before) == nil && !slices.Equal(before.Grants, grants) {
				events = append(events, audit.Event{Actor: actor, Action: "auth.grants_changed", Object: user,
					Details: map[string]any{"before": before.Grants, "after": grants}})
			}
		}
		return audit.Add(ctx, tx, events...)
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
