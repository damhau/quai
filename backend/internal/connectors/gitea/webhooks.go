package gitea

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	sdk "code.gitea.io/sdk/gitea"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/secrets"
)

type webhooks Connector

// hookEvents are the events Quai subscribes to.
var hookEvents = []string{"push", "pull_request", "pull_request_sync", "status"}

// Subscribe installs the webhook to url, or updates the one already delivering there.
func (w *webhooks) Subscribe(ctx context.Context, url string, secret secrets.Secret) error {
	c := (*Connector)(w)
	cl, err := c.client(ctx)
	if err != nil {
		return err
	}
	config := map[string]string{"url": url, "content_type": "json", "secret": secret.Reveal()}
	hooks, resp, err := cl.ListRepoHooks(c.owner, c.repo, sdk.ListHooksOptions{})
	if err := check(resp, err); err != nil {
		return err
	}
	active := true
	for _, h := range hooks {
		if h.Config["url"] == url {
			resp, err := cl.EditRepoHook(c.owner, c.repo, h.ID, sdk.EditHookOption{Config: config, Events: hookEvents, Active: &active, BranchFilter: "*"})
			return check(resp, err)
		}
	}
	_, resp, err = cl.CreateRepoHook(c.owner, c.repo, sdk.CreateHookOption{Type: sdk.HookTypeGitea, Config: config, Events: hookEvents, Active: true, BranchFilter: "*"})
	return check(resp, err)
}

// payload holds the fields Quai reads from Gitea's webhook payloads.
type payload struct {
	Repository *struct {
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
	Action      string `json:"action"`
	Number      int64  `json:"number"`
	PullRequest *struct {
		Merged bool `json:"merged"`
		Head   struct {
			Ref string `json:"ref"`
			Sha string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	// status
	SHA         string `json:"sha"`
	Context     string `json:"context"`
	State       string `json:"state"`
	TargetURL   string `json:"target_url"`
	Description string `json:"description"`
}

// Parse verifies the delivery's signature (X-Gitea-Signature, the HMAC-SHA256 of the body with
// the secret) and turns it into events.
func (w *webhooks) Parse(header http.Header, body []byte, secret secrets.Secret) (connectors.Delivery, error) {
	c := (*Connector)(w)
	mac := hmac.New(sha256.New, []byte(secret.Reveal()))
	mac.Write(body)
	got, err := hex.DecodeString(header.Get("X-Gitea-Signature"))
	if err != nil || secret.Reveal() == "" || !hmac.Equal(got, mac.Sum(nil)) {
		return connectors.Delivery{}, connectors.ErrUnauthenticated
	}
	d := connectors.Delivery{ID: header.Get("X-Gitea-Delivery")}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return d, fmt.Errorf("gitea webhook: %w", err)
	}
	if p.Repository == nil || !strings.EqualFold(p.Repository.FullName, c.owner+"/"+c.repo) {
		return d, nil // another repository's
	}
	switch header.Get("X-Gitea-Event") {
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
		case "synchronized", "edited":
			action = connectors.PRUpdated
		case "closed":
			action = connectors.PRClosed
			if p.PullRequest.Merged {
				action = connectors.PRCompleted
			}
		default:
			return d, nil // labels, assignees, reviews: nothing Quai acts on
		}
		d.Events = append(d.Events, connectors.PullRequestChanged{ID: strconv.FormatInt(p.Number, 10), Action: action,
			Head: p.PullRequest.Head.Sha, Source: p.PullRequest.Head.Ref, Target: p.PullRequest.Base.Ref})
	case "status":
		state := connectors.StatusState(p.State)
		if p.State == string(sdk.StatusWarning) {
			state = connectors.StatusFailure
		}
		d.Events = append(d.Events, connectors.StatusChanged{Commit: p.SHA,
			Status: connectors.Status{Name: p.Context, State: state, Description: p.Description, URL: p.TargetURL}})
	}
	return d, nil
}
