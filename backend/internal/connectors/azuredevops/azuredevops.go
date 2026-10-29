// Package azuredevops is the connector for Azure DevOps Server (on premises, first) and Azure
// DevOps Services (docs/design/12-git-connectors.md#azure-devops-first-server-then-services).
// One connector covers both editions: it is written against REST API 7.0, which Server 2022 and
// Services serve, through Client, which pins the version of every call. A personal access token
// (spec.credentials.ref) authenticates the API and git.
package azuredevops

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/secrets"
)

// Connector is the Azure DevOps connector for one repository.
type Connector struct {
	cfg    connectors.Config
	loc    Location
	client *Client

	mu       sync.Mutex
	resolved *Repository
	instance Instance
}

// Repository is a repository as Azure DevOps identifies it: names can change, IDs do not.
type Repository struct {
	ID            string
	Name          string
	ProjectID     string
	ProjectName   string
	DefaultBranch string // refs/heads/main
	WebURL        string
}

// New returns the connector for the repository at cfg.URL. It makes no call until it is used.
func New(cfg connectors.Config) (connectors.Connector, error) {
	loc, err := ParseURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	if cfg.HTTP == nil {
		cfg.HTTP = connectors.NewHTTPClient(http.DefaultTransport, cfg.Log)
	}
	if cfg.Credentials == nil {
		cfg.Credentials = func(context.Context) (secrets.Secret, bool, error) { return secrets.Secret{}, false, nil }
	}
	return &Connector{cfg: cfg, loc: loc, client: NewClient(loc.Collection, cfg.HTTP, cfg.Credentials)}, nil
}

// Client returns the API client of the repository's collection or organization.
func (c *Connector) Client() *Client { return c.client }

// Location returns where the repository lives.
func (c *Connector) Location() Location { return c.loc }

// Resolve detects the instance and finds the repository's IDs, once.
func (c *Connector) Resolve(ctx context.Context) (Repository, error) {
	c.mu.Lock()
	if c.resolved != nil {
		defer c.mu.Unlock()
		return *c.resolved, nil
	}
	c.mu.Unlock()
	in, err := c.client.Detect(ctx)
	if err != nil {
		return Repository{}, err
	}
	var repo struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		DefaultBranch string `json:"defaultBranch"`
		WebURL        string `json:"webUrl"`
		Project       struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
	}
	if err := c.client.Do(ctx, http.MethodGet, path(c.loc.Project, "_apis", "git", "repositories", c.loc.Repository), APIVersion, nil, nil, &repo); err != nil {
		return Repository{}, fmt.Errorf("repository %s/%s: %w", c.loc.Project, c.loc.Repository, err)
	}
	r := Repository{ID: repo.ID, Name: repo.Name, ProjectID: repo.Project.ID, ProjectName: repo.Project.Name, DefaultBranch: repo.DefaultBranch, WebURL: repo.WebURL}
	c.mu.Lock()
	c.resolved, c.instance = &r, in
	c.mu.Unlock()
	return r, nil
}

func (c *Connector) Info() connectors.Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	edition := "Server"
	if c.loc.Services {
		edition = "Services"
	}
	if c.resolved != nil && c.instance.MaxVersion != "" {
		edition += " (API " + c.instance.MaxVersion + ")"
	}
	return connectors.Info{Platform: "azure-devops", Edition: edition, APIVersion: APIVersion,
		Capabilities: []connectors.Capability{connectors.CapPullRequests, connectors.CapAutoComplete, connectors.CapEnforcedStatus, connectors.CapPipelines},
		MergeMethods: []connectors.MergeMethod{connectors.MergeCommit, connectors.Squash, connectors.Rebase}}
}

// GitAuth sends the PAT as git's Authorization header, as the API calls do.
func (c *Connector) GitAuth(ctx context.Context) (gitx.Credentials, error) {
	pat, ok, err := c.cfg.Credentials(ctx)
	if err != nil || !ok {
		return gitx.Credentials{}, err
	}
	return gitx.Credentials{Header: "Authorization: " + BasicAuth(pat.Reveal())}, nil
}

func (c *Connector) PullRequests() connectors.PullRequests { return (*pullRequests)(c) }
func (c *Connector) Statuses() connectors.Statuses         { return (*statuses)(c) }
func (c *Connector) Identities() connectors.Identities     { return (*identities)(c) }

func (c *Connector) Policies() connectors.Policies   { return (*policies)(c) }
func (c *Connector) Owners() connectors.Owners       { return (*owners)(c) }
func (c *Connector) Webhooks() connectors.Webhooks   { return (*webhooks)(c) }
func (c *Connector) Pipelines() connectors.Pipelines { return (*pipelines)(c) }

type identities Connector

// ByEmail finds the Azure DevOps identity with email (or UPN), as commit authors and PR
// reviewers are linked. On Services, identities live in the organization's vssps service.
func (i *identities) ByEmail(ctx context.Context, email string) (connectors.User, error) {
	c := (*Connector)(i)
	client, err := c.identityClient()
	if err != nil {
		return connectors.User{}, err
	}
	var list struct {
		Value []identity `json:"value"`
	}
	q := url.Values{"searchFilter": {"General"}, "filterValue": {email}, "queryMembership": {"None"}}
	if err := client.Do(ctx, http.MethodGet, "/_apis/identities", APIVersion, q, nil, &list); err != nil {
		return connectors.User{}, err
	}
	for _, id := range list.Value {
		for _, key := range []string{"Mail", "Account"} {
			if v := id.property(key); v != "" && strings.EqualFold(v, email) {
				return connectors.User{ID: id.ID, Name: id.ProviderDisplayName, Email: v}, nil
			}
		}
	}
	return connectors.User{}, fmt.Errorf("%w: no identity with the email %s", connectors.ErrNotFound, email)
}
