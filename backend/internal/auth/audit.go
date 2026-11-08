package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/authz"
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
		scope := g.Scope.Kind
		if g.Scope.Name != "" {
			scope += ":" + g.Scope.Name
		}
		out = append(out, string(g.Role)+"@"+scope)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// recordLogin writes a login in the audit log, with the grants the person has now, and their
// change since the person's last login. It is written before the session starts: a login the log
// cannot record does not happen.
func (a *Authenticator) recordLogin(ctx context.Context, u sessionUser, method string) error {
	grants := GrantNames(a.principal(u.Name, u.Groups, nil).Grants)
	actor := audit.Actor{Type: audit.User, ID: u.Name, IP: audit.IPFrom(ctx), AuthTime: u.AuthTime}
	user := audit.Object{Type: "user", ID: u.Name}
	return a.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		events := []audit.Event{{Actor: actor, Action: "auth.login", Object: user, Details: map[string]any{"method": method, "grants": grants}}}
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
