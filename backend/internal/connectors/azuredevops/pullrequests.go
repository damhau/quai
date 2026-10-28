package azuredevops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/damhau/quai/backend/internal/connectors"
)

// API versions of the endpoints that are still in preview in 7.0.
const (
	previewVersion = APIVersion + "-preview.1" // pull request statuses, policy evaluations
)

// maxDescription is the longest pull request description Azure DevOps accepts.
const maxDescription = 4000

// pr is a pull request as the API returns it.
type pr struct {
	ID                    int    `json:"pullRequestId"`
	Status                string `json:"status"` // active, abandoned, completed
	Title                 string `json:"title"`
	Description           string `json:"description"`
	SourceRefName         string `json:"sourceRefName"`
	TargetRefName         string `json:"targetRefName"`
	IsDraft               bool   `json:"isDraft"`
	MergeStatus           string `json:"mergeStatus"` // queued, conflicts, succeeded, rejectedByPolicy, failure
	CompletionQueueTime   string `json:"completionQueueTime"`
	LastMergeSourceCommit *struct {
		CommitID string `json:"commitId"`
	} `json:"lastMergeSourceCommit"`
	LastMergeCommit *struct {
		CommitID string `json:"commitId"`
	} `json:"lastMergeCommit"`
	AutoCompleteSetBy *struct {
		ID string `json:"id"`
	} `json:"autoCompleteSetBy"`
	Labels []struct {
		Name   string `json:"name"`
		Active bool   `json:"active"`
	} `json:"labels"`
	Repository struct {
		WebURL string `json:"webUrl"`
	} `json:"repository"`
}

func (p pr) convert(webURL string) connectors.PullRequest {
	out := connectors.PullRequest{ID: strconv.Itoa(p.ID), Title: p.Title, Description: p.Description,
		Source: strings.TrimPrefix(p.SourceRefName, "refs/heads/"), Target: strings.TrimPrefix(p.TargetRefName, "refs/heads/"),
		Draft: p.IsDraft, AutoComplete: p.AutoCompleteSetBy != nil, State: connectors.PROpen}
	if p.Repository.WebURL != "" {
		webURL = p.Repository.WebURL
	}
	out.URL = webURL + "/pullrequest/" + out.ID
	if p.LastMergeSourceCommit != nil {
		out.Head = p.LastMergeSourceCommit.CommitID
	}
	for _, l := range p.Labels {
		if l.Active {
			out.Labels = append(out.Labels, l.Name)
		}
	}
	switch p.Status {
	case "completed":
		out.State = connectors.PRMerged
		if p.LastMergeCommit != nil {
			out.MergeCommit = p.LastMergeCommit.CommitID
		}
	case "abandoned":
		out.State = connectors.PRAbandoned
	}
	return out
}

type pullRequests Connector

// api returns the path of the repository's resource under the collection, by IDs.
func (c *Connector) api(ctx context.Context, segments ...string) (string, Repository, error) {
	repo, err := c.Resolve(ctx)
	if err != nil {
		return "", Repository{}, err
	}
	return path(append([]string{repo.ProjectID, "_apis", "git", "repositories", repo.ID}, segments...)...), repo, nil
}

func (p *pullRequests) Open(ctx context.Context, n connectors.NewPullRequest) (connectors.PullRequest, error) {
	c := (*Connector)(p)
	at, repo, err := c.api(ctx, "pullrequests")
	if err != nil {
		return connectors.PullRequest{}, err
	}
	body := map[string]any{"sourceRefName": "refs/heads/" + n.Source, "targetRefName": "refs/heads/" + n.Target,
		"title": n.Title, "description": connectors.Truncate(n.Description, maxDescription), "isDraft": n.Draft}
	if len(n.Labels) > 0 {
		labels := make([]map[string]string, 0, len(n.Labels))
		for _, l := range n.Labels {
			labels = append(labels, map[string]string{"name": l})
		}
		body["labels"] = labels
	}
	var out pr
	if err := c.client.Do(ctx, http.MethodPost, at, APIVersion, nil, body, &out); err != nil {
		return connectors.PullRequest{}, err
	}
	return out.convert(repo.WebURL), nil
}

func (p *pullRequests) get(ctx context.Context, id string) (pr, Repository, error) {
	c := (*Connector)(p)
	if _, err := strconv.Atoi(id); err != nil {
		return pr{}, Repository{}, fmt.Errorf("%w: pull request %q", connectors.ErrNotFound, id)
	}
	at, repo, err := c.api(ctx, "pullrequests", id)
	if err != nil {
		return pr{}, Repository{}, err
	}
	var out pr
	err = c.client.Do(ctx, http.MethodGet, at, APIVersion, nil, nil, &out)
	return out, repo, err
}

func (p *pullRequests) Get(ctx context.Context, id string) (connectors.PullRequest, error) {
	out, repo, err := p.get(ctx, id)
	if err != nil {
		return connectors.PullRequest{}, err
	}
	return out.convert(repo.WebURL), nil
}

// patch updates a pull request.
func (p *pullRequests) patch(ctx context.Context, id string, body map[string]any) (pr, Repository, error) {
	c := (*Connector)(p)
	if _, err := strconv.Atoi(id); err != nil {
		return pr{}, Repository{}, fmt.Errorf("%w: pull request %q", connectors.ErrNotFound, id)
	}
	at, repo, err := c.api(ctx, "pullrequests", id)
	if err != nil {
		return pr{}, Repository{}, err
	}
	var out pr
	err = c.client.Do(ctx, http.MethodPatch, at, APIVersion, nil, body, &out)
	return out, repo, err
}

func (p *pullRequests) Update(ctx context.Context, id string, u connectors.PullRequestUpdate) (connectors.PullRequest, error) {
	body := map[string]any{}
	if u.Title != nil {
		body["title"] = *u.Title
	}
	if u.Description != nil {
		body["description"] = connectors.Truncate(*u.Description, maxDescription)
	}
	out, repo, err := p.patch(ctx, id, body)
	if err != nil {
		return connectors.PullRequest{}, err
	}
	return out.convert(repo.WebURL), nil
}

func (p *pullRequests) List(ctx context.Context, prefix string) ([]connectors.PullRequest, error) {
	c := (*Connector)(p)
	at, repo, err := c.api(ctx, "pullrequests")
	if err != nil {
		return nil, err
	}
	var out []connectors.PullRequest
	const page = 100
	for skip := 0; ; skip += page {
		var list struct {
			Value []pr `json:"value"`
		}
		q := url.Values{"searchCriteria.status": {"active"}, "$top": {strconv.Itoa(page)}, "$skip": {strconv.Itoa(skip)}}
		if err := c.client.Do(ctx, http.MethodGet, at, APIVersion, q, nil, &list); err != nil {
			return nil, err
		}
		for _, x := range list.Value {
			if strings.HasPrefix(strings.TrimPrefix(x.SourceRefName, "refs/heads/"), prefix) {
				out = append(out, x.convert(repo.WebURL))
			}
		}
		if len(list.Value) < page {
			return out, nil
		}
	}
}

// strategies are Azure DevOps's merge strategies for Quai's merge methods.
var strategies = map[connectors.MergeMethod]string{
	connectors.MergeCommit: "noFastForward",
	connectors.Squash:      "squash",
	connectors.Rebase:      "rebase",
}

func completion(o connectors.MergeOptions) (map[string]any, error) {
	strategy, ok := strategies[o.Method]
	if !ok {
		return nil, fmt.Errorf("%w: merge method %q", connectors.ErrUnsupported, o.Method)
	}
	return map[string]any{"mergeStrategy": strategy, "deleteSourceBranch": o.DeleteBranch,
		"mergeCommitMessage": strings.TrimSpace(o.Message), "bypassPolicy": false, "transitionWorkItems": false}, nil
}

// CompletionTimeout bounds the wait for Azure DevOps to complete a pull request it accepted to.
var CompletionTimeout = 2 * time.Minute

// Merge completes the pull request now, if its blocking policies are approved (ErrBlocked
// otherwise), and waits for Azure DevOps to finish, which it does in the background.
func (p *pullRequests) Merge(ctx context.Context, id string, o connectors.MergeOptions) (connectors.PullRequest, error) {
	opts, err := completion(o)
	if err != nil {
		return connectors.PullRequest{}, err
	}
	// An evaluation follows a status within a second or so: one still queued or running gets a
	// few seconds to settle; a rejected one blocks at once.
	var pending, rejected []string
	for attempt := 0; ; attempt++ {
		if pending, rejected, err = (*statuses)(p).blocking(ctx, id); err != nil {
			return connectors.PullRequest{}, err
		}
		if len(pending) == 0 || len(rejected) > 0 || attempt == 5 {
			break
		}
		if err := sleep(ctx, time.Second); err != nil {
			return connectors.PullRequest{}, err
		}
	}
	if blocking := append(rejected, pending...); len(blocking) > 0 {
		return connectors.PullRequest{}, fmt.Errorf("%w: %s", connectors.ErrBlocked, strings.Join(blocking, ", "))
	}
	body := map[string]any{"status": "completed", "completionOptions": opts}
	if o.Head != "" {
		body["lastMergeSourceCommit"] = map[string]string{"commitId": o.Head}
	}
	// Azure DevOps computes a pull request's merge in the background, and drops a completion asked
	// for before it is done: wait for the merge, ask, and ask again if the completion was not
	// queued.
	deadline := time.Now().Add(CompletionTimeout)
	var asked time.Time
	for {
		out, repo, err := p.get(ctx, id)
		if err != nil {
			return connectors.PullRequest{}, err
		}
		switch {
		case out.Status == "completed":
			return out.convert(repo.WebURL), nil
		case out.Status == "abandoned":
			return connectors.PullRequest{}, fmt.Errorf("azure devops: pull request %s was abandoned", id)
		case out.MergeStatus == "conflicts" || out.MergeStatus == "rejectedByPolicy" || out.MergeStatus == "failure":
			return connectors.PullRequest{}, fmt.Errorf("%w: the merge %s", connectors.ErrBlocked, out.MergeStatus)
		case time.Now().After(deadline):
			return connectors.PullRequest{}, fmt.Errorf("azure devops: pull request %s not completed after %s (merge status %s)", id, CompletionTimeout, out.MergeStatus)
		case out.MergeStatus == "succeeded" && out.CompletionQueueTime == "" && time.Since(asked) > 10*time.Second:
			if _, _, err := p.patch(ctx, id, body); err != nil {
				var apiErr *Error
				if errors.As(err, &apiErr) && (apiErr.Status == http.StatusBadRequest || apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusConflict) {
					return connectors.PullRequest{}, fmt.Errorf("%w: %s", connectors.ErrBlocked, apiErr.Message)
				}
				return connectors.PullRequest{}, err
			}
			asked = time.Now()
		}
		if err := sleep(ctx, time.Second); err != nil {
			return connectors.PullRequest{}, err
		}
	}
}

// AutoComplete sets auto-complete, as Quai's identity, with the completion options: Azure
// DevOps completes the pull request once every policy passes.
func (p *pullRequests) AutoComplete(ctx context.Context, id string, o connectors.MergeOptions) error {
	opts, err := completion(o)
	if err != nil {
		return err
	}
	in, err := (*Connector)(p).client.Detect(ctx)
	if err != nil {
		return err
	}
	_, _, err = p.patch(ctx, id, map[string]any{"autoCompleteSetBy": map[string]string{"id": in.User.ID}, "completionOptions": opts})
	return err
}

func (p *pullRequests) Abandon(ctx context.Context, id string) error {
	_, _, err := p.patch(ctx, id, map[string]any{"status": "abandoned"})
	return err
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

type statuses Connector

// statusContext splits a status name into Azure DevOps's genre and name: quai/approval is genre
// quai, name approval.
func statusContext(name string) map[string]string {
	if i := strings.LastIndex(name, "/"); i > 0 {
		return map[string]string{"genre": name[:i], "name": name[i+1:]}
	}
	return map[string]string{"name": name}
}

var toState = map[connectors.StatusState]string{
	connectors.StatusPending:       "pending",
	connectors.StatusSuccess:       "succeeded",
	connectors.StatusFailure:       "failed",
	connectors.StatusError:         "error",
	connectors.StatusNotApplicable: "notApplicable",
}

func fromState(s string) connectors.StatusState {
	for k, v := range toState {
		if v == s {
			return k
		}
	}
	return connectors.StatusState(s)
}

// latestIteration returns the pull request's latest iteration: a push makes a new one.
func (s *statuses) latestIteration(ctx context.Context, id string) (int, error) {
	c := (*Connector)(s)
	at, _, err := c.api(ctx, "pullRequests", id, "iterations")
	if err != nil {
		return 0, err
	}
	var list struct {
		Value []struct {
			ID int `json:"id"`
		} `json:"value"`
	}
	if err := c.client.Do(ctx, http.MethodGet, at, APIVersion, nil, nil, &list); err != nil {
		return 0, err
	}
	latest := 0
	for _, it := range list.Value {
		latest = max(latest, it.ID)
	}
	return latest, nil
}

// Set publishes a status: on the pull request's latest iteration, or else on the commit.
func (s *statuses) Set(ctx context.Context, t connectors.StatusTarget, st connectors.Status) error {
	c := (*Connector)(s)
	state, ok := toState[st.State]
	if !ok {
		return fmt.Errorf("%w: status state %q", connectors.ErrUnsupported, st.State)
	}
	body := map[string]any{"state": state, "description": st.Description, "context": statusContext(st.Name)}
	if st.URL != "" {
		body["targetUrl"] = st.URL
	}
	if t.PullRequest == "" {
		at, _, err := c.api(ctx, "commits", t.Commit, "statuses")
		if err != nil {
			return err
		}
		return c.client.Do(ctx, http.MethodPost, at, APIVersion, nil, body, nil)
	}
	iteration, err := s.latestIteration(ctx, t.PullRequest)
	if err != nil {
		return err
	}
	body["iterationId"] = iteration
	at, _, err := c.api(ctx, "pullRequests", t.PullRequest, "statuses")
	if err != nil {
		return err
	}
	return c.client.Do(ctx, http.MethodPost, at, previewVersion, nil, body, nil)
}

type status struct {
	ID          int    `json:"id"`
	State       string `json:"state"`
	Description string `json:"description"`
	TargetURL   string `json:"targetUrl"`
	IterationID int    `json:"iterationId"`
	Context     struct {
		Genre string `json:"genre"`
		Name  string `json:"name"`
	} `json:"context"`
}

func (st status) name() string {
	if st.Context.Genre == "" {
		return st.Context.Name
	}
	return st.Context.Genre + "/" + st.Context.Name
}

// List returns the latest status of each name on the pull request's latest iteration, then its
// policy evaluations as policy:<name> (build validation, required reviewers…), which
// waitForChecks names; on a commit, its statuses.
func (s *statuses) List(ctx context.Context, t connectors.StatusTarget) ([]connectors.Status, error) {
	c := (*Connector)(s)
	var list struct {
		Value []status `json:"value"`
	}
	if t.PullRequest == "" {
		at, _, err := c.api(ctx, "commits", t.Commit, "statuses")
		if err != nil {
			return nil, err
		}
		if err := c.client.Do(ctx, http.MethodGet, at, APIVersion, url.Values{"latestOnly": {"true"}}, nil, &list); err != nil {
			return nil, err
		}
		return latest(list.Value, -1), nil
	}
	iteration, err := s.latestIteration(ctx, t.PullRequest)
	if err != nil {
		return nil, err
	}
	at, _, err := c.api(ctx, "pullRequests", t.PullRequest, "statuses")
	if err != nil {
		return nil, err
	}
	if err := c.client.Do(ctx, http.MethodGet, at, previewVersion, nil, nil, &list); err != nil {
		return nil, err
	}
	out := latest(list.Value, iteration)
	evals, err := s.evaluations(ctx, t.PullRequest)
	if err != nil {
		return nil, err
	}
	for _, e := range evals {
		out = append(out, connectors.Status{Name: "policy:" + e.name, State: e.state()})
	}
	return out, nil
}

// latest keeps the newest status of each name, on iteration (any when negative).
func latest(all []status, iteration int) []connectors.Status {
	slices.SortFunc(all, func(a, b status) int { return b.ID - a.ID })
	seen := map[string]bool{}
	var out []connectors.Status
	for _, st := range all {
		if iteration >= 0 && st.IterationID != 0 && st.IterationID != iteration {
			continue
		}
		if seen[st.name()] {
			continue
		}
		seen[st.name()] = true
		out = append(out, connectors.Status{Name: st.name(), State: fromState(st.State), Description: st.Description, URL: st.TargetURL})
	}
	return out
}

// evaluation is a branch policy's evaluation on a pull request.
type evaluation struct {
	name     string
	status   string // queued, running, approved, rejected, notApplicable, broken
	blocking bool
}

func (e evaluation) state() connectors.StatusState {
	switch e.status {
	case "approved", "notApplicable":
		return connectors.StatusSuccess
	case "rejected", "broken":
		return connectors.StatusFailure
	}
	return connectors.StatusPending
}

// evaluations reads the pull request's policy evaluations.
func (s *statuses) evaluations(ctx context.Context, id string) ([]evaluation, error) {
	c := (*Connector)(s)
	repo, err := c.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	var list struct {
		Value []struct {
			Status        string `json:"status"`
			Configuration struct {
				IsEnabled  bool `json:"isEnabled"`
				IsBlocking bool `json:"isBlocking"`
				Type       struct {
					DisplayName string `json:"displayName"`
				} `json:"type"`
				Settings map[string]any `json:"settings"`
			} `json:"configuration"`
		} `json:"value"`
	}
	artifact := "vstfs:///CodeReview/CodeReviewId/" + repo.ProjectID + "/" + id
	if err := c.client.Do(ctx, http.MethodGet, path(repo.ProjectID, "_apis", "policy", "evaluations"), previewVersion,
		url.Values{"artifactId": {artifact}}, nil, &list); err != nil {
		return nil, err
	}
	var out []evaluation
	for _, v := range list.Value {
		if !v.Configuration.IsEnabled {
			continue
		}
		name := v.Configuration.Type.DisplayName
		if dn, ok := v.Configuration.Settings["displayName"].(string); ok && dn != "" {
			name = dn
		} else if sn, ok := v.Configuration.Settings["statusName"].(string); ok && sn != "" {
			if genre, ok := v.Configuration.Settings["statusGenre"].(string); ok && genre != "" {
				sn = genre + "/" + sn
			}
			name += ": " + sn
		}
		out = append(out, evaluation{name: name, status: v.Status, blocking: v.Configuration.IsBlocking})
	}
	return out, nil
}

// blocking returns the blocking policies not approved yet: pending (queued or running) and
// rejected.
func (s *statuses) blocking(ctx context.Context, id string) (pending, rejected []string, err error) {
	evals, err := s.evaluations(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range evals {
		if !e.blocking {
			continue
		}
		switch e.state() {
		case connectors.StatusPending:
			pending = append(pending, e.name+" ("+e.status+")")
		case connectors.StatusFailure:
			rejected = append(rejected, e.name+" ("+e.status+")")
		}
	}
	return pending, rejected, nil
}
