package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	gh "github.com/google/go-github/v88/github"

	"github.com/damhau/quai/backend/internal/connectors"
)

type policies Connector

// rules is what Quai reads of the rulesets that apply to a branch.
type rules struct {
	restricting  bool // a rule that keeps changes from landing without a pull request or checks
	checks       []*gh.RequiredStatusChecksBranchRule
	pullRequests []*gh.PullRequestBranchRule
}

// classic returns the branch protection of branch, or nil when it has none.
func (p *policies) classic(ctx context.Context, branch string) (*gh.Protection, *gh.Response, error) {
	c := (*Connector)(p)
	prot, resp, err := c.client.Repositories.GetBranchProtection(ctx, c.owner, c.repo, branch)
	if errors.Is(err, gh.ErrBranchNotProtected) {
		return nil, resp, nil
	}
	return prot, resp, err
}

// rulesets returns the active rules of the repository's and organization's rulesets that apply
// to branch. GitHub leaves out disabled rulesets (measured).
func (p *policies) rulesets(ctx context.Context, branch string) (rules, *gh.Response, error) {
	c := (*Connector)(p)
	var out rules
	opts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := c.client.Repositories.ListRulesForBranch(ctx, c.owner, c.repo, branch, opts)
		if err != nil {
			return rules{}, resp, err
		}
		if page != nil {
			out.restricting = out.restricting || len(page.PullRequest)+len(page.RequiredStatusChecks)+len(page.Update)+len(page.MergeQueue)+len(page.RequiredDeployments) > 0
			out.checks = append(out.checks, page.RequiredStatusChecks...)
			out.pullRequests = append(out.pullRequests, page.PullRequest...)
		}
		if resp.NextPage == 0 {
			return out, resp, nil
		}
		opts.Page = resp.NextPage
	}
}

// Protection reads the branch protection and the rulesets of branch together, as GitHub applies
// both. A required check counts as restricted when an app is its only source.
func (p *policies) Protection(ctx context.Context, branch string) (connectors.Protection, error) {
	classic, resp, err := p.classic(ctx, branch)
	if err := check(resp, err); err != nil {
		return connectors.Protection{}, fmt.Errorf("the branch protection of %s: %w", branch, err)
	}
	rs, resp, err := p.rulesets(ctx, branch)
	if err := check(resp, err); err != nil {
		return connectors.Protection{}, fmt.Errorf("the rulesets of %s: %w", branch, err)
	}
	return protection(branch, classic, rs), nil
}

func protection(branch string, classic *gh.Protection, rs rules) connectors.Protection {
	out := connectors.Protection{Branch: branch, Protected: rs.restricting}
	restricted := true
	add := func(context string, app *int64) {
		if !slices.Contains(out.RequiredChecks, context) {
			out.RequiredChecks = append(out.RequiredChecks, context)
		}
		restricted = restricted && app != nil && *app > 0
	}
	if classic != nil {
		reviews := classic.RequiredPullRequestReviews
		out.Protected = out.Protected || classic.RequiredStatusChecks != nil || reviews != nil || classic.Restrictions != nil ||
			classic.GetLockBranch().GetEnabled()
		for _, ch := range requiredChecks(classic.RequiredStatusChecks) {
			add(ch.Context, ch.AppID)
		}
		if reviews != nil {
			out.RequiredApprovals = reviews.RequiredApprovingReviewCount
			out.CodeOwnerReview = reviews.RequireCodeOwnerReviews
		}
	}
	for _, r := range rs.checks {
		for _, ch := range r.Parameters.RequiredStatusChecks {
			add(ch.Context, ch.IntegrationID)
		}
	}
	for _, r := range rs.pullRequests {
		out.RequiredApprovals = max(out.RequiredApprovals, r.Parameters.RequiredApprovingReviewCount)
		out.CodeOwnerReview = out.CodeOwnerReview || r.Parameters.RequireCodeOwnerReview
	}
	out.ChecksRestricted = len(out.RequiredChecks) > 0 && restricted
	// Checks belong to commits: a push brings a head without them.
	out.ResetOnPush = len(out.RequiredChecks) > 0
	return out
}

// requiredChecks returns the checks a branch protection requires; the legacy contexts accept
// any source.
func requiredChecks(r *gh.RequiredStatusChecks) []*gh.RequiredStatusCheck {
	if r == nil {
		return nil
	}
	if r.Checks != nil && len(*r.Checks) > 0 {
		return *r.Checks
	}
	var out []*gh.RequiredStatusCheck
	if r.Contexts != nil {
		for _, ctx := range *r.Contexts {
			out = append(out, &gh.RequiredStatusCheck{Context: ctx})
		}
	}
	return out
}

// Verify checks that branch requires check from Quai's app, in a branch protection or an active
// ruleset that Quai's app cannot bypass: a classic protection must apply to administrators, since
// it lets the app through otherwise (measured). It never counts what it could not read: a refusal
// (403, 404) or an answer it does not understand is a problem, "not verified", rather than an
// error, which would keep the last state; a transient error (rate limit, network, 5xx) is an
// error.
func (p *policies) Verify(ctx context.Context, branch, want string) (connectors.Enforcement, error) {
	c := (*Connector)(p)
	var problems []string
	enforced, found, unread := false, false, false
	classic, resp, err := p.classic(ctx, branch)
	if why, ok := unverifiable(resp, err); ok {
		unread = true
		problems = append(problems, fmt.Sprintf("the branch protection of %s could not be read (%s): not verified", branch, why))
	} else if err != nil {
		return connectors.Enforcement{}, check(resp, err)
	} else if classic != nil {
		for _, ch := range requiredChecks(classic.RequiredStatusChecks) {
			if ch.Context != want {
				continue
			}
			found = true
			switch app := ch.AppID; {
			case app == nil || *app <= 0:
				problems = append(problems, fmt.Sprintf("the branch protection of %s accepts %s from any source: set its source to Quai's GitHub App (ID %d)", branch, want, c.cfg.AppID))
			case *app != c.cfg.AppID:
				problems = append(problems, fmt.Sprintf("the branch protection of %s expects %s from another app (ID %d), not Quai's (ID %d)", branch, want, *app, c.cfg.AppID))
			case classic.GetEnforceAdmins() == nil || !classic.GetEnforceAdmins().Enabled:
				problems = append(problems, fmt.Sprintf("the branch protection of %s does not apply to administrators, and Quai's GitHub App is one: it can merge without %s; enable \"Do not allow bypassing the above settings\"", branch, want))
			default:
				enforced = true
			}
		}
	}
	rs, resp, err := p.rulesets(ctx, branch)
	if why, ok := unverifiable(resp, err); ok {
		unread = true
		problems = append(problems, fmt.Sprintf("the rulesets of %s could not be read (%s): not verified", branch, why))
	} else if err != nil {
		return connectors.Enforcement{}, check(resp, err)
	} else {
		for _, r := range rs.checks {
			for _, ch := range r.Parameters.RequiredStatusChecks {
				if ch.Context != want {
					continue
				}
				found = true
				name := fmt.Sprintf("ruleset %d of %s", r.RulesetID, r.RulesetSource)
				switch app := ch.IntegrationID; {
				case app == nil || *app <= 0:
					problems = append(problems, fmt.Sprintf("the %s accepts %s from any source: set its source to Quai's GitHub App (ID %d)", name, want, c.cfg.AppID))
				case *app != c.cfg.AppID:
					problems = append(problems, fmt.Sprintf("the %s expects %s from another app (ID %d), not Quai's (ID %d)", name, want, *app, c.cfg.AppID))
				default:
					problem, err := p.binding(ctx, r.RulesetID, name)
					if err != nil {
						return connectors.Enforcement{}, err
					}
					if problem != "" {
						problems = append(problems, problem)
					} else {
						enforced = true
					}
				}
			}
		}
	}
	if enforced {
		return connectors.Enforcement{Enforced: true}, nil
	}
	if !found && !unread {
		problems = append(problems, fmt.Sprintf("no branch protection or ruleset on %s requires %s: add a ruleset requiring it from Quai's GitHub App (ID %d)", branch, want, c.cfg.AppID))
	}
	return connectors.Enforcement{Problems: problems}, nil
}

// binding reads a ruleset that requires the check from Quai's app, and says why it does not bind
// Quai's app: it is not active, or the app may bypass it. The rules of a branch name their
// ruleset but not who may bypass it.
func (p *policies) binding(ctx context.Context, id int64, name string) (string, error) {
	c := (*Connector)(p)
	set, resp, err := c.client.Repositories.GetRuleset(ctx, c.owner, c.repo, id, true)
	if why, ok := unverifiable(resp, err); ok {
		return fmt.Sprintf("the %s could not be read (%s): not verified", name, why), nil
	} else if err != nil {
		return "", check(resp, err)
	}
	if set.Name != "" {
		name = fmt.Sprintf("ruleset %q (%d)", set.Name, id)
	}
	switch bypass := set.GetCurrentUserCanBypass(); {
	case set.Enforcement != gh.RulesetEnforcementActive:
		return fmt.Sprintf("the %s is %s, not active", name, set.Enforcement), nil
	case bypass == nil:
		return fmt.Sprintf("whether Quai's GitHub App can bypass the %s could not be read: not verified", name), nil
	case *bypass != gh.BypassModeNever:
		return fmt.Sprintf("Quai's GitHub App can bypass the %s (%s): remove it from the bypass list", name, *bypass), nil
	}
	return "", nil
}

// unverifiable tells a refusal or an answer Verify cannot read, which it reports as a problem,
// from a transient error, which it returns.
func unverifiable(resp *gh.Response, err error) (string, bool) {
	if err == nil || resp == nil {
		return "", false
	}
	var primary *gh.RateLimitError
	var secondary *gh.AbuseRateLimitError
	if errors.As(err, &primary) || errors.As(err, &secondary) {
		return "", false
	}
	switch code := resp.StatusCode; {
	case code == http.StatusForbidden || code == http.StatusNotFound:
		why := fmt.Sprintf("GitHub answered %d", code)
		var e *gh.ErrorResponse
		if errors.As(err, &e) && e.Message != "" {
			why += ": " + strings.TrimSuffix(e.Message, ".")
		}
		return why, true
	case code >= 200 && code < 300:
		return "the answer could not be read: " + err.Error(), true
	}
	return "", false
}
