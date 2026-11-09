package requests

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/engine/schema"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Break-glass (docs/design/05-delivery-and-approvals.md#break-glass): in an incident, a member of
// the break-glass role merges a request without its approvals and whatever its change windows
// say. It goes through the request's one delivery (gate, the merge lock, the head pinned), lifting
// only Quai's approvals and Quai's change windows: the platform's own policies still apply. The
// record (who, why, which content) and its post-incident review are written in the transaction
// that approves the request, before any call to the platform, so that the merge is recognised as
// break-glass, never as a bypass, and a crash after the merge still leaves the record. The review
// closes when two people sign it off (open question 27).

// Limits of what a person types for break-glass.
const (
	// MaxBreakGlassReason is the longest reason, in characters.
	MaxBreakGlassReason = 500
	// MaxIncident is the longest incident reference.
	MaxIncident = 64
	// MaxSignOffComment is the longest comment of a sign-off.
	MaxSignOffComment = 2000
)

// SignOffsNeeded is how many people sign off a break-glass review to close it (open question 27).
const SignOffsNeeded = 2

// ReviewRemindEvery is how often an open break-glass review is reminded, from the break-glass and
// then from the last reminder (open question 99).
const ReviewRemindEvery = 24 * time.Hour

// incidentRe is an incident reference: a ticket's identifier (INC0012345, SEC-42,
// PagerDuty#P1A2B3C), without spaces or markup.
var incidentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/#-]*$`)

// BreakGlassInput is a break-glass: why, and the content the breaker saw, which must be the
// request's still (the change set's hash and, with a pull request, its head).
type BreakGlassInput struct {
	Reason, Incident, ChangeSet, Head string
}

// broken reports whether a request merges as broken by break-glass, its approvals and change
// windows lifted.
func broken(r db.Request) bool { return r.BreakGlassAt.Valid }

// Broken reports whether a request merges, or merged, by break-glass.
func Broken(r db.Request) bool { return broken(r) }

// Breakable reports whether break-glass takes a request in its state (open question 95): pending
// approval, or approved and held by its change windows; not broken already. An approved request
// its windows do not hold merges without it; the others are decided, or delivering.
func Breakable(r db.Request) bool {
	if broken(r) {
		return false
	}
	return r.State == PendingApproval || r.State == Approved && r.WindowHeldAt.Valid
}

// unsafeRune reports a character refused in what people type for break-glass: control
// characters, the line and paragraph separators (U+2028, U+2029) that end a line in some readers,
// and the bidirectional marks, embeddings, overrides and isolates that make text read otherwise
// than it is. Built from code points: an editor may turn escapes of them into the characters.
func unsafeRune(r rune) bool {
	switch {
	case unicode.IsControl(r), unicode.In(r, unicode.Zl, unicode.Zp):
		return true
	case r == 0x200e, r == 0x200f, r == 0x061c, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// typed checks what a person typed: required, at most max characters, no unsafe character
// (newlines too, unless multiline). It returns why it is refused, or "".
func typed(v string, max int, multiline bool) string {
	switch {
	case v == "":
		return "is required"
	case utf8.RuneCountInString(v) > max:
		return fmt.Sprintf("is longer than %d characters", max)
	case strings.ContainsFunc(v, func(r rune) bool { return unsafeRune(r) && !(multiline && r == '\n') }):
		return "holds a control or direction character: type it again"
	}
	return ""
}

// printable drops the unsafe characters from a name Quai puts in a status, a trailer or a comment.
func printable(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unsafeRune(r) {
			return -1
		}
		return r
	}, s))
}

// breakGlassDescription is quai/approval's description of a broken request: "Break-glass by
// <name>", within the platform's limit. Never the reason, which the platform shows to anyone who
// reads the pull request.
func breakGlassDescription(r db.Request) string {
	return clip("Break-glass by "+printable(r.BreakGlassBy), MaxReason)
}

// approvalDescription is quai/approval's description of an approved request free to merge.
func approvalDescription(r db.Request) string {
	if broken(r) {
		return breakGlassDescription(r)
	}
	return "Approved in Quai"
}

// BreakGlass merges a request without its approvals: who must be a member of the break-glass role
// (authz.UseBreakGlass, which the caller checked), in a browser session, never the requester,
// their delegate or one of the request's definition authors, not deactivated, and step up whenever
// auth.stepUp is configured. The request must be breakable (Breakable) and hold what the breaker
// saw: in.ChangeSet and, with a pull request, in.Head (ErrConflict otherwise). Decided under the
// request's row lock: a review, a cancellation or the expiry at the same moment is taken before or
// after, never both.
func (s *Service) BreakGlass(ctx context.Context, who auth.Identity, id uuid.UUID, in BreakGlassInput) (db.Request, error) {
	ctx = acting(ctx, who)
	if who.Principal.Name == "" || who.TokenID != "" || who.Principal.Scopes != nil {
		return db.Request{}, fmt.Errorf("%w: break-glass is done in Quai, signed in, never with an API token", ErrNotAllowed)
	}
	r, err := s.get(ctx, id)
	if err != nil {
		return r, err
	}
	reason, incident := strings.TrimSpace(in.Reason), strings.TrimSpace(in.Incident)
	fields := map[string]string{}
	if why := typed(reason, MaxBreakGlassReason, false); why != "" {
		fields["reason"] = why
	}
	if why := typed(incident, MaxIncident, false); why != "" {
		fields["incident"] = why
	} else if !incidentRe.MatchString(incident) {
		fields["incident"] = "must be the incident's identifier (letters, digits and . _ : / # -), such as INC0012345"
	}
	if len(fields) > 0 {
		return r, &schema.ValidationError{Fields: fields}
	}
	if why, err := s.mayBreak(ctx, s.store.Queries, r, who.Principal); err != nil {
		return r, err
	} else if why != "" {
		return r, fmt.Errorf("%w: %s", ErrNotAllowed, why)
	}
	if !Breakable(r) {
		return r, unbreakable(r)
	}
	// Breaking glass needs a recent authentication whenever step-up is configured (open question
	// 49), through the one check every step-up goes through.
	steppedUp := s.stepUp.Check(who) == ""
	if s.stepUp.Configured() {
		if err := s.stepUp.Require(ctx, who, "request.break_glass", audit.Object{Type: "request", ID: r.ID.String(), Resource: r.Target}); err != nil {
			return r, err
		}
		steppedUp = true
	}
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	if err != nil {
		return r, err
	}
	if len(sets) == 0 || sets[0].Hash != in.ChangeSet {
		return r, changedSince()
	}
	// As a review would: content that no longer applies cleanly, definitions changed since the
	// plan or a separation of duties tightened make a request pending approval stale, never
	// broken. One approved and held was planned and approved with its content already.
	if r.State == PendingApproval {
		why := map[string]any{}
		if stale, err := s.stale(ctx, sets); err != nil {
			return r, err
		} else if stale != "" {
			why["region"] = stale
		} else if changed, err := s.definitionsChanged(ctx, r, sets); err != nil {
			return r, err
		} else if changed != "" {
			why["definitions"] = changed
		} else if tightened := s.separationTightened(r, sets[0].Repository); tightened != "" {
			why["separation"] = tightened
		}
		if len(why) > 0 {
			if _, err := s.transition(ctx, r, PendingApproval, Stale, "stale", why); err != nil {
				return r, err
			}
			return r, fmt.Errorf("%w: the request went stale: plan it again", ErrConflict)
		}
	}
	var next db.Request
	err = s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		locked, err := q.LockRequest(ctx, r.ID)
		if err != nil {
			return err
		}
		r = locked
		if !Breakable(r) {
			return unbreakable(r)
		}
		// Again under the lock: a delegation made or a deactivation meanwhile counts.
		if why, err := s.mayBreak(ctx, q, r, who.Principal); err != nil {
			return err
		} else if why != "" {
			return fmt.Errorf("%w: %s", ErrNotAllowed, why)
		}
		sets, err := q.ListChangeSets(ctx, r.ID)
		if err != nil {
			return err
		}
		if len(sets) == 0 || sets[0].Hash != in.ChangeSet {
			return changedSince()
		}
		// Once per request: one broken before, whose content changed since, takes its approvals
		// (open question 95).
		if _, err := q.GetBreakGlass(ctx, r.ID); err == nil {
			return fmt.Errorf("%w: glass was broken for this request before, for content it no longer has: it needs its approvals", ErrConflict)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		head := ""
		if d, err := q.GetDelivery(ctx, r.ID); err == nil {
			if d.PullRequest == "" {
				return fmt.Errorf("%w: its pull request is being opened: break glass when the page shows it", ErrConflict)
			}
			if d.Head != in.Head {
				return changedSince()
			}
			head = d.Head
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		now := s.now()
		from, held := r.State, r.WindowHeldAt.Valid
		if from == PendingApproval {
			if _, err := q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: PendingApproval, ToState: Approved}); err != nil {
				return conflict(err, r)
			}
		}
		if held {
			if err := q.SetRequestWindowHeld(ctx, db.SetRequestWindowHeldParams{ID: r.ID}); err != nil {
				return err
			}
		}
		if next, err = q.SetRequestBreakGlass(ctx, db.SetRequestBreakGlassParams{ID: r.ID, At: instant(now), Breaker: who.Principal.Name}); err != nil {
			return conflict(err, r)
		}
		if _, err := q.CreateBreakGlass(ctx, db.CreateBreakGlassParams{RequestID: r.ID, Breaker: who.Principal.Name, BreakerEmail: who.Principal.Email,
			AuthTime: pgtype.Timestamptz{Time: who.AuthTime, Valid: !who.AuthTime.IsZero()}, Acr: who.ACR, StepUp: steppedUp,
			Reason: reason, Incident: incident, ChangeSetHash: sets[0].Hash, Head: head, FromState: from, At: instant(now)}); err != nil {
			return err
		}
		data := map[string]any{"incident": incident, "reason": reason, "changeSet": sets[0].Hash, "from": from}
		details := map[string]any{"from": from, "changeSet": sets[0].Hash, "incident": incident, "reason": audit.Personal(reason), "stepUp": steppedUp}
		if head != "" {
			data["head"], details["head"] = head, head
		}
		if held {
			data["windowHeld"], details["windowHeld"] = true, true
		}
		if who.ACR != "" {
			details["acr"] = who.ACR
		}
		if len(who.ACRS) > 0 {
			details["acrs"] = who.ACRS
		}
		if err := s.event(ctx, tx, next, "break_glass", who.Principal.Name, data); err != nil {
			return err
		}
		if err := recordRequest(ctx, tx, next, "break_glass", details); err != nil {
			return err
		}
		// The alert carries identifiers only (open question 55): never the reason.
		alert := map[string]any{"request": r.ID.String(), "number": r.Number, "state": from, "by": who.Principal.Name,
			"incident": incident, "owners": ownerEntries(r.Owners)}
		if d, err := q.GetDelivery(ctx, r.ID); err == nil && d.PullRequest != "" {
			alert["pullRequest"] = d.PullRequest
		}
		if err := s.alert(ctx, tx, sets[0].Repository, "break_glass", alert); err != nil {
			return err
		}
		if err := s.statusJob(ctx, tx, q, next); err != nil {
			return err
		}
		if s.jobs == nil {
			return nil
		}
		return jobs.InsertTx(ctx, s.jobs, tx, DeliverArgs{RequestID: r.ID}, nil)
	})
	if err != nil {
		return r, err
	}
	return next, nil
}

// unbreakable says why a request cannot be broken in its state.
func unbreakable(r db.Request) error {
	switch {
	case broken(r):
		return fmt.Errorf("%w: glass was broken for this request already", ErrConflict)
	case r.State == Approved:
		return fmt.Errorf("%w: the request is approved and its change windows do not hold it: it merges without break-glass", ErrConflict)
	}
	return fmt.Errorf("%w: break-glass takes a request pending approval or held by its change windows; this one is %s", ErrConflict, r.State)
}

// changedSince is a request that changed since the breaker read it.
func changedSince() error {
	return fmt.Errorf("%w: the request changed since you read it: read it again, then break glass if it is still needed", ErrConflict)
}

// mayBreak returns why p may not break glass for r, beyond the role (open question 94): it is
// their request, they hold a delegation of the requester's approvals (neither revoked nor ended),
// they changed the request's definitions in the separation of duties' period, or Quai has no
// record of them or they were deactivated. "" when nothing keeps them.
func (s *Service) mayBreak(ctx context.Context, q *db.Queries, r db.Request, p authz.Principal) (string, error) {
	if p.Name == r.Requester {
		return "break-glass cannot be used on your own request", nil
	}
	c := contextOf(r)
	if slices.Contains(c.DefinitionAuthors, p.Name) || p.Email != "" && slices.ContainsFunc(c.DefinitionAuthorEmails, func(e string) bool { return strings.EqualFold(e, p.Email) }) {
		return "you changed the definitions this request was planned with: someone else breaks glass for it", nil
	}
	if delegate, err := s.delegateOf(ctx, q, p.Name, r.Requester); err != nil || delegate {
		return "you hold a delegation of the requester's approvals: someone else breaks glass for it", err
	}
	return s.active(ctx, q, p.Name)
}

// WhyNotBreak says why p, who holds the break-glass role, may not break glass for r beyond its
// state (mayBreak); "" when they may.
func (s *Service) WhyNotBreak(ctx context.Context, r db.Request, p authz.Principal) (string, error) {
	return s.mayBreak(ctx, s.store.Queries, r, p)
}

// BreakGlassStepUp reports whether break-glass and its review's sign-offs need a step-up:
// whenever auth.stepUp is configured.
func (s *Service) BreakGlassStepUp() bool { return s.stepUp.Configured() }

// active returns why Quai does not take an action of a person: it has no record of them, or they
// were deactivated; "" otherwise.
func (s *Service) active(ctx context.Context, q *db.Queries, name string) (string, error) {
	u, err := q.GetUser(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "Quai has no record of you: sign in again", nil
	}
	if err != nil {
		return "", err
	}
	if u.DeactivatedAt.Valid {
		return "you were deactivated", nil
	}
	return "", nil
}

// delegateOf reports whether name holds a delegation from one of delegators neither revoked nor
// ended now, active or to come.
func (s *Service) delegateOf(ctx context.Context, q *db.Queries, name string, delegators ...string) (bool, error) {
	ds, err := q.DelegationsOf(ctx, db.DelegationsOfParams{Names: delegators, At: instant(s.now())})
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(ds, func(d db.Delegation) bool { return d.Delegate == name && slices.Contains(delegators, d.Delegator) }), nil
}

// ownerEntries are a request's owners as owner entries: a bare name is user:<name>.
func ownerEntries(owners []string) []string {
	out := make([]string, 0, len(owners))
	for _, o := range owners {
		if o == "" {
			continue
		}
		if !strings.Contains(o, ":") {
			o = "user:" + o
		}
		out = append(out, o)
	}
	return out
}

// BreakGlassObject is a request as break-glass and its review see it: its repository, its
// requester, and its resources' owners, who may sign off its review.
func (s *Service) BreakGlassObject(ctx context.Context, r db.Request) authz.Object {
	return authz.Object{Repository: s.Repository(ctx, r), Requester: r.Requester, Owners: ownerEntries(r.Owners)}
}

// BreakGlassOf returns a request's break-glass and the sign-offs of its review; pgx.ErrNoRows
// wrapped in ErrNotFound when glass was never broken for it.
func (s *Service) BreakGlassOf(ctx context.Context, id uuid.UUID) (db.BreakGlass, []db.BreakGlassSignOff, error) {
	bg, err := s.store.GetBreakGlass(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return bg, nil, fmt.Errorf("%w: glass was never broken for this request", ErrNotFound)
	}
	if err != nil {
		return bg, nil, err
	}
	signs, err := s.store.ListBreakGlassSignOffs(ctx, id)
	return bg, signs, err
}

// ListBreakGlass returns the break-glass records, the open reviews first, then the newest, at
// most limit; only the open ones with open.
func (s *Service) ListBreakGlass(ctx context.Context, open bool, limit int32) ([]db.BreakGlass, error) {
	return s.store.ListBreakGlass(ctx, db.ListBreakGlassParams{OpenOnly: open, MaxRows: limit})
}

// WhyNotSignOff says why p cannot sign off the open review of a request's break-glass, or "" when
// they can, from the record: they broke glass, requested the change, hold a delegation from either,
// or signed it off already. Whether their role or ownership lets them is authz's
// (authz.BreakGlassReviewer).
func (s *Service) WhyNotSignOff(ctx context.Context, bg db.BreakGlass, r db.Request, signs []db.BreakGlassSignOff, p authz.Principal) (string, error) {
	return s.whyNotSignOff(ctx, s.store.Queries, bg, r, signs, p)
}

func (s *Service) whyNotSignOff(ctx context.Context, q *db.Queries, bg db.BreakGlass, r db.Request, signs []db.BreakGlassSignOff, p authz.Principal) (string, error) {
	switch {
	case bg.ClosedAt.Valid:
		return "the review is closed", nil
	case p.Name == bg.Breaker:
		return "you broke glass: two other people sign the review off", nil
	case p.Name == r.Requester:
		return "you requested the change: two other people sign the review off", nil
	case slices.ContainsFunc(signs, func(v db.BreakGlassSignOff) bool { return v.Signer == p.Name }):
		return "you signed it off already: it waits for someone else", nil
	}
	if delegate, err := s.delegateOf(ctx, q, p.Name, bg.Breaker, r.Requester); err != nil || delegate {
		return "you hold a delegation from the person who broke glass or the requester: someone else signs it off", err
	}
	return "", nil
}

// SignOffBreakGlass signs off the post-incident review of a request's break-glass, in who's
// capacity (authz.BreakGlassReviewer: a platform admin, an auditor or an owner of the request),
// with a comment, in a browser session, after a step-up whenever auth.stepUp is configured. Never
// the breaker, the requester or their delegates; each person once; a sign-off is never withdrawn.
// The second sign-off closes the review, and nothing else does (open questions 27 and 99).
func (s *Service) SignOffBreakGlass(ctx context.Context, who auth.Identity, id uuid.UUID, comment string) (db.Request, error) {
	ctx = acting(ctx, who)
	if who.Principal.Name == "" || who.TokenID != "" || who.Principal.Scopes != nil {
		return db.Request{}, fmt.Errorf("%w: a break-glass review is signed off in Quai, signed in, never with an API token", ErrNotAllowed)
	}
	r, err := s.get(ctx, id)
	if err != nil {
		return r, err
	}
	comment = strings.TrimSpace(comment)
	if why := typed(comment, MaxSignOffComment, true); why != "" {
		return r, &schema.ValidationError{Fields: map[string]string{"comment": why}}
	}
	capacity := authz.BreakGlassReviewer(who.Principal, s.BreakGlassObject(ctx, r))
	if capacity == "" {
		return r, fmt.Errorf("%w: signing off a break-glass review needs to be a platform admin, an auditor or an owner of the request", ErrNotAllowed)
	}
	if _, err := s.store.GetBreakGlass(ctx, r.ID); errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("%w: glass was never broken for this request", ErrNotFound)
	} else if err != nil {
		return r, err
	}
	steppedUp := s.stepUp.Check(who) == ""
	if s.stepUp.Configured() {
		if err := s.stepUp.Require(ctx, who, "request.break_glass_sign_off", audit.Object{Type: "request", ID: r.ID.String(), Resource: r.Target}); err != nil {
			return r, err
		}
		steppedUp = true
	}
	err = s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		bg, err := q.LockBreakGlass(ctx, r.ID)
		if err != nil {
			return err
		}
		signs, err := q.ListBreakGlassSignOffs(ctx, r.ID)
		if err != nil {
			return err
		}
		if why, err := s.whyNotSignOff(ctx, q, bg, r, signs, who.Principal); err != nil {
			return err
		} else if why != "" {
			if bg.ClosedAt.Valid || strings.HasPrefix(why, "you signed") {
				return fmt.Errorf("%w: %s", ErrConflict, why)
			}
			return fmt.Errorf("%w: %s", ErrNotAllowed, why)
		}
		if why, err := s.active(ctx, q, who.Principal.Name); err != nil {
			return err
		} else if why != "" {
			return fmt.Errorf("%w: %s", ErrNotAllowed, why)
		}
		now := s.now()
		if _, err := q.AddBreakGlassSignOff(ctx, db.AddBreakGlassSignOffParams{RequestID: r.ID, Signer: who.Principal.Name, SignerEmail: who.Principal.Email,
			Capacity: capacity, Comment: comment, AuthTime: pgtype.Timestamptz{Time: who.AuthTime, Valid: !who.AuthTime.IsZero()},
			StepUp: steppedUp, At: instant(now)}); err != nil {
			return err
		}
		n := len(signs) + 1
		if err := s.event(ctx, tx, r, "break_glass_signed_off", who.Principal.Name, map[string]any{"capacity": capacity, "comment": comment,
			"signOffs": n, "needed": SignOffsNeeded}); err != nil {
			return err
		}
		if err := recordRequest(ctx, tx, r, "break_glass_signed_off", map[string]any{"capacity": capacity, "comment": audit.Personal(comment),
			"stepUp": steppedUp, "signOffs": n, "incident": bg.Incident}); err != nil {
			return err
		}
		if n < SignOffsNeeded {
			return nil
		}
		if err := q.CloseBreakGlass(ctx, db.CloseBreakGlassParams{RequestID: r.ID, At: instant(now)}); err != nil {
			return err
		}
		signers := []audit.Personal{}
		names := []string{}
		for _, v := range signs {
			signers, names = append(signers, audit.Personal(v.Signer)), append(names, v.Signer)
		}
		signers, names = append(signers, audit.Personal(who.Principal.Name)), append(names, who.Principal.Name)
		if err := s.event(ctx, tx, r, "break_glass_review_closed", "", map[string]any{"signedOffBy": names}); err != nil {
			return err
		}
		return recordRequest(ctx, tx, r, "break_glass_review_closed", map[string]any{"signedOffBy": signers, "incident": bg.Incident,
			"brokenAt": audit.Time(bg.At.Time)})
	})
	if err != nil {
		return r, err
	}
	return s.get(ctx, r.ID)
}

// sweepReviews reminds the open break-glass reviews due at now: each every ReviewRemindEvery, from
// the break-glass and then from its last reminder, decided under the record's row lock, so that a
// sweep run twice reminds once.
func (s *Service) sweepReviews(ctx context.Context, now time.Time) error {
	open, err := s.store.ListOpenBreakGlass(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, bg := range open {
		if reviewDue(bg, now) {
			if err := s.remindReview(ctx, bg.RequestID); err != nil {
				errs = append(errs, fmt.Errorf("break-glass review of request %s: %w", bg.RequestID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// reviewDue reports whether an open break-glass review is to be reminded at now.
func reviewDue(bg db.BreakGlass, now time.Time) bool {
	last := bg.At.Time
	if bg.RemindedAt.Valid {
		last = bg.RemindedAt.Time
	}
	return !bg.ClosedAt.Valid && !now.Before(last.Add(ReviewRemindEvery))
}

// remindReview reminds an open break-glass review, when it is still due: an alert to the people
// the break-glass alerted, but those who signed it off, with identifiers only.
func (s *Service) remindReview(ctx context.Context, id uuid.UUID) error {
	return s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		bg, err := q.LockBreakGlass(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		now := s.now()
		if !reviewDue(bg, now) {
			return nil // signed off, or reminded, meanwhile
		}
		r, err := q.GetRequest(ctx, id)
		if err != nil {
			return err
		}
		signs, err := q.ListBreakGlassSignOffs(ctx, id)
		if err != nil {
			return err
		}
		if err := q.SetBreakGlassReminded(ctx, db.SetBreakGlassRemindedParams{RequestID: id, At: instant(now)}); err != nil {
			return err
		}
		signed := []string{}
		for _, v := range signs {
			signed = append(signed, v.Signer)
		}
		data := map[string]any{"signOffs": len(signs), "needed": SignOffsNeeded}
		if err := s.event(ctx, tx, r, "break_glass_reminded", "", data); err != nil {
			return err
		}
		if err := recordRequest(ctx, tx, r, "break_glass_reminded", map[string]any{"signOffs": len(signs), "incident": bg.Incident,
			"brokenAt": audit.Time(bg.At.Time)}); err != nil {
			return err
		}
		return s.alert(ctx, tx, s.repositoryIn(ctx, q, r), "break_glass_review", map[string]any{"request": r.ID.String(), "number": r.Number,
			"incident": bg.Incident, "signOffs": len(signs), "needed": SignOffsNeeded, "owners": ownerEntries(r.Owners), "signed": signed})
	})
}

// repositoryIn is a request's repository, read in a transaction.
func (s *Service) repositoryIn(ctx context.Context, q *db.Queries, r db.Request) string {
	if sets, err := q.ListChangeSets(ctx, r.ID); err == nil && len(sets) > 0 {
		return sets[0].Repository
	}
	return strings.SplitN(r.Target, "/", 2)[0]
}
