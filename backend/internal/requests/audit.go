package requests

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/store/db"
)

// The audit log records what happens to requests (docs/design/09-security-and-compliance.md#audit-log):
// submitted, reviewed, approved, rejected, cancelled, retried, stale, merged (or merged without
// approval), applied and failed, in the transaction of the change.

// acting returns ctx with who as its actor, for the audit events of what they do.
func acting(ctx context.Context, who auth.Identity) context.Context {
	return audit.WithActor(ctx, auth.ActorOf(ctx, who))
}

// recordRequest adds a request's audit event to the transaction.
func recordRequest(ctx context.Context, tx pgx.Tx, r db.Request, action string, details map[string]any) error {
	return audit.Add(ctx, tx, audit.Event{Action: "request." + action,
		Object: audit.Object{Type: "request", ID: r.ID.String(), Resource: r.Target}, Details: details})
}

// transitionAudit are the audit actions of a transition, by the state it reaches: none for the
// steps in between (merging), two for a direct commit that is final (merged, then applied).
func transitionAudit(to, kind string) []string {
	var out []string
	if kind == "committed" {
		out = append(out, "merged")
	}
	switch to {
	case Stale:
		out = append(out, "stale")
	case Cancelled:
		out = append(out, "cancelled")
	case Failed:
		out = append(out, "failed")
	case Applied:
		out = append(out, "applied")
	}
	return out
}

// auditData are the keys of a transition's timeline data that its audit event keeps: what Quai
// observed, never what a person wrote.
var auditData = []string{"commit", "pullRequest", "run", "stack", "status", "error", "region", "definitions", "separation", "path", "reason", "note"}

// transitionDetails are the audit details of a transition.
func transitionDetails(from, kind string, data map[string]any) map[string]any {
	d := map[string]any{"from": from, "event": kind}
	for _, k := range auditData {
		if v, ok := data[k]; ok {
			d[k] = v
		}
	}
	return d
}

// approvedDetails are the audit details of a request approved with the reviews of its change set
// (hash): the people whose approvals count (policy.Standing), as personal data, and those given on
// someone's behalf, with the delegator and the delegation.
func approvedDetails(hash string, r db.Request, reviews []db.Review) map[string]any {
	approvals := []audit.Personal{}
	var delegated []map[string]any
	for _, v := range standing(r, reviews) {
		if v.Decision != policy.Approve {
			continue
		}
		approvals = append(approvals, audit.Personal(v.Reviewer))
		if v.OnBehalfOf != "" {
			d := map[string]any{"approver": audit.Personal(v.Reviewer), "onBehalfOf": audit.Personal(v.OnBehalfOf)}
			if v.DelegationID.Valid {
				d["delegation"] = uuid.UUID(v.DelegationID.Bytes).String()
			}
			delegated = append(delegated, d)
		}
	}
	details := map[string]any{"changeSet": hash, "approvals": approvals}
	if delegated != nil {
		details["onBehalfOf"] = delegated
	}
	return details
}
