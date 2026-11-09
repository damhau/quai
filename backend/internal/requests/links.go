package requests

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/chatlink"
	"github.com/damhau/quai/backend/internal/store/db"
)

// UseLinks gives the signer of the links of approval cards in chat (notifications.links); without
// it, every link is refused.
func (s *Service) UseLinks(l *chatlink.Signer) { s.links = l }

// OpenLink checks a link of an approval card in chat for the person signed in, on a request as it
// is now, for a decision ("" when only opening it): a browser session, not an API token; a link
// Quai made, not expired; for this request, this person and this decision; the request still
// waiting for approval, with the change set the card showed. It changes nothing; a refusal is a
// *chatlink.Refusal, in the audit log as request.link_refused. A link does not make anyone an
// approver: a review with it has every check of a review on top (ReviewLink).
func (s *Service) OpenLink(ctx context.Context, who auth.Identity, r db.Request, token, decision string) (chatlink.Link, error) {
	ctx = acting(ctx, who)
	if who.TokenID != "" {
		return chatlink.Link{}, s.linkRefused(ctx, who, r, nil, &chatlink.Refusal{Code: chatlink.Session,
			Reason: "a link from a chat card opens in a browser, signed in to Quai, not with an API token"})
	}
	if s.links == nil {
		return chatlink.Link{}, s.linkRefused(ctx, who, r, nil, &chatlink.Refusal{Code: chatlink.Signature,
			Reason: "this Quai does not sign links for chat cards (notifications.links): review the request here"})
	}
	l, err := s.links.Open(ctx, token)
	var ref *chatlink.Refusal
	if errors.As(err, &ref) {
		return chatlink.Link{}, s.linkRefused(ctx, who, r, nil, ref)
	}
	if err != nil {
		return chatlink.Link{}, err
	}
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	if err != nil {
		return chatlink.Link{}, err
	}
	var hash string
	if len(sets) > 0 {
		hash = sets[0].Hash
	}
	if ref := l.Check(chatlink.Use{Request: r.ID, Approver: who.Principal.Name, Decision: decision, Pending: r.State == PendingApproval,
		ChangeSet: hash}); ref != nil {
		return chatlink.Link{}, s.linkRefused(ctx, who, r, &l, ref)
	}
	return l, nil
}

// linkRefused records a link refused, and returns the refusal. What the audit event holds of the
// link is what its MAC vouches for (l), never what an unverified link says.
func (s *Service) linkRefused(ctx context.Context, who auth.Identity, r db.Request, l *chatlink.Link, ref *chatlink.Refusal) error {
	ctx = acting(ctx, who)
	details := map[string]any{"code": ref.Code, "reason": ref.Reason}
	if l != nil {
		details["channel"] = l.Channel
		details["decision"] = l.Decision
		details["changeSet"] = l.ChangeSet
		details["sentTo"] = audit.Personal(l.Approver)
		details["expires"] = l.Expires.UTC().Format(time.RFC3339)
	}
	if err := s.store.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		return recordRequest(ctx, tx, r, "link_refused", details)
	}); err != nil {
		return err
	}
	return ref
}
