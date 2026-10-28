package azuredevops

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/damhau/quai/backend/internal/connectors"
)

// Policy types, by their well-known IDs.
const (
	statusPolicy       = "cbdc66da-9728-4af8-aada-9a5a32e4a226" // "Status check": a required PR status
	reviewersPolicy    = "fd2167ab-b0be-447a-8ec8-39368250530e" // "Automatically included reviewers"
	minReviewersPolicy = "fa4e907d-c16b-4a4c-9dfa-4906e5d171dd" // "Minimum number of reviewers"
)

// policy is a branch policy configuration.
type policy struct {
	ID         int  `json:"id"`
	IsEnabled  bool `json:"isEnabled"`
	IsBlocking bool `json:"isBlocking"`
	IsDeleted  bool `json:"isDeleted"`
	Type       struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	} `json:"type"`
	Settings policySettings `json:"settings"`
}

type policySettings struct {
	StatusGenre              string   `json:"statusGenre"`
	StatusName               string   `json:"statusName"`
	AuthorID                 string   `json:"authorId"`
	InvalidateOnSourceUpdate bool     `json:"invalidateOnSourceUpdate"`
	PolicyApplicability      *int     `json:"policyApplicability"` // 1: only when a status is posted
	FilenamePatterns         []string `json:"filenamePatterns"`
	RequiredReviewerIDs      []string `json:"requiredReviewerIds"`
	MinimumApproverCount     int      `json:"minimumApproverCount"`
}

func (s policySettings) status() string {
	if s.StatusGenre == "" {
		return s.StatusName
	}
	return s.StatusGenre + "/" + s.StatusName
}

// branchPolicies returns the enabled policies that apply to branch of the repository, those of
// the project included.
func (c *Connector) branchPolicies(ctx context.Context, branch string) ([]policy, Repository, error) {
	repo, err := c.Resolve(ctx)
	if err != nil {
		return nil, Repository{}, err
	}
	var list struct {
		Value []policy `json:"value"`
	}
	q := url.Values{"repositoryId": {repo.ID}, "refName": {"refs/heads/" + branch}, "$top": {"1000"}}
	if err := c.client.Do(ctx, http.MethodGet, path(repo.ProjectID, "_apis", "git", "policy", "configurations"), APIVersion, q, nil, &list); err != nil {
		return nil, Repository{}, err
	}
	var out []policy
	for _, p := range list.Value {
		if p.IsEnabled && !p.IsDeleted {
			out = append(out, p)
		}
	}
	return out, repo, nil
}

type policies Connector

func (p *policies) Protection(ctx context.Context, branch string) (connectors.Protection, error) {
	list, _, err := (*Connector)(p).branchPolicies(ctx, branch)
	if err != nil {
		return connectors.Protection{}, err
	}
	return protection(branch, list), nil
}

// protection reads what the policies of a branch require. Any policy on a branch makes changes
// go through pull requests.
func protection(branch string, list []policy) connectors.Protection {
	out := connectors.Protection{Branch: branch, Protected: len(list) > 0}
	restricted, reset := true, true
	for _, p := range list {
		if !p.IsBlocking {
			continue
		}
		switch p.Type.ID {
		case statusPolicy:
			out.RequiredChecks = append(out.RequiredChecks, p.Settings.status())
			restricted = restricted && p.Settings.AuthorID != ""
			reset = reset && p.Settings.InvalidateOnSourceUpdate
		case minReviewersPolicy:
			out.RequiredApprovals = max(out.RequiredApprovals, p.Settings.MinimumApproverCount)
		}
	}
	out.ChecksRestricted = len(out.RequiredChecks) > 0 && restricted
	out.ResetOnPush = len(out.RequiredChecks) > 0 && reset
	return out
}

// Verify checks that a blocking "Status check" policy requires check on branch as Quai needs it:
// only Quai's identity can set it, a push resets it, and it applies to every pull request.
func (p *policies) Verify(ctx context.Context, branch, check string) (connectors.Enforcement, error) {
	c := (*Connector)(p)
	list, _, err := c.branchPolicies(ctx, branch)
	if err != nil {
		return connectors.Enforcement{}, err
	}
	in, err := c.client.Detect(ctx)
	if err != nil {
		return connectors.Enforcement{}, err
	}
	return verify(branch, check, in.User, list), nil
}

func verify(branch, check string, quai User, list []policy) connectors.Enforcement {
	var problems []string
	found := false
	for _, p := range list {
		if p.Type.ID != statusPolicy || p.Settings.status() != check {
			continue
		}
		found = true
		s := p.Settings
		switch {
		case !p.IsBlocking:
			problems = append(problems, fmt.Sprintf("the status check policy for %s on %s is optional: make it required", check, branch))
		case s.AuthorID == "":
			problems = append(problems, fmt.Sprintf("any identity can set %s: set the policy's authorized identity to Quai's (%s)", check, quai.Name))
		case !strings.EqualFold(s.AuthorID, quai.ID):
			problems = append(problems, fmt.Sprintf("the authorized identity of the %s policy is not Quai's (%s)", check, quai.Name))
		}
		if !s.InvalidateOnSourceUpdate {
			problems = append(problems, fmt.Sprintf("%s is not reset when new changes are pushed: enable \"Reset status whenever there are new changes\"", check))
		}
		if s.PolicyApplicability != nil && *s.PolicyApplicability == 1 {
			problems = append(problems, fmt.Sprintf("the %s policy applies only when the status is posted: make it apply by default", check))
		}
		if len(s.FilenamePatterns) > 0 {
			problems = append(problems, fmt.Sprintf("the %s policy applies only to some paths (%s)", check, strings.Join(s.FilenamePatterns, ", ")))
		}
	}
	if !found {
		problems = append(problems, fmt.Sprintf("no status check policy on %s requires %s", branch, check))
	}
	return connectors.Enforcement{Enforced: len(problems) == 0, Problems: problems}
}

type owners Connector

// Rules returns the "Automatically included reviewers" policies of branch: their path filters,
// and the reviewers (as their account name, or display name for a group). A policy without
// path filters covers every path ("*").
func (o *owners) Rules(ctx context.Context, branch string) ([]connectors.OwnerRule, error) {
	c := (*Connector)(o)
	list, _, err := c.branchPolicies(ctx, branch)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, p := range list {
		if p.Type.ID == reviewersPolicy {
			ids = append(ids, p.Settings.RequiredReviewerIDs...)
		}
	}
	names, err := c.identityNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	var rules []connectors.OwnerRule
	for _, p := range list {
		if p.Type.ID != reviewersPolicy {
			continue
		}
		var owners []string
		for _, id := range p.Settings.RequiredReviewerIDs {
			owners = append(owners, names[strings.ToLower(id)])
		}
		patterns := p.Settings.FilenamePatterns
		if len(patterns) == 0 {
			patterns = []string{"*"}
		}
		for _, pattern := range patterns {
			rules = append(rules, connectors.OwnerRule{Pattern: pattern, Owners: owners})
		}
	}
	return rules, nil
}

// identityClient returns the client of the identities service: the collection on Server, the
// organization's vssps host on Services.
func (c *Connector) identityClient() (*Client, error) {
	if !c.loc.Services {
		return c.client, nil
	}
	u, err := url.Parse(c.loc.Collection)
	if err != nil {
		return nil, err
	}
	if u.Host == "dev.azure.com" {
		u.Host = "vssps.dev.azure.com"
	} else {
		u.Host = strings.Replace(u.Host, ".visualstudio.com", ".vssps.visualstudio.com", 1)
	}
	return NewClient(u.String(), c.cfg.HTTP, c.cfg.Credentials), nil
}

// identity is an Azure DevOps identity as the identities API returns it.
type identity struct {
	ID                  string `json:"id"`
	ProviderDisplayName string `json:"providerDisplayName"`
	Properties          map[string]struct {
		Value any `json:"$value"`
	} `json:"properties"`
}

func (i identity) property(key string) string {
	v, _ := i.Properties[key].Value.(string)
	return v
}

// identityNames maps identity IDs (lower case) to their account name, or display name.
func (c *Connector) identityNames(ctx context.Context, ids []string) (map[string]string, error) {
	names := map[string]string{}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return names, nil
	}
	client, err := c.identityClient()
	if err != nil {
		return nil, err
	}
	var list struct {
		Value []identity `json:"value"`
	}
	q := url.Values{"identityIds": {strings.Join(ids, ",")}, "queryMembership": {"None"}}
	if err := client.Do(ctx, http.MethodGet, "/_apis/identities", APIVersion, q, nil, &list); err != nil {
		return nil, err
	}
	for _, id := range list.Value {
		name := id.property("Account")
		if name == "" {
			name = id.ProviderDisplayName
		}
		names[strings.ToLower(id.ID)] = name
	}
	for _, id := range ids {
		if names[strings.ToLower(id)] == "" {
			names[strings.ToLower(id)] = id
		}
	}
	return names, nil
}
