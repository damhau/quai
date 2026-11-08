// Package github is the connector for github.com (docs/design/12-git-connectors.md#github-second).
// It authenticates as a GitHub App. A JWT signed with the app's private key (spec.credentials.ref;
// the app's ID is spec.credentials.appId) finds the app's installation on the repository and is
// exchanged for installation tokens, limited to that repository, which the API and git use.
//
// It has Git transport, pull requests merged by Quai or by GitHub (auto-merge, the merge queue),
// quai/approval as the app's check runs, branch protection and rulesets, CODEOWNERS, and signed
// webhooks. Auto-merge and the merge queue are GraphQL's only.
package github

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gh "github.com/google/go-github/v88/github"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/secrets"
)

const (
	// host is the only one New connects to: it sends the app's JWT and tokens to spec.url's host,
	// so another one waits for GitHub Enterprise Server (#137).
	host = "github.com"
	// apiURL is github.com's API. It lives in this one place, for GitHub Enterprise Server.
	apiURL = "https://api.github.com/"
	// apiVersion is the REST API version go-github pins its calls to.
	apiVersion = "2022-11-28"
)

// Connector is the GitHub connector for one repository.
type Connector struct {
	cfg         connectors.Config
	owner, repo string
	api         string            // the API's URL, with a trailing slash
	base        http.RoundTripper // the shared transport: rate limits, logging, proxy, CA bundle
	client      *gh.Client        // the API, as the app's installation

	mu   sync.Mutex
	key  [sha256.Size]byte         // a digest of the private key auth was made from: the key itself would print
	auth *ghinstallation.Transport // holds the installation's token, nil until the first use
}

// New returns the connector for the repository at cfg.URL (https://github.com/owner/repo, with or
// without .git). It makes no call until it is used.
func New(cfg connectors.Config) (connectors.Connector, error) {
	c, err := newConnector(cfg, apiURL)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func newConnector(cfg connectors.Config, api string) (*Connector, error) {
	owner, repo, err := parseURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	if cfg.AppID <= 0 {
		return nil, errors.New("github: spec.credentials.appId must name the GitHub App")
	}
	if cfg.HTTP == nil {
		cfg.HTTP = connectors.NewHTTPClient(http.DefaultTransport, cfg.Log)
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Credentials == nil {
		cfg.Credentials = func(context.Context) (secrets.Secret, bool, error) { return secrets.Secret{}, false, nil }
	}
	apiHost, err := url.Parse(api)
	if err != nil {
		return nil, fmt.Errorf("github: the API's URL: %w", err)
	}
	c := &Connector{cfg: cfg, owner: owner, repo: repo, api: api, base: cfg.HTTP.Transport}
	if c.base == nil {
		c.base = http.DefaultTransport
	}
	c.client, err = c.newClient(&tokenTransport{c: c, host: apiHost.Host})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// newClient returns an API client over auth. The shared transport honours the rate limits, so
// go-github does not keep its own account of them.
func (c *Connector) newClient(auth http.RoundTripper) (*gh.Client, error) {
	return gh.NewClient(gh.WithHTTPClient(&http.Client{Transport: auth, Timeout: c.cfg.HTTP.Timeout}), gh.WithURLs(&c.api, &c.api), gh.WithDisableRateLimitCheck())
}

// parseURL returns the owner and name of the repository at raw.
func parseURL(raw string) (owner, repo string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", errors.New("github: the repository URL is not a URL: spec.url must be https://github.com/<owner>/<repository>")
	}
	if u.Scheme != "https" || !strings.EqualFold(u.Host, host) {
		return "", "", fmt.Errorf("github: %q is not a repository on https://github.com (GitHub Enterprise Server is not supported yet)", connectors.Redact(u))
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[1] == ".git" {
		return "", "", fmt.Errorf("github: %q does not name an owner and a repository", connectors.Redact(u))
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

// installation returns the transport holding the installation's token. The private key is read on
// each call, so a rotated key is picked up; the installation is looked up with it once.
func (c *Connector) installation(ctx context.Context) (*ghinstallation.Transport, error) {
	key, ok, err := c.cfg.Credentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: the app's private key: %w", err)
	}
	if !ok {
		return nil, errors.New("github: spec.credentials.ref must name the app's private key")
	}
	digest := sha256.Sum256([]byte(key.Reveal()))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.auth != nil && c.key == digest {
		return c.auth, nil
	}
	apps, err := ghinstallation.NewAppsTransport(c.base, c.cfg.AppID, []byte(key.Reveal()))
	if err != nil {
		return nil, fmt.Errorf("github: the app's private key: %w", err)
	}
	apps.BaseURL = strings.TrimSuffix(c.api, "/")
	asApp, err := c.newClient(apps)
	if err != nil {
		return nil, err
	}
	in, resp, err := asApp.Apps.GetRepositoryInstallation(ctx, c.owner, c.repo)
	if err := check(resp, err); err != nil {
		if errors.Is(err, connectors.ErrNotFound) {
			return nil, fmt.Errorf("the app %d is not installed on %s/%s: %w", c.cfg.AppID, c.owner, c.repo, err)
		}
		return nil, err
	}
	auth := ghinstallation.NewFromAppsTransport(apps, in.GetID())
	// A connector is for one repository, and its token reaches no other, even when the app is
	// installed on the whole organization.
	auth.InstallationTokenOptions = &gh.InstallationTokenOptions{Repositories: []string{c.repo}}
	c.auth, c.key = auth, digest
	return auth, nil
}

// token returns the installation's token, which ghinstallation renews a minute before it expires.
// When GitHub refuses to issue one, the installation is looked up again on the next call: the app
// may have been installed anew, under another ID.
func (c *Connector) token(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, connectors.Timeout) // ghinstallation sets none
	defer cancel()
	auth, err := c.installation(ctx)
	if err != nil {
		return "", err
	}
	token, err := auth.Token(ctx)
	if err == nil {
		return token, nil
	}
	var refused *ghinstallation.HTTPError
	if !errors.As(err, &refused) || refused.Response == nil {
		return "", fmt.Errorf("github: an installation token: %w", err)
	}
	defer func() { _ = refused.Response.Body.Close() }()
	if s := refused.Response.StatusCode; s >= 400 && s < 500 {
		c.mu.Lock()
		if c.auth == auth {
			c.auth = nil
		}
		c.mu.Unlock()
	}
	body, _ := io.ReadAll(io.LimitReader(refused.Response.Body, 1<<12))
	return "", fmt.Errorf("github: an installation token: %s: %s", refused.Response.Status, body)
}

// tokenTransport adds the installation's token to the calls to the API's host.
type tokenTransport struct {
	c    *Connector
	host string
}

func (t *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != t.host {
		return t.c.base.RoundTrip(r)
	}
	token, err := t.c.token(r.Context())
	if err != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, err
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "token "+token)
	return t.c.base.RoundTrip(r)
}

// check maps go-github's errors to the connectors' own.
func check(resp *gh.Response, err error) error {
	if err == nil {
		return nil
	}
	var primary *gh.RateLimitError
	var secondary *gh.AbuseRateLimitError
	if errors.As(err, &primary) || errors.As(err, &secondary) {
		return fmt.Errorf("%w: %v", connectors.ErrRateLimited, err)
	}
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %v", connectors.ErrNotFound, err)
	}
	return fmt.Errorf("github: %w", err)
}

// Info reports what the connector supports. A required check pinned to the app is one only the app
// satisfies. GitHub merges (auto-complete): through auto-merge, or the merge queue where the base
// branch has one.
func (c *Connector) Info() connectors.Info {
	return connectors.Info{
		Platform:   "github",
		Edition:    host,
		APIVersion: apiVersion,
		Capabilities: []connectors.Capability{connectors.CapPullRequests, connectors.CapEnforcedStatus, connectors.CapAutoComplete,
			connectors.CapMergeQueue, connectors.CapSignedWebhooks, connectors.CapCodeOwnersFile},
		MergeMethods: []connectors.MergeMethod{connectors.MergeCommit, connectors.Squash, connectors.Rebase},
	}
}

// GitAuth gives git the installation's token, fresh for each command, as GitHub takes it over
// HTTPS: as the password of the user x-access-token.
func (c *Connector) GitAuth(ctx context.Context) (gitx.Credentials, error) {
	token, err := c.token(ctx)
	if err != nil {
		return gitx.Credentials{}, err
	}
	return gitx.Credentials{Header: "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))}, nil
}

func (c *Connector) PullRequests() connectors.PullRequests { return (*pullRequests)(c) }

func (c *Connector) Statuses() connectors.Statuses     { return (*statuses)(c) }
func (c *Connector) Policies() connectors.Policies     { return (*policies)(c) }
func (c *Connector) Owners() connectors.Owners         { return (*owners)(c) }
func (c *Connector) Webhooks() connectors.Webhooks     { return (*webhooks)(c) }
func (c *Connector) Pipelines() connectors.Pipelines   { return nil }
func (c *Connector) Identities() connectors.Identities { return nil }
