package github

import (
	"context"
	"fmt"

	gh "github.com/google/go-github/v88/github"

	"github.com/damhau/quai/backend/internal/connectors"
)

type statuses Connector

// commit returns the commit a status goes on: the given one, or the pull request's head.
func (s *statuses) commit(ctx context.Context, t connectors.StatusTarget) (string, error) {
	if t.Commit != "" {
		return t.Commit, nil
	}
	pr, err := (*pullRequests)(s).Get(ctx, t.PullRequest)
	return pr.Head, err
}

// Set publishes st as a check run of the app on the commit. Each call creates a new run: GitHub
// counts the latest run of a name, for a merge and for a listing (measured). A stale approval is
// action_required, which blocks a merge; neutral or skipped would let it through.
func (s *statuses) Set(ctx context.Context, t connectors.StatusTarget, st connectors.Status) error {
	c := (*Connector)(s)
	run := gh.CreateCheckRunOptions{Name: st.Name}
	switch st.State {
	case connectors.StatusPending:
		run.Status = gh.Ptr("in_progress")
	case connectors.StatusSuccess:
		run.Conclusion = gh.Ptr("success")
	case connectors.StatusFailure, connectors.StatusError:
		run.Conclusion = gh.Ptr("failure")
	case connectors.StatusNotApplicable:
		run.Conclusion = gh.Ptr("action_required")
	default:
		return fmt.Errorf("%w: status state %q", connectors.ErrUnsupported, st.State)
	}
	if run.Conclusion != nil {
		run.Status = gh.Ptr("completed")
	}
	if st.URL != "" {
		run.DetailsURL = gh.Ptr(st.URL)
	}
	if st.Description != "" {
		run.Output = &gh.CheckRunOutput{Title: gh.Ptr(st.Description), Summary: gh.Ptr(st.Description)}
	}
	sha, err := s.commit(ctx, t)
	if err != nil {
		return err
	}
	run.HeadSHA = sha
	_, resp, err := c.client.Checks.CreateCheckRun(ctx, c.owner, c.repo, run)
	return check(resp, err)
}

// List returns the checks on the commit, one per name: the latest check run of each app, and the
// commit statuses. When a name has several (two apps, or a check run and a status), the least
// favourable state stands, so that a check waited for is not taken as passed.
func (s *statuses) List(ctx context.Context, t connectors.StatusTarget) ([]connectors.Status, error) {
	c := (*Connector)(s)
	sha, err := s.commit(ctx, t)
	if err != nil {
		return nil, err
	}
	var all []connectors.Status
	opts := &gh.ListCheckRunsOptions{Filter: gh.Ptr("latest"), ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.client.Checks.ListCheckRunsForRef(ctx, c.owner, c.repo, sha, opts)
		if err := check(resp, err); err != nil {
			return nil, err
		}
		for _, r := range runs.CheckRuns {
			st := connectors.Status{Name: r.GetName(), State: runState(r.GetStatus(), r.GetConclusion()), URL: r.GetDetailsURL()}
			if st.Description = r.GetOutput().GetTitle(); st.Description == "" {
				st.Description = r.GetOutput().GetSummary()
			}
			all = append(all, st)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	page := &gh.ListOptions{PerPage: 100}
	for {
		combined, resp, err := c.client.Repositories.GetCombinedStatus(ctx, c.owner, c.repo, sha, page)
		if err := check(resp, err); err != nil {
			return nil, err
		}
		for _, st := range combined.Statuses {
			state := connectors.StatusState(st.GetState())
			if state != connectors.StatusPending && state != connectors.StatusSuccess && state != connectors.StatusFailure {
				state = connectors.StatusError
			}
			all = append(all, connectors.Status{Name: st.GetContext(), State: state, Description: st.GetDescription(), URL: st.GetTargetURL()})
		}
		if resp.NextPage == 0 {
			break
		}
		page.Page = resp.NextPage
	}
	return worst(all), nil
}

// runState reads a check run as a status. GitHub's required checks pass on success, neutral and
// skipped; action_required is how Quai publishes a stale approval.
func runState(status, conclusion string) connectors.StatusState {
	if status != "completed" {
		return connectors.StatusPending
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return connectors.StatusSuccess
	case "failure":
		return connectors.StatusFailure
	case "action_required":
		return connectors.StatusNotApplicable
	}
	return connectors.StatusError // cancelled, timed_out, stale, startup_failure
}

// rank orders states from the most to the least favourable.
var rank = map[connectors.StatusState]int{connectors.StatusSuccess: 0, connectors.StatusPending: 1, connectors.StatusNotApplicable: 2,
	connectors.StatusError: 3, connectors.StatusFailure: 4}

// worst keeps one status per name, the least favourable, in the order names first appear.
func worst(all []connectors.Status) []connectors.Status {
	var out []connectors.Status
	at := map[string]int{}
	for _, st := range all {
		i, ok := at[st.Name]
		switch {
		case !ok:
			at[st.Name] = len(out)
			out = append(out, st)
		case rank[st.State] > rank[out[i].State]:
			out[i] = st
		}
	}
	return out
}
