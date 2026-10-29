package azuredevops

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/secrets"
)

// SecretHeader carries the subscription's secret on each delivery: Azure DevOps does not sign
// service hook payloads, so Quai checks a secret of its own, sent over TLS, and re-reads from
// the API whatever an event names.
const SecretHeader = "X-Quai-Webhook-Secret" // #nosec G101 -- a header name, not a credential

// hookEvents are the service hook events of the repository Quai subscribes to.
var hookEvents = []string{"git.push", "git.pullrequest.created", "git.pullrequest.updated", "git.pullrequest.merged"}

// runEvent is the project's pipeline runs changing state: the runner adapter's signal
// (docs/design/06-runs-status-and-drift.md#adapters). Quai reads each run again, and keeps
// those of its repository.
const runEvent = "ms.vss-pipelines.run-state-changed-event"

type webhooks Connector

type subscription struct {
	ID               string            `json:"id,omitempty"`
	PublisherID      string            `json:"publisherId"`
	EventType        string            `json:"eventType"`
	ResourceVersion  string            `json:"resourceVersion"`
	ConsumerID       string            `json:"consumerId"`
	ConsumerActionID string            `json:"consumerActionId"`
	PublisherInputs  map[string]string `json:"publisherInputs"`
	ConsumerInputs   map[string]string `json:"consumerInputs"`
}

// Subscriptions returns the IDs of the repository's service hook subscriptions to url.
func (w *webhooks) subscriptions(ctx context.Context, url string) (map[string]string, error) {
	c := (*Connector)(w)
	repo, err := c.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	var list struct {
		Value []subscription `json:"value"`
	}
	q := map[string][]string{"consumerId": {"webHooks"}, "consumerActionId": {"httpRequest"}}
	if err := c.client.Do(ctx, http.MethodGet, "/_apis/hooks/subscriptions", APIVersion, q, nil, &list); err != nil {
		return nil, err
	}
	out := map[string]string{} // by event type
	for _, s := range list.Value {
		ours := s.PublisherID == "tfs" && strings.EqualFold(s.PublisherInputs["repository"], repo.ID) ||
			s.PublisherID == "pipelines" && strings.EqualFold(s.PublisherInputs["projectId"], repo.ProjectID)
		if s.ConsumerInputs["url"] == url && ours {
			out[s.EventType] = s.ID
		}
	}
	return out, nil
}

// Subscribe creates the service hook subscriptions (Web Hooks consumer) of the repository's
// pushes and pull requests, and of the project's pipeline runs, to url, with the secret in
// SecretHeader, or updates those already delivering there. Creating them needs "Edit
// subscriptions" on the project.
func (w *webhooks) Subscribe(ctx context.Context, url string, secret secrets.Secret) error {
	c := (*Connector)(w)
	if strings.ContainsAny(secret.Reveal(), "\r\n") || secret.Reveal() == "" {
		return fmt.Errorf("azure devops: the webhook secret must be one non-empty line")
	}
	repo, err := c.Resolve(ctx)
	if err != nil {
		return err
	}
	existing, err := w.subscriptions(ctx, url)
	if err != nil {
		return err
	}
	for _, event := range append(slices.Clone(hookEvents), runEvent) {
		s := subscription{PublisherID: "tfs", EventType: event, ResourceVersion: "1.0", ConsumerID: "webHooks", ConsumerActionID: "httpRequest",
			PublisherInputs: map[string]string{"projectId": repo.ProjectID, "repository": repo.ID, "branch": ""},
			ConsumerInputs: map[string]string{"url": url, "httpHeaders": SecretHeader + ": " + secret.Reveal(),
				"resourceDetailsToSend": "all", "messagesToSend": "none", "detailedMessagesToSend": "none"}}
		switch event {
		case "git.pullrequest.merged":
			s.PublisherInputs["mergeResult"] = ""
		case runEvent:
			s.PublisherID, s.ResourceVersion = "pipelines", "5.1-preview.1"
			s.PublisherInputs = map[string]string{"projectId": repo.ProjectID, "pipelineId": "", "runStateId": "", "runResultId": ""}
		}
		if id, ok := existing[event]; ok {
			s.ID = id
			err = c.client.Do(ctx, http.MethodPut, "/_apis/hooks/subscriptions/"+id, APIVersion, nil, s, nil)
		} else {
			err = c.client.Do(ctx, http.MethodPost, "/_apis/hooks/subscriptions", APIVersion, nil, s, nil)
		}
		if err != nil {
			return fmt.Errorf("service hook %s: %w", event, err)
		}
	}
	return nil
}

// Unsubscribe deletes the repository's subscriptions to url.
func (w *webhooks) Unsubscribe(ctx context.Context, url string) error {
	c := (*Connector)(w)
	existing, err := w.subscriptions(ctx, url)
	if err != nil {
		return err
	}
	for _, id := range existing {
		if err := c.client.Do(ctx, http.MethodDelete, "/_apis/hooks/subscriptions/"+id, APIVersion, nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// event is a service hook payload.
type event struct {
	ID        string          `json:"id"`
	EventType string          `json:"eventType"`
	Resource  json.RawMessage `json:"resource"`
}

type pushResource struct {
	Repository struct {
		ID string `json:"id"`
	} `json:"repository"`
	RefUpdates []struct {
		Name        string `json:"name"`
		OldObjectID string `json:"oldObjectId"`
		NewObjectID string `json:"newObjectId"`
	} `json:"refUpdates"`
}

// Parse checks the delivery's secret (SecretHeader, compared in constant time) and turns it into
// events: a push per ref update, and pull requests created, updated (new commits, edits, votes),
// completed or abandoned. Other repositories' events, and other event types, are dropped.
func (w *webhooks) Parse(header http.Header, body []byte, secret secrets.Secret) (connectors.Delivery, error) {
	c := (*Connector)(w)
	got := header.Get(SecretHeader)
	if secret.Reveal() == "" || subtle.ConstantTimeCompare([]byte(got), []byte(secret.Reveal())) != 1 {
		return connectors.Delivery{}, connectors.ErrUnauthenticated
	}
	var e event
	if err := json.Unmarshal(body, &e); err != nil {
		return connectors.Delivery{}, fmt.Errorf("azure devops service hook: %w", err)
	}
	d := connectors.Delivery{ID: e.ID}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, err := c.Resolve(ctx)
	if err != nil {
		return d, err
	}
	switch {
	case e.EventType == runEvent:
		var r struct {
			Run struct {
				ID       int `json:"id"`
				Pipeline struct {
					Name string `json:"name"`
				} `json:"pipeline"`
			} `json:"run"`
		}
		if err := json.Unmarshal(e.Resource, &r); err != nil || r.Run.ID == 0 {
			return d, fmt.Errorf("azure devops %s: no run (%v)", e.EventType, err)
		}
		d.Events = append(d.Events, connectors.PipelineRunChanged{Run: connectors.Run{ID: strconv.Itoa(r.Run.ID), Pipeline: r.Run.Pipeline.Name}})
	case e.EventType == "git.push":
		var r pushResource
		if err := json.Unmarshal(e.Resource, &r); err != nil {
			return d, fmt.Errorf("azure devops git.push: %w", err)
		}
		if !strings.EqualFold(r.Repository.ID, repo.ID) {
			return d, nil
		}
		for _, u := range r.RefUpdates {
			d.Events = append(d.Events, connectors.Push{Ref: u.Name, Before: u.OldObjectID, After: u.NewObjectID})
		}
	case strings.HasPrefix(e.EventType, "git.pullrequest."):
		var r pr
		var scope struct {
			Repository struct {
				ID string `json:"id"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(e.Resource, &r); err != nil {
			return d, fmt.Errorf("azure devops %s: %w", e.EventType, err)
		}
		_ = json.Unmarshal(e.Resource, &scope)
		if !strings.EqualFold(scope.Repository.ID, repo.ID) {
			return d, nil
		}
		action := connectors.PRUpdated
		switch {
		case r.Status == "completed":
			action = connectors.PRCompleted
		case r.Status == "abandoned":
			action = connectors.PRClosed
		case e.EventType == "git.pullrequest.created":
			action = connectors.PROpened
		}
		x := r.convert(repo.WebURL)
		d.Events = append(d.Events, connectors.PullRequestChanged{ID: strconv.Itoa(r.ID), Action: action, Head: x.Head, Source: x.Source, Target: x.Target})
	}
	return d, nil
}
