// Package notify tells people what happens to requests, and alerts the administrators of a
// repository (docs/design/05-delivery-and-approvals.md#notifications).
//
// An event a person must hear of (request submitted, approval needed, approved, …, an alert) is
// recorded on a timeline with store.RecordEvent, whose hook (EventRecorded) inserts a fan-out job
// in the same transaction: an event is notified if and only if it is committed. The fan-out
// resolves the recipients when it runs, from the directory, and adds a row per recipient and
// channel to the notifications table, whose key makes each sent once however often the fan-out
// runs, with a job per row that sends it. A send that fails is retried with backoff
// (MaxAttempts); notifications have a queue of their own (jobs.QueueNotifications), so a failing
// or hanging mail server never holds a request back.
//
// People choose, per channel and kind, whether a notification goes at once, in their daily digest
// or not at all (Settings, Preferences); alerts always go at once. A notification for the digest
// waits (state InDigest) until the digest job (Periodic) gathers it into the recipient's digest
// for the day, which is sent once.
//
// A channel (Channel) renders a Message its own way: email now; Microsoft Teams and Slack later.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/engine/policy"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
	"github.com/damhau/quai/backend/internal/telemetry"
)

// What a message says: its kind, in the notifications table and the templates.
const (
	Submitted      = "submitted"
	ApprovalNeeded = "approval_needed"
	Approved       = "approved"
	Rejected       = "rejected"
	Stale          = "stale"
	Merged         = "merged"
	Applied        = "applied"
	Failed         = "failed"
	ExpiringSoon   = "expiring_soon" // P4-14's expiring resources: no event triggers it yet
	Alert          = "alert"
)

// DigestKind is the kind of a digest's message.
const DigestKind = "digest"

// States of a notification, and of a digest (pending, sent, skipped, failed).
const (
	Pending  = "pending"
	InDigest = "digest" // waits for its digest, or for its digest to be sent
	Sent     = "sent"
	Skipped  = "skipped"
	Failure  = "failed"
)

// MaxAttempts is how often a notification is tried: with Backoff, for about fourteen hours.
const MaxAttempts = 20

// Backoff is how long after its nth failed attempt a job is tried again: 30 seconds, doubling, up
// to an hour.
func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		return time.Hour
	}
	return min(30*time.Second<<(attempt-1), time.Hour)
}

// Channel delivers messages to people: email; Microsoft Teams and Slack later.
type Channel interface {
	// Name names the channel in the notifications table and the metrics.
	Name() string
	// Address returns where a person of the directory is reached on the channel, and false when
	// they are not (no verified email).
	Address(u db.User) (string, bool)
	// Send delivers a message. Its error must never hold a credential.
	Send(ctx context.Context, to Recipient, m Message) error
}

// Recipient is who a message goes to.
type Recipient struct {
	ID      string // user:<name>, or email:<address> (notifications.alerts)
	Name    string // the person's name; empty for an address
	Address string // on the channel
}

// Options configures the notifications.
type Options struct {
	Store        *store.Store
	Requests     *requests.Service
	Repositories *repositories.Registry
	Directory    *auth.Directory
	Channels     []Channel
	// Alerts are the addresses of notifications.alerts, on the email channel.
	Alerts []string
	// PublicURL is server.publicURL: every link in a message starts with it.
	PublicURL string
	// Settings are what people may choose, and the defaults (NewSettings).
	Settings Settings
	Log      *slog.Logger
	// Backoff is how long after a failed attempt a job is tried again; Backoff by default.
	Backoff func(attempt int) time.Duration
}

// Service notifies.
type Service struct {
	opts     Options
	channels map[string]Channel
	jobs     *jobs.Client
	now      func() time.Time
}

// New returns the notifications service. Give it the job client with UseJobs, and its hook to
// the store (store.UseEvents(s.EventRecorded)).
func New(opts Options) *Service {
	if opts.Backoff == nil {
		opts.Backoff = Backoff
	}
	if opts.Directory == nil {
		opts.Directory = &auth.Directory{}
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	s := &Service{opts: opts, channels: map[string]Channel{}, now: time.Now}
	for _, c := range opts.Channels {
		s.channels[c.Name()] = c
	}
	return s
}

// UseJobs sets the client that inserts jobs.
func (s *Service) UseJobs(c *jobs.Client) { s.jobs = c }

// UseClock sets the clock: when notifications are added, when digests are due, when a failed
// attempt is tried again.
func (s *Service) UseClock(now func() time.Time) { s.now = now }

// Reach reports whether a person has an address on a channel: notifications reach them.
func (s *Service) Reach(u db.User) bool {
	for _, c := range s.opts.Channels {
		if _, ok := c.Address(u); ok {
			return true
		}
	}
	return false
}

// EventArgs fans an event out to its recipients.
type EventArgs struct {
	EventID int64 `json:"eventId"`
}

// Kind names the job.
func (EventArgs) Kind() string { return "notify_event" }

// InsertOpts puts it on the notifications' queue.
func (EventArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: jobs.QueueNotifications, MaxAttempts: MaxAttempts}
}

// SendArgs sends a notification.
type SendArgs struct {
	NotificationID int64 `json:"notificationId"`
}

// Kind names the job.
func (SendArgs) Kind() string { return "send_notification" }

// InsertOpts puts it on the notifications' queue.
func (SendArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: jobs.QueueNotifications, MaxAttempts: MaxAttempts}
}

// Workers adds the notifications' workers.
func (s *Service) Workers(w *river.Workers) {
	river.AddWorker(w, &fanOutWorker{s: s})
	river.AddWorker(w, &sendWorker{s: s})
	river.AddWorker(w, &digestWorker{s: s})
	river.AddWorker(w, &sendDigestWorker{s: s})
}

// EventRecorded is the store's event hook: an event someone must hear of gets its fan-out job,
// in the transaction that records it.
func (s *Service) EventRecorded(ctx context.Context, tx pgx.Tx, e db.Event) error {
	if audienceOf(e).none() {
		return nil
	}
	if s.jobs == nil {
		return errors.New("notifications: no job client (UseJobs)")
	}
	return jobs.InsertTx(ctx, s.jobs, tx, EventArgs{EventID: e.ID}, nil)
}

// audience is who hears of an event: the message the requester gets, the one the approvers the
// request waits for get, and whether it is an alert for the repository's administrators.
type audience struct {
	requester string
	approvers string
	admins    bool
}

func (a audience) none() bool { return a == audience{} }

// audienceOf says who hears of an event. A cancelled request is not notified: its requester or
// an administrator cancelled it, or its pull request was closed or changed outside Quai.
func audienceOf(e db.Event) audience {
	switch e.Kind {
	case "submitted":
		return audience{requester: Submitted, approvers: ApprovalNeeded}
	case "approval_needed", "plan_decided":
		return audience{approvers: ApprovalNeeded}
	case "modified_outside_quai":
		// Back to pending approval with new content (the event names its hash); otherwise the
		// request was cancelled.
		var d struct{ Hash string }
		if json.Unmarshal(e.Data, &d) == nil && d.Hash != "" {
			return audience{approvers: ApprovalNeeded}
		}
	case "approved":
		return audience{requester: Approved}
	case "rejected":
		return audience{requester: Rejected}
	case "stale":
		return audience{requester: Stale}
	case "merged", "merged_without_approval", "committed":
		return audience{requester: Merged}
	case "applied":
		return audience{requester: Applied}
	case "failed", "apply_failed":
		return audience{requester: Failed}
	case "expiring_soon":
		return audience{requester: ExpiringSoon}
	case "alert_bypass", "alert_enforcement", "alert_break_glass":
		return audience{admins: true}
	}
	return audience{}
}

type fanOutWorker struct {
	river.WorkerDefaults[EventArgs]
	s *Service
}

func (w *fanOutWorker) Work(ctx context.Context, job *river.Job[EventArgs]) error {
	return w.s.fanOut(ctx, job.Args.EventID)
}

func (w *fanOutWorker) NextRetry(job *river.Job[EventArgs]) time.Time {
	return w.s.now().Add(w.s.opts.Backoff(job.Attempt))
}

type sendWorker struct {
	river.WorkerDefaults[SendArgs]
	s *Service
}

func (w *sendWorker) Work(ctx context.Context, job *river.Job[SendArgs]) error {
	return w.s.send(ctx, job.Args.NotificationID, job.Attempt >= job.MaxAttempts)
}

func (w *sendWorker) NextRetry(job *river.Job[SendArgs]) time.Time {
	return w.s.now().Add(w.s.opts.Backoff(job.Attempt))
}

// Timeout leaves a send its own deadline (sendTimeout) and the database's work around it.
func (w *sendWorker) Timeout(*river.Job[SendArgs]) time.Duration { return sendTimeout + 30*time.Second }

// sendTimeout bounds one attempt at sending, connecting included.
const sendTimeout = time.Minute

// recipient is someone an event is notified to, with the message they get.
type recipient struct {
	id   string   // user:<name> or email:<address>
	user *db.User // the person, for user: recipients; nil when the directory does not know them
	kind string
}

// fanOut adds a notification per recipient of an event and channel, each with its send job; a
// recipient a channel cannot reach gets a skipped row that says why. Recipients are resolved now:
// the approvers the request waits for at this moment, the people of the directory as they last
// signed in. Their preferences say whether it goes at once, waits for their digest (no job: the
// digest sends it), or is skipped.
func (s *Service) fanOut(ctx context.Context, id int64) error {
	e, err := s.opts.Store.GetEvent(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	rs, err := s.recipients(ctx, e)
	if err != nil || len(rs) == 0 {
		return err
	}
	prefs := map[string]Preferences{}
	for _, r := range rs {
		if r.user != nil && r.kind != Alert {
			if prefs[r.id], err = s.preferences(ctx, r.user.Name); err != nil {
				return err
			}
		}
	}
	now := s.now()
	type outcome struct{ channel, result string }
	var counts []outcome
	err = s.opts.Store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		counts = counts[:0]
		for _, r := range rs {
			for _, c := range s.opts.Channels {
				state, why := Pending, ""
				switch {
				case r.user != nil:
					if _, ok := c.Address(*r.user); !ok {
						state, why = Skipped, "Quai knows no address of theirs on this channel (a verified email for email)"
						break
					}
					switch s.opts.Settings.Mode(prefs[r.id], c.Name(), r.kind) {
					case config.ModeOff:
						state, why = Skipped, "they turned it off in their preferences"
					case config.ModeDigest:
						state = InDigest
					}
				case strings.HasPrefix(r.id, "email:"):
					if c.Name() != "email" {
						continue
					}
				default:
					state, why = Skipped, "not in the directory: they never signed in"
				}
				n, err := q.AddNotification(ctx, db.AddNotificationParams{EventID: e.ID, Recipient: r.id, Channel: c.Name(), Kind: r.kind,
					State: state, LastError: why, CreatedAt: ts(now)})
				if errors.Is(err, pgx.ErrNoRows) {
					continue // added by an earlier run
				}
				if err != nil {
					return err
				}
				if state == Skipped {
					counts = append(counts, outcome{c.Name(), Skipped})
					continue
				}
				if state == InDigest {
					continue // counted when its digest goes
				}
				if err := jobs.InsertTx(ctx, s.jobs, tx, SendArgs{NotificationID: n.ID}, nil); err != nil {
					return err
				}
			}
		}
		return nil
	})
	for _, c := range counts {
		telemetry.Notified(ctx, c.channel, c.result)
	}
	return err
}

// recipients returns who hears of an event, each once: the requester, the approvers the request
// waits for, or the repository's administrators and notifications.alerts.
func (s *Service) recipients(ctx context.Context, e db.Event) ([]recipient, error) {
	a := audienceOf(e)
	var out []recipient
	seen := map[string]bool{}
	add := func(r recipient) {
		if !seen[r.id] {
			seen[r.id] = true
			out = append(out, r)
		}
	}
	if e.RequestID.Valid && (a.requester != "" || a.approvers != "") {
		r, err := s.opts.Requests.Get(ctx, uuid.UUID(e.RequestID.Bytes))
		if errors.Is(err, requests.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		// A request no rule applies to is approved at once: its requester hears of that, not of
		// its submission.
		if a.requester == Submitted && (len(r.Rules) == 0 || string(r.Rules) == "null" || string(r.Rules) == "[]") {
			a.requester = ""
		}
		if a.requester != "" {
			u, err := s.person(ctx, r.Requester)
			if err != nil {
				return nil, err
			}
			if u == nil || !u.DeactivatedAt.Valid {
				add(recipient{id: "user:" + r.Requester, user: u, kind: a.requester})
			}
		}
		if a.approvers != "" {
			awaiting, err := s.opts.Requests.Awaiting(ctx, r)
			if err != nil {
				return nil, err
			}
			only := decided(e)
			for _, aw := range awaiting {
				if aw.Rule.Undecided() || only != nil && !slices.ContainsFunc(only, func(names []string) bool { return slices.Equal(names, aw.Rule.Names) }) {
					continue
				}
				for _, p := range aw.People {
					add(recipient{id: "user:" + p.Name, user: &p, kind: a.approvers})
				}
			}
		}
	}
	if a.admins {
		users, err := s.admins(ctx, strings.TrimPrefix(e.Target, "repository:"))
		if err != nil {
			return nil, err
		}
		addresses := map[string]bool{}
		for _, u := range users {
			add(recipient{id: "user:" + u.Name, user: &u, kind: Alert})
			if u.Email != "" {
				addresses[strings.ToLower(u.Email)] = true
			}
		}
		for _, addr := range s.opts.Alerts {
			if !addresses[strings.ToLower(addr)] {
				add(recipient{id: "email:" + addr, kind: Alert})
			}
		}
	}
	return out, nil
}

// decided returns the rules a plan_decided event says apply, by their names; nil for another
// event: then every rule the request waits for counts.
func decided(e db.Event) [][]string {
	if e.Kind != "plan_decided" {
		return nil
	}
	var d struct {
		Decisions []struct {
			Rules   []string `json:"rules"`
			Applies bool     `json:"applies"`
		} `json:"decisions"`
	}
	_ = json.Unmarshal(e.Data, &d)
	out := [][]string{}
	for _, dec := range d.Decisions {
		if dec.Applies {
			out = append(out, dec.Rules)
		}
	}
	return out
}

// person returns a person of the directory; nil when it does not know them.
func (s *Service) person(ctx context.Context, name string) (*db.User, error) {
	u, err := s.opts.Store.GetUser(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// admins returns the people of the directory who administer a repository: its owners
// (metadata.owners), entries that name people, groups, teams or emails, matched as approval rules
// match owners.
func (s *Service) admins(ctx context.Context, repository string) ([]db.User, error) {
	if s.opts.Repositories == nil {
		return nil, nil
	}
	repo, err := s.opts.Repositories.Get(repository)
	if err != nil {
		return nil, nil // disconnected since: no owners to tell
	}
	owners := repo.Document.Metadata.Owners
	if len(owners) == 0 {
		return nil, nil
	}
	users, err := s.opts.Store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var out []db.User
	ownersRule := policy.Rule{Approvers: []string{"owners"}}
	for _, u := range users {
		p := policy.Person{Name: u.Name, Email: u.Email, Groups: u.Groups, Teams: s.opts.Directory.Teams(u.Groups)}
		if !u.DeactivatedAt.Valid && policy.CanReview(ownersRule, p, policy.Context{Owners: owners}) {
			out = append(out, u)
		}
	}
	return out, nil
}

// send sends a notification, once: a sent or skipped one is left alone. A failed attempt is
// recorded with its error and returned, for River to try again after the backoff; after the last
// one (last), the notification has failed.
func (s *Service) send(ctx context.Context, id int64, last bool) error {
	n, err := s.opts.Store.GetNotification(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if n.State != Pending {
		return nil
	}
	c := s.channels[n.Channel]
	skip := func(why string) error {
		telemetry.Notified(ctx, n.Channel, Skipped)
		return s.opts.Store.NotificationSkipped(ctx, db.NotificationSkippedParams{ID: n.ID, LastError: why})
	}
	if c == nil {
		return skip("the channel is no longer configured")
	}
	to, m, why, err := s.compose(ctx, n, c)
	if err != nil {
		return err
	}
	if why != "" {
		return skip(why)
	}
	if n.Kind != Alert && strings.HasPrefix(n.Recipient, "user:") {
		m.Preferences = s.link("/profile")
	}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	err = c.Send(sctx, to, m)
	cancel()
	if err != nil {
		state, result, level := Pending, "retried", slog.LevelWarn
		if last {
			state, result, level = Failure, Failure, slog.LevelError
		}
		msg := clip(err.Error(), 1000)
		telemetry.Notified(ctx, n.Channel, result)
		s.opts.Log.Log(ctx, level, "a notification was not sent", slog.Int64("notification", n.ID), slog.String("channel", n.Channel),
			slog.String("recipient", n.Recipient), slog.String("kind", n.Kind), slog.Int("attempt", int(n.Attempts)+1), slog.Bool("last", last),
			slog.String("error", msg))
		if uerr := s.opts.Store.NotificationAttempted(ctx, db.NotificationAttemptedParams{ID: n.ID, LastError: msg, State: state}); uerr != nil {
			return errors.Join(err, uerr)
		}
		return fmt.Errorf("notification %d: %w", n.ID, err)
	}
	telemetry.Notified(ctx, n.Channel, Sent)
	return s.opts.Store.NotificationSent(ctx, n.ID)
}

// clip shortens s to at most n bytes, on a character boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "..."
}
