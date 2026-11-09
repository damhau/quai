package api

import (
	"bufio"
	"cmp"
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/store"
)

// AccessRoleView is a role a person has at a scope, and the groups that give it to them.
type AccessRoleView struct {
	Role   string   `json:"role"`
	Scope  string   `json:"scope" doc:"global, or team:, repository: or catalog-item: and its name"`
	Groups []string `json:"groups" doc:"The person's groups that auth.roles gives the role to"`
}

// AccessTokenView is an API token that still works: it acts with the groups its owner had when
// creating it, narrowed by its scopes.
type AccessTokenView struct {
	ID         string           `json:"id"`
	Name       string           `json:"name"`
	Scopes     []string         `json:"scopes"`
	Groups     []string         `json:"groups" doc:"The groups its owner had when creating it, which it acts with"`
	Roles      []AccessRoleView `json:"roles" doc:"The roles those groups give now; the scopes narrow what the token may do with them"`
	ExpiresAt  time.Time        `json:"expiresAt"`
	LastUsedAt *time.Time       `json:"lastUsedAt,omitempty"`
}

// AccessUserView is a person in the access report: as their last login described them, with the
// teams and roles the configuration gives their groups now.
type AccessUserView struct {
	Name          string            `json:"name"`
	Email         string            `json:"email,omitempty" doc:"Only an email the identity provider vouches for"`
	Subject       string            `json:"subject,omitempty" doc:"The identity provider's subject (the ID token's sub)"`
	ObjectID      string            `json:"objectId,omitempty" doc:"Entra ID's object ID (oid)"`
	Groups        []string          `json:"groups"`
	Teams         []string          `json:"teams"`
	Roles         []AccessRoleView  `json:"roles"`
	Tokens        []AccessTokenView `json:"tokens" doc:"Their API tokens that still work"`
	State         string            `json:"state" enum:"active,deactivated"`
	LastLogin     *time.Time        `json:"lastLogin,omitempty" doc:"Absent when no sign-in is recorded: the person's tokens predate the directory"`
	AuthTime      *time.Time        `json:"authTime,omitempty" doc:"When the person last authenticated with the identity provider"`
	DeactivatedAt *time.Time        `json:"deactivatedAt,omitempty"`
}

// AccessReportView is who can do what in Quai: every person who signed in or holds an API token,
// by name.
type AccessReportView struct {
	GeneratedAt time.Time        `json:"generatedAt"`
	Users       []AccessUserView `json:"users"`
}

func registerAccess(a huma.API, s *store.Store, dir *auth.Directory, log *slog.Logger) {
	huma.Register(a, huma.Operation{
		OperationID: "get-access-report", Method: http.MethodGet, Path: Prefix + "/access-report",
		Summary: "Read the access report: every user with their groups, teams and roles (auditors and platform admins)",
		Tags:    []string{"Access"},
	}, func(ctx context.Context, _ *struct{}) (*out[AccessReportView], error) {
		if _, err := authorize(ctx, authz.ReadAccessReport, authz.Object{}); err != nil {
			return nil, err
		}
		r, err := accessReport(ctx, s, dir)
		if err != nil {
			return nil, err
		}
		return &out[AccessReportView]{Body: r}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "export-access-report", Method: http.MethodGet, Path: Prefix + "/access-report/export",
		Summary: "Download the access report as CSV (auditors and platform admins)",
		Description: "One row per person or API token (via), role, scope and the group that gives it; one without a role " +
			"has one row with the role columns empty. Cells a spreadsheet would read as a formula start with an apostrophe.",
		Tags: []string{"Access"},
		Responses: map[string]*huma.Response{"200": {
			Description: "The report, in CSV",
			Content:     map[string]*huma.MediaType{"text/csv": {Schema: &huma.Schema{Type: "string"}}},
		}},
	}, func(ctx context.Context, _ *struct{}) (*huma.StreamResponse, error) {
		if _, err := authorize(ctx, authz.ReadAccessReport, authz.Object{}); err != nil {
			return nil, err
		}
		r, err := accessReport(ctx, s, dir)
		if err != nil {
			return nil, err
		}
		return &huma.StreamResponse{Body: func(hc huma.Context) {
			hc.SetHeader("Content-Type", "text/csv; charset=utf-8")
			hc.SetHeader("Content-Disposition", `attachment; filename="quai-access-report-`+r.GeneratedAt.Format(time.DateOnly)+`.csv"`)
			hc.SetHeader("Cache-Control", "no-store")
			hc.SetStatus(http.StatusOK)
			if err := writeAccessCSV(hc.BodyWriter(), r); err != nil {
				log.Error("access report export", slog.Any("error", err))
			}
		}}, nil
	})
}

// accessReport lists the people in the directory, and the owners of live API tokens, with what
// their groups give them now.
func accessReport(ctx context.Context, s *store.Store, dir *auth.Directory) (AccessReportView, error) {
	users, err := s.ListUsers(ctx)
	if err != nil {
		return AccessReportView{}, err
	}
	tokens, err := s.ListLiveAPITokens(ctx)
	if err != nil {
		return AccessReportView{}, err
	}
	r := AccessReportView{GeneratedAt: time.Now().UTC().Truncate(time.Second), Users: make([]AccessUserView, 0, len(users))}
	at := func(t pgtype.Timestamptz) *time.Time {
		if !t.Valid {
			return nil
		}
		v := t.Time.UTC()
		return &v
	}
	for _, u := range users {
		v := AccessUserView{
			Name: u.Name, Email: u.Email, Subject: u.Subject, ObjectID: u.ObjectID,
			Groups: slices.Sorted(slices.Values(u.Groups)), Teams: append([]string{}, dir.Teams(u.Groups)...), Roles: rolesOf(dir, u.Groups),
			Tokens: []AccessTokenView{}, State: "active", LastLogin: at(u.LastLogin), AuthTime: at(u.AuthTime), DeactivatedAt: at(u.DeactivatedAt),
		}
		if u.DeactivatedAt.Valid {
			v.State = "deactivated"
		}
		r.Users = append(r.Users, v)
	}
	for _, t := range tokens {
		i := slices.IndexFunc(r.Users, func(u AccessUserView) bool { return u.Name == t.Owner })
		if i < 0 { // a token from before the directory, whose owner has not signed in since
			r.Users = append(r.Users, AccessUserView{Name: t.Owner, Groups: []string{}, Teams: []string{}, Roles: []AccessRoleView{}, Tokens: []AccessTokenView{}, State: "active"})
			i = len(r.Users) - 1
		}
		r.Users[i].Tokens = append(r.Users[i].Tokens, AccessTokenView{
			ID: t.ID.String(), Name: t.Name, Scopes: t.Scopes, Groups: slices.Sorted(slices.Values(t.Groups)), Roles: rolesOf(dir, t.Groups),
			ExpiresAt: t.ExpiresAt.Time.UTC(), LastUsedAt: at(t.LastUsedAt),
		})
	}
	slices.SortFunc(r.Users, func(a, b AccessUserView) int { return strings.Compare(a.Name, b.Name) })
	return r, nil
}

// rolesOf lists the roles groups give, by role and scope, each with the groups that give it.
func rolesOf(dir *auth.Directory, groups []string) []AccessRoleView {
	roles := []AccessRoleView{}
	for _, g := range dir.Grants(groups) {
		role, scope := string(g.Role), auth.ScopeName(g.Scope)
		i := slices.IndexFunc(roles, func(r AccessRoleView) bool { return r.Role == role && r.Scope == scope })
		if i < 0 {
			roles = append(roles, AccessRoleView{Role: role, Scope: scope})
			i = len(roles) - 1
		}
		roles[i].Groups = append(roles[i].Groups, g.Group)
	}
	slices.SortFunc(roles, func(a, b AccessRoleView) int {
		return cmp.Or(cmp.Compare(a.Role, b.Role), cmp.Compare(a.Scope, b.Scope))
	})
	for i := range roles {
		slices.Sort(roles[i].Groups)
	}
	return roles
}

// accessColumns are the access report's CSV columns.
var accessColumns = []string{"name", "email", "subject", "object id", "state", "last login", "authenticated", "groups", "teams",
	"via", "token scopes", "token expires", "role", "scope", "granted by"}

// writeAccessCSV writes the report in CSV (RFC 4180, UTF-8 with a byte order mark for
// spreadsheets): one row per person or token, role, scope and granting group.
func writeAccessCSV(w io.Writer, r AccessReportView) error {
	bw := bufio.NewWriter(w)
	_, _ = bw.WriteString("\ufeff")
	writeCSVRow(bw, accessColumns)
	when := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.Format(time.RFC3339)
	}
	for _, u := range r.Users {
		person := []string{u.Name, u.Email, u.Subject, u.ObjectID, u.State, when(u.LastLogin), when(u.AuthTime),
			strings.Join(u.Groups, "; "), strings.Join(u.Teams, "; ")}
		rows := func(via []string, roles []AccessRoleView) {
			head := append(slices.Clip(person), via...)
			if len(roles) == 0 {
				writeCSVRow(bw, append(slices.Clip(head), "", "", ""))
			}
			for _, role := range roles {
				for _, g := range role.Groups {
					writeCSVRow(bw, append(slices.Clip(head), role.Role, role.Scope, g))
				}
			}
		}
		rows([]string{"sign-in", "", ""}, u.Roles)
		for _, t := range u.Tokens {
			rows([]string{"token:" + t.Name, strings.Join(t.Scopes, " "), when(&t.ExpiresAt)}, t.Roles)
		}
	}
	return bw.Flush()
}

// writeCSVRow writes a CSV row with every cell quoted, so that no separator a spreadsheet may use
// (a comma, or a semicolon in some locales) splits a cell, and each cell neutralized.
func writeCSVRow(w *bufio.Writer, cells []string) {
	for i, c := range cells {
		if i > 0 {
			_ = w.WriteByte(',')
		}
		_ = w.WriteByte('"')
		_, _ = w.WriteString(strings.ReplaceAll(csvSafe(c), `"`, `""`))
		_ = w.WriteByte('"')
	}
	_, _ = w.WriteString("\r\n")
}

// csvSafe neutralizes a cell a spreadsheet would read as a formula: names and groups come from the
// identity provider. A cell that starts with =, +, -, @, a tab or a carriage return gets a leading
// apostrophe (OWASP, CSV injection).
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
