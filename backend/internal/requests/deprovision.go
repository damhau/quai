package requests

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Deprovisioned counts what a deactivation ended.
type Deprovisioned struct {
	Delegations int `json:"delegations"`
	Cancelled   int `json:"cancelled"`
	Voided      int `json:"voided"`
}

// deactivatedReason is why Quai cancels a deactivated person's requests.
const deactivatedReason = "its requester was deactivated"

// Deprovision ends what a person SCIM deactivated leaves on requests (open question 86), as the
// actor of ctx, the provisioning: the delegations they gave or received are revoked; their
// requests not delivered yet (Cancellable) are cancelled, quai/approval and their pull requests
// following; and their reviews, or those made on their behalf, of the requests that still wait
// for approval no longer count, the approvers told again (reviews_voided). A request delivered,
// or approved before, keeps its approvals, as a review made before a delegation's revocation does.
// Each is an audit event. Running it again finishes what a failure left: what it ended is ended.
func (s *Service) Deprovision(ctx context.Context, name string) (Deprovisioned, error) {
	var out Deprovisioned
	at := instant(s.now())
	err := s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		ds, err := q.RevokeDelegationsOf(ctx, db.RevokeDelegationsOfParams{At: at, RevokedBy: "provisioning", Name: name})
		if err != nil {
			return err
		}
		out.Delegations = len(ds)
		for _, d := range ds {
			if err := audit.Add(ctx, tx, audit.Event{Action: "delegation.revoked", Object: audit.Object{Type: "delegation", ID: d.ID.String()},
				Details: map[string]any{"delegator": audit.Personal(d.Delegator), "delegate": audit.Personal(d.Delegate), "by": "deactivation",
					"starts": audit.Time(d.StartsAt.Time), "ends": audit.Time(d.EndsAt.Time)}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return out, err
	}

	mine, err := s.store.ListRequests(ctx, db.ListRequestsParams{States: []string{PendingApproval, Approved, Stale, Failed}, Requester: name, MaxRows: 100000})
	if err != nil {
		return out, err
	}
	for _, r := range mine {
		_, err := s.transition(ctx, r, r.State, Cancelled, "cancelled", map[string]any{"by": "provisioning", "reason": deactivatedReason})
		switch {
		case errors.Is(err, ErrConflict): // it moved on meanwhile
		case err != nil:
			return out, err
		default:
			out.Cancelled++
		}
	}

	reviewed, err := s.store.PendingReviewedBy(ctx, name)
	if err != nil {
		return out, err
	}
	for _, r := range reviewed {
		err := s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
			cur, err := q.LockRequest(ctx, r.ID)
			if err != nil || cur.State != PendingApproval {
				return err
			}
			voided, err := q.VoidReviewsOf(ctx, db.VoidReviewsOfParams{RequestID: cur.ID, Name: name, At: at})
			if err != nil || len(voided) == 0 {
				return err
			}
			out.Voided += len(voided)
			if err := s.event(ctx, tx, cur, "reviews_voided", "", map[string]any{"approver": name, "reviews": len(voided),
				"reason": "they were deactivated"}); err != nil {
				return err
			}
			return recordRequest(ctx, tx, cur, "reviews_voided", map[string]any{"approver": audit.Personal(name), "reviews": len(voided)})
		})
		if err != nil {
			return out, err
		}
	}
	return out, nil
}
