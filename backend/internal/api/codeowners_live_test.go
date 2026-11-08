package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v88/github"

	"github.com/damhau/quai/backend/internal/adotest"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/azuredevops"
	"github.com/damhau/quai/backend/internal/connectors/github"
	"github.com/damhau/quai/backend/internal/githubtest"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
)

// owned waits until a resource shows the owners want says, as the platform names them: a new
// rule takes a moment to show, and Quai keeps the owners it read for a minute.
func (u *user) owned(id string, want func(ResourceDetail) bool) ResourceDetail {
	u.h.t.Helper()
	var res ResourceDetail
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(3 * time.Second) {
		res = ResourceDetail{}
		if status, _ := u.do(http.MethodGet, Prefix+"/resource?id="+url.QueryEscape(id), nil, &res); status == http.StatusOK && want(res) {
			return res
		}
		if time.Now().After(deadline) {
			u.h.t.Fatalf("%s: owners %v, unmatched %v\nlog:\n%s", id, res.Owners, res.UnmatchedOwners, u.h.log.String())
		}
	}
}

// approvedBy submits a resize of key as requester, checks that the plan names the code owners as
// owners and unmatched say, that carol (no code owner) may not approve, and that approver's
// approval satisfies the rule.
func approvedBy(t *testing.T, requester, approver, carol *user, key, owners, unmatched string) {
	t.Helper()
	body := resizeBody(6) // web01 grows, db01 shrinks: neither becomes large
	body["key"] = key
	var r RequestView
	if status, b := requester.do(http.MethodPost, Prefix+"/requests/type", body, &r); status != http.StatusCreated {
		t.Fatalf("submit %s = %d %s", key, status, b)
	}
	if r.Codeowners == nil || fmt.Sprint(r.Codeowners.Owners) != owners || fmt.Sprint(r.Codeowners.Unmatched) != unmatched {
		t.Errorf("%s: codeowners %+v, want %s and %s", key, r.Codeowners, owners, unmatched)
	}
	if status, _ := carol.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusForbidden {
		t.Errorf("%s: carol approves: %d", key, status)
	}
	if status, b := approver.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, &r); status != http.StatusOK {
		t.Fatalf("%s: approve = %d %s", key, status, b)
	}
	if r.State == "pending_approval" || len(r.Rules) != 1 || !r.Rules[0].Satisfied {
		t.Errorf("%s: after the code owner's approval: %s, %+v", key, r.State, r.Rules)
	}
}

// TestAzureDevOpsCodeowners: on the sandbox, the reviewers of an "Automatically included
// reviewers" policy whose path filter matches are the code owners: the PAT's user, matched by its
// account name (its email), and a project group the repository maps approve a codeowners rule,
// and own the resources of a type owned through them; a group without a mapping is shown on the
// request. Skipped without the sandbox's PAT.
func TestAzureDevOpsCodeowners(t *testing.T) {
	sb := adotest.New(t)
	ctx := context.Background()
	me, err := azuredevops.NewClient(sb.Collection, sb.HTTP, sb.PAT).Detect(ctx)
	if err != nil || !strings.Contains(me.User.Email, "@") {
		t.Fatalf("the PAT's user: %+v, %v", me.User, err)
	}
	name, id := sb.Repository(t)
	pushEdited(t, sb.URL(name), func(ctx context.Context) (gitx.Credentials, error) {
		pat, _, _ := sb.PAT(ctx)
		return gitx.Credentials{Header: "Authorization: " + azuredevops.BasicAuth(pat.Reveal())}, nil
	}, sb.Network.Proxy, codeownersType, nil)
	contributors, readers := sb.GroupID(t, "Contributors"), sb.GroupID(t, "Readers")
	policy := map[string]any{"isEnabled": true, "isBlocking": true, "type": map[string]string{"id": "fd2167ab-b0be-447a-8ec8-39368250530e"},
		"settings": map[string]any{"requiredReviewerIds": []string{me.User.ID, contributors, readers}, "minimumApproverCount": 1, "creatorVoteCounts": true,
			"filenamePatterns": []string{"/envs/prod/*"}, "scope": []map[string]any{{"repositoryId": id, "refName": "refs/heads/main", "matchKind": "exact"}}}}
	if err := sb.Do(ctx, http.MethodPost, adotest.Path(sb.Project, "_apis", "policy", "configurations"), nil, policy, nil); err != nil {
		t.Fatal(err)
	}
	h := start(t, t.TempDir(), `connector: azure-devops
  url: `+sb.URL(name)+`
  credentials: { ref: env:ADO_PAT }
  delivery: { mode: pull-request, merge: manual }
  codeowners: { mapping: { '[`+sb.Project+`]\Contributors': group:finops } }
  sync: { minInterval: 1ns }`, repositories.Options{
		Secrets:    secrets.New(map[string]secrets.Backend{"env": sandboxPAT{sb}}),
		Connectors: &connectors.Registry{Factories: map[string]connectors.Factory{"azure-devops": azuredevops.New}, HTTP: sb.HTTP},
		Network:    sb.Network,
	}, devUser("erin", strings.ToUpper(me.User.Email), "developers"))
	repo, err := h.reg.Get("infra-live")
	if err != nil {
		t.Fatal(err)
	}
	requireApproval(t, sb, repo, id)

	bob, alice, carol, erin, fiona := h.as("bob"), h.as("alice"), h.as("carol"), h.as("erin"), h.as("fiona")
	owners, unmatched := "[email:"+strings.ToLower(me.User.Email)+" group:finops]", `[[`+sb.Project+`]\Readers]`
	res := erin.owned("infra-live/vm/prod/web01", func(r ResourceDetail) bool { return len(r.Owners) > 0 })
	if fmt.Sprint(res.Owners) != owners || fmt.Sprint(res.UnmatchedOwners) != unmatched || !res.CanOperate {
		t.Errorf("web01: owners %v, unmatched %v, erin operates %v", res.Owners, res.UnmatchedOwners, res.CanOperate)
	}
	if res := bob.owned("infra-live/vm/prod/web01", func(ResourceDetail) bool { return true }); res.CanOperate {
		t.Error("bob operates a resource the policy does not name him an owner of")
	}
	approvedBy(t, alice, erin, carol, "prod/web01", owners, unmatched) // the user, by email
	approvedBy(t, erin, fiona, carol, "prod/db01", owners, unmatched)  // the mapped group, on an owner's request
}

// TestGitHubCodeowners: on the sandbox, the owners CODEOWNERS names for the changed paths, a
// member (its admin) and a team, are the code owners: the team, mapped to a Quai group, approves a
// codeowners rule and owns the resources of a type owned through CODEOWNERS; the member, without a
// mapping and without an email in the organization's verified domains (the sandbox has none), is
// shown on the request. Mapped too, the member counts. Skipped without the sandbox's key.
func TestGitHubCodeowners(t *testing.T) {
	s := githubtest.New(t)
	ctx := context.Background()
	admins, _, err := s.Client.Organizations.ListMembers(ctx, s.Org, &gh.ListMembersOptions{Role: "admin"})
	if err != nil || len(admins) == 0 {
		t.Fatalf("the organization's admins: %d, %v", len(admins), err)
	}
	member, team := admins[0].GetLogin(), s.Org+"/"+githubtest.Team
	name, _ := s.Repository(t)
	c, err := github.New(connectors.Config{URL: s.URL(name), HTTP: s.HTTP, Credentials: s.PrivateKey, AppID: s.AppID})
	if err != nil {
		t.Fatal(err)
	}
	pushEdited(t, s.URL(name), c.(*github.Connector).GitAuth, s.Network.Proxy, codeownersType,
		map[string]string{".github/CODEOWNERS": "/envs/prod/ @" + member + " @" + team + "\n"})
	if err := s.AddTeam(ctx, name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequireCheck(ctx, name, "quai/approval"); err != nil {
		t.Fatal(err)
	}
	spec := func(mapping string) string {
		return `connector: github
  url: ` + s.URL(name) + `
  credentials: { ref: env:GITHUB_KEY, appId: ` + strconv.FormatInt(s.AppID, 10) + ` }
  delivery: { mode: pull-request, merge: manual }
  codeowners: { mapping: { ` + mapping + ` } }
  sync: { minInterval: 1ns }`
	}
	h := start(t, t.TempDir(), spec("'@"+team+"': group:finops"), githubOptions(s), devUser("erin", "", "developers"))
	bob, alice, carol, erin, fiona := h.as("bob"), h.as("alice"), h.as("carol"), h.as("erin"), h.as("fiona")

	res := fiona.owned("infra-live/vm/prod/web01", func(r ResourceDetail) bool { return len(r.Owners) > 0 })
	if fmt.Sprint(res.Owners) != "[group:finops]" || fmt.Sprint(res.UnmatchedOwners) != "[@"+member+"]" || !res.CanOperate {
		t.Errorf("web01: owners %v, unmatched %v, fiona operates %v", res.Owners, res.UnmatchedOwners, res.CanOperate)
	}
	if res := bob.owned("infra-live/vm/prod/web01", func(ResourceDetail) bool { return true }); res.CanOperate {
		t.Error("bob operates a resource CODEOWNERS does not name him an owner of")
	}
	approvedBy(t, alice, fiona, carol, "prod/web01", "[group:finops]", "[@"+member+"]")

	// The member mapped to erin: erin owns the resources and approves.
	docs, err := repositories.Parse([]byte("apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: infra-live, title: Infrastructure }\nspec:\n  " +
		spec("'@"+team+"': group:finops, '@"+strings.ToUpper(member)+"': user:erin") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repositories.Reconcile(ctx, h.store, docs); err != nil {
		t.Fatal(err)
	}
	if err := h.reg.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	res = erin.owned("infra-live/vm/prod/db01", func(r ResourceDetail) bool { return len(r.Owners) == 2 })
	if fmt.Sprint(res.Owners) != "[group:finops user:erin]" || len(res.UnmatchedOwners) != 0 || !res.CanOperate {
		t.Errorf("db01: owners %v, unmatched %v, erin operates %v", res.Owners, res.UnmatchedOwners, res.CanOperate)
	}
	approvedBy(t, alice, erin, carol, "prod/db01", "[group:finops user:erin]", "[]")
}
