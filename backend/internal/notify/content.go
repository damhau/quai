package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/engine/plan"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/runs"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Message is what a notification says, whatever the channel: each renders it its own way.
type Message struct {
	Kind     string // Submitted, ApprovalNeeded, …, Alert, DigestKind
	Subject  string // one line
	Headline string // what happened, and what the recipient may do
	Request  *RequestContent
	Alert    *AlertContent
	Digest   []DigestItem // a digest's notifications
	Link     string       // the request's page, or the repository's: always under server.publicURL
	// Preferences is the page where the recipient chooses what Quai sends them; empty for an
	// alert, or an address that is not a person's.
	Preferences string
	// Approve and Deny open the request with that decision ready: signed links (internal/chatlink)
	// on an approval card in chat; empty elsewhere.
	Approve, Deny string
	// Closed is a card updated once the request no longer waits for its recipient, and Status
	// says why; it then shows identifiers only.
	Closed bool
	Status string
}

// RequestContent is what a message says of a request: what the request page shows, in short.
type RequestContent struct {
	ID         uuid.UUID
	State      string
	ChangeSet  string // the hash of its change set; empty before it has one
	Number     int64
	Title      string
	Repository string
	Requester  string
	Reason     string
	Waiting    []string // the rules it waits for approvals of, by name, with their approvers
	Changes    []FileChange
	Plans      []PlanContent
	// OnBehalfOf is the delegator whose approval an approval needed asks of its recipient, their
	// delegate; empty when the recipient gives their own.
	OnBehalfOf string
}

// FileChange is a file a request changes, and how many lines.
type FileChange struct {
	Path, Op       string
	Added, Removed int
}

// PlanContent is a plan of a request's pull request: its counts, cost and policies, and the
// Terraform resources it changes, by address, never a value (open question 52).
type PlanContent struct {
	Stack, Status        string
	Add, Change, Destroy int
	Cost                 string // "+12.50 USD a month"; empty without an estimate
	Policies             []PolicyResult
	Resources            []ResourceChange // the first maxResources
	More                 int              // how many more
}

// PolicyResult is a policy check of a plan.
type PolicyResult struct{ Name, Result string }

// ResourceChange is what a plan does to a Terraform resource.
type ResourceChange struct{ Action, Address string }

// AlertContent is what an alert says: the repository, and the details its event recorded.
type AlertContent struct {
	Repository string
	Details    []Detail
}

// Detail is a labelled value of an alert.
type Detail struct{ Label, Value string }

// maxResources bounds the resources a message lists for each plan; the page has them all.
const maxResources = 50

// compose returns the message of a notification and its recipient, or why there is nothing to
// send: the person deactivated or without an address, an approval the request no longer waits
// for from them.
func (s *Service) compose(ctx context.Context, n db.Notification, c Channel) (Recipient, Message, string, error) {
	var to Recipient
	switch kind, v, _ := strings.Cut(n.Recipient, ":"); kind {
	case "user":
		u, err := s.person(ctx, v)
		if err != nil {
			return to, Message{}, "", err
		}
		if u == nil || u.DeactivatedAt.Valid {
			return to, Message{}, "they are no longer in the directory, or were deactivated", nil
		}
		addr, ok := c.Address(*u)
		if !ok {
			return to, Message{}, c.NoAddress(), nil
		}
		to = Recipient{ID: n.Recipient, Name: u.Name, Address: addr}
	case "email":
		to = Recipient{ID: n.Recipient, Address: v}
	case "teams-channel", "slack-channel":
		to = Recipient{ID: n.Recipient}
	default:
		return to, Message{}, "an unknown recipient", nil
	}
	e, err := s.opts.Store.GetEvent(ctx, n.EventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return to, Message{}, "the event was deleted", nil
	}
	if err != nil {
		return to, Message{}, "", err
	}
	if n.Kind == Alert {
		return to, s.alertMessage(e), "", nil
	}
	if group(n.Recipient) {
		return s.channelMessage(ctx, n, e, to)
	}
	if !e.RequestID.Valid {
		return to, Message{}, "the event is not a request's", nil
	}
	r, err := s.opts.Requests.Get(ctx, uuid.UUID(e.RequestID.Bytes))
	if errors.Is(err, requests.ErrNotFound) {
		return to, Message{}, "the request was deleted", nil
	}
	if err != nil {
		return to, Message{}, "", err
	}
	var awaiting []requests.Awaiting
	if n.Kind == ApprovalNeeded || n.Kind == Submitted {
		if awaiting, err = s.opts.Requests.Awaiting(ctx, r); err != nil {
			return to, Message{}, "", err
		}
	}
	if n.Kind == ApprovalNeeded {
		// Only while the request waits for them: they may read it then, and approve it.
		if r.State != requests.PendingApproval {
			return to, Message{}, "the request no longer waits for approval", nil
		}
		onBehalfOf, waits := requests.WaitsFor(awaiting, strings.TrimPrefix(n.Recipient, "user:"))
		if !waits {
			return to, Message{}, "the request no longer waits for their approval", nil
		}
		m, err := s.requestMessage(ctx, n.Kind, e, r, awaiting)
		if onBehalfOf != "" {
			// A delegate reads what the delegator's approval lets them review (Awaiting), and
			// knows whose approval it is.
			m.Request.OnBehalfOf = onBehalfOf
			m.Headline += " You hold " + onBehalfOf + "'s approvals through a delegation: you review it on their behalf."
		}
		return to, m, "", err
	}
	m, err := s.requestMessage(ctx, n.Kind, e, r, awaiting)
	return to, m, "", err
}

// title is a request's title, as its page shows it.
func title(r db.Request) string {
	switch r.Kind {
	case "create":
		return "Create " + r.Target
	case "update":
		return "Update " + r.Target
	case "delete":
		return "Delete " + r.Target
	case "action":
		a := r.Action
		if a == "" {
			a = "Action"
		}
		return a + " on " + r.Target
	case "migrate":
		return "Migrate " + r.Target
	}
	return r.Target
}

// link returns the address of a page of Quai.
func (s *Service) link(path string) string {
	return strings.TrimSuffix(s.opts.PublicURL, "/") + path
}

// subjects are the beginnings of the messages' subjects.
var subjects = map[string]string{
	Submitted: "Submitted", ApprovalNeeded: "Approval needed", Approved: "Approved", Rejected: "Rejected", Stale: "Stale",
	Merged: "Merged", Applied: "Applied", Failed: "Failed", ExpiringSoon: "Expiring soon",
}

func (s *Service) requestMessage(ctx context.Context, kind string, e db.Event, r db.Request, awaiting []requests.Awaiting) (Message, error) {
	rc := &RequestContent{ID: r.ID, State: r.State, Number: r.Number, Title: title(r), Repository: s.opts.Requests.Repository(ctx, r),
		Requester: r.Requester, Reason: r.Reason}
	for _, a := range awaiting {
		if !a.Rule.Undecided() {
			w := strings.Join(a.Rule.Names, ", ")
			if a.Rule.Min > 1 {
				w += fmt.Sprintf(" (%d of %s)", a.Rule.Min, strings.Join(a.Rule.Approvers, ", "))
			} else {
				w += " (" + strings.Join(a.Rule.Approvers, ", ") + ")"
			}
			rc.Waiting = append(rc.Waiting, w)
		}
	}
	m := Message{Kind: kind, Subject: fmt.Sprintf("[Quai] %s: #%d %s", subjects[kind], r.Number, rc.Title), Request: rc,
		Link: s.link("/requests/" + url.PathEscape(r.ID.String()))}
	num := fmt.Sprintf("#%d", r.Number)
	var d map[string]any
	_ = json.Unmarshal(e.Data, &d)
	str := func(k string) string { v, _ := d[k].(string); return v }
	switch kind {
	case Submitted:
		m.Headline = "Your request " + num + " was submitted: it waits for approvals."
	case ApprovalNeeded:
		switch e.Kind {
		case "submitted":
			m.Headline = r.Requester + " asks for your approval of request " + num + "."
		case "modified_outside_quai":
			m.Headline = "Request " + num + " was changed outside Quai: its approvals are void, and it needs yours for the new content."
		default:
			m.Headline = "Request " + num + " needs your approval: its plan makes a rule apply."
		}
	case Approved:
		m.Headline = "Your request " + num + " was approved: Quai delivers it."
	case Rejected:
		m.Headline = "Your request " + num + " was rejected"
		if e.Actor != "" {
			m.Headline += " by " + e.Actor
			if o := str("onBehalfOf"); o != "" {
				m.Headline += " on behalf of " + o
			}
		}
		m.Headline += "."
	case Stale:
		m.Headline = "Your request " + num + " is stale: what it was planned on changed since. Request it again."
	case Merged:
		switch e.Kind {
		case "committed":
			m.Headline = "Your request " + num + " was committed."
		case "merged_without_approval":
			m.Headline = "Your request " + num + "'s pull request was merged without Quai's approval."
		default:
			m.Headline = "Your request " + num + " was merged."
		}
	case Applied:
		m.Headline = "Your request " + num + " was applied."
	case Failed:
		if e.Kind == "apply_failed" {
			m.Headline = "The apply of your request " + num + " failed"
			if st := str("stack"); st != "" {
				m.Headline += " in stack " + st
			}
			m.Headline += "."
		} else {
			m.Headline = "Delivering your request " + num + " failed: its page says why."
		}
	case ExpiringSoon:
		m.Headline = "What your request " + num + " created expires soon."
	}
	if err := s.changes(ctx, r, rc); err != nil {
		return m, err
	}
	return m, s.plans(ctx, r, rc)
}

// changes adds a request's files, with the lines each adds and removes.
func (s *Service) changes(ctx context.Context, r db.Request, rc *RequestContent) error {
	sets, err := s.opts.Store.ListChangeSets(ctx, r.ID)
	if err != nil {
		return err
	}
	if len(sets) > 0 {
		rc.ChangeSet = sets[0].Hash // what a review approves (requests.Review)
	}
	for _, cs := range sets {
		changes, err := s.opts.Store.ListChanges(ctx, cs.ID)
		if err != nil {
			return err
		}
		for _, c := range changes {
			fc := FileChange{Path: c.Path, Op: c.Op}
			for _, l := range (plan.Change{Path: c.Path, Op: c.Op, Before: c.Before, After: c.After}).Diff() {
				switch l.Op {
				case '+':
					fc.Added++
				case '-':
					fc.Removed++
				}
			}
			rc.Changes = append(rc.Changes, fc)
		}
	}
	return nil
}

// plans adds the latest plan of each stack of the request's pull request: what the request page
// shows of it, less the attribute values (open question 52).
func (s *Service) plans(ctx context.Context, r db.Request, rc *RequestContent) error {
	list, err := s.opts.Store.LatestRequestRuns(ctx, db.LatestRequestRunsParams{RequestID: pgtype.UUID{Bytes: r.ID, Valid: true}, Kind: "plan"})
	if err != nil {
		return err
	}
	for _, run := range list {
		p := PlanContent{Stack: run.Stack, Status: run.Status}
		var sum struct{ Add, Change, Destroy int }
		_ = json.Unmarshal(run.Summary, &sum)
		p.Add, p.Change, p.Destroy = sum.Add, sum.Change, sum.Destroy
		var cost struct {
			Currency     string  `json:"currency"`
			MonthlyDelta float64 `json:"monthlyDelta"`
		}
		if json.Unmarshal(run.Cost, &cost) == nil && cost.Currency != "" {
			p.Cost = fmt.Sprintf("%+.2f %s a month", cost.MonthlyDelta, cost.Currency)
		}
		var policies []struct{ Name, Result string }
		_ = json.Unmarshal(run.Policies, &policies)
		for _, pol := range policies {
			p.Policies = append(p.Policies, PolicyResult(pol))
		}
		var sp runs.Plan
		if json.Unmarshal(run.Plan, &sp) == nil {
			var all []ResourceChange
			for _, res := range sp.Resources {
				for _, c := range res.Changes {
					all = append(all, ResourceChange{Action: c.Action, Address: c.Address})
				}
			}
			for _, c := range sp.Unmapped {
				all = append(all, ResourceChange{Action: c.Action, Address: c.Address})
			}
			for _, c := range sp.Ambiguous {
				all = append(all, ResourceChange{Action: c.Action, Address: c.Address})
			}
			if len(all) > maxResources {
				p.More, all = len(all)-maxResources, all[:maxResources]
			}
			p.Resources = all
		}
		rc.Plans = append(rc.Plans, p)
	}
	return nil
}

// alertMessage is an alert: what the event recorded, never a request's content.
func (s *Service) alertMessage(e db.Event) Message {
	repo := strings.TrimPrefix(e.Target, "repository:")
	var d map[string]any
	_ = json.Unmarshal(e.Data, &d)
	str := func(k string) string {
		switch v := d[k].(type) {
		case string:
			return v
		case float64:
			return fmt.Sprintf("%.0f", v)
		}
		return ""
	}
	a := &AlertContent{Repository: repo}
	detail := func(label, key string) {
		if v := str(key); v != "" {
			a.Details = append(a.Details, Detail{label, v})
		}
	}
	m := Message{Kind: Alert, Alert: a, Link: s.link("/admin/repositories/" + url.PathEscape(repo))}
	switch e.Kind {
	case "alert_bypass":
		m.Subject = "[Quai] Alert: a pull request was merged without approval in " + repo
		m.Headline = "A pull request of " + repo + " was merged without Quai's approval: check the branch protection."
		detail("Request", "number")
		detail("State then", "state")
		detail("Pull request", "pullRequest")
		detail("Merge commit", "commit")
		if id := str("request"); id != "" {
			m.Link = s.link("/requests/" + url.PathEscape(id))
		}
	case "alert_enforcement":
		m.Subject = "[Quai] Alert: approvals are not enforced on " + repo
		m.Headline = "The platform no longer makes " + repo + " wait for Quai's approval: Quai opens no pull request there until it does."
		detail("Branch", "branch")
		detail("Required check", "check")
		if ps, ok := d["problems"].([]any); ok {
			for _, p := range ps {
				if v, ok := p.(string); ok {
					a.Details = append(a.Details, Detail{"Problem", v})
				}
			}
		}
	default:
		m.Subject = "[Quai] Alert: " + strings.ReplaceAll(strings.TrimPrefix(e.Kind, "alert_"), "_", " ") + " on " + repo
		// Identifiers only: alerts reach admins and addresses that may not read the request, so
		// no text of the event (a justification, a reason) goes in them.
		m.Headline = "Something an administrator of " + repo + " must look at happened."
		detail("Request", "number")
	}
	return m
}

// channelMessage is a post to a group channel (Teams', Slack's): its people may not all read the
// request, so it says what an alert says, identifiers and the state, and links to the request; never
// its reason, files or plan (open question 64). An approval needed is posted while the request
// still waits for approval.
func (s *Service) channelMessage(ctx context.Context, n db.Notification, e db.Event, to Recipient) (Recipient, Message, string, error) {
	if !e.RequestID.Valid {
		return to, Message{}, "the event is not a request's", nil
	}
	r, err := s.opts.Requests.Get(ctx, uuid.UUID(e.RequestID.Bytes))
	if errors.Is(err, requests.ErrNotFound) {
		return to, Message{}, "the request was deleted", nil
	}
	if err != nil {
		return to, Message{}, "", err
	}
	if n.Kind == ApprovalNeeded && r.State != requests.PendingApproval {
		return to, Message{}, "the request no longer waits for approval", nil
	}
	num := fmt.Sprintf("#%d", r.Number)
	// No title either: it names the resource, as an alert does not (open question 55).
	rc := &RequestContent{ID: r.ID, State: r.State, Number: r.Number, Repository: s.opts.Requests.Repository(ctx, r)}
	m := Message{Kind: n.Kind, Subject: fmt.Sprintf("[Quai] %s: #%d", subjects[n.Kind], r.Number), Request: rc,
		Link: s.link("/requests/" + url.PathEscape(r.ID.String()))}
	switch n.Kind {
	case Submitted:
		m.Headline = "Request " + num + " was submitted."
	case ApprovalNeeded:
		m.Headline = "Request " + num + " waits for approval."
		if e.Kind == "modified_outside_quai" {
			m.Headline = "Request " + num + " was changed outside Quai: it waits for approval of its new content."
		}
	case Approved:
		m.Headline = "Request " + num + " was approved."
	case Rejected:
		m.Headline = "Request " + num + " was rejected."
	case Stale:
		m.Headline = "Request " + num + " is stale."
	case Merged:
		m.Headline = "Request " + num + " was merged."
		switch e.Kind {
		case "committed":
			m.Headline = "Request " + num + " was committed."
		case "merged_without_approval":
			m.Headline = "Request " + num + "'s pull request was merged without Quai's approval."
		}
	case Applied:
		m.Headline = "Request " + num + " was applied."
	case Failed:
		m.Headline = "Delivering request " + num + " failed."
	default:
		m.Headline = "Request " + num + ": " + strings.ReplaceAll(n.Kind, "_", " ") + "."
	}
	return to, m, "", nil
}
