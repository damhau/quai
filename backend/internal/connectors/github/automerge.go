package github

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/damhau/quai/backend/internal/connectors"
)

// pullRequestState is what AutoComplete reads of a pull request, from GraphQL: where it stands in
// auto-merge and in the merge queue, which the REST API does not tell.
const pullRequestState = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      id state merged isDraft headRefOid isMergeQueueEnabled
      autoMergeRequest { mergeMethod }
      mergeQueueEntry { state headCommit { oid } }
    }
  }
}`

const (
	enableAutoMerge = `mutation($id: ID!, $method: PullRequestMergeMethod!, $head: GitObjectID, $title: String, $body: String) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: $method, expectedHeadOid: $head, commitHeadline: $title, commitBody: $body}) { clientMutationId }
}`
	disableAutoMerge = `mutation($id: ID!) { disablePullRequestAutoMerge(input: {pullRequestId: $id}) { clientMutationId } }`
	enqueue          = `mutation($id: ID!, $head: GitObjectID) { enqueuePullRequest(input: {pullRequestId: $id, expectedHeadOid: $head}) { clientMutationId } }`
	dequeue          = `mutation($id: ID!) { dequeuePullRequest(input: {id: $id}) { clientMutationId } }`
)

type prState struct {
	ID                  string `json:"id"`
	State               string `json:"state"` // OPEN, CLOSED, MERGED
	Merged              bool   `json:"merged"`
	IsDraft             bool   `json:"isDraft"`
	HeadRefOid          string `json:"headRefOid"`
	IsMergeQueueEnabled bool   `json:"isMergeQueueEnabled"`
	AutoMergeRequest    *struct {
		MergeMethod string `json:"mergeMethod"`
	} `json:"autoMergeRequest"`
	MergeQueueEntry *struct {
		State      string `json:"state"`
		HeadCommit *struct {
			Oid string `json:"oid"`
		} `json:"headCommit"`
	} `json:"mergeQueueEntry"`
}

func (c *Connector) pullRequestState(ctx context.Context, n int) (prState, error) {
	var data struct {
		Repository *struct {
			PullRequest *prState `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := c.graphql(ctx, pullRequestState, map[string]any{"owner": c.owner, "name": c.repo, "number": n}, &data); err != nil {
		return prState{}, err
	}
	if data.Repository == nil || data.Repository.PullRequest == nil {
		return prState{}, fmt.Errorf("%w: pull request %d", connectors.ErrNotFound, n)
	}
	return *data.Repository.PullRequest, nil
}

// mergeableNow is GitHub's refusal to enable auto-merge on a pull request it could merge at once:
// it merges one only on a merge call.
var mergeableNow = regexp.MustCompile(`is in (clean|unstable|has_hooks) status`)

// AutoComplete asks GitHub to merge the pull request, pinned to the approved head, once the base
// branch's rules pass:
//   - with a merge queue on the base branch, the pull request joins the queue. GitHub then checks
//     the merge group's commit, which holds the pull request on top of the base and of the entries
//     ahead of it, and requires quai/approval there too: AutoComplete publishes o.Approval on it.
//     The queue merges with the method and the message the ruleset sets, not o's;
//   - otherwise, auto-merge is enabled with o's method and message. GitHub refuses it on a pull
//     request it could merge at once, which is merged at once, as Merge does; a repository whose
//     settings do not allow auto-merge is merged the same way, Quai asking again until the checks
//     pass.
//
// Quai asks again until the pull request is merged, so each call takes up where the last left
// off. A head that moved since the approval withdraws auto-merge and the queue entry, and blocks.
func (p *pullRequests) AutoComplete(ctx context.Context, id string, o connectors.MergeOptions) error {
	c := (*Connector)(p)
	n, err := number(id)
	if err != nil {
		return err
	}
	method, ok := methods[o.Method]
	if !ok {
		return fmt.Errorf("%w: merge method %q", connectors.ErrUnsupported, o.Method)
	}
	pr, err := c.pullRequestState(ctx, n)
	if err != nil {
		return err
	}
	switch {
	case pr.Merged || pr.State == "MERGED":
		return nil
	case pr.State == "CLOSED":
		return fmt.Errorf("github: pull request %s was closed without merging", id)
	case o.Head != "" && pr.HeadRefOid != o.Head:
		if err := c.withdraw(ctx, pr); err != nil {
			return err
		}
		return fmt.Errorf("%w: pull request %s is at %s, not at the approved %s", connectors.ErrBlocked, id, short(pr.HeadRefOid, 12), short(o.Head, 12))
	case pr.IsDraft:
		return fmt.Errorf("%w: pull request %s is a draft", connectors.ErrBlocked, id)
	case pr.IsMergeQueueEnabled:
		return c.queue(ctx, pr, o)
	}
	return c.autoMerge(ctx, id, pr, method, o)
}

// withdraw disables the pull request's auto-merge and takes it out of the merge queue: GitHub
// keeps auto-merge enabled after a push (measured), and would merge a head nobody approved once
// its checks pass.
func (c *Connector) withdraw(ctx context.Context, pr prState) error {
	if pr.AutoMergeRequest != nil {
		if err := c.graphql(ctx, disableAutoMerge, map[string]any{"id": pr.ID}, nil); err != nil {
			return err
		}
	}
	if pr.MergeQueueEntry != nil {
		return c.graphql(ctx, dequeue, map[string]any{"id": pr.ID}, nil)
	}
	return nil
}

// autoMerge enables auto-merge, unless it is already, with the same method.
func (c *Connector) autoMerge(ctx context.Context, id string, pr prState, method string, o connectors.MergeOptions) error {
	want := strings.ToUpper(method)
	if pr.AutoMergeRequest != nil {
		if pr.AutoMergeRequest.MergeMethod == want {
			return nil
		}
		if err := c.graphql(ctx, disableAutoMerge, map[string]any{"id": pr.ID}, nil); err != nil {
			return err
		}
	}
	vars := map[string]any{"id": pr.ID, "method": want, "head": optional(o.Head)}
	if o.Method != connectors.Rebase { // a rebase keeps the commits as they are
		title, body, _ := strings.Cut(strings.TrimSpace(o.Message), "\n")
		vars["title"], vars["body"] = optional(title), optional(strings.TrimSpace(body))
	}
	err := c.graphql(ctx, enableAutoMerge, vars, nil)
	var refused *graphQLError
	switch {
	case err == nil:
		return nil
	case !errors.As(err, &refused):
		return err
	case mergeableNow.MatchString(refused.Message), strings.Contains(refused.Message, "Auto merge is not allowed"):
		_, err := (*pullRequests)(c).Merge(ctx, id, o)
		return err
	case strings.Contains(refused.Message, "is not allowed on this repository"):
		return fmt.Errorf("%w: %s", connectors.ErrUnsupported, refused.Message) // the method: waiting will not change it
	}
	return fmt.Errorf("%w: %s", connectors.ErrBlocked, refused.Message)
}

// queue has the pull request join the merge queue, or, once it is in and GitHub has built its merge
// group, publishes the approval on the group's commit. GitHub refuses to enqueue a pull request
// whose required checks have not passed: blocked, for Quai to ask again.
func (c *Connector) queue(ctx context.Context, pr prState, o connectors.MergeOptions) error {
	if e := pr.MergeQueueEntry; e != nil {
		if e.HeadCommit == nil || e.HeadCommit.Oid == "" || o.Approval.Name == "" {
			return nil // queued: the group is not built yet
		}
		return c.approveGroup(ctx, e.HeadCommit.Oid, o.Approval)
	}
	err := c.graphql(ctx, enqueue, map[string]any{"id": pr.ID, "head": optional(o.Head)}, nil)
	var refused *graphQLError
	if errors.As(err, &refused) {
		return fmt.Errorf("%w: %s", connectors.ErrBlocked, refused.Message)
	}
	return err
}

// approveGroup publishes approval on a merge group's commit, unless it shows there already.
func (c *Connector) approveGroup(ctx context.Context, sha string, approval connectors.Status) error {
	s := (*statuses)(c)
	target := connectors.StatusTarget{Commit: sha}
	current, err := s.List(ctx, target)
	if err != nil {
		return err
	}
	for _, st := range current {
		if st.Name == approval.Name && st.State == approval.State {
			return nil
		}
	}
	return s.Set(ctx, target, approval)
}
