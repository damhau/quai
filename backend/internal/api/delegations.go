package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/store/db"
)

// DelegationView is a delegation of approvals (docs/design/05-delivery-and-approvals.md#delegation).
type DelegationView struct {
	ID        string    `json:"id"`
	Delegator string    `json:"delegator" doc:"Who handed their approvals on"`
	Delegate  string    `json:"delegate" doc:"Who may review on the delegator's behalf during the period"`
	StartsAt  time.Time `json:"startsAt" doc:"When it starts, inclusive"`
	EndsAt    time.Time `json:"endsAt" doc:"When it ends, exclusive"`
	CreatedAt time.Time `json:"createdAt"`
	Active    bool      `json:"active" doc:"It passes the delegator's approvals on now"`
}

// DelegationsOutput lists delegations neither revoked nor ended.
type DelegationsOutput struct {
	Body struct {
		Delegations []DelegationView `json:"delegations"`
		MaxDays     int              `json:"maxDays" doc:"How long a delegation lasts at most, in days"`
		StepUp      bool             `json:"stepUp" doc:"Making a delegation needs a recent sign-in (auth.stepUp is configured)"`
	}
}

// DelegationOutput is a delegation.
type DelegationOutput struct {
	Body DelegationView
}

// DelegateBody hands the caller's approvals to a colleague for a period.
type DelegateBody struct {
	Delegate string    `json:"delegate" minLength:"1" maxLength:"256" doc:"The colleague, by their name in Quai: someone who has signed in and is not deactivated"`
	StartsAt time.Time `json:"startsAt" doc:"When it starts, now at the earliest: an instant, with its offset"`
	EndsAt   time.Time `json:"endsAt" doc:"When it ends, at most 90 days after the start"`
}

// DelegateInput is a new delegation.
type DelegateInput struct {
	Body DelegateBody
}

// DelegationIDInput names a delegation.
type DelegationIDInput struct {
	ID string `path:"id" format:"uuid"`
}

// registerDelegations registers delegations of approvals: one's own, from the session, never for
// someone else, never with an API token (authz.ManageOwnDelegations); platform admins list and
// revoke anyone's (authz.ManageDelegations), and make none.
func registerDelegations(a huma.API, svc *requests.Service) {
	list := func(ds []db.Delegation, err error) (*DelegationsOutput, error) {
		if err != nil {
			return nil, err
		}
		out := &DelegationsOutput{}
		out.Body.Delegations = []DelegationView{}
		for _, d := range ds {
			out.Body.Delegations = append(out.Body.Delegations, delegationView(svc, d))
		}
		out.Body.MaxDays = int(policy.MaxDelegation / (24 * time.Hour))
		out.Body.StepUp = svc.DelegationStepUp()
		return out, nil
	}

	huma.Register(a, huma.Operation{
		OperationID: "list-my-delegations", Method: http.MethodGet, Path: Prefix + "/me/delegations",
		Summary: "List the delegations of approvals you gave or received", Tags: []string{"Identity"},
		Description: "Those neither revoked nor ended. An API token cannot read them.",
	}, func(ctx context.Context, _ *struct{}) (*DelegationsOutput, error) {
		id, err := authorize(ctx, authz.ManageOwnDelegations, authz.Object{})
		if err != nil {
			return nil, err
		}
		return list(svc.Delegations(ctx, id.Principal.Name))
	})

	huma.Register(a, huma.Operation{
		OperationID: "create-delegation", Method: http.MethodPost, Path: Prefix + "/me/delegations",
		Summary: "Delegate your approvals to a colleague for a period", Tags: []string{"Identity"}, DefaultStatus: http.StatusCreated,
		Description: "During the period the delegate may approve or deny, on your behalf, what you could approve or deny. " +
			"One delegate at a time; at most 90 days; never to someone who has delegated their own approvals for part of the period. " +
			"It needs a recent sign-in when step-up is configured, and cannot be made with an API token.",
	}, func(ctx context.Context, in *DelegateInput) (*DelegationOutput, error) {
		id, err := authorize(ctx, authz.ManageOwnDelegations, authz.Object{})
		if err != nil {
			return nil, err
		}
		d, err := svc.Delegate(ctx, id, in.Body.Delegate, in.Body.StartsAt, in.Body.EndsAt)
		if err != nil {
			return nil, Problem(err, "body")
		}
		return &DelegationOutput{Body: delegationView(svc, d)}, nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "list-delegations", Method: http.MethodGet, Path: Prefix + "/delegations",
		Summary: "List everyone's delegations of approvals", Tags: []string{"Identity"},
		Description: "Those neither revoked nor ended, for platform admins.",
	}, func(ctx context.Context, _ *struct{}) (*DelegationsOutput, error) {
		if _, err := authorize(ctx, authz.ManageDelegations, authz.Object{}); err != nil {
			return nil, err
		}
		return list(svc.AllDelegations(ctx))
	})

	huma.Register(a, huma.Operation{
		OperationID: "revoke-delegation", Method: http.MethodDelete, Path: Prefix + "/delegations/{id}",
		Summary: "Revoke a delegation of approvals", Tags: []string{"Identity"}, DefaultStatus: http.StatusNoContent,
		Description: "Yours, or anyone's for a platform admin. Reviews already made on its strength stay.",
	}, func(ctx context.Context, in *DelegationIDInput) (*struct{}, error) {
		id, err := authorize(ctx, authz.ManageOwnDelegations, authz.Object{})
		if err != nil {
			return nil, err
		}
		did, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, huma.Error404NotFound("no such delegation")
		}
		if _, err := svc.RevokeDelegation(ctx, id, did, allowed(ctx, authz.ManageDelegations, authz.Object{})); err != nil {
			return nil, Problem(err, "path")
		}
		return nil, nil
	})
}

func delegationView(svc *requests.Service, d db.Delegation) DelegationView {
	return DelegationView{ID: d.ID.String(), Delegator: d.Delegator, Delegate: d.Delegate, StartsAt: d.StartsAt.Time, EndsAt: d.EndsAt.Time,
		CreatedAt: d.CreatedAt.Time, Active: svc.DelegationActive(d)}
}
