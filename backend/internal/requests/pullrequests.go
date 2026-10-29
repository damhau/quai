package requests

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Pull-request delivery (docs/design/05-delivery-and-approvals.md#pull-request-protocol): at
// submit, a branch quai/r-<number>-<slug> from the target's head with the request's one commit,
// a pull request, and quai/approval pending. The status follows the request; once approved, Quai
// merges (merge: auto, through the platform's auto-complete once waitForChecks pass) or leaves
// the merge to a person (merge: manual). The merge, however it happens, applies the request.

// PullRequestMode is the delivery mode of repositories that deliver through pull requests.
const PullRequestMode = "pull-request"

// UsePublicURL gives the URL people reach Quai at, for the links in pull requests.
func (s *Service) UsePublicURL(u string) { s.publicURL = strings.TrimSuffix(u, "/") }

// Workers adds the delivery workers.
func (s *Service) Workers(w *river.Workers) {
	river.AddWorker(w, NewWorker(s))
	river.AddWorker(w, &openWorker{s: s})
	river.AddWorker(w, &statusWorker{s: s})
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// branch names a request's branch: quai/r-1042-web01-resize.
func branch(prefix string, r db.Request) string {
	verb := r.Kind
	if r.Action != "" {
		verb = r.Action
	}
	name := r.Target[strings.LastIndex(r.Target, "/")+1:]
	slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name+"-"+verb), "-"), "-")
	if len(slug) > 40 {
		slug = strings.TrimRight(slug[:40], "-")
	}
	return fmt.Sprintf("%sr-%d-%s", prefix, r.Number, slug)
}

// link is the request's page, or "" without a public URL.
func (s *Service) link(r db.Request) string {
	if s.publicURL == "" {
		return ""
	}
	return s.publicURL + "/requests/" + r.ID.String()
}

// description is the pull request's body: the reason, the link to the request, the files, and
// the marker that maps events back to the request.
func (s *Service) description(r db.Request, changes []db.Change) string {
	var b strings.Builder
	if reason := strings.TrimSpace(r.Reason); reason != "" {
		b.WriteString(reason + "\n\n")
	}
	if l := s.link(r); l != "" {
		fmt.Fprintf(&b, "Requested by %s in Quai: [request #%d](%s).\n\n", r.Requester, r.Number, l)
	} else {
		fmt.Fprintf(&b, "Requested by %s in Quai, request #%d.\n\n", r.Requester, r.Number)
	}
	b.WriteString("Changes:\n")
	for _, c := range changes {
		fmt.Fprintf(&b, "- `%s` (%s)\n", c.Path, c.Op)
	}
	b.WriteString("\nApproval happens in Quai: the `quai/approval` check follows it.\n\n")
	b.WriteString(connectors.Marker(r.ID.String()))
	return b.String()
}

// labels are the pull request's: quai, and the stack's.
func labels(r db.Request) []string {
	out := []string{"quai"}
	if r.Stack != "" {
		out = append(out, "stack:"+r.Stack)
	}
	return out
}

// OpenArgs pushes a request's branch and opens its pull request.
type OpenArgs struct {
	RequestID uuid.UUID `json:"requestId"`
}

// Kind names the job.
func (OpenArgs) Kind() string { return "open_pull_request" }

type openWorker struct {
	river.WorkerDefaults[OpenArgs]
	s *Service
}

func (w *openWorker) Work(ctx context.Context, job *river.Job[OpenArgs]) error {
	s := w.s
	r, err := s.get(ctx, job.Args.RequestID)
	if err != nil {
		return err
	}
	if r.State != PendingApproval && r.State != Approved {
		return nil // cancelled or rejected before its pull request was opened
	}
	d, err := s.store.GetDelivery(ctx, r.ID)
	if err != nil {
		return err
	}
	if d.PullRequest != "" {
		return nil
	}
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	if err != nil || len(sets) == 0 {
		return fmt.Errorf("request %s has no change set: %w", r.ID, err)
	}
	cs := sets[0]
	repo, err := s.reg.Get(cs.Repository)
	if err != nil {
		return err
	}
	prs := repo.Connector.PullRequests()
	if prs == nil {
		_, err := s.transition(ctx, r, r.State, Failed, "failed", map[string]any{"error": "the " + repo.Connector.Info().Platform + " connector has no pull requests"})
		return err
	}
	if err := s.deliverable(ctx, repo); err != nil {
		// Enforcement was lost since the request was submitted: its pull request waits.
		s.log.WarnContext(ctx, "not opening a pull request", slog.String("request", r.ID.String()), slog.Any("error", err))
		return river.JobSnooze(time.Minute)
	}
	changes, err := s.store.ListChanges(ctx, cs.ID)
	if err != nil {
		return err
	}
	return jobs.WithRepositoryLock(ctx, s.store.Pool, repo.Name, 5*time.Second, func(ctx context.Context) error {
		commit, err := s.commit(ctx, r, cs, repo, false)
		if err != nil {
			return err
		}
		commit.Branch, commit.Base = d.Branch, repo.Branch
		sha, err := repo.Mirror.CommitAndPush(ctx, commit)
		switch {
		case errors.Is(err, gitx.ErrStale):
			_, err = s.transition(ctx, r, r.State, Stale, "stale", map[string]any{"error": err.Error()})
			return err
		case errors.Is(err, gitx.ErrNoChange):
			// A retry: the branch holds the commit already.
		case err != nil:
			return err
		}
		subject, _, _ := strings.Cut(commit.Message, "\n")
		pr, err := s.findOrOpen(ctx, prs, connectors.NewPullRequest{Source: d.Branch, Target: repo.Branch, Title: subject,
			Description: s.description(r, changes), Labels: labels(r)})
		if err != nil {
			return err
		}
		if sha == "" {
			sha = pr.Head
		}
		return s.store.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
			if _, err := q.PutDelivery(ctx, db.PutDeliveryParams{RequestID: r.ID, Repository: repo.Name, Branch: d.Branch, PullRequest: pr.ID, Url: pr.URL, Head: sha}); err != nil {
				return err
			}
			if err := event(ctx, q, r, "pull_request_opened", "", map[string]any{"pullRequest": pr.ID, "url": pr.URL, "branch": d.Branch, "commit": sha}); err != nil {
				return err
			}
			if err := jobs.InsertTx(ctx, s.jobs, tx, StatusArgs{RequestID: r.ID}, nil); err != nil {
				return err
			}
			if r.State == Approved {
				return jobs.InsertTx(ctx, s.jobs, tx, DeliverArgs{RequestID: r.ID}, nil)
			}
			return nil
		})
	})
}

// findOrOpen returns the open pull request of the branch, opening it unless a retry finds it.
func (s *Service) findOrOpen(ctx context.Context, prs connectors.PullRequests, n connectors.NewPullRequest) (connectors.PullRequest, error) {
	open, err := prs.List(ctx, n.Source)
	if err != nil {
		return connectors.PullRequest{}, err
	}
	for _, pr := range open {
		if pr.Source == n.Source {
			return pr, nil
		}
	}
	return prs.Open(ctx, n)
}

// StatusArgs publishes quai/approval as the request's state says, and closes the pull request of
// a request that is over without a merge.
type StatusArgs struct {
	RequestID uuid.UUID `json:"requestId"`
	// Description replaces the state's description: why the request waits again.
	Description string `json:"description,omitempty"`
}

// Kind names the job.
func (StatusArgs) Kind() string { return "publish_status" }

type statusWorker struct {
	river.WorkerDefaults[StatusArgs]
	s *Service
}

func (w *statusWorker) Work(ctx context.Context, job *river.Job[StatusArgs]) error {
	s := w.s
	r, err := s.get(ctx, job.Args.RequestID)
	if err != nil {
		return err
	}
	d, err := s.store.GetDelivery(ctx, r.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if d.PullRequest == "" {
		if r.State == PendingApproval || r.State == Approved {
			return river.JobSnooze(5 * time.Second) // the pull request is being opened
		}
		return nil
	}
	repo, err := s.reg.Get(d.Repository)
	if err != nil {
		return err
	}
	return s.publish(ctx, repo, r, d, job.Args.Description)
}

// publish sets quai/approval for the request's state, with description if given; a rejected or
// cancelled request's pull request is closed and its branch deleted.
func (s *Service) publish(ctx context.Context, repo *repositories.Repo, r db.Request, d db.Delivery, description string) error {
	st := connectors.Status{Name: repo.Document.Spec.Enforcement.RequiredCheck, URL: s.link(r)}
	closePR := false
	switch r.State {
	case PendingApproval:
		st.State, st.Description = connectors.StatusPending, "Waiting for approval in Quai"
	case Approved, Merging, Applied:
		st.State, st.Description = connectors.StatusSuccess, "Approved in Quai"
	case Rejected:
		st.State, st.Description, closePR = connectors.StatusFailure, "Rejected in Quai", true
	case Cancelled:
		st.State, st.Description, closePR = connectors.StatusFailure, "Cancelled in Quai", true
	case Stale:
		st.State, st.Description = connectors.StatusNotApplicable, "Stale: plan the request again in Quai"
	default:
		return nil
	}
	if description != "" {
		st.Description = description
	}
	if err := repo.Connector.Statuses().Set(ctx, connectors.StatusTarget{PullRequest: d.PullRequest, Commit: d.Head}, st); err != nil {
		return fmt.Errorf("publish %s on pull request %s: %w", st.Name, d.PullRequest, err)
	}
	if !closePR {
		return nil
	}
	if pr, err := repo.Connector.PullRequests().Get(ctx, d.PullRequest); err == nil && pr.State == connectors.PROpen {
		if err := repo.Connector.PullRequests().Abandon(ctx, d.PullRequest); err != nil {
			return err
		}
	}
	return repo.Mirror.DeleteBranch(ctx, d.Branch)
}

// deliverPR takes an approved request through its pull request: quai/approval succeeds, then,
// with merge: auto and once the waitForChecks pass, the platform is asked to merge.
func (s *Service) deliverPR(ctx context.Context, job *river.Job[DeliverArgs], r db.Request, d db.Delivery, repo *repositories.Repo) error {
	if d.PullRequest == "" {
		return river.JobSnooze(5 * time.Second) // the pull request is being opened
	}
	spec := repo.Document.Spec.Delivery
	prs := repo.Connector.PullRequests()
	pr, err := prs.Get(ctx, d.PullRequest)
	if err != nil {
		return err
	}
	if pr.State == connectors.PRMerged {
		return s.merged(ctx, r, d, pr)
	}
	if r.State == Approved {
		if err := s.publish(ctx, repo, r, d, ""); err != nil {
			return err
		}
		// When the merge keeps Quai's commit (a rebase), or a person merges, the approvals are a
		// final comment on the pull request; otherwise the merge's message carries them.
		if spec.Merge == "manual" || spec.MergeMethod == string(connectors.Rebase) {
			if err := s.commentApprovals(ctx, prs, r, d); err != nil {
				return err
			}
		}
		if spec.Merge == "manual" {
			return nil // a person merges; the merge applies the request
		}
		statuses, err := repo.Connector.Statuses().List(ctx, connectors.StatusTarget{PullRequest: d.PullRequest, Commit: d.Head})
		if err != nil {
			return err
		}
		if waiting := pending(spec.WaitForChecks, statuses); len(waiting) > 0 {
			s.log.InfoContext(ctx, "waiting for checks before merging", slog.String("request", r.ID.String()), slog.Any("checks", waiting))
			return river.JobSnooze(30 * time.Second)
		}
		if r, err = s.transition(ctx, r, Approved, Merging, "merging", map[string]any{"pullRequest": d.PullRequest}); err != nil {
			return err
		}
	}
	if r.State != Merging {
		return nil
	}
	msg, err := s.mergeMessage(ctx, r)
	if err != nil {
		return err
	}
	opts := connectors.MergeOptions{Head: d.Head, Method: connectors.MergeMethod(spec.MergeMethod), Message: msg, DeleteBranch: *spec.DeleteBranch}
	if repo.Connector.Info().Has(connectors.CapAutoComplete) {
		err = prs.AutoComplete(ctx, d.PullRequest, opts)
	} else {
		_, err = prs.Merge(ctx, d.PullRequest, opts)
	}
	if errors.Is(err, connectors.ErrBlocked) && job.Attempt < job.MaxAttempts {
		return river.JobSnooze(30 * time.Second)
	}
	if err != nil {
		if job.Attempt >= job.MaxAttempts {
			_, _ = s.transition(ctx, r, Merging, Failed, "failed", map[string]any{"error": err.Error()})
		}
		return err
	}
	if pr, err := prs.Get(ctx, d.PullRequest); err == nil && pr.State == connectors.PRMerged {
		return s.merged(ctx, r, d, pr)
	}
	return nil // the platform merges once its policies pass; webhooks or the poll tell
}

// mergeMessage is the message of the merge or squash commit: the request's, with its
// Approved-by trailers.
func (s *Service) mergeMessage(ctx context.Context, r db.Request) (string, error) {
	sets, err := s.store.ListChangeSets(ctx, r.ID)
	if err != nil || len(sets) == 0 {
		return "", fmt.Errorf("request %s has no change set: %w", r.ID, err)
	}
	approved, err := s.approvals(ctx, r)
	if err != nil {
		return "", err
	}
	return message(r, sets[0], approved...), nil
}

// commentApprovals records the approvals on the pull request, once.
func (s *Service) commentApprovals(ctx context.Context, prs connectors.PullRequests, r db.Request, d db.Delivery) error {
	events, err := s.store.ListRequestEvents(ctx, pgtype.UUID{Bytes: r.ID, Valid: true})
	if err != nil {
		return err
	}
	for _, e := range events {
		if e.Kind == "approvals_commented" {
			return nil
		}
	}
	approved, err := s.approvals(ctx, r)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Approved in Quai (request #%d).\n\n", r.Number)
	for _, t := range approved {
		b.WriteString(t.Key + ": " + t.Value + "\n")
	}
	if l := s.link(r); l != "" {
		b.WriteString("\n" + l + "\n")
	}
	if err := prs.Comment(ctx, d.PullRequest, b.String()); err != nil {
		return fmt.Errorf("comment on pull request %s: %w", d.PullRequest, err)
	}
	return s.store.Tx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		return event(ctx, q, r, "approvals_commented", "", map[string]any{"pullRequest": d.PullRequest})
	})
}

// pending returns the checks of names that have not succeeded.
func pending(names []string, statuses []connectors.Status) []string {
	var out []string
	for _, name := range names {
		ok := false
		for _, st := range statuses {
			if st.Name == name || st.Name == "policy:"+name {
				ok = st.State == connectors.StatusSuccess
			}
		}
		if !ok {
			out = append(out, name)
		}
	}
	return out
}

// merged records the merge of a request's pull request: the request is applied (without a
// runner, merged means applied).
func (s *Service) merged(ctx context.Context, r db.Request, d db.Delivery, pr connectors.PullRequest) error {
	if r.State == Applied {
		return nil
	}
	from := r.State
	kind := "merged"
	data := map[string]any{"commit": pr.MergeCommit, "pullRequest": pr.ID}
	bypass := from != Approved && from != Merging
	if bypass {
		// Merged without Quai's approval: the enforcement was bypassed.
		kind = "merged_without_approval"
	}
	err := s.store.Tx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		if err := q.SetDeliveryMerged(ctx, db.SetDeliveryMergedParams{RequestID: r.ID, MergeCommit: pr.MergeCommit}); err != nil {
			return err
		}
		if err := q.SetChangeSetCommit(ctx, db.SetChangeSetCommitParams{RequestID: r.ID, Repository: d.Repository, Commit: pr.MergeCommit}); err != nil {
			return err
		}
		if _, err := q.TransitionRequest(ctx, db.TransitionRequestParams{ID: r.ID, FromState: from, ToState: Applied}); err != nil {
			return conflict(err, r)
		}
		if err := event(ctx, q, r, kind, "", data); err != nil {
			return err
		}
		if bypass {
			return alert(ctx, q, d.Repository, "bypass", map[string]any{"request": r.ID.String(), "number": r.Number, "state": from,
				"pullRequest": pr.ID, "commit": pr.MergeCommit, "message": fmt.Sprintf("pull request %s was merged without Quai's approval: check the branch protection", pr.ID)})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if bypass {
		s.delivered(ctx, d.Repository, "bypassed")
	} else {
		s.delivered(ctx, d.Repository, Applied)
	}
	if repo, err := s.reg.Get(d.Repository); err == nil {
		if _, err := repo.Snapshot(ctx, true); err != nil {
			s.log.WarnContext(ctx, "refresh after a merge", slog.Any("error", err))
		}
	}
	return nil
}

// PullRequest acts on one of Quai's pull requests as re-read from the platform after a webhook
// or a poll (ingest.Handler): a merge applies its request, a pull request closed outside Quai
// cancels it, and a change on an approved one tries the merge again (its checks may pass now).
func (s *Service) PullRequest(ctx context.Context, repo *repositories.Repo, pr connectors.PullRequest) error {
	d, err := s.store.GetDeliveryByPullRequest(ctx, db.GetDeliveryByPullRequestParams{Repository: repo.Name, PullRequest: pr.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // not a request's
	}
	if err != nil {
		return err
	}
	r, err := s.get(ctx, d.RequestID)
	if err != nil {
		return err
	}
	switch {
	case pr.State == connectors.PRMerged:
		return s.merged(ctx, r, d, pr)
	case pr.State == connectors.PRAbandoned && (r.State == PendingApproval || r.State == Approved || r.State == Merging || r.State == Stale):
		_, err := s.transition(ctx, r, r.State, Cancelled, "cancelled", map[string]any{"reason": "the pull request was closed outside Quai"})
		return err
	case pr.State == connectors.PROpen && (r.State == Approved || r.State == Merging) && s.jobs != nil:
		_, err := s.jobs.Insert(ctx, DeliverArgs{RequestID: r.ID}, &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: 10 * time.Second}})
		return err
	}
	return nil
}

// Awaited returns the pull requests of the repository whose requests are not over (ingest.Handler).
func (s *Service) Awaited(ctx context.Context, repo *repositories.Repo) ([]string, error) {
	return s.store.ListAwaitedPullRequests(ctx, repo.Name)
}
