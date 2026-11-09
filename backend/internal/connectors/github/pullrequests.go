package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v88/github"

	"github.com/damhau/quai/backend/internal/connectors"
)

// What GitHub accepts at most, in characters.
const (
	maxDescription = 65536
	maxTitle       = 256
	maxLabel       = 50
)

// How long and how often Merge keeps asking while GitHub's refusal is a passing one: the base branch
// moved under the merge, or the pull request's mergeability is not computed yet.
var (
	mergeTimeout = 30 * time.Second
	mergeRetry   = time.Second
)

type pullRequests Connector

// short cuts s to n characters.
func short(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func number(id string) (int, error) {
	n, err := strconv.Atoi(id)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: pull request %q", connectors.ErrNotFound, id)
	}
	return n, nil
}

func merged(pr *gh.PullRequest) bool { return pr.GetMerged() || pr.MergedAt != nil }

func convert(pr *gh.PullRequest) connectors.PullRequest {
	out := connectors.PullRequest{ID: strconv.Itoa(pr.GetNumber()), URL: pr.GetHTMLURL(), Title: pr.GetTitle(), Description: pr.GetBody(),
		Source: pr.GetHead().GetRef(), Target: pr.GetBase().GetRef(), Head: pr.GetHead().GetSHA(), Draft: pr.GetDraft(),
		AutoComplete: pr.AutoMerge != nil, State: connectors.PROpen}
	for _, l := range pr.Labels {
		out.Labels = append(out.Labels, l.GetName())
	}
	switch {
	case merged(pr):
		out.State, out.MergeCommit = connectors.PRMerged, pr.GetMergeCommitSHA()
	case pr.GetState() == "closed":
		out.State = connectors.PRAbandoned
	}
	return out
}

// sameRepository reports whether the pull request's source branch is in the repository: anyone can
// open one from a fork of a public repository, with a branch named as they like. head.repo is null
// when the fork is gone.
func sameRepository(pr *gh.PullRequest) bool {
	head, base := pr.GetHead().GetRepo(), pr.GetBase().GetRepo()
	return head != nil && base != nil && head.GetID() == base.GetID()
}

// Open opens the pull request, then labels it: GitHub does the two apart, in the issue that every
// pull request also is. Labels it does not know yet are created.
func (p *pullRequests) Open(ctx context.Context, n connectors.NewPullRequest) (connectors.PullRequest, error) {
	c := (*Connector)(p)
	pr, resp, err := c.client.PullRequests.Create(ctx, c.owner, c.repo, &gh.NewPullRequest{Title: gh.Ptr(short(n.Title, maxTitle)), Head: gh.Ptr(n.Source), Base: gh.Ptr(n.Target),
		Body: gh.Ptr(connectors.Truncate(n.Description, maxDescription)), Draft: gh.Ptr(n.Draft)})
	if err := check(resp, err); err != nil {
		return connectors.PullRequest{}, err
	}
	out := convert(pr)
	if len(n.Labels) == 0 {
		return out, nil
	}
	names := make([]string, 0, len(n.Labels))
	for _, l := range n.Labels {
		names = append(names, short(l, maxLabel))
	}
	labels, resp, err := c.client.Issues.AddLabelsToIssue(ctx, c.owner, c.repo, pr.GetNumber(), names)
	if err := check(resp, err); err != nil {
		return connectors.PullRequest{}, fmt.Errorf("the labels of pull request %d: %w", pr.GetNumber(), err)
	}
	out.Labels = out.Labels[:0]
	for _, l := range labels {
		out.Labels = append(out.Labels, l.GetName())
	}
	return out, nil
}

func (p *pullRequests) Get(ctx context.Context, id string) (connectors.PullRequest, error) {
	c := (*Connector)(p)
	n, err := number(id)
	if err != nil {
		return connectors.PullRequest{}, err
	}
	pr, resp, err := c.client.PullRequests.Get(ctx, c.owner, c.repo, n)
	if err := check(resp, err); err != nil {
		return connectors.PullRequest{}, err
	}
	return convert(pr), nil
}

func (p *pullRequests) Update(ctx context.Context, id string, u connectors.PullRequestUpdate) (connectors.PullRequest, error) {
	c := (*Connector)(p)
	n, err := number(id)
	if err != nil {
		return connectors.PullRequest{}, err
	}
	edit := &gh.PullRequest{}
	if u.Title != nil {
		edit.Title = gh.Ptr(short(*u.Title, maxTitle))
	}
	if u.Description != nil {
		edit.Body = gh.Ptr(connectors.Truncate(*u.Description, maxDescription))
	}
	pr, resp, err := c.client.PullRequests.Edit(ctx, c.owner, c.repo, n, edit)
	if err := check(resp, err); err != nil {
		return connectors.PullRequest{}, err
	}
	return convert(pr), nil
}

// List returns the open pull requests whose source branch, in this repository, starts with prefix.
// A fork's branch never counts, whatever its name.
func (p *pullRequests) List(ctx context.Context, prefix string) ([]connectors.PullRequest, error) {
	c := (*Connector)(p)
	opts := &gh.PullRequestListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: 100}}
	var out []connectors.PullRequest
	for {
		prs, resp, err := c.client.PullRequests.List(ctx, c.owner, c.repo, opts)
		if err := check(resp, err); err != nil {
			return nil, err
		}
		for _, pr := range prs {
			if sameRepository(pr) && strings.HasPrefix(pr.GetHead().GetRef(), prefix) {
				out = append(out, convert(pr))
			}
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// methods are GitHub's names for Quai's merge methods.
var methods = map[connectors.MergeMethod]string{
	connectors.MergeCommit: "merge",
	connectors.Squash:      "squash",
	connectors.Rebase:      "rebase",
}

// Merge merges the pull request now, pinned to the approved head, and then deletes its branch,
// which GitHub's merge does not do. GitHub refuses with a 405 or a 409, and most refusals are
// ErrBlocked, for Quai to ask again later. These are not:
//   - the base branch moved under the merge ("Base branch was modified"), as when two pull requests
//     merge at once: asked again here;
//   - GitHub has not computed the pull request's mergeability yet: asked again here;
//   - a 409 while the pull request still shows the approved head, so the merge saw a newer one than
//     the API does yet: asked again here, until the API settles which it is;
//   - the repository's settings forbid the method, or the branch merges only through its merge
//     queue (AutoComplete's way): ErrUnsupported, since waiting will not help.
//
// The merge commit is the response's, which GitHub is sure of, and a pull request found merged
// already is a success, so a retry finds what the first attempt did.
func (p *pullRequests) Merge(ctx context.Context, id string, o connectors.MergeOptions) (connectors.PullRequest, error) {
	c := (*Connector)(p)
	n, err := number(id)
	if err != nil {
		return connectors.PullRequest{}, err
	}
	method, ok := methods[o.Method]
	if !ok {
		return connectors.PullRequest{}, fmt.Errorf("%w: merge method %q", connectors.ErrUnsupported, o.Method)
	}
	pr, resp, err := c.client.PullRequests.Get(ctx, c.owner, c.repo, n)
	if err := check(resp, err); err != nil {
		return connectors.PullRequest{}, err
	}
	message := strings.TrimSpace(o.Message)
	title, body, _ := strings.Cut(message, "\n")
	opts := &gh.PullRequestOptions{CommitTitle: title, SHA: o.Head, MergeMethod: method, DontDefaultIfBlank: message != ""}
	deadline := time.Now().Add(mergeTimeout)
	for !merged(pr) {
		if pr.GetState() == "closed" {
			return connectors.PullRequest{}, fmt.Errorf("github: pull request %s was closed without merging", id)
		}
		result, resp, err := c.client.PullRequests.Merge(ctx, c.owner, c.repo, n, strings.TrimSpace(body), opts)
		switch {
		case err == nil && result.GetMerged():
			out := convert(pr)
			out.State, out.MergeCommit = connectors.PRMerged, result.GetSHA()
			c.deleteBranch(ctx, pr, o)
			return out, nil
		case err == nil:
			return connectors.PullRequest{}, fmt.Errorf("%w: %s", connectors.ErrBlocked, result.GetMessage())
		case resp == nil || (resp.StatusCode != http.StatusMethodNotAllowed && resp.StatusCode != http.StatusConflict):
			return connectors.PullRequest{}, check(resp, err)
		}
		refusal := refusalOf(err)
		if strings.Contains(refusal, "not allowed on this repository") || strings.Contains(refusal, "through the merge queue") {
			return connectors.PullRequest{}, fmt.Errorf("%w: %s", connectors.ErrUnsupported, refusal)
		}
		refused := resp.StatusCode
		pr, resp, err = c.client.PullRequests.Get(ctx, c.owner, c.repo, n)
		if err := check(resp, err); err != nil {
			return connectors.PullRequest{}, err
		}
		if merged(pr) || pr.GetState() == "closed" {
			continue // someone else's merge or close: the loop's conditions take it
		}
		moved := o.Head != "" && pr.GetHead().GetSHA() != o.Head
		passing := strings.Contains(refusal, "Base branch was modified") || pr.Mergeable == nil ||
			refused == http.StatusConflict // the head the API shows is the approved one: it was behind the merge
		if moved || pr.GetDraft() || !passing || time.Now().After(deadline) {
			return connectors.PullRequest{}, fmt.Errorf("%w: %s", connectors.ErrBlocked, refusal)
		}
		if err := sleep(ctx, mergeRetry); err != nil {
			return connectors.PullRequest{}, err
		}
	}
	c.deleteBranch(ctx, pr, o)
	return convert(pr), nil
}

// refusalOf returns what GitHub said when it refused.
func refusalOf(err error) string {
	var e *gh.ErrorResponse
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

// deleteBranch deletes a merged pull request's source branch when asked. A fork's branch is not
// the repository's, whatever its name, so it stays. The merge stands whatever happens here, so a
// failure is only logged. A branch already gone (422) is not one: the repository's "automatically
// delete head branches" setting, or an earlier attempt.
func (c *Connector) deleteBranch(ctx context.Context, pr *gh.PullRequest, o connectors.MergeOptions) {
	if !o.DeleteBranch || !sameRepository(pr) {
		return
	}
	resp, err := c.client.Git.DeleteRef(ctx, c.owner, c.repo, "heads/"+pr.GetHead().GetRef())
	if err == nil || (resp != nil && resp.StatusCode == http.StatusUnprocessableEntity) {
		return
	}
	c.cfg.Log.WarnContext(ctx, "the merged pull request's branch was not deleted", slog.Int("pullRequest", pr.GetNumber()), slog.String("branch", pr.GetHead().GetRef()), slog.Any("error", err))
}

func (p *pullRequests) Abandon(ctx context.Context, id string) error {
	c := (*Connector)(p)
	n, err := number(id)
	if err != nil {
		return err
	}
	_, resp, err := c.client.PullRequests.Edit(ctx, c.owner, c.repo, n, &gh.PullRequest{State: gh.Ptr("closed")})
	return check(resp, err)
}

// Comment adds a comment to the pull request, as GitHub keeps them: on its issue.
func (p *pullRequests) Comment(ctx context.Context, id, body string) error {
	c := (*Connector)(p)
	n, err := number(id)
	if err != nil {
		return err
	}
	_, resp, err := c.client.Issues.CreateComment(ctx, c.owner, c.repo, n, &gh.IssueComment{Body: gh.Ptr(connectors.Truncate(body, maxDescription))})
	return check(resp, err)
}

// Comments returns the bodies of the comments on the pull request's issue, oldest first.
func (p *pullRequests) Comments(ctx context.Context, id string) ([]string, error) {
	c := (*Connector)(p)
	n, err := number(id)
	if err != nil {
		return nil, err
	}
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var out []string
	for {
		comments, resp, err := c.client.Issues.ListComments(ctx, c.owner, c.repo, n, opts)
		if err := check(resp, err); err != nil {
			return nil, err
		}
		for _, cm := range comments {
			out = append(out, cm.GetBody())
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
