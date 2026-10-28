// Package gitea is the connector for Gitea and Forgejo (docs/design/12-git-connectors.md#bitbucket-gitea-generic),
// the reference connector of the conformance suite: pull requests, commit statuses
// (quai/approval), branch protection with required status checks, merges and branch deletion,
// HMAC-signed webhooks, and owners from the CODEOWNERS file. It authenticates with an access
// token, spec.credentials.ref, for the API and for git.
package gitea

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "code.gitea.io/sdk/gitea"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/secrets"
)

// Connector is the Gitea connector for one repository.
type Connector struct {
	cfg         connectors.Config
	base        string // the instance's URL, without credentials or a trailing slash
	host        string
	owner, repo string
	http        *http.Client

	mu      sync.Mutex
	version string // the server's, read on first use
}

// New returns the connector for the repository at cfg.URL (https://host/owner/repo.git, possibly
// under a sub-path). It makes no call until it is used.
func New(cfg connectors.Config) (connectors.Connector, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("gitea: %q is not an HTTP(S) repository URL", redact(cfg.URL))
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("gitea: %q does not name an owner and a repository", redact(cfg.URL))
	}
	owner, repo := parts[len(parts)-2], strings.TrimSuffix(parts[len(parts)-1], ".git")
	base := url.URL{Scheme: u.Scheme, Host: u.Host, Path: strings.Join(parts[:len(parts)-2], "/")}
	if base.Path != "" {
		base.Path = "/" + base.Path
	}
	if cfg.HTTP == nil {
		cfg.HTTP = connectors.NewHTTPClient(http.DefaultTransport, cfg.Log)
	}
	if cfg.Credentials == nil {
		cfg.Credentials = func(context.Context) (secrets.Secret, bool, error) { return secrets.Secret{}, false, nil }
	}
	c := &Connector{cfg: cfg, base: base.String(), host: u.Host, owner: owner, repo: repo}
	transport := cfg.HTTP.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	c.http = &http.Client{Transport: &tokenTransport{base: transport, host: u.Host, creds: cfg.Credentials}, Timeout: cfg.HTTP.Timeout}
	return c, nil
}

func redact(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return connectors.Redact(u)
	}
	return "the URL"
}

// tokenTransport adds the access token to the calls to the instance's host, read on each call
// so a rotated token is picked up.
type tokenTransport struct {
	base  http.RoundTripper
	host  string
	creds func(context.Context) (secrets.Secret, bool, error)
}

func (t *tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == t.host {
		token, ok, err := t.creds(r.Context())
		if err != nil {
			return nil, err
		}
		if ok {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "token "+token.Reveal())
		}
	}
	return t.base.RoundTrip(r)
}

// client returns an SDK client for one call. The server's version is read once; after that,
// making a client makes no call.
func (c *Connector) client(ctx context.Context) (*sdk.Client, error) {
	c.mu.Lock()
	v := c.version
	c.mu.Unlock()
	if v == "" {
		probe, err := sdk.NewClient(c.base, sdk.SetHTTPClient(c.http), sdk.SetGiteaVersion(""), sdk.SetContext(ctx))
		if err != nil {
			return nil, err
		}
		if v, _, err = probe.ServerVersion(); err != nil {
			return nil, fmt.Errorf("gitea %s: %w", c.base, err)
		}
		c.mu.Lock()
		c.version = v
		c.mu.Unlock()
	}
	return sdk.NewClient(c.base, sdk.SetHTTPClient(c.http), sdk.SetGiteaVersion(v), sdk.SetContext(ctx))
}

// check maps the SDK's errors to the connectors' own.
func check(resp *sdk.Response, err error) error {
	if err == nil {
		return nil
	}
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %v", connectors.ErrNotFound, err)
	}
	return err
}

func (c *Connector) Info() connectors.Info {
	c.mu.Lock()
	defer c.mu.Unlock()
	edition := ""
	if c.version != "" {
		edition = "Gitea " + c.version
	}
	return connectors.Info{
		Platform:     "gitea",
		Edition:      edition,
		APIVersion:   "v1",
		Capabilities: []connectors.Capability{connectors.CapPullRequests, connectors.CapAutoComplete, connectors.CapSignedWebhooks, connectors.CapCodeOwnersFile},
		MergeMethods: []connectors.MergeMethod{connectors.MergeCommit, connectors.Squash, connectors.Rebase},
	}
}

// GitAuth sends the access token as an HTTP header, as the API calls do.
func (c *Connector) GitAuth(ctx context.Context) (gitx.Credentials, error) {
	token, ok, err := c.cfg.Credentials(ctx)
	if err != nil || !ok {
		return gitx.Credentials{}, err
	}
	return gitx.Credentials{Header: "Authorization: token " + token.Reveal()}, nil
}

func (c *Connector) PullRequests() connectors.PullRequests { return (*pullRequests)(c) }
func (c *Connector) Statuses() connectors.Statuses         { return (*statuses)(c) }
func (c *Connector) Policies() connectors.Policies         { return (*policies)(c) }
func (c *Connector) Owners() connectors.Owners             { return (*owners)(c) }
func (c *Connector) Webhooks() connectors.Webhooks         { return (*webhooks)(c) }
func (c *Connector) Pipelines() connectors.Pipelines       { return nil }
func (c *Connector) Identities() connectors.Identities     { return (*identities)(c) }

type pullRequests Connector

// draftPrefix marks a draft pull request: Gitea reads drafts from the title.
const draftPrefix = "WIP: "

func (p *pullRequests) Open(ctx context.Context, n connectors.NewPullRequest) (connectors.PullRequest, error) {
	c := (*Connector)(p)
	cl, err := c.client(ctx)
	if err != nil {
		return connectors.PullRequest{}, err
	}
	labels, err := c.labelIDs(cl, n.Labels)
	if err != nil {
		return connectors.PullRequest{}, err
	}
	title := n.Title
	if n.Draft {
		title = draftPrefix + title
	}
	pr, resp, err := cl.CreatePullRequest(c.owner, c.repo, sdk.CreatePullRequestOption{Head: n.Source, Base: n.Target, Title: title, Body: n.Description, Labels: labels})
	if err := check(resp, err); err != nil {
		return connectors.PullRequest{}, err
	}
	return convert(pr), nil
}

// labelIDs returns the IDs of the repository's labels named names, creating those it lacks.
func (c *Connector) labelIDs(cl *sdk.Client, names []string) ([]int64, error) {
	if len(names) == 0 {
		return nil, nil
	}
	existing := map[string]int64{}
	for page := 1; ; page++ {
		labels, resp, err := cl.ListRepoLabels(c.owner, c.repo, sdk.ListLabelsOptions{ListOptions: sdk.ListOptions{Page: page, PageSize: 50}})
		if err := check(resp, err); err != nil {
			return nil, err
		}
		for _, l := range labels {
			existing[l.Name] = l.ID
		}
		if len(labels) < 50 {
			break
		}
	}
	var ids []int64
	for _, name := range names {
		id, ok := existing[name]
		if !ok {
			l, resp, err := cl.CreateLabel(c.owner, c.repo, sdk.CreateLabelOption{Name: name, Color: "#6e7781"})
			if err := check(resp, err); err != nil {
				return nil, fmt.Errorf("label %s: %w", name, err)
			}
			id = l.ID
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func index(id string) (int64, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: pull request %q", connectors.ErrNotFound, id)
	}
	return n, nil
}

func (p *pullRequests) Get(ctx context.Context, id string) (connectors.PullRequest, error) {
	c := (*Connector)(p)
	n, err := index(id)
	if err != nil {
		return connectors.PullRequest{}, err
	}
	cl, err := c.client(ctx)
	if err != nil {
		return connectors.PullRequest{}, err
	}
	pr, resp, err := cl.GetPullRequest(c.owner, c.repo, n)
	if err := check(resp, err); err != nil {
		return connectors.PullRequest{}, err
	}
	return convert(pr), nil
}

func (p *pullRequests) Update(ctx context.Context, id string, u connectors.PullRequestUpdate) (connectors.PullRequest, error) {
	c := (*Connector)(p)
	n, err := index(id)
	if err != nil {
		return connectors.PullRequest{}, err
	}
	cl, err := c.client(ctx)
	if err != nil {
		return connectors.PullRequest{}, err
	}
	opt := sdk.EditPullRequestOption{Body: u.Description}
	if u.Title != nil {
		opt.Title = *u.Title
	}
	pr, resp, err := cl.EditPullRequest(c.owner, c.repo, n, opt)
	if err := check(resp, err); err != nil {
		return connectors.PullRequest{}, err
	}
	return convert(pr), nil
}

func (p *pullRequests) List(ctx context.Context, prefix string) ([]connectors.PullRequest, error) {
	c := (*Connector)(p)
	cl, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	var out []connectors.PullRequest
	for page := 1; ; page++ {
		prs, resp, err := cl.ListRepoPullRequests(c.owner, c.repo, sdk.ListPullRequestsOptions{State: sdk.StateOpen, ListOptions: sdk.ListOptions{Page: page, PageSize: 50}})
		if err := check(resp, err); err != nil {
			return nil, err
		}
		for _, pr := range prs {
			if pr.Head != nil && strings.HasPrefix(pr.Head.Ref, prefix) {
				out = append(out, convert(pr))
			}
		}
		if len(prs) < 50 {
			return out, nil
		}
	}
}

var styles = map[connectors.MergeMethod]sdk.MergeStyle{
	connectors.MergeCommit: sdk.MergeStyleMerge,
	connectors.Squash:      sdk.MergeStyleSquash,
	connectors.Rebase:      sdk.MergeStyleRebase,
}

func (p *pullRequests) merge(ctx context.Context, id string, o connectors.MergeOptions, auto bool) error {
	c := (*Connector)(p)
	n, err := index(id)
	if err != nil {
		return err
	}
	style, ok := styles[o.Method]
	if !ok {
		return fmt.Errorf("%w: merge method %q", connectors.ErrUnsupported, o.Method)
	}
	title, message, _ := strings.Cut(strings.TrimSpace(o.Message), "\n")
	opt := sdk.MergePullRequestOption{Style: style, Title: title, Message: strings.TrimSpace(message),
		DeleteBranchAfterMerge: &o.DeleteBranch, HeadCommitId: o.Head, MergeWhenChecksSucceed: auto}
	// Gitea checks a new pull request's mergeability in the background, and asks to try again
	// until it is done.
	for attempt := 0; ; attempt++ {
		cl, err := c.client(ctx)
		if err != nil {
			return err
		}
		ok, resp, err := cl.MergePullRequest(c.owner, c.repo, n, opt)
		if err == nil && ok {
			return nil
		}
		if resp != nil && resp.StatusCode == http.StatusMethodNotAllowed {
			if strings.Contains(strings.ToLower(errorText(err)), "try again later") && attempt < 20 {
				if err := sleep(ctx, 500*time.Millisecond); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("%w: %v", connectors.ErrBlocked, err)
		}
		if err == nil {
			err = errors.New("gitea did not merge")
		}
		return check(resp, err)
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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

func (p *pullRequests) Merge(ctx context.Context, id string, o connectors.MergeOptions) (connectors.PullRequest, error) {
	if err := p.merge(ctx, id, o, false); err != nil {
		return connectors.PullRequest{}, err
	}
	return p.Get(ctx, id)
}

// AutoComplete asks Gitea to merge once the required checks succeed.
func (p *pullRequests) AutoComplete(ctx context.Context, id string, o connectors.MergeOptions) error {
	return p.merge(ctx, id, o, true)
}

func (p *pullRequests) Abandon(ctx context.Context, id string) error {
	c := (*Connector)(p)
	n, err := index(id)
	if err != nil {
		return err
	}
	cl, err := c.client(ctx)
	if err != nil {
		return err
	}
	closed := sdk.StateClosed
	_, resp, err := cl.EditPullRequest(c.owner, c.repo, n, sdk.EditPullRequestOption{State: &closed})
	return check(resp, err)
}

func convert(pr *sdk.PullRequest) connectors.PullRequest {
	out := connectors.PullRequest{ID: strconv.FormatInt(pr.Index, 10), URL: pr.HTMLURL, Title: pr.Title, Description: pr.Body,
		Draft: pr.Draft, State: connectors.PROpen}
	if pr.Head != nil {
		out.Source, out.Head = pr.Head.Ref, pr.Head.Sha
	}
	if pr.Base != nil {
		out.Target = pr.Base.Ref
	}
	for _, l := range pr.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	switch {
	case pr.HasMerged:
		out.State = connectors.PRMerged
		if pr.MergedCommitID != nil {
			out.MergeCommit = *pr.MergedCommitID
		}
	case pr.State == sdk.StateClosed:
		out.State = connectors.PRAbandoned
	}
	return out
}

type statuses Connector

var toGitea = map[connectors.StatusState]sdk.StatusState{
	connectors.StatusPending: sdk.StatusPending,
	connectors.StatusSuccess: sdk.StatusSuccess,
	connectors.StatusFailure: sdk.StatusFailure,
	connectors.StatusError:   sdk.StatusError,
	// Gitea has no "not applicable": a stale approval must block the merge, as a failure does.
	connectors.StatusNotApplicable: sdk.StatusFailure,
}

// commit returns the commit a status goes on: the given one, or the pull request's head.
func (s *statuses) commit(ctx context.Context, t connectors.StatusTarget) (string, error) {
	if t.Commit != "" {
		return t.Commit, nil
	}
	pr, err := (*pullRequests)(s).Get(ctx, t.PullRequest)
	return pr.Head, err
}

func (s *statuses) Set(ctx context.Context, t connectors.StatusTarget, st connectors.Status) error {
	c := (*Connector)(s)
	state, ok := toGitea[st.State]
	if !ok {
		return fmt.Errorf("%w: status state %q", connectors.ErrUnsupported, st.State)
	}
	sha, err := s.commit(ctx, t)
	if err != nil {
		return err
	}
	cl, err := c.client(ctx)
	if err != nil {
		return err
	}
	_, resp, err := cl.CreateStatus(c.owner, c.repo, sha, sdk.CreateStatusOption{State: state, TargetURL: st.URL, Description: st.Description, Context: st.Name})
	return check(resp, err)
}

// List returns the latest status of each name on the commit.
func (s *statuses) List(ctx context.Context, t connectors.StatusTarget) ([]connectors.Status, error) {
	c := (*Connector)(s)
	sha, err := s.commit(ctx, t)
	if err != nil {
		return nil, err
	}
	cl, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	combined, resp, err := cl.GetCombinedStatus(c.owner, c.repo, sha)
	if err := check(resp, err); err != nil {
		return nil, err
	}
	var out []connectors.Status
	for _, st := range combined.Statuses {
		state := connectors.StatusState(st.State)
		if st.State == sdk.StatusWarning {
			state = connectors.StatusFailure
		}
		out = append(out, connectors.Status{Name: st.Context, State: state, Description: st.Description, URL: st.TargetURL})
	}
	return out, nil
}

type policies Connector

// rule returns the protection rule that applies to branch: the one named after it, or else the
// first whose glob matches it.
func (p *policies) rule(ctx context.Context, branch string) (*sdk.BranchProtection, error) {
	c := (*Connector)(p)
	cl, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	rules, resp, err := cl.ListBranchProtections(c.owner, c.repo, sdk.ListBranchProtectionsOptions{})
	if err := check(resp, err); err != nil {
		return nil, err
	}
	for _, r := range rules {
		if r.RuleName == branch || r.BranchName == branch {
			return r, nil
		}
	}
	for _, r := range rules {
		if ok, _ := path.Match(r.RuleName, branch); ok {
			return r, nil
		}
	}
	return nil, nil
}

func (p *policies) Protection(ctx context.Context, branch string) (connectors.Protection, error) {
	r, err := p.rule(ctx, branch)
	if err != nil || r == nil {
		return connectors.Protection{Branch: branch}, err
	}
	out := connectors.Protection{Branch: branch, Protected: true, RequiredApprovals: int(r.RequiredApprovals),
		// Statuses are set on commits: a push brings a head without them.
		ResetOnPush: true}
	if r.EnableStatusCheck {
		out.RequiredChecks = slices.Clone(r.StatusCheckContexts)
	}
	return out, nil
}

// Verify checks that branch is protected and requires check. Gitea cannot restrict who sets a
// status, so it does not have CapEnforcedStatus.
func (p *policies) Verify(ctx context.Context, branch, check string) (connectors.Enforcement, error) {
	prot, err := p.Protection(ctx, branch)
	if err != nil {
		return connectors.Enforcement{}, err
	}
	var problems []string
	switch {
	case !prot.Protected:
		problems = append(problems, fmt.Sprintf("branch %s is not protected", branch))
	case !slices.ContainsFunc(prot.RequiredChecks, func(pattern string) bool { ok, _ := path.Match(pattern, check); return ok }):
		problems = append(problems, fmt.Sprintf("the protection of %s does not require the status check %s", branch, check))
	}
	return connectors.Enforcement{Enforced: len(problems) == 0, Problems: problems}, nil
}

type owners Connector

// codeOwnersFiles are where Gitea reads CODEOWNERS from, in order.
var codeOwnersFiles = []string{"CODEOWNERS", "docs/CODEOWNERS", ".gitea/CODEOWNERS"}

// Rules reads the CODEOWNERS file of branch. Gitea's patterns are regular expressions; they are
// returned as written.
func (o *owners) Rules(ctx context.Context, branch string) ([]connectors.OwnerRule, error) {
	c := (*Connector)(o)
	cl, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	for _, file := range codeOwnersFiles {
		b, resp, err := cl.GetFile(c.owner, c.repo, branch, file)
		if err := check(resp, err); errors.Is(err, connectors.ErrNotFound) {
			continue
		} else if err != nil {
			return nil, err
		}
		return parseCodeOwners(string(b)), nil
	}
	return nil, nil
}

func parseCodeOwners(src string) []connectors.OwnerRule {
	var rules []connectors.OwnerRule
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		rule := connectors.OwnerRule{Pattern: fields[0]}
		for _, f := range fields[1:] {
			if name, ok := strings.CutPrefix(f, "@"); ok && name != "" {
				rule.Owners = append(rule.Owners, name)
			}
		}
		if len(rule.Owners) > 0 {
			rules = append(rules, rule)
		}
	}
	return rules
}

type identities Connector

// ByEmail finds the user with email, among those the token can see.
func (i *identities) ByEmail(ctx context.Context, email string) (connectors.User, error) {
	c := (*Connector)(i)
	cl, err := c.client(ctx)
	if err != nil {
		return connectors.User{}, err
	}
	users, resp, err := cl.SearchUsers(sdk.SearchUsersOption{KeyWord: email})
	if err := check(resp, err); err != nil {
		return connectors.User{}, err
	}
	for _, u := range users {
		if strings.EqualFold(u.Email, email) {
			return connectors.User{ID: strconv.FormatInt(u.ID, 10), Name: u.UserName, Email: u.Email}, nil
		}
	}
	return connectors.User{}, fmt.Errorf("%w: no user with the email %s", connectors.ErrNotFound, email)
}
