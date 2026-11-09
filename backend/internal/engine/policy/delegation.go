package policy

import (
	"time"
)

// Delegation is an approver's approvals handed to a named colleague for a period, out of office
// (docs/design/05-delivery-and-approvals.md#delegation): from Starts, inclusive, to Ends,
// exclusive. During it, the delegate may approve or deny what the delegator could, on the
// delegator's behalf (Delegated); the delegate's own rights, and what the delegate holds through
// other delegations, are never passed on.
type Delegation struct {
	Delegator, Delegate string
	Starts, Ends        time.Time
	Revoked             bool
}

// The bounds of a delegation (open question 24): at most MaxDelegation long, starting at most
// MaxDelegationLead ahead, never in the past. A start less than DelegationSkew in the past, the
// delegator's clock being behind, starts now.
const (
	MaxDelegation     = 90 * 24 * time.Hour
	MaxDelegationLead = 365 * 24 * time.Hour
	DelegationSkew    = time.Minute
)

// Active reports whether a delegation passes on the delegator's approvals at an instant.
func (d Delegation) Active(at time.Time) bool {
	return !d.Revoked && d.Delegator != "" && d.Delegate != "" && d.Delegator != d.Delegate &&
		!at.Before(d.Starts) && at.Before(d.Ends)
}

// overlaps reports whether two periods share an instant.
func (d Delegation) overlaps(o Delegation) bool {
	return d.Starts.Before(o.Ends) && o.Starts.Before(d.Ends)
}

// Bounds checks a new delegation's period at now: it returns the period, a start that is now but
// for the clock skew made now, or why it cannot be, by field (starts, ends).
func Bounds(starts, ends, now time.Time) (time.Time, time.Time, map[string]string) {
	fields := map[string]string{}
	switch {
	case starts.Before(now.Add(-DelegationSkew)):
		fields["startsAt"] = "is in the past: a delegation starts now at the earliest"
	case starts.Before(now):
		starts = now
	case starts.Sub(now) > MaxDelegationLead:
		fields["startsAt"] = "is more than a year ahead"
	}
	switch {
	case !ends.After(starts):
		fields["endsAt"] = "must be after the start"
	case ends.Sub(starts) > MaxDelegation:
		fields["endsAt"] = "is more than 90 days after the start: a delegation lasts 90 days at most"
	}
	if len(fields) > 0 {
		return starts, ends, fields
	}
	return starts, ends, nil
}

// Conflict returns why a new delegation cannot be made beside others, the delegations not revoked
// of its delegator and its delegate, or "" when it can: a delegator has one delegate at a time, and
// nobody delegates to someone who has delegated their own approvals for part of the period, so
// that delegations never form a cycle. Someone who holds approvals through a delegation may still
// delegate their own: Delegated never passes the first on.
func Conflict(d Delegation, others []Delegation) string {
	for _, o := range others {
		if o.Revoked || !d.overlaps(o) {
			continue
		}
		switch o.Delegator {
		case d.Delegator:
			return "you have delegated your approvals to " + o.Delegate + " for part of that period: revoke that delegation first"
		case d.Delegate:
			return d.Delegate + " has delegated their own approvals for part of that period: choose someone else"
		}
	}
	return ""
}
