package github

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v88/github"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/githubtest"
	"github.com/damhau/quai/backend/internal/secrets"
)

// approved is quai/approval as delivery publishes it once the request is approved.
var approved = connectors.Status{Name: quaiCheck, State: connectors.StatusSuccess, Description: "Approved in Quai", URL: "https://quai.example.com/requests/1"}

// publish publishes quai/approval on a commit of a pull request.
func (r *liveRepo) publish(t *testing.T, pr connectors.PullRequest, head string, st connectors.StatusState) {
	t.Helper()
	s := approved
	s.State = st
	if err := r.c.Statuses().Set(t.Context(), connectors.StatusTarget{PullRequest: pr.ID, Commit: head}, s); err != nil {
		t.Fatal(err)
	}
}

// awaitMerged asks ask, if any, every few seconds as delivery does, until the pull request is
// merged, and returns it.
func (r *liveRepo) awaitMerged(t *testing.T, id string, within time.Duration, ask func() error) connectors.PullRequest {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if ask != nil {
			if err := ask(); err != nil && !errors.Is(err, connectors.ErrBlocked) {
				t.Fatal(err)
			}
		}
		pr, err := r.prs.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if pr.State == connectors.PRMerged {
			return pr
		}
		if time.Now().After(deadline) {
			t.Fatalf("pull request %s is not merged after %s: %+v", id, within, pr)
		}
		time.Sleep(5 * time.Second)
	}
}

// commitMessage returns a commit's message on GitHub.
func (r *liveRepo) commitMessage(t *testing.T, sha string) string {
	t.Helper()
	c, _, err := r.s.Client.Git.GetCommit(t.Context(), r.s.Org, r.name, sha)
	if err != nil {
		t.Fatal(err)
	}
	return c.GetMessage()
}

// TestLiveAutoMerge: on a branch requiring quai/approval, AutoComplete enables GitHub's auto-merge,
// pinned to the approved head, with Quai's method and message; GitHub merges once the check
// succeeds, keeping the branch. A push after it withdraws auto-merge. A repository that does not
// allow auto-merge is merged by AutoComplete itself once the check passes.
func TestLiveAutoMerge(t *testing.T) {
	s := githubtest.New(t)
	ctx := t.Context()
	r := newLiveRepo(t, s)
	if _, err := s.RequireCheck(ctx, r.name, quaiCheck); err != nil {
		t.Fatal(err)
	}
	const message = "Resize web01\n\nApproved-by: Alice <alice@example.com>\n"

	t.Run("auto-merge not allowed", func(t *testing.T) {
		pr := r.open(t, connectors.NewPullRequest{})
		r.publish(t, pr, pr.Head, connectors.StatusPending)
		o := connectors.MergeOptions{Head: pr.Head, Method: connectors.Squash, Message: message, DeleteBranch: true}
		if err := r.prs.AutoComplete(ctx, pr.ID, o); !errors.Is(err, connectors.ErrBlocked) {
			t.Fatalf("with the check pending: %v, want ErrBlocked", err)
		}
		r.publish(t, pr, pr.Head, connectors.StatusSuccess)
		merged := r.awaitMerged(t, pr.ID, time.Minute, func() error { return r.prs.AutoComplete(ctx, pr.ID, o) })
		if !strings.Contains(r.commitMessage(t, merged.MergeCommit), "Approved-by: Alice") || r.head(t, pr.Source) != "" {
			t.Errorf("merged by Quai: %q, the branch at %q", r.commitMessage(t, merged.MergeCommit), r.head(t, pr.Source))
		}
	})

	if err := s.AllowAutoMerge(ctx, r.name, true); err != nil {
		t.Fatal(err)
	}
	t.Run("auto-merge", func(t *testing.T) {
		pr := r.open(t, connectors.NewPullRequest{})
		r.publish(t, pr, pr.Head, connectors.StatusPending)
		o := connectors.MergeOptions{Head: pr.Head, Method: connectors.Squash, Message: message, DeleteBranch: true}
		if err := r.prs.AutoComplete(ctx, pr.ID, o); err != nil {
			t.Fatal(err)
		}
		if got, err := r.prs.Get(ctx, pr.ID); err != nil || !got.AutoComplete || got.State != connectors.PROpen {
			t.Fatalf("auto-merge enabled: %+v, %v", got, err)
		}
		if err := r.prs.AutoComplete(ctx, pr.ID, o); err != nil {
			t.Errorf("asked again: %v", err)
		}

		// A push after the approval: auto-merge is withdrawn, so GitHub merges nothing unapproved.
		head := r.push(t, pr.Source, "later.txt", "later\n")
		r.waitHead(t, pr.ID, head)
		if err := r.prs.AutoComplete(ctx, pr.ID, o); !errors.Is(err, connectors.ErrBlocked) {
			t.Fatalf("after a push: %v, want ErrBlocked", err)
		}
		if got, err := r.prs.Get(ctx, pr.ID); err != nil || got.AutoComplete {
			t.Fatalf("auto-merge withdrawn: %+v, %v", got, err)
		}

		// Approved again, on the new head: GitHub merges once the check succeeds, by itself.
		o.Head = head
		r.publish(t, pr, head, connectors.StatusPending)
		if err := r.prs.AutoComplete(ctx, pr.ID, o); err != nil {
			t.Fatal(err)
		}
		r.publish(t, pr, head, connectors.StatusSuccess)
		merged := r.awaitMerged(t, pr.ID, 2*time.Minute, nil)
		if got := r.commitMessage(t, merged.MergeCommit); !strings.HasPrefix(got, "Resize web01\n\n") || !strings.Contains(got, "Approved-by: Alice <alice@example.com>") {
			t.Errorf("the squash commit's message: %q", got)
		}
		if r.head(t, pr.Source) != head {
			t.Error("GitHub's auto-merge deleted the branch: delivery need not")
		}
		if err := r.prs.AutoComplete(ctx, pr.ID, o); err != nil {
			t.Errorf("asked once merged: %v", err)
		}
	})

	t.Run("mergeable at once", func(t *testing.T) {
		pr := r.open(t, connectors.NewPullRequest{})
		r.publish(t, pr, pr.Head, connectors.StatusSuccess)
		o := connectors.MergeOptions{Head: pr.Head, Method: connectors.MergeCommit, Message: message, DeleteBranch: true}
		r.awaitMerged(t, pr.ID, time.Minute, func() error { return r.prs.AutoComplete(ctx, pr.ID, o) })
	})
}

// TestLiveMergeQueue: on a branch merging through a merge queue, AutoComplete enqueues the pull
// request once its required check passes, then publishes quai/approval on the merge group's commit,
// which GitHub requires there too; the queue merges with its own method and message, as the app.
// The deliveries of the queue (merge_group, the pull request's enqueued) are parsed as changes of
// the pull request.
func TestLiveMergeQueue(t *testing.T) {
	s := githubtest.New(t)
	ctx := t.Context()
	r := newLiveRepo(t, s)
	repository, _, err := s.Client.Repositories.Get(ctx, s.Org, r.name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MergeQueue(ctx, r.name, quaiCheck); err != nil {
		t.Fatal(err)
	}
	pr := r.open(t, connectors.NewPullRequest{Title: "Queued"})
	r.publish(t, pr, pr.Head, connectors.StatusPending)
	o := connectors.MergeOptions{Head: pr.Head, Method: connectors.Squash, Message: "Queued\n\nApproved-by: Alice <alice@example.com>\n", DeleteBranch: true, Approval: approved}
	if err := r.prs.AutoComplete(ctx, pr.ID, o); !errors.Is(err, connectors.ErrBlocked) {
		t.Fatalf("enqueued with the check pending: %v, want ErrBlocked", err)
	}
	r.publish(t, pr, pr.Head, connectors.StatusSuccess)
	// The queue takes a merge from nobody else, approved or not.
	if _, err := r.prs.Merge(ctx, pr.ID, o); !errors.Is(err, connectors.ErrUnsupported) {
		t.Errorf("a merge outside the queue: %v, want ErrUnsupported", err)
	}
	merged := r.awaitMerged(t, pr.ID, 4*time.Minute, func() error { return r.prs.AutoComplete(ctx, pr.ID, o) })
	if got := r.head(t, githubtest.Branch); got != merged.MergeCommit {
		t.Errorf("main is at %s, the merge commit is %s", got, merged.MergeCommit)
	}
	full, _, err := s.Client.PullRequests.Get(ctx, s.Org, r.name, mustNumber(t, pr.ID))
	if err != nil || !strings.HasSuffix(full.GetMergedBy().GetLogin(), "[bot]") {
		t.Errorf("merged by %q, %v: want the app", full.GetMergedBy().GetLogin(), err)
	}
	if got := r.commitMessage(t, merged.MergeCommit); strings.Contains(got, "Approved-by") {
		t.Logf("the queue kept Quai's message: %q", got) // measured otherwise: worth knowing
	}
	if r.head(t, pr.Source) != pr.Head {
		t.Error("the queue deleted the branch: delivery need not")
	}

	// The queue's deliveries name the pull request.
	secret := s.WebhookSecret(t)
	deliveries := s.Deliveries(t, repository.GetID(), func(ds []githubtest.Delivery) bool {
		return githubtest.HasEvent(ds, "merge_group", "checks_requested") && githubtest.HasEvent(ds, "pull_request", "enqueued") && githubtest.HasEvent(ds, "pull_request", "closed")
	})
	kinds := map[string]bool{}
	for _, d := range deliveries {
		got, err := r.c.Webhooks().Parse(d.Header, d.Body, secret)
		if err != nil {
			t.Fatalf("delivery %d (%s): %v", d.ID, d.Event, err)
		}
		for _, e := range got.Events {
			if changed, ok := e.(connectors.PullRequestChanged); ok && changed.ID == pr.ID {
				kinds[d.Event+"."+d.Action+"="+string(changed.Action)] = true
			}
		}
	}
	for _, want := range []string{"merge_group.checks_requested=updated", "pull_request.enqueued=updated", "pull_request.closed=completed"} {
		if !kinds[want] {
			t.Errorf("no %s among %v", want, kinds)
		}
	}
}

// TestLiveWebhooks: the deliveries GitHub recorded for a repository, replayed as they are, verify
// with the app's webhook secret and none other, and become the events Quai acts on: pushes, pull
// requests opened, updated and closed, check runs of other apps and commit statuses on their
// commits and pull requests, and changes to rulesets and branch protection.
func TestLiveWebhooks(t *testing.T) {
	s := githubtest.New(t)
	secret := s.WebhookSecret(t)
	ctx := t.Context()
	r := newLiveRepo(t, s)
	repository, _, err := s.Client.Repositories.Get(ctx, s.Org, r.name)
	if err != nil {
		t.Fatal(err)
	}
	pr := r.open(t, connectors.NewPullRequest{})
	r.publish(t, pr, pr.Head, connectors.StatusPending)
	if _, _, err := s.Client.Repositories.CreateStatus(ctx, s.Org, r.name, pr.Head, gh.RepoStatus{State: gh.Ptr("success"), Context: gh.Ptr("ci/legacy")}); err != nil {
		t.Fatal(err)
	}
	head := r.push(t, pr.Source, "later.txt", "later\n")
	r.waitHead(t, pr.ID, head)
	if err := r.prs.Abandon(ctx, pr.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Client.Repositories.UpdateBranchProtection(ctx, s.Org, r.name, githubtest.Branch, &gh.ProtectionRequest{EnforceAdmins: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequireCheck(ctx, r.name, quaiCheck); err != nil {
		t.Fatal(err)
	}
	deliveries := s.Deliveries(t, repository.GetID(), func(ds []githubtest.Delivery) bool {
		for _, want := range [][2]string{{"push", ""}, {"pull_request", "opened"}, {"pull_request", "synchronize"}, {"pull_request", "closed"},
			{"check_run", ""}, {"status", ""}, {"branch_protection_rule", ""}, {"repository_ruleset", ""}} {
			if !githubtest.HasEvent(ds, want[0], want[1]) {
				return false
			}
		}
		return true
	})

	// The app's own check runs are Quai's; as another app's, they are status changes.
	other, err := New(connectors.Config{URL: s.URL(r.name), Credentials: s.PrivateKey, AppID: s.AppID + 1})
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, d := range deliveries {
		got, err := r.c.Webhooks().Parse(d.Header, d.Body, secret)
		if err != nil {
			t.Fatalf("delivery %d (%s.%s): %v", d.ID, d.Event, d.Action, err)
		}
		if got.ID == "" || got.ID != d.Header.Get("X-GitHub-Delivery") {
			t.Errorf("delivery %d's ID: %q", d.ID, got.ID)
		}
		if _, err := r.c.Webhooks().Parse(d.Header, d.Body, secrets.FromValue(secret.Reveal()+"-wrong")); !errors.Is(err, connectors.ErrUnauthenticated) {
			t.Errorf("delivery %d with a wrong secret: %v", d.ID, err)
		}
		if d.Event == "check_run" {
			if len(got.Events) != 0 {
				t.Errorf("Quai's own check run became %+v", got.Events)
			}
			if got, err = other.Webhooks().Parse(d.Header, d.Body, secret); err != nil {
				t.Fatal(err)
			}
		}
		for _, e := range got.Events {
			switch e := e.(type) {
			case connectors.Push:
				events = append(events, "push "+e.Ref)
			case connectors.PullRequestChanged:
				events = append(events, "pull request "+string(e.Action))
				if e.ID != pr.ID {
					t.Errorf("a pull request event for %s: %+v", e.ID, e)
				}
			case connectors.StatusChanged:
				events = append(events, "status "+e.Status.Name+" "+string(e.Status.State))
				if e.Status.Name == quaiCheck && e.PullRequest != pr.ID {
					t.Errorf("a check run without its pull request: %+v", e)
				}
			case connectors.PolicyChanged:
				events = append(events, "policy")
			}
		}
	}
	for _, want := range []string{"push refs/heads/" + pr.Source, "pull request opened", "pull request updated", "pull request abandoned",
		"status " + quaiCheck + " pending", "status ci/legacy success", "policy"} {
		if !slices.Contains(events, want) {
			t.Errorf("no %q among %v", want, events)
		}
	}
}
