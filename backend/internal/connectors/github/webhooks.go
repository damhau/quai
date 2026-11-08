package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v88/github"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/secrets"
)

type webhooks Connector

// hookEvents are the events Quai subscribes to.
var hookEvents = []string{"push", "pull_request", "check_run", "status", "merge_group", "branch_protection_rule", "repository_ruleset"}

// Subscribe installs a repository webhook delivering to url, or updates the one already delivering
// there. The app needs the Webhooks (read and write) repository permission; without it, GitHub
// refuses, and the error says what to add or set up by hand. An app's own webhook is one URL for
// every repository it is installed on, so Quai does not use it (open question 15).
func (w *webhooks) Subscribe(ctx context.Context, url string, secret secrets.Secret) error {
	c := (*Connector)(w)
	if secret.Reveal() == "" {
		return fmt.Errorf("github: a webhook needs a secret")
	}
	hook := &gh.Hook{Active: gh.Ptr(true), Events: hookEvents,
		Config: &gh.HookConfig{URL: gh.Ptr(url), ContentType: gh.Ptr("json"), InsecureSSL: gh.Ptr("0"), Secret: gh.Ptr(secret.Reveal())}}
	opts := &gh.ListOptions{PerPage: 100}
	for {
		hooks, resp, err := c.client.Repositories.ListHooks(ctx, c.owner, c.repo, opts)
		if err != nil {
			return hookError(resp, err)
		}
		for _, h := range hooks {
			if h.GetConfig().GetURL() == url {
				_, resp, err := c.client.Repositories.EditHook(ctx, c.owner, c.repo, h.GetID(), hook)
				return hookError(resp, err)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	_, resp, err := c.client.Repositories.CreateHook(ctx, c.owner, c.repo, hook)
	return hookError(resp, err)
}

// hookError names the permission GitHub refused for.
func hookError(resp *gh.Response, err error) error {
	if err == nil {
		return nil
	}
	if resp != nil && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
		return fmt.Errorf("the GitHub App needs the Webhooks (read and write) repository permission to install the webhook: %w", check(resp, err))
	}
	return check(resp, err)
}

// payload holds the fields Quai reads from GitHub's webhook payloads.
type payload struct {
	Action     string `json:"action"`
	Repository *struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	} `json:"repository"`
	// push
	Ref     string `json:"ref"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Commits []struct {
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"commits"`
	// pull_request
	PullRequest *struct {
		Number int  `json:"number"`
		Merged bool `json:"merged"`
		Head   struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	// check_run
	CheckRun *struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HeadSHA    string `json:"head_sha"`
		DetailsURL string `json:"details_url"`
		Output     struct {
			Title string `json:"title"`
		} `json:"output"`
		App *struct {
			ID int64 `json:"id"`
		} `json:"app"`
		PullRequests []struct {
			Number int `json:"number"`
			Base   struct {
				Repo struct {
					ID int64 `json:"id"`
				} `json:"repo"`
			} `json:"base"`
		} `json:"pull_requests"`
	} `json:"check_run"`
	// status
	SHA         string `json:"sha"`
	Context     string `json:"context"`
	State       string `json:"state"`
	TargetURL   string `json:"target_url"`
	Description string `json:"description"`
	// branch_protection_rule
	Rule *struct {
		Name string `json:"name"`
	} `json:"rule"`
	// merge_group
	MergeGroup *struct {
		HeadRef string `json:"head_ref"`
		BaseRef string `json:"base_ref"`
	} `json:"merge_group"`
}

// verify checks a delivery's X-Hub-Signature-256: sha256=, then the hex HMAC-SHA256 of the body
// with the secret, compared in constant time. GitHub's older SHA-1 signature is not accepted.
func verify(signature string, body []byte, secret secrets.Secret) error {
	digest, ok := strings.CutPrefix(signature, "sha256=")
	got, err := hex.DecodeString(digest)
	if !ok || err != nil || secret.Reveal() == "" {
		return connectors.ErrUnauthenticated
	}
	mac := hmac.New(sha256.New, []byte(secret.Reveal()))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return connectors.ErrUnauthenticated
	}
	return nil
}

// Parse verifies the delivery's signature and turns it into events. It reads the deliveries of a
// repository webhook and of an app's webhook alike, in JSON or, for a webhook set up by hand, as a
// form. Events of another repository, and Quai's own check runs, are dropped: Quai knows what it
// published, and re-reading the pull request on each would publish again.
func (w *webhooks) Parse(header http.Header, body []byte, secret secrets.Secret) (connectors.Delivery, error) {
	c := (*Connector)(w)
	if err := verify(header.Get("X-Hub-Signature-256"), body, secret); err != nil {
		return connectors.Delivery{}, err
	}
	d := connectors.Delivery{ID: header.Get("X-GitHub-Delivery")}
	raw := body
	if media, _, _ := mime.ParseMediaType(header.Get("Content-Type")); media == "application/x-www-form-urlencoded" {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return d, fmt.Errorf("github webhook: %w", err)
		}
		raw = []byte(form.Get("payload"))
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return d, fmt.Errorf("github webhook: %w", err)
	}
	event := header.Get("X-GitHub-Event")
	ours := p.Repository != nil && strings.EqualFold(p.Repository.FullName, c.owner+"/"+c.repo)
	// An organization's ruleset applies to the repository too, and its events name no repository.
	if !ours && (event != "repository_ruleset" || p.Repository != nil) {
		return d, nil
	}
	switch event {
	case "push":
		push := connectors.Push{Ref: p.Ref, Before: p.Before, After: p.After}
		seen := map[string]bool{}
		for _, cm := range p.Commits {
			for _, list := range [][]string{cm.Added, cm.Removed, cm.Modified} {
				for _, f := range list {
					if !seen[f] {
						seen[f] = true
						push.Paths = append(push.Paths, f)
					}
				}
			}
		}
		d.Events = append(d.Events, push)
	case "pull_request":
		if p.PullRequest == nil {
			break
		}
		var action connectors.PullRequestAction
		switch p.Action {
		case "opened", "reopened":
			action = connectors.PROpened
		case "synchronize", "edited", "ready_for_review", "converted_to_draft", "enqueued", "dequeued", "auto_merge_enabled", "auto_merge_disabled":
			action = connectors.PRUpdated
		case "closed":
			action = connectors.PRClosed
			if p.PullRequest.Merged {
				action = connectors.PRCompleted
			}
		default:
			return d, nil // labels, assignees, reviews: nothing Quai acts on
		}
		d.Events = append(d.Events, connectors.PullRequestChanged{ID: strconv.Itoa(p.PullRequest.Number), Action: action,
			Head: p.PullRequest.Head.SHA, Source: p.PullRequest.Head.Ref, Target: p.PullRequest.Base.Ref})
	case "check_run":
		run := p.CheckRun
		if run == nil || run.App != nil && c.cfg.AppID != 0 && run.App.ID == c.cfg.AppID {
			break
		}
		st := connectors.Status{Name: run.Name, State: runState(run.Status, run.Conclusion), Description: run.Output.Title, URL: run.DetailsURL}
		changed := connectors.StatusChanged{Commit: run.HeadSHA, Status: st}
		listed := false
		for _, pr := range run.PullRequests {
			if pr.Base.Repo.ID == p.Repository.ID { // a fork's pull request is listed with its own base
				changed.PullRequest, listed = strconv.Itoa(pr.Number), true
				d.Events = append(d.Events, changed)
			}
		}
		if !listed {
			d.Events = append(d.Events, changed)
		}
	case "status":
		state := connectors.StatusState(p.State)
		if state != connectors.StatusPending && state != connectors.StatusSuccess && state != connectors.StatusFailure {
			state = connectors.StatusError
		}
		d.Events = append(d.Events, connectors.StatusChanged{Commit: p.SHA,
			Status: connectors.Status{Name: p.Context, State: state, Description: p.Description, URL: p.TargetURL}})
	case "branch_protection_rule":
		if p.Rule != nil {
			d.Events = append(d.Events, connectors.PolicyChanged{Branch: p.Rule.Name})
		}
	case "repository_ruleset":
		d.Events = append(d.Events, connectors.PolicyChanged{})
	case "merge_group":
		// The group's branch names the pull request it was built for:
		// refs/heads/gh-readonly-queue/<base>/pr-<number>-<the base's commit>.
		if p.MergeGroup == nil {
			break
		}
		i := strings.LastIndex(p.MergeGroup.HeadRef, "/pr-")
		if !strings.HasPrefix(p.MergeGroup.HeadRef, "refs/heads/gh-readonly-queue/") || i < 0 {
			break
		}
		number, _, _ := strings.Cut(p.MergeGroup.HeadRef[i+len("/pr-"):], "-")
		if n, err := strconv.Atoi(number); err == nil && n > 0 {
			d.Events = append(d.Events, connectors.PullRequestChanged{ID: strconv.Itoa(n), Action: connectors.PRUpdated,
				Target: strings.TrimPrefix(p.MergeGroup.BaseRef, "refs/heads/")})
		}
	}
	return d, nil
}
