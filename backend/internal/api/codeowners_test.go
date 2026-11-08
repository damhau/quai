package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/config"
	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/giteatest"
)

// codeownersType makes the example's vm type owned through CODEOWNERS, with production changes
// approved by the code owners.
func codeownersType(p string, b []byte) []byte {
	if p != ".quai/types/vm.yaml" {
		return b
	}
	s := strings.Replace(string(b), "ownership: { field: owner }", "ownership: { codeowners: true }", 1)
	return []byte(strings.Replace(s, "approvers: [group:platform-admins]", "approvers: [codeowners]", 1))
}

// devUser adds a user to the development switcher, with an email.
func devUser(name, email string, groups ...string) func(*config.Config) {
	return func(c *config.Config) {
		c.Auth.Dev.Users = append(c.Auth.Dev.Users, config.DevUser{Name: name, Email: email, Groups: groups})
	}
}

// TestCodeowners: on Gitea, a rule whose approvers are codeowners is satisfied by an owner the
// CODEOWNERS file names, a user matched by the email Gitea gives or a team the repository maps to a
// Quai group, and by no one else; a team without a mapping and an unknown user match no one and are
// shown on the request. A type owned through CODEOWNERS is owned by the same owners.
func TestCodeowners(t *testing.T) {
	t.Parallel()
	g := giteatest.Get(t)
	owner := g.User(t, "erin") // a Gitea user whose email Quai's erin has
	file := fmt.Sprintf("envs/prod/.* @%s @acme/dba @acme/ops @nobody-%s\n", owner.Name, owner.Name)
	h, _ := giteaHarnessOver(t, "auto", "\n  codeowners: { mapping: { '@ACME/dba': group:finops } }", codeownersType,
		map[string]string{".gitea/CODEOWNERS": file}, giteaconn.New, devUser("erin", strings.ToUpper(owner.Name)+"@Example.com", "developers"))
	bob, alice, carol, erin, fiona := h.as("bob"), h.as("alice"), h.as("carol"), h.as("erin"), h.as("fiona")
	email := "email:" + owner.Name + "@example.com"
	unmatched := fmt.Sprintf("[@acme/ops @nobody-%s]", owner.Name)

	// The resource is owned by the owners CODEOWNERS names for its path, not by its owner field.
	var res ResourceDetail
	if status, body := erin.do(http.MethodGet, Prefix+"/resource?id="+url.QueryEscape("infra-live/vm/prod/web01"), nil, &res); status != http.StatusOK {
		t.Fatalf("resource = %d %s", status, body)
	}
	if res.Owner != "" || fmt.Sprint(res.Owners) != "["+email+" group:finops]" || fmt.Sprint(res.UnmatchedOwners) != unmatched || !res.CanOperate {
		t.Errorf("resource owners: %q %v %v, can operate %v", res.Owner, res.Owners, res.UnmatchedOwners, res.CanOperate)
	}
	bob.do(http.MethodGet, Prefix+"/resource?id="+url.QueryEscape("infra-live/vm/prod/web01"), nil, &res)
	if res.CanOperate {
		t.Error("bob, the owner field's, operates a resource owned through CODEOWNERS")
	}
	plan := resizeBody(6)
	delete(plan, "reason")
	if status, _ := bob.do(http.MethodPost, Prefix+"/plans/type", plan, nil); status != http.StatusForbidden {
		t.Errorf("bob plans a change to a resource he does not own: %d", status)
	}

	// A platform admin's request: the plan names the code owners, those that match no one too.
	var p PlanView
	if status, body := alice.do(http.MethodPost, Prefix+"/plans/type", plan, &p); status != http.StatusOK {
		t.Fatalf("plan = %d %s", status, body)
	}
	if p.Codeowners == nil || fmt.Sprint(p.Codeowners.Owners) != "["+email+" group:finops]" || fmt.Sprint(p.Codeowners.Unmatched) != unmatched {
		t.Errorf("plan codeowners = %+v", p.Codeowners)
	}
	var r RequestView
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/type", resizeBody(6), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	if r.Codeowners == nil || fmt.Sprint(r.Codeowners.Unmatched) != unmatched || r.State != "pending_approval" {
		t.Errorf("request: %s, codeowners %+v", r.State, r.Codeowners)
	}
	// Someone CODEOWNERS does not name may not approve.
	carol.do(http.MethodGet, Prefix+"/requests/"+r.ID, nil, &r)
	if r.CanReview {
		t.Error("carol may review")
	}
	if status, _ := carol.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusForbidden {
		t.Errorf("carol approves: %d", status)
	}
	// The owner Gitea names, matched by email, approves; the rule is satisfied.
	erin.do(http.MethodGet, Prefix+"/requests/"+r.ID, nil, &r)
	if !r.CanReview {
		t.Error("erin, a code owner, may not review")
	}
	if status, body := erin.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, &r); status != http.StatusOK {
		t.Fatalf("erin approves: %d %s", status, body)
	}
	if r.State == "pending_approval" || len(r.Rules) != 1 || !r.Rules[0].Satisfied || fmt.Sprint(r.Rules[0].Approvals) != "[erin]" {
		t.Errorf("after erin's approval: %s, %+v", r.State, r.Rules)
	}

	// An owner submits a change to a resource they own; a member of a mapped team approves.
	db01 := resizeBody(4)
	db01["key"] = "prod/db01"
	if status, body := erin.do(http.MethodPost, Prefix+"/requests/type", db01, &r); status != http.StatusCreated {
		t.Fatalf("erin's request = %d %s", status, body)
	}
	if status, body := fiona.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, &r); status != http.StatusOK {
		t.Fatalf("fiona approves: %d %s", status, body)
	}
	if r.State == "pending_approval" || fmt.Sprint(r.Rules[0].Approvals) != "[fiona]" {
		t.Errorf("after fiona's approval: %s, %+v", r.State, r.Rules)
	}
}
