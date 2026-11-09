package requests

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/store/db"
)

// withManager fills the plan's manager when a rule's approvers include manager, a rule that reads
// the plan included: the person of the directory the requester's manager identifier names, or none
// and why (docs/design/05-delivery-and-approvals.md#approver-kinds, open question 69). Submitting
// keeps them with the request, and nothing resolves them again (open question 70): a manager
// changed later does not change who may approve it.
func (s *Service) withManager(ctx context.Context, who auth.Identity, p *Plan) error {
	p.Manager, p.ManagerReason = "", ""
	if !slices.ContainsFunc(p.Rules, func(r policy.Rule) bool { return slices.Contains(r.Approvers, "manager") }) {
		return nil
	}
	var err error
	p.Manager, p.ManagerReason, err = s.managerOf(ctx, who.Principal.Name)
	return err
}

// managerOf resolves a requester's manager from the directory: the identity provider's immutable
// identifier the requester's last sign-in found (an Entra ID oid from Microsoft Graph; a sub or an
// oid from a claim), which must name exactly one person who has signed in, is not deactivated, and
// is not the requester. Anything else is no manager, and the reason says why: a manager rule then
// cannot be satisfied.
func (s *Service) managerOf(ctx context.Context, requester string) (name, reason string, err error) {
	u, err := s.store.GetUser(ctx, requester)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", requester + " is not in the directory, so Quai knows no manager for them", nil
	case err != nil:
		return "", "", err
	case u.ManagerID == "" && u.ManagerReason != "":
		return "", "Quai knows no manager for " + requester + ": " + u.ManagerReason, nil
	case u.ManagerID == "":
		return "", "Quai knows no manager for " + requester + ": no ID token claim or Microsoft Graph lookup names one", nil
	}
	var found []db.User
	if u.ManagerSource == auth.ManagerFromSCIM { // the manager's SCIM id (open question 87)
		id, perr := uuid.Parse(u.ManagerID)
		m, err := s.store.SCIMUser(ctx, pgtype.UUID{Bytes: id, Valid: perr == nil})
		switch {
		case perr != nil, errors.Is(err, pgx.ErrNoRows):
			return "", requester + "'s manager is not provisioned in Quai", nil
		case err != nil:
			return "", "", err
		}
		found = []db.User{m}
	} else {
		found, err = s.store.UsersByIdentifier(ctx, db.UsersByIdentifierParams{ID: u.ManagerID, BySubject: u.ManagerSource != auth.ManagerFromGraph})
	}
	switch {
	case err != nil:
		return "", "", err
	case len(found) == 0:
		return "", requester + "'s manager has not signed in to Quai yet", nil
	case len(found) > 1:
		return "", requester + "'s manager is ambiguous: their identifier names several people in the directory", nil
	case found[0].Name == requester:
		return "", requester + " is their own manager in the identity provider: nobody can approve as their manager", nil
	case found[0].DeactivatedAt.Valid:
		return "", requester + "'s manager is deactivated in Quai", nil
	}
	return found[0].Name, "", nil
}
