package requests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/engine/schema"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Delegation (docs/design/05-delivery-and-approvals.md#delegation): an approver hands their
// approvals to a named colleague for a period, out of office. During it, the delegate may approve
// or deny, on the delegator's behalf, what the delegator could approve or deny at that moment, and
// the delegator's approvals needed go to the delegate. One human makes one approval
// (policy.Standing); a delegation never passes on what its delegator holds on someone else's
// behalf (policy.Delegated).

// UseClock sets the clock delegations are judged by, for tests.
func (s *Service) UseClock(now func() time.Time) { s.now = now }

// ErrNotAllowed is returned when someone may not do what they ask, such as making a delegation
// with an API token.
var ErrNotAllowed = errors.New("not allowed")

// Delegate hands who's approvals to delegate from starts, inclusive, to ends, exclusive: only
// who, for themselves, in a browser session, after a step-up when auth.stepUp is configured. The
// delegate is someone else who has signed in to Quai and is not deactivated; the period is at most
// 90 days and not in the past (policy.Bounds); who has one delegate at a time, and nobody
// delegates to someone who has delegated their own approvals for part of the period
// (policy.Conflict). The delegation is in the audit log as delegation.created.
func (s *Service) Delegate(ctx context.Context, who auth.Identity, delegate string, starts, ends time.Time) (db.Delegation, error) {
	ctx = acting(ctx, who)
	me := who.Principal.Name
	if me == "" || who.TokenID != "" || who.Principal.Scopes != nil {
		return db.Delegation{}, fmt.Errorf("%w: delegations are made in Quai, signed in, never with an API token", ErrNotAllowed)
	}
	now := s.now()
	delegate = strings.TrimSpace(delegate)
	starts, ends, fields := policy.Bounds(starts, ends, now)
	if fields == nil {
		fields = map[string]string{}
	}
	switch {
	case delegate == "":
		fields["delegate"] = "is required"
	case delegate == me:
		fields["delegate"] = "is you: choose a colleague"
	}
	if len(fields) > 0 {
		return db.Delegation{}, &schema.ValidationError{Fields: fields}
	}
	// Handing on one's approvals is as sensitive as approving: a step-up whenever auth.stepUp is
	// configured (open question 75).
	if s.stepUp.Configured() {
		if err := s.stepUp.Require(ctx, who, "delegation.create", audit.Object{Type: "user", ID: me}); err != nil {
			return db.Delegation{}, err
		}
	}
	var d db.Delegation
	err := s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := q.LockDelegations(ctx); err != nil {
			return err
		}
		self, err := q.GetUser(ctx, me)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && self.DeactivatedAt.Valid {
			return fmt.Errorf("%w: Quai has no record of you, or you were deactivated: sign in again", ErrConflict)
		}
		if err != nil {
			return err
		}
		u, err := q.GetUser(ctx, delegate)
		switch {
		case errors.Is(err, pgx.ErrNoRows) || err == nil && !u.LastLogin.Valid:
			return &schema.ValidationError{Fields: map[string]string{"delegate": "is no one who has signed in to Quai: they must sign in once first"}}
		case err != nil:
			return err
		case u.DeactivatedAt.Valid:
			return &schema.ValidationError{Fields: map[string]string{"delegate": "was deactivated: choose someone else"}}
		}
		others, err := q.DelegationsOf(ctx, db.DelegationsOfParams{At: instant(now), Names: []string{me, delegate}})
		if err != nil {
			return err
		}
		next := policy.Delegation{Delegator: me, Delegate: delegate, Starts: starts, Ends: ends}
		if why := policy.Conflict(next, delegationsOf(others)); why != "" {
			return fmt.Errorf("%w: %s", ErrConflict, why)
		}
		if d, err = q.CreateDelegation(ctx, db.CreateDelegationParams{ID: uuid.New(), Delegator: me, Delegate: delegate,
			StartsAt: instant(starts), EndsAt: instant(ends)}); err != nil {
			return err
		}
		return audit.Add(ctx, tx, audit.Event{Action: "delegation.created", Object: audit.Object{Type: "delegation", ID: d.ID.String()},
			Details: map[string]any{"delegator": audit.Personal(me), "delegate": audit.Personal(delegate),
				"starts": audit.Time(starts), "ends": audit.Time(ends)}})
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return db.Delegation{}, fmt.Errorf("%w: Quai no longer has a record of you or of %s: sign in again", ErrConflict, delegate)
	}
	return d, err
}

// DelegationStepUp reports whether making a delegation needs a step-up: whenever auth.stepUp is
// configured.
func (s *Service) DelegationStepUp() bool { return s.stepUp.Configured() }

// DelegationActive reports whether a delegation passes its delegator's approvals on now.
func (s *Service) DelegationActive(d db.Delegation) bool { return delegationOf(d).Active(s.now()) }

// Delegations returns the delegations someone gave or received that are neither revoked nor ended.
func (s *Service) Delegations(ctx context.Context, name string) ([]db.Delegation, error) {
	return s.store.DelegationsOf(ctx, db.DelegationsOfParams{At: instant(s.now()), Names: []string{name}})
}

// AllDelegations returns every delegation neither revoked nor ended, for platform admins.
func (s *Service) AllDelegations(ctx context.Context) ([]db.Delegation, error) {
	return s.store.ListDelegations(ctx, instant(s.now()))
}

// RevokeDelegation ends a delegation now: its delegator's, or anyone's for a platform admin
// (admin, which the caller decides), in the audit log as delegation.revoked. A review made on its
// strength before stays: rights are those of the moment of the review. ErrNotFound for a
// delegation that is not who's to revoke, ErrConflict for one already revoked or ended.
func (s *Service) RevokeDelegation(ctx context.Context, who auth.Identity, id uuid.UUID, admin bool) (db.Delegation, error) {
	ctx = acting(ctx, who)
	var d db.Delegation
	err := s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		cur, err := q.GetDelegation(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && cur.Delegator != who.Principal.Name && !admin {
			return fmt.Errorf("%w: no such delegation of yours", ErrNotFound)
		}
		if err != nil {
			return err
		}
		d, err = q.RevokeDelegation(ctx, db.RevokeDelegationParams{At: instant(s.now()), RevokedBy: who.Principal.Name, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: the delegation is already revoked, or has ended", ErrConflict)
		}
		if err != nil {
			return err
		}
		by := "delegator"
		if d.Delegator != who.Principal.Name {
			by = "admin"
		}
		return audit.Add(ctx, tx, audit.Event{Action: "delegation.revoked", Object: audit.Object{Type: "delegation", ID: d.ID.String()},
			Details: map[string]any{"delegator": audit.Personal(d.Delegator), "delegate": audit.Personal(d.Delegate), "by": by,
				"starts": audit.Time(d.StartsAt.Time), "ends": audit.Time(d.EndsAt.Time)}})
	})
	return d, err
}

// Delegator is someone whose approvals a person holds now through a delegation, as the directory
// describes them now.
type Delegator struct {
	Delegation db.Delegation
	User       db.User
}

// Delegators returns the people whose approvals someone holds now, through the delegations
// active now that they received: delegators still in the directory and not deactivated. None
// with an API token: a delegation is handed to a person, who reviews on the delegator's behalf
// in Quai, signed in (open question 75).
func (s *Service) Delegators(ctx context.Context, p authz.Principal) ([]Delegator, error) {
	if p.Name == "" || p.Scopes != nil {
		return nil, nil
	}
	ds, err := s.store.ActiveDelegationsTo(ctx, db.ActiveDelegationsToParams{Delegate: p.Name, At: instant(s.now())})
	if err != nil {
		return nil, err
	}
	var out []Delegator
	for _, d := range ds {
		if !delegationOf(d).Active(s.now()) {
			continue
		}
		u, err := s.store.GetUser(ctx, d.Delegator)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !u.DeactivatedAt.Valid {
			out = append(out, Delegator{Delegation: d, User: u})
		}
	}
	return out, nil
}

// Capacity is how someone may review a request now: in their own right (CanReview), and on
// behalf of each delegator whose approvals they hold now and whose rights reach the request, under
// the checks no delegation lifts (policy.Delegated). A delegator who reviewed the request's content
// themselves is left out: their own decision stands.
type Capacity struct {
	Own bool
	For []Delegator
}

// Any reports whether someone may review the request at all.
func (c Capacity) Any() bool { return c.Own || len(c.For) > 0 }

// Delegator returns the delegator of For named name.
func (c Capacity) Delegator(name string) (Delegator, bool) {
	for _, d := range c.For {
		if d.User.Name == name {
			return d, true
		}
	}
	return Delegator{}, false
}

// CapacityOf says how someone may review a request now, given the delegators whose approvals they
// hold (Delegators). A failure to read the reviews leaves only their own right.
func (s *Service) CapacityOf(ctx context.Context, r db.Request, p authz.Principal, delegators []Delegator) Capacity {
	c := Capacity{Own: CanReview(r, p)}
	if r.State != PendingApproval || len(delegators) == 0 {
		return c
	}
	reviews, err := s.reviews(ctx, r)
	if err != nil {
		return c
	}
	var rules []policy.Rule
	_ = json.Unmarshal(r.Rules, &rules)
	pc := contextOf(r)
	who := policy.Person{Name: p.Name, Email: p.Email, Groups: p.Groups, Teams: p.Teams}
	for _, d := range delegators {
		if reviewedBy(reviews, d.User.Name) {
			continue
		}
		for i := range rules {
			if policy.Delegated(rules, i, who, s.person(d.User), pc) {
				c.For = append(c.For, d)
				break
			}
		}
	}
	return c
}

// reviewingFor returns the delegator on whose behalf someone reviews a request now (ReviewLink):
// one whose approvals they hold now, who has not reviewed its content themselves, and whose rights
// reach it under the reviewer's own checks.
func (s *Service) reviewingFor(ctx context.Context, r db.Request, who auth.Identity, name string) (Delegator, error) {
	if who.TokenID != "" || who.Principal.Scopes != nil {
		return Delegator{}, fmt.Errorf("%w on %s's behalf: a review on someone's behalf is made in Quai, signed in, never with an API token", ErrNotApprover, name)
	}
	delegators, err := s.Delegators(ctx, who.Principal)
	if err != nil {
		return Delegator{}, err
	}
	if !slices.ContainsFunc(delegators, func(d Delegator) bool { return d.User.Name == name }) {
		return Delegator{}, fmt.Errorf("%w on %s's behalf: they have not delegated their approvals to you now", ErrNotApprover, name)
	}
	reviews, err := s.reviews(ctx, r)
	if err != nil {
		return Delegator{}, err
	}
	if reviewedBy(reviews, name) {
		return Delegator{}, fmt.Errorf("%w: %s reviewed this content themselves: their decision stands", ErrConflict, name)
	}
	d, ok := s.CapacityOf(ctx, r, who.Principal, delegators).Delegator(name)
	if !ok {
		return Delegator{}, fmt.Errorf("%w on %s's behalf: they could not review it themselves now, or you cannot review it, having requested it or changed its definitions", ErrNotApprover, name)
	}
	return d, nil
}

// MayReview reports whether someone may review a request now, in their own right or on someone's
// behalf.
func (s *Service) MayReview(ctx context.Context, r db.Request, p authz.Principal) bool {
	if CanReview(r, p) {
		return true
	}
	if r.State != PendingApproval {
		return false
	}
	delegators, err := s.Delegators(ctx, p)
	return err == nil && s.CapacityOf(ctx, r, p, delegators).Any()
}

// person is someone of the directory as the policy sees them, with the teams of their groups.
func (s *Service) person(u db.User) policy.Person {
	return policy.Person{Name: u.Name, Email: u.Email, Groups: u.Groups, Teams: s.dir.Teams(u.Groups)}
}

// reviewedBy reports whether someone reviewed in their own right among reviews.
func reviewedBy(reviews []db.Review, name string) bool {
	for _, v := range reviews {
		if v.Reviewer == name {
			return true
		}
	}
	return false
}

func delegationOf(d db.Delegation) policy.Delegation {
	return policy.Delegation{Delegator: d.Delegator, Delegate: d.Delegate, Starts: d.StartsAt.Time, Ends: d.EndsAt.Time, Revoked: d.RevokedAt.Valid}
}

func delegationsOf(ds []db.Delegation) []policy.Delegation {
	out := make([]policy.Delegation, len(ds))
	for i, d := range ds {
		out[i] = delegationOf(d)
	}
	return out
}

// instant is a time for a query.
func instant(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// onBehalfLabel is how a review on someone's behalf names both people: "fiona (on behalf of
// alice)"; the reviewer alone for a review in their own right.
func onBehalfLabel(v db.Review) string {
	if v.OnBehalfOf == "" {
		return v.Reviewer
	}
	return v.Reviewer + " (on behalf of " + v.OnBehalfOf + ")"
}
