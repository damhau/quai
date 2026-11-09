package github

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v88/github"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/conformance"
	"github.com/damhau/quai/backend/internal/githubtest"
)

const quaiCheck = conformance.Check

// verified waits until Verify reports want: "" is enforced, anything else a problem containing
// it. GitHub shows a change to its rules a moment after the call that made it, if at all.
func (r *liveRepo) verified(t *testing.T, what, want string) {
	t.Helper()
	var e connectors.Enforcement
	var err error
	for deadline := time.Now().Add(conformance.Timeout); ; time.Sleep(2 * time.Second) {
		e, err = r.c.Policies().Verify(t.Context(), githubtest.Branch, quaiCheck)
		if err == nil && (want == "" && e.Enforced && len(e.Problems) == 0 ||
			want != "" && !e.Enforced && slices.ContainsFunc(e.Problems, func(p string) bool { return strings.Contains(p, want) })) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: verify = %+v, %v; want %q", what, e, err, want)
		}
	}
}

// set publishes quai/approval on the pull request's head.
func (r *liveRepo) set(t *testing.T, pr connectors.PullRequest, state connectors.StatusState) {
	t.Helper()
	if err := r.c.Statuses().Set(t.Context(), connectors.StatusTarget{PullRequest: pr.ID, Commit: pr.Head},
		connectors.Status{Name: quaiCheck, State: state, Description: "Quai says " + string(state), URL: "https://quai.example.com/requests/1"}); err != nil {
		t.Fatal(err)
	}
}

// approval returns what List reads of quai/approval on the pull request.
func (r *liveRepo) approval(t *testing.T, pr connectors.PullRequest) connectors.Status {
	t.Helper()
	list, err := r.c.Statuses().List(t.Context(), connectors.StatusTarget{PullRequest: pr.ID, Commit: pr.Head})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range list {
		if st.Name == quaiCheck {
			return st
		}
	}
	return connectors.Status{}
}

// approvalShows waits until List reads the state last set of quai/approval and returns it. GitHub
// may list a status a moment after accepting it: the one set before it was read (live, 2026-11-09).
func (r *liveRepo) approvalShows(t *testing.T, pr connectors.PullRequest, want connectors.StatusState) connectors.Status {
	t.Helper()
	for deadline := time.Now().Add(conformance.Timeout); ; time.Sleep(2 * time.Second) {
		st := r.approval(t, pr)
		if st.State == want || time.Now().After(deadline) {
			return st
		}
	}
}

// TestLiveRuleset: a ruleset requiring quai/approval from the app enforces it, for the app too.
// The latest check run counts: a stale approval after a success blocks the merge again. Verify
// finds the ruleset made to accept any source, bypassed by the app, disabled, restored, removed
// and created again.
func TestLiveRuleset(t *testing.T) {
	s := githubtest.New(t)
	ctx := t.Context()
	r := newLiveRepo(t, s)
	r.verified(t, "no ruleset", "no branch protection or ruleset on main requires quai/approval")
	id, err := s.RequireCheck(ctx, r.name, quaiCheck)
	if err != nil {
		t.Fatal(err)
	}
	r.verified(t, "the ruleset", "")
	p, err := r.c.Policies().Protection(ctx, githubtest.Branch)
	if err != nil || !p.Protected || !slices.Equal(p.RequiredChecks, []string{quaiCheck}) || !p.ChecksRestricted || !p.ResetOnPush || p.CodeOwnerReview {
		t.Errorf("protection = %+v, %v", p, err)
	}

	pr := r.open(t, connectors.NewPullRequest{})
	merge := func() error {
		_, err := r.prs.Merge(ctx, pr.ID, connectors.MergeOptions{Head: pr.Head, Method: connectors.Squash, Message: "Merge\n", DeleteBranch: true})
		return err
	}
	r.set(t, pr, connectors.StatusSuccess)
	r.set(t, pr, connectors.StatusNotApplicable)
	if st := r.approvalShows(t, pr, connectors.StatusNotApplicable); st.State != connectors.StatusNotApplicable || st.Description != "Quai says not-applicable" || st.URL != "https://quai.example.com/requests/1" {
		t.Errorf("after success then not-applicable, read %+v", st)
	}
	if err := merge(); !errors.Is(err, connectors.ErrBlocked) {
		t.Errorf("merging a stale approval: %v", err)
	}
	r.set(t, pr, connectors.StatusPending)
	if st := r.approvalShows(t, pr, connectors.StatusPending); st.State != connectors.StatusPending {
		t.Errorf("pending, read %+v", st)
	}
	if err := merge(); !errors.Is(err, connectors.ErrBlocked) {
		t.Errorf("merging a pending approval: %v", err)
	}
	r.set(t, pr, connectors.StatusFailure)
	if err := merge(); !errors.Is(err, connectors.ErrBlocked) {
		t.Errorf("merging a failed approval: %v", err)
	}

	update := func(set gh.RepositoryRuleset) {
		t.Helper()
		if _, _, err := s.Client.Repositories.UpdateRuleset(ctx, s.Org, r.name, id, set); err != nil {
			t.Fatal(err)
		}
	}
	anySource := s.ApprovalRuleset(quaiCheck)
	anySource.Rules.RequiredStatusChecks.RequiredStatusChecks[0].IntegrationID = nil
	update(anySource)
	r.verified(t, "any source", "accepts quai/approval from any source: set its source to Quai's GitHub App")
	bypassed := s.ApprovalRuleset(quaiCheck)
	bypassed.BypassActors = []*gh.BypassActor{{ActorID: gh.Ptr(s.AppID), ActorType: gh.Ptr(gh.BypassActorTypeIntegration), BypassMode: gh.Ptr(gh.BypassModeAlways)}}
	update(bypassed)
	r.verified(t, "the app bypasses it", "Quai's GitHub App can bypass the ruleset \"quai\"")
	disabled := s.ApprovalRuleset(quaiCheck)
	disabled.Enforcement = gh.RulesetEnforcementDisabled
	update(disabled)
	r.verified(t, "disabled", "no branch protection or ruleset on main requires quai/approval")
	update(s.ApprovalRuleset(quaiCheck))
	r.verified(t, "restored", "")

	r.set(t, pr, connectors.StatusSuccess)
	if err := merge(); err != nil {
		t.Errorf("merging an approval: %v", err)
	}

	if _, err := s.Client.Repositories.DeleteRuleset(ctx, s.Org, r.name, id); err != nil {
		t.Fatal(err)
	}
	r.verified(t, "removed", "no branch protection or ruleset on main requires quai/approval")
	if _, err := s.RequireCheck(ctx, r.name, quaiCheck); err != nil {
		t.Fatal(err)
	}
	r.verified(t, "created again", "")
}

// TestLiveBranchProtection: a branch protection requiring quai/approval from the app enforces it
// only when it applies to administrators, since the app is one; Verify finds it accepting any
// app, removed and restored.
func TestLiveBranchProtection(t *testing.T) {
	s := githubtest.New(t)
	ctx := t.Context()
	r := newLiveRepo(t, s)
	protect := func(app int64, admins bool) {
		t.Helper()
		req := &gh.ProtectionRequest{EnforceAdmins: admins,
			RequiredStatusChecks: &gh.RequiredStatusChecks{Checks: &[]*gh.RequiredStatusCheck{{Context: quaiCheck, AppID: gh.Ptr(app)}}}}
		if _, _, err := s.Client.Repositories.UpdateBranchProtection(ctx, s.Org, r.name, githubtest.Branch, req); err != nil {
			t.Fatal(err)
		}
	}
	merge := func(pr connectors.PullRequest) error {
		_, err := r.prs.Merge(ctx, pr.ID, connectors.MergeOptions{Head: pr.Head, Method: connectors.Squash, Message: "Merge\n", DeleteBranch: true})
		return err
	}

	protect(s.AppID, true)
	r.verified(t, "pinned, administrators included", "")
	p, err := r.c.Policies().Protection(ctx, githubtest.Branch)
	if err != nil || !p.Protected || !slices.Equal(p.RequiredChecks, []string{quaiCheck}) || !p.ChecksRestricted || !p.ResetOnPush {
		t.Errorf("protection = %+v, %v", p, err)
	}
	pr := r.open(t, connectors.NewPullRequest{})
	r.set(t, pr, connectors.StatusPending)
	if err := merge(pr); !errors.Is(err, connectors.ErrBlocked) {
		t.Errorf("merging a pending approval: %v", err)
	}
	r.set(t, pr, connectors.StatusSuccess)
	if err := merge(pr); err != nil {
		t.Errorf("merging an approval: %v", err)
	}

	// Without administrators, the app merges a pending approval: why Verify asks for them.
	protect(s.AppID, false)
	r.verified(t, "administrators bypass it", "does not apply to administrators")
	pr = r.open(t, connectors.NewPullRequest{})
	r.set(t, pr, connectors.StatusPending)
	if err := merge(pr); err != nil {
		t.Errorf("GitHub no longer lets the app bypass a protection that spares administrators, and Verify can count it: %v", err)
	}

	protect(-1, true)
	r.verified(t, "any app", "the branch protection of main accepts quai/approval from any source")
	if _, err := s.Client.Repositories.RemoveBranchProtection(ctx, s.Org, r.name, githubtest.Branch); err != nil {
		t.Fatal(err)
	}
	r.verified(t, "removed", "no branch protection or ruleset on main requires quai/approval")
	protect(s.AppID, true)
	r.verified(t, "restored", "")
}

// TestLiveOwners reads a CODEOWNERS file as GitHub applies it: without the owners GitHub does not
// know or who lack write access, nor the line it cannot read; the last matching line counts.
func TestLiveOwners(t *testing.T) {
	s := githubtest.New(t)
	ctx := t.Context()
	r := newLiveRepo(t, s)
	if rules, err := r.c.Owners().Rules(ctx, githubtest.Branch); err != nil || rules != nil {
		t.Errorf("no CODEOWNERS: %v, %v", rules, err)
	}
	if err := s.AddTeam(ctx, r.name); err != nil {
		t.Fatal(err)
	}
	team := s.Org + "/" + githubtest.Team
	src := "# Owners\n* @" + team + "\n/.quai/ @" + team + " @nobody-quai-xyz-404\n/.quai/blueprints/\n/unknown/ @nobody-quai-xyz-404\n" +
		"/no-access/ @octocat\n/bad[/ @" + team + "\n/mail/ someone@example.com  # an address GitHub does not know\n"
	if _, err := s.AddFiles(ctx, r.name, githubtest.Branch, "Owners", map[string]string{".github/CODEOWNERS": src}); err != nil {
		t.Fatal(err)
	}
	var rules []connectors.OwnerRule
	var err error
	for deadline := time.Now().Add(conformance.Timeout); ; time.Sleep(2 * time.Second) {
		// GitHub checks a new file after a moment; until then, Rules says so.
		if rules, err = r.c.Owners().Rules(ctx, githubtest.Branch); err == nil {
			break
		}
		if !strings.Contains(err.Error(), "has not checked .github/CODEOWNERS") || time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	var got []string
	for _, rule := range rules {
		got = append(got, rule.Pattern+" "+strings.Join(rule.Owners, ","))
	}
	if want := []string{"* " + team, "/.quai/ " + team, "/.quai/blueprints/ ", "/unknown/ ", "/no-access/ ", "/mail/ "}; !slices.Equal(got, want) {
		t.Errorf("rules = %q, want %q", got, want)
	}
	for path, want := range map[string]bool{
		"README.md": true, ".quai/types/x.yaml": true, ".quai/blueprints/x.yaml": false, "unknown/x": false, "no-access/x": false, "bad/x": true, "mail/x": false,
	} {
		if got := connectors.Covered(rules, path); got != want {
			t.Errorf("%s covered = %v, want %v", path, got, want)
		}
	}
}
