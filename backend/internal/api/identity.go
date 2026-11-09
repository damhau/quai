package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// authorize returns the caller's identity if they may do action on obj, and the problem to
// return otherwise: 401 when nobody is signed in, 403 with the reason when the answer is no.
func authorize(ctx context.Context, action authz.Action, obj authz.Object) (auth.Identity, error) {
	id, _ := auth.FromContext(ctx)
	d, err := authz.Authorize(ctx, id.Principal, action, obj)
	switch {
	case err != nil:
		return id, err
	case d.Allowed:
		return id, nil
	case id.Principal.Name == "":
		return id, huma.Error401Unauthorized(d.Reason)
	}
	return id, huma.Error403Forbidden(d.Reason)
}

// RoleView is a role and where it applies.
type RoleView struct {
	Role  string `json:"role"`
	Scope string `json:"scope" doc:"global, or team:, repository: or catalog-item: and a name"`
}

// MeOutput describes the caller.
type MeOutput struct {
	Body struct {
		Name      string     `json:"name"`
		Email     string     `json:"email,omitempty"`
		Groups    []string   `json:"groups"`
		Teams     []string   `json:"teams" doc:"The teams of the groups (auth.teams), which team ownership matches"`
		Roles     []RoleView `json:"roles"`
		Scopes    []string   `json:"scopes,omitempty" doc:"The API token's scopes, when the call uses one"`
		CSRFToken string     `json:"csrfToken,omitempty" doc:"Send it as X-CSRF-Token on requests that change something"`
		StepUp    StepUpView `json:"stepUp" doc:"Whether the caller's authentication satisfies a step-up now"`
	}
}

// StepUpView says whether the caller's authentication satisfies a step-up now: the server checks
// again when the action is made.
type StepUpView struct {
	Satisfied bool       `json:"satisfied"`
	Reason    string     `json:"reason,omitempty" doc:"Why not: sign in again"`
	Until     *time.Time `json:"until,omitempty" doc:"When it stops satisfying it, on the server's clock"`
	MaxAge    int        `json:"maxAge" doc:"How long, in seconds, a sign-in satisfies a step-up"`
	Admin     bool       `json:"admin" doc:"Changing repository connections needs a step-up (auth.stepUp.admin)"`
}

// TokenView is an API token as listed: never the token itself.
type TokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
}

// CreateTokenInput asks for a new API token.
type CreateTokenInput struct {
	Body struct {
		Name          string   `json:"name" minLength:"1" maxLength:"63" pattern:"^[a-z0-9][a-z0-9-]*$"`
		Scopes        []string `json:"scopes" minItems:"1" uniqueItems:"true" enum:"catalog:read,inventory:read,requests:read,requests:write,evidence:read"`
		ExpiresInDays int      `json:"expiresInDays,omitempty" minimum:"1" maximum:"365" default:"90"`
	}
}

// CreatedTokenOutput holds the new token, shown this once.
type CreatedTokenOutput struct {
	Body struct {
		TokenView
		Token string `json:"token" doc:"Shown once: Quai keeps only its hash"`
	}
}

// TokenListOutput lists the caller's tokens.
type TokenListOutput struct {
	Body struct {
		Tokens []TokenView `json:"tokens"`
	}
}

// TokenIDInput names a token.
type TokenIDInput struct {
	ID string `path:"id" format:"uuid"`
}

func registerIdentity(a huma.API, s *store.Store, su *auth.StepUp) {
	// authz: public — it describes only the caller, and says 401 to nobody.
	huma.Register(a, huma.Operation{
		OperationID: "get-me", Method: http.MethodGet, Path: Prefix + "/me",
		Summary: "Describe the signed-in user", Tags: []string{"Identity"},
	}, func(ctx context.Context, _ *struct{}) (*MeOutput, error) {
		id, ok := auth.FromContext(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("not signed in")
		}
		out := &MeOutput{}
		out.Body.Name, out.Body.Email, out.Body.CSRFToken = id.Principal.Name, id.Email, id.CSRFToken
		out.Body.Groups = append([]string{}, id.Principal.Groups...)
		out.Body.Teams = append([]string{}, id.Principal.Teams...)
		out.Body.Scopes = id.Principal.Scopes
		out.Body.Roles = []RoleView{}
		for _, g := range id.Principal.Grants {
			out.Body.Roles = append(out.Body.Roles, RoleView{Role: string(g.Role), Scope: auth.ScopeName(g.Scope)})
		}
		out.Body.StepUp = StepUpView{Reason: su.Check(id), MaxAge: int(su.MaxAge() / time.Second), Admin: su.Admin()}
		if out.Body.StepUp.Reason == "" {
			until := su.Until(id)
			out.Body.StepUp.Satisfied, out.Body.StepUp.Until = true, &until
		}
		return out, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "create-token", Method: http.MethodPost, Path: Prefix + "/tokens",
		Summary: "Create an API token", Tags: []string{"Identity"}, DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *CreateTokenInput) (*CreatedTokenOutput, error) {
		id, err := authorize(ctx, authz.ManageOwnTokens, authz.Object{})
		if err != nil {
			return nil, err
		}
		token, hash := auth.NewToken()
		groups := id.Principal.Groups
		if groups == nil {
			groups = []string{}
		}
		var t db.ApiToken
		err = s.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
			var err error
			t, err = q.CreateAPIToken(ctx, db.CreateAPITokenParams{
				ID: uuid.New(), Owner: id.Principal.Name, Groups: groups, Name: in.Body.Name, Hash: hash,
				Scopes:    slices.Sorted(slices.Values(in.Body.Scopes)),
				ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Duration(in.Body.ExpiresInDays) * 24 * time.Hour), Valid: true},
			})
			if err != nil {
				return err
			}
			return audit.Add(ctx, tx, audit.Event{Action: "token.created", Object: audit.Object{Type: "token", ID: t.ID.String()},
				Details: map[string]any{"name": t.Name, "scopes": t.Scopes, "expiresAt": audit.Time(t.ExpiresAt.Time)}})
		})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, huma.Error409Conflict("you already have a token named " + in.Body.Name)
		}
		if err != nil {
			return nil, err
		}
		out := &CreatedTokenOutput{}
		out.Body.TokenView, out.Body.Token = tokenView(t), token
		return out, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "list-tokens", Method: http.MethodGet, Path: Prefix + "/tokens",
		Summary: "List your API tokens", Tags: []string{"Identity"},
	}, func(ctx context.Context, _ *struct{}) (*TokenListOutput, error) {
		id, err := authorize(ctx, authz.ManageOwnTokens, authz.Object{})
		if err != nil {
			return nil, err
		}
		ts, err := s.ListAPITokens(ctx, id.Principal.Name)
		if err != nil {
			return nil, err
		}
		out := &TokenListOutput{}
		out.Body.Tokens = []TokenView{}
		for _, t := range ts {
			out.Body.Tokens = append(out.Body.Tokens, tokenView(t))
		}
		return out, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "revoke-token", Method: http.MethodDelete, Path: Prefix + "/tokens/{id}",
		Summary: "Revoke one of your API tokens", Tags: []string{"Identity"}, DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *TokenIDInput) (*struct{}, error) {
		id, err := authorize(ctx, authz.ManageOwnTokens, authz.Object{})
		if err != nil {
			return nil, err
		}
		tokenID, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, huma.Error404NotFound("no such token")
		}
		var n int64
		err = s.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
			var err error
			if n, err = q.RevokeAPIToken(ctx, db.RevokeAPITokenParams{ID: tokenID, Owner: id.Principal.Name}); err != nil || n == 0 {
				return err
			}
			return audit.Add(ctx, tx, audit.Event{Action: "token.revoked", Object: audit.Object{Type: "token", ID: tokenID.String()}})
		})
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, huma.Error404NotFound("no such token, or it is already revoked")
		}
		return nil, nil
	})
}

func tokenView(t db.ApiToken) TokenView {
	v := TokenView{ID: t.ID.String(), Name: t.Name, Scopes: t.Scopes, ExpiresAt: t.ExpiresAt.Time, CreatedAt: t.CreatedAt.Time}
	if t.LastUsedAt.Valid {
		v.LastUsedAt = &t.LastUsedAt.Time
	}
	if t.RevokedAt.Valid {
		v.RevokedAt = &t.RevokedAt.Time
	}
	return v
}

// allowed reports whether the caller may do action on obj, for a view to offer it; the handler
// that does it still checks.
func allowed(ctx context.Context, action authz.Action, obj authz.Object) bool {
	return decide(ctx, action, obj).Allowed
}

// decide is Authorize's answer for the caller, with its reason; a failure to decide is a denial.
func decide(ctx context.Context, action authz.Action, obj authz.Object) authz.Decision {
	id, _ := auth.FromContext(ctx)
	d, err := authz.Authorize(ctx, id.Principal, action, obj)
	if err != nil {
		return authz.Decision{Reason: err.Error()}
	}
	return d
}
