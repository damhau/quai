package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/damhau/quai/backend/internal/chatlink"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Approval cards in chat: a person's card asking for their approval carries Approve and Deny,
// links signed for them and the change set it shows (chatlink), and is recorded (chat_cards) to be
// updated once the request no longer waits for them.

// card reports whether a notification is an approval card in chat: an approval needed, for a
// person, on a channel that updates what it sent.
func (s *Service) card(n db.Notification, c Channel, m Message) bool {
	_, ok := c.(Updater)
	return ok && n.Kind == ApprovalNeeded && strings.HasPrefix(n.Recipient, "user:") && m.Request != nil
}

// decisionLinks returns the links that open a request with a decision ready, for an approver and
// the change set they are shown: empty without a signer or a change set.
func (s *Service) decisionLinks(ctx context.Context, approver, channel string, rc *RequestContent) (approve, deny string, err error) {
	if s.opts.Links == nil || rc.ChangeSet == "" {
		return "", "", nil
	}
	page := s.link("/requests/" + url.PathEscape(rc.ID.String()))
	for _, d := range []struct {
		decision string
		out      *string
	}{{chatlink.Approve, &approve}, {chatlink.Deny, &deny}} {
		tok, err := s.opts.Links.Make(ctx, chatlink.Link{Request: rc.ID, Approver: approver, Decision: d.decision, ChangeSet: rc.ChangeSet, Channel: channel})
		if err != nil {
			return "", "", err
		}
		*d.out = page + "?" + url.Values{"link": {tok}}.Encode()
	}
	return approve, deny, nil
}

// cardSent records a card sent, with the notification, and a refresh of the request's cards: the
// request may have been decided while the card was on its way.
func (s *Service) cardSent(ctx context.Context, tx pgx.Tx, q *db.Queries, n db.Notification, m Message, at Receipt) error {
	if err := q.AddChatCard(ctx, db.AddChatCardParams{NotificationID: n.ID, RequestID: m.Request.ID, Channel: n.Channel, Recipient: n.Recipient,
		Conversation: at.Conversation, Message: at.Message, ChangeSet: m.Request.ChangeSet}); err != nil {
		return err
	}
	if err := q.NotificationSent(ctx, n.ID); err != nil {
		return err
	}
	return jobs.InsertTx(ctx, s.jobs, tx, RefreshArgs{RequestID: m.Request.ID}, nil)
}

// refreshCards inserts, with an event of a request that has cards to update, the job that
// updates them.
func (s *Service) refreshCards(ctx context.Context, tx pgx.Tx, e db.Event) error {
	if !e.RequestID.Valid || !slices.ContainsFunc(s.opts.Channels, func(c Channel) bool { _, ok := c.(Updater); return ok }) {
		return nil
	}
	open, err := db.New(tx).HasOpenChatCards(ctx, uuid.UUID(e.RequestID.Bytes))
	if err != nil || !open {
		return err
	}
	return jobs.InsertTx(ctx, s.jobs, tx, RefreshArgs{RequestID: uuid.UUID(e.RequestID.Bytes)}, nil)
}

// RefreshArgs updates the approval cards of a request that no longer waits for their recipients.
type RefreshArgs struct {
	RequestID uuid.UUID `json:"requestId"`
}

// Kind names the job.
func (RefreshArgs) Kind() string { return "refresh_chat_cards" }

// InsertOpts puts it on the notifications' queue.
func (RefreshArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: jobs.QueueNotifications, MaxAttempts: MaxAttempts}
}

type refreshWorker struct {
	river.WorkerDefaults[RefreshArgs]
	s *Service
}

func (w *refreshWorker) Work(ctx context.Context, job *river.Job[RefreshArgs]) error {
	return w.s.refresh(ctx, job.Args.RequestID)
}

func (w *refreshWorker) NextRetry(job *river.Job[RefreshArgs]) time.Time {
	return w.s.now().Add(w.s.opts.Backoff(job.Attempt))
}

// Timeout leaves each card's update its own deadline.
func (w *refreshWorker) Timeout(*river.Job[RefreshArgs]) time.Duration { return 10 * time.Minute }

// refresh updates each open card of a request that no longer waits for its recipient, and marks
// it closed. A card is taken before it is updated, so two jobs never both update it; an update
// that failed puts it back, for the job's next attempt. A card the channel refuses for good
// (Teams' app uninstalled, the message deleted, Slack's edit window closed) is left closed.
func (s *Service) refresh(ctx context.Context, id uuid.UUID) error {
	cards, err := s.opts.Store.OpenChatCards(ctx, id)
	if err != nil || len(cards) == 0 {
		return err
	}
	r, err := s.opts.Requests.Get(ctx, id)
	if errors.Is(err, requests.ErrNotFound) {
		return nil // its cards were deleted with it
	}
	if err != nil {
		return err
	}
	var (
		hash     string
		reviews  []db.Review
		awaiting []requests.Awaiting
	)
	sets, err := s.opts.Store.ListChangeSets(ctx, id)
	if err != nil {
		return err
	}
	if len(sets) > 0 {
		hash = sets[0].Hash
	}
	if reviews, _, err = s.opts.Requests.Reviews(ctx, r); err != nil {
		return err
	}
	if awaiting, err = s.opts.Requests.Awaiting(ctx, r); err != nil {
		return err
	}
	var errs []error
	for _, card := range cards {
		status := cardStatus(r, card, hash, reviews, awaiting)
		if status == "" {
			continue
		}
		n, err := s.opts.Store.CloseChatCard(ctx, db.CloseChatCardParams{NotificationID: card.NotificationID, ClosedAt: ts(s.now())})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if n == 0 {
			continue // another job took it
		}
		u, ok := s.channels[card.Channel].(Updater)
		if !ok {
			continue // the channel is no longer configured: nothing to update it with
		}
		m := Message{Kind: ApprovalNeeded, Headline: fmt.Sprintf("Approval of request #%d", r.Number), Status: status, Closed: true,
			Request: &RequestContent{ID: r.ID, State: r.State, Number: r.Number, Title: title(r), Repository: s.opts.Requests.Repository(ctx, r)},
			Link:    s.link("/requests/" + url.PathEscape(r.ID.String()))}
		uctx, cancel := context.WithTimeout(ctx, sendTimeout)
		err = u.Update(uctx, Receipt{Conversation: card.Conversation, Message: card.Message}, m)
		cancel()
		if err == nil {
			continue
		}
		var pe interface{ Permanent() bool }
		if errors.As(err, &pe) && pe.Permanent() {
			s.opts.Log.WarnContext(ctx, "an approval card was not updated: the channel refused it", slog.Int64("notification", card.NotificationID),
				slog.String("channel", card.Channel), slog.String("recipient", card.Recipient), slog.String("error", clip(err.Error(), 1000)))
			continue
		}
		if rerr := s.opts.Store.ReopenChatCard(ctx, card.NotificationID); rerr != nil {
			err = errors.Join(err, rerr)
		}
		s.opts.Log.WarnContext(ctx, "an approval card was not updated", slog.Int64("notification", card.NotificationID),
			slog.String("channel", card.Channel), slog.String("recipient", card.Recipient), slog.String("error", clip(err.Error(), 1000)))
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// cardStatus says why a card no longer asks its recipient for a decision, or "" while it does: the
// recipient reviewed it, the request was decided, its change set changed, or it no longer waits
// for them.
func cardStatus(r db.Request, card db.ChatCard, hash string, reviews []db.Review, awaiting []requests.Awaiting) string {
	name := strings.TrimPrefix(card.Recipient, "user:")
	state := strings.ReplaceAll(r.State, "_", " ")
	for _, v := range reviews {
		if v.Reviewer == name && v.ChangeSetHash == card.ChangeSet {
			status := "You denied it."
			if v.Decision == "approve" {
				status = "You approved it."
			}
			if r.State != requests.PendingApproval {
				status += " It is " + state + " now."
			}
			return status
		}
	}
	switch {
	case r.State != requests.PendingApproval:
		return "It no longer waits for approval: it is " + state + "."
	case card.ChangeSet != hash:
		return "It changed since this card was sent: review its new content in Quai."
	}
	if !slices.ContainsFunc(awaiting, func(a requests.Awaiting) bool {
		return slices.ContainsFunc(a.People, func(u db.User) bool { return u.Name == name })
	}) {
		return "It no longer waits for your approval."
	}
	return ""
}
