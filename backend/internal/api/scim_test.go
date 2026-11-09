package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/graphtest"
	"github.com/damhau/quai/backend/internal/mailtest"
	"github.com/damhau/quai/backend/internal/oidctest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/scim"
	"github.com/damhau/quai/backend/internal/secrets"
)

// The SCIM tests send the requests Microsoft Entra ID's provisioning service sends, as Microsoft
// documents them (no tenant: docs/plan/phase-4-backlog.md#test-infrastructure), read on
// 2026-11-09:
//   - https://learn.microsoft.com/en-us/entra/identity/app-provisioning/use-scim-to-provision-users-and-groups
//     (the requests and the answers it expects, the test connection, the manager reference check),
//   - https://learn.microsoft.com/en-us/entra/identity/app-provisioning/entra-id-scim-api-schema-documentation,
//   - https://learn.microsoft.com/en-us/entra/identity/app-provisioning/application-provisioning-config-problem-scim-compatibility
//     (booleans as strings, PATCH without a path, members removed by a value filter, aadOptscim062020).
// Not verified against the real service.

// The tests' SCIM tokens: entra's, and the next one, while rotating.
var (
	scimToken = scim.TokenPrefix + strings.Repeat("5c", 32)
	scimNext  = scim.TokenPrefix + strings.Repeat("a7", 32)
)

const patchOp = "urn:ietf:params:scim:api:messages:2.0:PatchOp"

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// scimHarness is a harness where people sign in with an identity provider whose provisioning
// calls SCIM with the tokens entra and next; breakglass@contoso.com is reserved. The vm type's
// large-vm rule waits for the requester's manager.
func scimHarness(t *testing.T, configure ...func(*config.Config)) (*harness, *oidctest.Provider) {
	t.Helper()
	sec := secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(name string) (string, bool) {
		switch name {
		case "SCIM_ENTRA":
			return sha256Hex(scimToken), true
		case "SCIM_NEXT":
			return sha256Hex(scimNext), true
		}
		return "", false
	}}})
	h, p, _ := oidcHarness(t, repositories.Options{Secrets: sec}, func(c *config.Config, _ *graphtest.Graph) {
		c.Auth.SCIM = &config.SCIM{GroupName: config.SCIMGroupExternalID, Reserved: []string{"breakglass@contoso.com"},
			Tokens: []config.SCIMToken{{Name: "entra", SHA256: config.SecretRef{Ref: "env:SCIM_ENTRA"}}, {Name: "next", SHA256: config.SecretRef{Ref: "env:SCIM_NEXT"}}}}
		for _, f := range configure {
			f(c)
		}
	})
	vm, err := os.ReadFile(filepath.Join(h.work, ".quai/types/vm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const large = "        approvers: [group:finops]\n"
	if strings.Count(string(vm), large) != 1 {
		t.Fatal("the example vm type changed its large-vm rule")
	}
	h.push(".quai/types/vm.yaml", strings.Replace(string(vm), large, "        approvers: [manager]\n", 1))
	return h, p
}

// scimClient calls SCIM as the identity provider's provisioning does.
type scimClient struct {
	h     *harness
	token string
}

func (h *harness) provisioning(token string) scimClient { return scimClient{h: h, token: token} }

// do sends a SCIM request, with a body marshalled to JSON or a string sent as it is, and decodes
// a SCIM answer into out.
func (c scimClient) do(method, path string, body, out any) (int, string) {
	c.h.t.Helper()
	return c.send(method, path, "application/scim+json", body, out)
}

func (c scimClient) send(method, path, contentType string, body, out any) (int, string) {
	c.h.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	case []byte:
		r = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		r = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, c.h.srv.URL+path, r)
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 && resp.Header.Get("Content-Type") != "application/scim+json" {
		c.h.t.Fatalf("%s %s: a %q answer: %s", method, path, resp.Header.Get("Content-Type"), raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.h.t.Fatalf("%s %s: %v\n%s", method, path, err, raw)
		}
	}
	return resp.StatusCode, string(raw)
}

// entraUser is a user as Microsoft Entra ID's provisioning creates them (its documented request),
// externalId mapped to the object ID.
func entraUser(userName, oid string) map[string]any {
	return map[string]any{
		"schemas":    []string{"urn:ietf:params:scim:schemas:core:2.0:User", "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"},
		"externalId": oid, "userName": userName, "active": true,
		"emails": []map[string]any{{"primary": true, "type": "work", "value": userName}},
		"meta":   map[string]any{"resourceType": "User"},
		"name":   map[string]any{"formatted": "givenName familyName", "familyName": "familyName", "givenName": "givenName"},
		"roles":  []any{},
	}
}

type scimUser struct {
	ID         string `json:"id"`
	ExternalID string `json:"externalId"`
	UserName   string `json:"userName"`
	Active     bool   `json:"active"`
	Enterprise *struct {
		Manager *struct {
			Value string `json:"value"`
		} `json:"manager"`
	} `json:"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"`
	Meta struct {
		ResourceType string `json:"resourceType"`
		Location     string `json:"location"`
	} `json:"meta"`
}

func (u scimUser) manager() string {
	if u.Enterprise == nil || u.Enterprise.Manager == nil {
		return ""
	}
	return u.Enterprise.Manager.Value
}

type scimGroup struct {
	ID          string `json:"id"`
	ExternalID  string `json:"externalId"`
	DisplayName string `json:"displayName"`
	Members     *[]struct {
		Value string `json:"value"`
	} `json:"members"`
}

type scimList[T any] struct {
	TotalResults int `json:"totalResults"`
	StartIndex   int `json:"startIndex"`
	ItemsPerPage int `json:"itemsPerPage"`
	Resources    []T `json:"Resources"`
}

type scimError struct {
	Schemas  []string `json:"schemas"`
	Status   string   `json:"status"`
	ScimType string   `json:"scimType"`
	Detail   string   `json:"detail"`
}

// createUser provisions a user as Entra ID does: it looks them up by userName first.
func (c scimClient) createUser(userName, oid string) string {
	c.h.t.Helper()
	var found scimList[scimUser]
	if status, body := c.do(http.MethodGet, scimFilter("Users", `userName eq "`+userName+`"`), nil, &found); status != http.StatusOK || found.TotalResults != 0 {
		c.h.t.Fatalf("look up %s = %d %s", userName, status, body)
	}
	var u scimUser
	if status, body := c.do(http.MethodPost, "/scim/v2/Users", entraUser(userName, oid), &u); status != http.StatusCreated || u.ID == "" {
		c.h.t.Fatalf("create %s = %d %s", userName, status, body)
	}
	return u.ID
}

func (c scimClient) createGroup(externalID, displayName string) string {
	c.h.t.Helper()
	var g scimGroup
	if status, body := c.do(http.MethodPost, "/scim/v2/Groups", map[string]any{
		"schemas":    []string{"urn:ietf:params:scim:schemas:core:2.0:Group", "http://schemas.microsoft.com/2006/11/ResourceManagement/ADSCIM/2.0/Group"},
		"externalId": externalID, "displayName": displayName, "meta": map[string]any{"resourceType": "Group"},
	}, &g); status != http.StatusCreated || g.ID == "" {
		c.h.t.Fatalf("create group %s = %d %s", displayName, status, body)
	}
	return g.ID
}

// patch sends a PATCH of operations, as Entra ID does.
func (c scimClient) patch(path string, ops ...map[string]any) (int, string) {
	c.h.t.Helper()
	return c.do(http.MethodPatch, path, map[string]any{"schemas": []string{patchOp}, "Operations": ops}, nil)
}

func (c scimClient) mustPatch(path string, want int, ops ...map[string]any) {
	c.h.t.Helper()
	if status, body := c.patch(path, ops...); status != want {
		c.h.t.Fatalf("PATCH %s %v = %d %s", path, ops, status, body)
	}
}

func (c scimClient) user(id string) scimUser {
	c.h.t.Helper()
	var u scimUser
	if status, body := c.do(http.MethodGet, "/scim/v2/Users/"+id, nil, &u); status != http.StatusOK {
		c.h.t.Fatalf("user %s = %d %s", id, status, body)
	}
	return u
}

func scimFilter(resource, filter string, query ...string) string {
	return "/scim/v2/" + resource + "?" + strings.Join(append(query, "filter="+url.QueryEscape(filter)), "&")
}

func op(name, path string, value any) map[string]any {
	o := map[string]any{"op": name}
	if path != "" {
		o["path"] = path
	}
	if value != nil {
		o["value"] = value
	}
	return o
}

func members(ids ...string) []map[string]any {
	out := []map[string]any{}
	for _, id := range ids {
		out = append(out, map[string]any{"$ref": nil, "value": id})
	}
	return out
}

// entraSignIn signs in through the identity provider as Entra ID would describe the person.
func (h *harness) entraSignIn(p *oidctest.Provider, name, oid string, groups ...string) *user {
	h.t.Helper()
	return h.signIn(p, entraClaims(name, oid, groups...))
}

func entraClaims(name, oid string, groups ...string) map[string]any {
	return map[string]any{"preferred_username": name, "sub": "sub-" + oid, "oid": oid, "groups": groups, "email": name, "email_verified": true}
}

// trySignIn signs in through the identity provider, and returns the browser, the status of the
// sign-in's last page and its body: the browser is signed in when /me answers.
func (h *harness) trySignIn(p *oidctest.Provider, claims map[string]any) (*user, int, string) {
	h.t.Helper()
	p.SetClaims(claims)
	jar, _ := cookiejar.New(nil)
	u := &user{h: h, c: &http.Client{Jar: jar, Transport: p.Transport()}}
	resp, err := u.c.Get(h.srv.URL + "/auth/login")
	if err != nil {
		h.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var me struct{ CSRFToken string }
	status, _ := u.do(http.MethodGet, Prefix+"/me", nil, &me)
	u.csrf = me.CSRFToken
	if status == http.StatusOK {
		return u, status, string(body)
	}
	return u, resp.StatusCode, string(body)
}

func (u *user) me() (int, MeOutput) {
	u.h.t.Helper()
	var me MeOutput
	status, _ := u.do(http.MethodGet, Prefix+"/me", nil, &me.Body)
	return status, me
}

// token makes an API token for the user.
func (u *user) token(name string, scopes ...string) string {
	u.h.t.Helper()
	var tok CreatedTokenOutput
	if status, body := u.do(http.MethodPost, Prefix+"/tokens", map[string]any{"name": name, "scopes": scopes}, &tok.Body); status != http.StatusCreated {
		u.h.t.Fatalf("token %s = %d %s", name, status, body)
	}
	return tok.Body.Token
}

// withToken calls the API with an API token.
func (h *harness) withToken(token, method, path string, out any) int {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode == http.StatusOK {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode
}

// auditActions returns the audit events of the actions named: "action actorType:actorID" each,
// in order.
func (h *harness) auditActions(actions ...string) []string {
	h.t.Helper()
	rows, err := h.store.Pool.Query(context.Background(),
		`SELECT action, actor->>'type', actor->>'id' FROM audit_events WHERE action = ANY($1) ORDER BY seq`, actions)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var action, typ, id string
		if err := rows.Scan(&action, &typ, &id); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, action+" "+typ+":"+id)
	}
	return out
}

// TestSCIMEntraSequence: Microsoft Entra ID's provisioning, as Microsoft documents its requests
// (docs/design/08-identity-access-tenancy.md#scim-provisioning): the test connection, users looked
// up by userName then created, a duplicate refused, the manager added and checked as a reference,
// attributes Quai does not keep sent in Entra's forms (paths with filters, PATCH without path,
// booleans as strings), groups created empty, members added and removed (both forms) and
// checked, a rename, a user disabled then deleted, pages. What Quai does not take is refused
// with SCIM's errors. Everything it changes is the provisioning's in the audit log.
func TestSCIMEntraSequence(t *testing.T) {
	t.Parallel()
	h, _ := scimHarness(t)
	entra := h.provisioning(scimToken)
	oid := map[string]string{}
	for _, n := range []string{"alice", "mia", "bob", "carol"} {
		oid[n] = uuid.NewString()
	}

	// Test Connection: a user and a group that do not exist; the tenant URL's feature flag.
	var none scimList[scimUser]
	if status, body := entra.do(http.MethodGet, scimFilter("Users", `userName eq "`+uuid.NewString()+`"`, "aadOptscim062020"), nil, &none); status != http.StatusOK ||
		none.TotalResults != 0 || none.Resources == nil || none.StartIndex != 1 {
		t.Fatalf("test connection = %d %s", status, body)
	}
	var noGroup scimList[scimGroup]
	if status, body := entra.do(http.MethodGet, scimFilter("Groups", `displayName eq "Platform admins"`, "excludedAttributes=members"), nil, &noGroup); status != http.StatusOK || noGroup.TotalResults != 0 {
		t.Fatalf("group lookup = %d %s", status, body)
	}
	var spc map[string]any
	if status, body := entra.do(http.MethodGet, "/scim/v2/ServiceProviderConfig", nil, &spc); status != http.StatusOK || spc["patch"].(map[string]any)["supported"] != true {
		t.Fatalf("service provider config = %d %s", status, body)
	}
	for _, path := range []string{"/scim/v2/ResourceTypes", "/scim/v2/Schemas"} {
		var l scimList[map[string]any]
		if status, body := entra.do(http.MethodGet, path, nil, &l); status != http.StatusOK || l.TotalResults == 0 {
			t.Errorf("%s = %d %s", path, status, body)
		}
	}

	// The initial cycle: users looked up, then created.
	alice, mia := entra.createUser("alice@contoso.com", oid["alice"]), entra.createUser("mia@contoso.com", oid["mia"])
	var dup scimError
	if status, body := entra.do(http.MethodPost, "/scim/v2/Users", entraUser("alice@contoso.com", oid["alice"]), &dup); status != http.StatusConflict ||
		dup.ScimType != "uniqueness" || dup.Status != "409" || !slices.Contains(dup.Schemas, "urn:ietf:params:scim:api:messages:2.0:Error") {
		t.Errorf("duplicate = %d %s", status, body)
	}
	if u := entra.user(alice); u.UserName != "alice@contoso.com" || u.ExternalID != oid["alice"] || !u.Active || u.Meta.ResourceType != "User" ||
		u.Meta.Location != "https://quai.example.com/scim/v2/Users/"+alice {
		t.Errorf("alice = %+v", u)
	}
	var missing scimError
	if status, body := entra.do(http.MethodGet, "/scim/v2/Users/"+uuid.NewString(), nil, &missing); status != http.StatusNotFound || missing.Status != "404" {
		t.Errorf("unknown user = %d %s", status, body)
	}
	for _, f := range []string{`USERNAME Eq "ALICE@CONTOSO.COM"`, `externalId eq "` + oid["alice"] + `"`, `id eq "` + alice + `"`} {
		var l scimList[scimUser]
		if status, body := entra.do(http.MethodGet, scimFilter("Users", f), nil, &l); status != http.StatusOK || l.TotalResults != 1 || l.Resources[0].ID != alice {
			t.Errorf("filter %s = %d %s", f, status, body)
		}
	}

	// The manager, added as a reference, and checked as Entra checks references.
	entra.mustPatch("/scim/v2/Users/"+alice, http.StatusOK, op("Add", "manager", []map[string]any{{"$ref": "https://quai.example.com/scim/v2/Users/" + mia, "value": mia}}))
	if m := entra.user(alice).manager(); m != mia {
		t.Errorf("alice's manager = %q", m)
	}
	var ref scimList[scimUser]
	if status, body := entra.do(http.MethodGet, scimFilter("Users", `id eq "`+alice+`" and manager eq "`+mia+`"`), nil, &ref); status != http.StatusOK || ref.TotalResults != 1 {
		t.Errorf("manager reference = %d %s", status, body)
	}
	entra.mustPatch("/scim/v2/Users/"+alice, http.StatusOK, op("Replace", "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:manager", mia))
	// Attributes Quai does not keep, in Entra's forms: taken, not kept.
	entra.mustPatch("/scim/v2/Users/"+alice, http.StatusOK,
		op("Replace", `emails[type eq "work"].value`, "updatedEmail@contoso.com"),
		op("Replace", "name.familyName", "updatedFamilyName"),
		op("Replace", "", map[string]any{"displayName": "Alice A", "name.givenName": "Alice", "active": "True",
			"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:employeeNumber": "42"}))
	if status, body := entra.do(http.MethodGet, "/scim/v2/Users/"+alice, nil, nil); status != http.StatusOK || strings.Contains(body, "updatedEmail") || strings.Contains(body, "Alice A") {
		t.Errorf("alice keeps what Quai does not = %d %s", status, body)
	}

	// Groups: created empty, members added, checked, removed both ways, renamed.
	admins := entra.createGroup("platform-admins", "Platform admins")
	var dupGroup scimError
	if status, body := entra.do(http.MethodPost, "/scim/v2/Groups", map[string]any{"displayName": "PLATFORM ADMINS"}, &dupGroup); status != http.StatusConflict || dupGroup.ScimType != "uniqueness" {
		t.Errorf("duplicate group = %d %s", status, body)
	}
	if status, body := entra.patch("/scim/v2/Groups/"+admins, op("Add", "members", members(alice, mia))); status != http.StatusNoContent || body != "" {
		t.Fatalf("add members = %d %s", status, body)
	}
	if status, body := entra.do(http.MethodGet, "/scim/v2/Groups/"+admins+"?excludedAttributes=members", nil, nil); status != http.StatusOK || strings.Contains(body, "members") {
		t.Errorf("group without members = %d %s", status, body)
	}
	memberIDs := func() []string {
		t.Helper()
		var g scimGroup
		if status, body := entra.do(http.MethodGet, "/scim/v2/Groups/"+admins, nil, &g); status != http.StatusOK || g.Members == nil {
			t.Fatalf("group = %d %s", status, body)
		}
		var ids []string
		for _, m := range *g.Members {
			ids = append(ids, m.Value)
		}
		slices.Sort(ids)
		return ids
	}
	want := []string{alice, mia}
	slices.Sort(want)
	if got := memberIDs(); !slices.Equal(got, want) {
		t.Errorf("members = %v, want %v", got, want)
	}
	isMember := func(id string) int {
		t.Helper()
		var l scimList[scimGroup]
		if status, body := entra.do(http.MethodGet, scimFilter("Groups", `id eq "`+admins+`" and members eq "`+id+`"`, "excludedAttributes=members"), nil, &l); status != http.StatusOK {
			t.Fatalf("member check = %d %s", status, body)
		}
		return l.TotalResults
	}
	if isMember(alice) != 1 || isMember(mia) != 1 {
		t.Error("the member checks do not find the members")
	}
	entra.mustPatch("/scim/v2/Groups/"+admins, http.StatusNoContent, op("Remove", "members", members(mia)))
	entra.mustPatch("/scim/v2/Groups/"+admins, http.StatusNoContent, op("remove", `members[value eq "`+alice+`"]`, nil))
	if isMember(alice) != 0 || isMember(mia) != 0 || len(memberIDs()) != 0 {
		t.Error("removed members are still members")
	}
	entra.mustPatch("/scim/v2/Groups/"+admins, http.StatusNoContent, op("Replace", "displayName", "Platform administrators"))
	var renamed scimList[scimGroup]
	if status, body := entra.do(http.MethodGet, scimFilter("Groups", `displayName eq "platform administrators"`), nil, &renamed); status != http.StatusOK ||
		renamed.TotalResults != 1 || renamed.Resources[0].ExternalID != "platform-admins" {
		t.Errorf("renamed group = %d %s", status, body)
	}

	// A rename: the userName changes; the name in Quai does not.
	entra.mustPatch("/scim/v2/Users/"+alice, http.StatusOK, op("Replace", "userName", "alice.smith@contoso.com"))
	if u := entra.user(alice); u.UserName != "alice.smith@contoso.com" {
		t.Errorf("renamed = %+v", u)
	}
	if user, err := h.store.SCIMUserByUserName(context.Background(), "alice.smith@contoso.com"); err != nil || user.Name != "alice@contoso.com" {
		t.Errorf("alice in Quai = %+v %v", user.Name, err)
	}

	// Disabled, then deleted: gone from SCIM, deactivated in Quai.
	var disabled scimUser
	if status, body := entra.do(http.MethodPatch, "/scim/v2/Users/"+mia, map[string]any{
		"Operations": []map[string]any{op("Replace", "active", false)}, "schemas": []string{patchOp}}, &disabled); status != http.StatusOK || disabled.Active {
		t.Fatalf("disable = %d %s", status, body)
	}
	if status, body := entra.do(http.MethodDelete, "/scim/v2/Users/"+mia, nil, nil); status != http.StatusNoContent || body != "" {
		t.Fatalf("delete = %d %s", status, body)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if status, _ := entra.do(method, "/scim/v2/Users/"+mia, nil, nil); status != http.StatusNotFound {
			t.Errorf("%s a deleted user = %d", method, status)
		}
	}
	if u, err := h.store.GetUser(context.Background(), "mia@contoso.com"); err != nil || !u.DeactivatedAt.Valid || u.ScimID.Valid {
		t.Errorf("mia in Quai = %+v %v", u, err)
	}
	if status, _ := entra.do(http.MethodDelete, "/scim/v2/Groups/"+admins, nil, nil); status != http.StatusNoContent {
		t.Errorf("delete group = %d", status)
	}
	if status, _ := entra.do(http.MethodGet, "/scim/v2/Groups/"+admins, nil, nil); status != http.StatusNotFound {
		t.Errorf("deleted group = %d", status)
	}

	// Pages: by name, startIndex from 1, count bounded.
	entra.createUser("bob@contoso.com", oid["bob"])
	entra.createUser("carol@contoso.com", oid["carol"])
	var p2 scimList[scimUser]
	if status, body := entra.do(http.MethodGet, "/scim/v2/Users?startIndex=2&count=1", nil, &p2); status != http.StatusOK || p2.TotalResults != 3 ||
		p2.StartIndex != 2 || p2.ItemsPerPage != 1 || p2.Resources[0].UserName != "bob@contoso.com" {
		t.Errorf("page 2 = %d %s", status, body)
	}
	var counted scimList[scimUser]
	if status, body := entra.do(http.MethodGet, "/scim/v2/Users?count=0", nil, &counted); status != http.StatusOK || counted.TotalResults != 3 || len(counted.Resources) != 0 {
		t.Errorf("count 0 = %d %s", status, body)
	}

	// What Quai does not take.
	for _, f := range []string{`userName co "a"`, `title eq "x"`, `userName eq alice`, `userName eq "a" or userName eq "b"`,
		`userName eq "a" and userName eq "b"`, `userName eq "a`, `userName eq "a"and externalId eq "b"`, `(userName eq "a")`, strings.Repeat("x", 2000)} {
		var e scimError
		if status, body := entra.do(http.MethodGet, scimFilter("Users", f), nil, &e); status != http.StatusBadRequest || e.ScimType != "invalidFilter" {
			t.Errorf("filter %.40s = %d %s", f, status, body)
		}
	}
	refused := []struct {
		name, method, path, contentType string
		body                            any
		status                          int
		scimType                        string
	}{
		{"PUT", http.MethodPut, "/scim/v2/Users/" + alice, "application/scim+json", entraUser("alice@contoso.com", oid["alice"]), http.StatusNotImplemented, ""},
		{"bulk", http.MethodPost, "/scim/v2/Bulk", "application/scim+json", map[string]any{}, http.StatusNotImplemented, ""},
		{"unknown resource", http.MethodGet, "/scim/v2/Things", "", nil, http.StatusNotFound, ""},
		{"text", http.MethodPost, "/scim/v2/Users", "text/plain", "userName=x", http.StatusUnsupportedMediaType, ""},
		{"over 1 MiB", http.MethodPost, "/scim/v2/Users", "application/json", bytes.Repeat([]byte(" "), 1<<20+1), http.StatusRequestEntityTooLarge, ""},
		{"trailing data", http.MethodPost, "/scim/v2/Users", "application/scim+json", `{"userName": "x@contoso.com", "externalId": "x"} {}`, http.StatusBadRequest, "invalidSyntax"},
		{"a key twice", http.MethodPost, "/scim/v2/Users", "application/scim+json", `{"userName": "x@contoso.com", "USERNAME": "y@contoso.com", "externalId": "x"}`, http.StatusBadRequest, "invalidSyntax"},
		{"no externalId", http.MethodPost, "/scim/v2/Users", "application/scim+json", map[string]any{"userName": "x@contoso.com"}, http.StatusBadRequest, "invalidValue"},
		{"a space in userName", http.MethodPost, "/scim/v2/Users", "application/scim+json", entraUser("x y@contoso.com", uuid.NewString()), http.StatusBadRequest, "invalidValue"},
		{"active maybe", http.MethodPost, "/scim/v2/Users", "application/scim+json", map[string]any{"userName": "x@contoso.com", "externalId": "x", "active": "maybe"}, http.StatusBadRequest, "invalidValue"},
		{"no PatchOp", http.MethodPatch, "/scim/v2/Users/" + alice, "application/scim+json", map[string]any{"Operations": []any{op("Replace", "active", true)}}, http.StatusBadRequest, "invalidSyntax"},
		{"move", http.MethodPatch, "/scim/v2/Users/" + alice, "application/scim+json", map[string]any{"schemas": []string{patchOp}, "Operations": []any{op("move", "active", true)}}, http.StatusBadRequest, "invalidSyntax"},
		{"remove without path", http.MethodPatch, "/scim/v2/Users/" + alice, "application/scim+json", map[string]any{"schemas": []string{patchOp}, "Operations": []any{op("Remove", "", nil)}}, http.StatusBadRequest, "noTarget"},
		{"a new externalId", http.MethodPatch, "/scim/v2/Users/" + alice, "application/scim+json", map[string]any{"schemas": []string{patchOp}, "Operations": []any{op("Replace", "externalId", uuid.NewString())}}, http.StatusBadRequest, "mutability"},
		{"a manager not an id", http.MethodPatch, "/scim/v2/Users/" + alice, "application/scim+json", map[string]any{"schemas": []string{patchOp}, "Operations": []any{op("Add", "manager", "mia")}}, http.StatusBadRequest, "invalidValue"},
		{"an unknown member", http.MethodPost, "/scim/v2/Groups", "application/scim+json", map[string]any{"displayName": "Ops", "members": members(uuid.NewString())}, http.StatusBadRequest, "invalidValue"},
	}
	for _, tt := range refused {
		var e scimError
		status, body := entra.send(tt.method, tt.path, tt.contentType, tt.body, &e)
		if status != tt.status || e.ScimType != tt.scimType || e.Status != strconv.Itoa(tt.status) {
			t.Errorf("%s = %d %s", tt.name, status, body)
		}
	}

	// Every change is the provisioning's, the token entra's, never a person's.
	events := h.auditActions("scim.user_created", "scim.user_changed", "scim.user_deactivated", "scim.user_deleted",
		"scim.group_created", "scim.group_changed", "scim.group_deleted", "scim.members_changed")
	if len(events) < 12 {
		t.Errorf("audit events = %v", events)
	}
	for _, e := range events {
		if !strings.HasSuffix(e, " scim:entra") {
			t.Errorf("an audit event not the provisioning's: %s", e)
		}
	}
}

// TestSCIMGroups: the groups SCIM provisions take away at once what a person's sign-in gave them
// and SCIM does not, from their sessions and their API tokens too (open question 85): out of the
// admin group, a platform admin is one no longer, at their next request; back in, again. SCIM
// never gives a group the sign-in did not. Each change is in the audit log with the groups and
// grants before and after. Someone SCIM does not provision, a reserved emergency account, keeps
// what their sign-in gives.
func TestSCIMGroups(t *testing.T) {
	t.Parallel()
	h, p := scimHarness(t)
	entra := h.provisioning(scimToken)
	aliceOID := uuid.NewString()
	aliceID := entra.createUser("alice@contoso.com", aliceOID)
	alice := h.entraSignIn(p, "alice@contoso.com", aliceOID, "platform-admins", "developers")
	token := alice.token("ci", "requests:read")
	isAdmin := func(what string, want bool) {
		t.Helper()
		_, me := alice.me()
		status, _ := alice.do(http.MethodGet, Prefix+"/access-report", nil, nil)
		var viaToken MeOutput
		h.withToken(token, http.MethodGet, Prefix+"/me", &viaToken.Body)
		if got := slices.Contains(me.Body.Groups, "platform-admins"); got != want || (status == http.StatusOK) != want ||
			slices.Contains(viaToken.Body.Groups, "platform-admins") != want {
			t.Errorf("%s: admin %v, want %v (groups %v, access report %d, token's groups %v)", what, got, want, me.Body.Groups, status, viaToken.Body.Groups)
		}
	}
	isAdmin("signed in", true)

	// The group, provisioned without her: she is out at once, her token too.
	admins := entra.createGroup("platform-admins", "Platform admins")
	isAdmin("the group provisioned without her", false)
	entra.mustPatch("/scim/v2/Groups/"+admins, http.StatusNoContent, op("Add", "members", members(aliceID)))
	isAdmin("added", true)
	entra.mustPatch("/scim/v2/Groups/"+admins, http.StatusNoContent, op("Remove", "members", members(aliceID)))
	isAdmin("removed", false)
	// Signing in again with the group in her ID token does not bring it back.
	alice = h.entraSignIn(p, "alice@contoso.com", aliceOID, "platform-admins", "developers")
	isAdmin("signed in again", false)
	if _, me := alice.me(); !slices.Contains(me.Body.Groups, "developers") {
		t.Errorf("a group SCIM does not provision is gone: %v", me.Body.Groups)
	}

	// A group the sign-in did not give: SCIM does not give it.
	finops := entra.createGroup("finops", "FinOps")
	entra.mustPatch("/scim/v2/Groups/"+finops, http.StatusNoContent, op("add", "members", members(aliceID)))
	if _, me := alice.me(); slices.Contains(me.Body.Groups, "finops") {
		t.Errorf("SCIM gave a group the sign-in did not: %v", me.Body.Groups)
	}

	// Back in: a session signed in without the group gets it at the next sign-in; her token,
	// made with it, has it at once.
	entra.mustPatch("/scim/v2/Groups/"+admins, http.StatusNoContent, op("Add", "members", members(aliceID)))
	if _, me := alice.me(); slices.Contains(me.Body.Groups, "platform-admins") {
		t.Errorf("a session got a group it signed in without: %v", me.Body.Groups)
	}
	var viaToken MeOutput
	if h.withToken(token, http.MethodGet, Prefix+"/me", &viaToken.Body); !slices.Contains(viaToken.Body.Groups, "platform-admins") {
		t.Errorf("the token made with the group: %v", viaToken.Body.Groups)
	}
	alice = h.entraSignIn(p, "alice@contoso.com", aliceOID, "platform-admins", "developers")
	isAdmin("added again, signed in again", true)

	// Deleting the group: its name stays SCIM's, nobody provisioned holds it.
	if status, _ := entra.do(http.MethodDelete, "/scim/v2/Groups/"+admins, nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete group = %d", status)
	}
	isAdmin("the group deleted", false)

	// Each change, with the groups and the grants before and after.
	rows, err := h.store.Pool.Query(context.Background(), `SELECT details FROM audit_events WHERE action = 'auth.groups_changed' AND actor->>'type' = 'scim' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	var changes []map[string][]string
	for rows.Next() {
		var d map[string][]string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		changes = append(changes, d)
	}
	rows.Close()
	if len(changes) != 5 {
		t.Fatalf("group changes = %+v", changes)
	}
	if c := changes[0]; !slices.Contains(c["before"], "platform-admins") || slices.Contains(c["after"], "platform-admins") ||
		!slices.Contains(c["grantsBefore"], "platform-admin@global") || slices.Contains(c["grantsAfter"], "platform-admin@global") {
		t.Errorf("first change = %+v", c)
	}

	// Out of SCIM's reach: a reserved emergency account, never provisioned, keeps its groups.
	var reserved scimError
	if status, body := entra.do(http.MethodPost, "/scim/v2/Users", entraUser("BreakGlass@contoso.com", uuid.NewString()), &reserved); status != http.StatusConflict || reserved.ScimType != "uniqueness" {
		t.Errorf("reserved = %d %s", status, body)
	}
	glassOID := uuid.NewString()
	glass := h.entraSignIn(p, "breakglass@contoso.com", glassOID, "platform-admins")
	if status, _ := glass.do(http.MethodGet, Prefix+"/access-report", nil, nil); status != http.StatusOK {
		t.Errorf("the emergency account is not an admin: %d", status)
	}
	// Provisioned before its name was reserved: SCIM can no longer change it, deactivate it or
	// delete it.
	glassID := uuid.NewString()
	if _, err := h.store.Pool.Exec(context.Background(), `UPDATE users SET scim_id = $1, scim_external_id = $2, scim_user_name = name
		WHERE name = 'breakglass@contoso.com'`, glassID, glassOID); err != nil {
		t.Fatal(err)
	}
	if status, body := entra.patch("/scim/v2/Users/"+glassID, op("Replace", "active", false)); status != http.StatusForbidden {
		t.Errorf("deactivate the emergency account = %d %s", status, body)
	}
	if status, body := entra.do(http.MethodDelete, "/scim/v2/Users/"+glassID, nil, nil); status != http.StatusForbidden {
		t.Errorf("delete the emergency account = %d %s", status, body)
	}
	if status, _ := glass.do(http.MethodGet, Prefix+"/access-report", nil, nil); status != http.StatusOK {
		t.Errorf("the emergency account after SCIM tried: %d", status)
	}
}

// TestSCIMDeactivation: a deactivation takes effect at once and completely (open question 86):
// the person's sessions end at their next request, their API tokens are revoked, the delegations
// they gave or received too, their requests not delivered yet are cancelled, and their approvals
// of requests still waiting no longer count, the other approvers told again; a request approved
// before keeps its approvals. They cannot sign in. Each is an audit event, the provisioning's. A
// reactivation lets them sign in again and brings back none of it. The manager SCIM names is the
// manager approver.
func TestSCIMDeactivation(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h, p := scimHarness(t, func(c *config.Config) {
		c.Notifications = config.Notifications{Email: &config.Email{Host: mail.Host, Port: mail.Port, Transport: config.EmailSTARTTLS, From: "Quai <quai@example.com>"}}
		c.Network.CAFile = withCA(t, c.Network.CAFile, mail.CAFile)
	})
	now := time.Now().UTC().Truncate(time.Second)
	h.clock.set(now)
	entra := h.provisioning(scimToken)
	oid, id := map[string]string{}, map[string]string{}
	for _, n := range []string{"rita", "bob", "dan", "mia", "fiona"} {
		oid[n] = uuid.NewString()
		id[n] = entra.createUser(n+"@contoso.com", oid[n])
	}
	entra.mustPatch("/scim/v2/Users/"+id["dan"], http.StatusOK, op("Add", "manager", []map[string]any{{"value": id["mia"]}}))
	as := func(n string, groups ...string) *user { return h.entraSignIn(p, n+"@contoso.com", oid[n], groups...) }
	rita, bob, dan, mia, fiona := as("rita", "developers"), as("bob", "platform-admins"), as("dan", "developers"), as("mia", "developers"), as("fiona", "platform-admins")
	ritaToken, bobToken := rita.token("ci", "requests:read"), bob.token("ci", "requests:read")
	week := now.Add(7 * 24 * time.Hour)
	bob.mustDelegate("fiona@contoso.com", now, week)
	mia.mustDelegate("rita@contoso.com", now, week)

	submit := func(u *user, name string, cpu int) RequestView {
		t.Helper()
		var r RequestView
		if status, body := u.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", name, cpu), &r); status != http.StatusCreated {
			t.Fatalf("submit %s = %d %s", name, status, body)
		}
		return r
	}
	delivered := submit(rita, "scim-01", 2)
	if status := bob.tryApprove(delivered.ID); status != http.StatusOK {
		t.Fatalf("bob's approval = %d", status)
	}
	rita.await(delivered.ID, "applied")
	waiting := submit(rita, "scim-02", 2)
	large := submit(dan, "scim-03", 16)
	if large.Manager == nil || large.Manager.Name != "mia@contoso.com" {
		t.Fatalf("the manager SCIM names = %+v", large.Manager)
	}
	if status := bob.tryApprove(large.ID); status != http.StatusOK {
		t.Fatalf("bob's approval of the large VM = %d", status)
	}
	h.quiet(t)

	// Bob leaves: disabled as Entra sends it, active "False".
	entra.mustPatch("/scim/v2/Users/"+id["bob"], http.StatusOK, op("Replace", "active", "False"))
	if status, _ := bob.me(); status != http.StatusUnauthorized {
		t.Errorf("bob's session = %d", status)
	}
	if status := h.withToken(bobToken, http.MethodGet, Prefix+"/me", nil); status != http.StatusUnauthorized {
		t.Errorf("bob's token = %d", status)
	}
	if d := fiona.delegations(); len(d.Body.Delegations) != 0 {
		t.Errorf("bob's delegation = %+v", d.Body.Delegations)
	}
	r := dan.read(large.ID)
	if r.State != "pending_approval" || len(r.Reviews) != 1 || !r.Reviews[0].Voided || r.Reviews[0].Reviewer != "bob@contoso.com" ||
		!slices.Contains(eventKinds(r), "reviews_voided") {
		t.Errorf("the large VM after bob left = %s %+v %v", r.State, r.Reviews, eventKinds(r))
	}
	if r := rita.read(delivered.ID); r.State != "applied" || len(r.Reviews) != 1 || r.Reviews[0].Voided {
		t.Errorf("the delivered request = %s %+v", r.State, r.Reviews)
	}
	if _, status, body := h.trySignIn(p, entraClaims("bob@contoso.com", oid["bob"], "platform-admins")); status != http.StatusForbidden || !strings.Contains(body, "deactivated") {
		t.Errorf("bob signs in = %d %s", status, body)
	}
	// The approvers are told again; bob is not.
	h.quiet(t)
	var sent []string
	rows, err := h.store.Pool.Query(context.Background(), `SELECT n.recipient FROM notifications n JOIN events e ON e.id = n.event_id
		WHERE e.request_id = $1 AND e.kind = 'reviews_voided' AND n.state = 'sent' ORDER BY n.recipient`, large.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var to string
		if err := rows.Scan(&to); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, to)
	}
	rows.Close()
	// Fiona, a platform admin; rita in the place of mia, the manager, who delegated to her.
	if !slices.Equal(sent, []string{"user:fiona@contoso.com", "user:rita@contoso.com"}) {
		t.Errorf("told again: %v", sent)
	}
	if got := mail.To(t, "bob@contoso.com"); len(got) != 0 {
		for _, m := range got {
			if strings.Contains(m.Subject, "again") {
				t.Errorf("bob was told: %s", m.Subject)
			}
		}
	}
	told := false
	for _, m := range mail.To(t, "fiona@contoso.com") {
		if strings.Contains(mail.Message(t, m.ID).Text, "bob@contoso.com was deactivated") {
			told = true
		}
	}
	if !told {
		t.Error("fiona was not told bob's approval no longer counts")
	}

	// Rita leaves: deleted.
	if status, body := entra.do(http.MethodDelete, "/scim/v2/Users/"+id["rita"], nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete rita = %d %s", status, body)
	}
	if status, _ := rita.me(); status != http.StatusUnauthorized {
		t.Errorf("rita's session = %d", status)
	}
	if status := h.withToken(ritaToken, http.MethodGet, Prefix+"/me", nil); status != http.StatusUnauthorized {
		t.Errorf("rita's token = %d", status)
	}
	if d := mia.delegations(); len(d.Body.Delegations) != 0 {
		t.Errorf("the delegation to rita = %+v", d.Body.Delegations)
	}
	if r := dan.read(waiting.ID); r.State != "cancelled" || !strings.Contains(string(r.Events[len(r.Events)-1].Data), "its requester was deactivated") {
		t.Errorf("rita's waiting request = %s %+v", r.State, r.Events)
	}
	h.quiet(t)
	for _, n := range h.notifications(t, waiting.ID) {
		if n.Recipient == "user:rita@contoso.com" && n.Kind != "submitted" {
			t.Errorf("rita was notified after she left: %+v", n)
		}
	}

	// Each effect is the provisioning's in the audit log.
	for _, e := range h.auditActions("scim.user_deactivated", "token.revoked", "delegation.revoked", "request.cancelled", "request.reviews_voided", "scim.user_deleted") {
		if !strings.HasSuffix(e, " scim:entra") {
			t.Errorf("not the provisioning's: %s", e)
		}
	}
	if got := h.auditActions("scim.user_deactivated", "token.revoked", "delegation.revoked", "request.cancelled", "request.reviews_voided"); len(got) != 8 {
		t.Errorf("deactivations' events = %v", got)
	}

	// Bob is back: he signs in again; his session, token and delegation stay ended.
	entra.mustPatch("/scim/v2/Users/"+id["bob"], http.StatusOK, op("Replace", "", map[string]any{"active": true}))
	again := as("bob", "platform-admins")
	if status, _ := bob.me(); status != http.StatusUnauthorized {
		t.Errorf("bob's old session = %d", status)
	}
	if status := h.withToken(bobToken, http.MethodGet, Prefix+"/me", nil); status != http.StatusUnauthorized {
		t.Errorf("bob's old token = %d", status)
	}
	if d := fiona.delegations(); len(d.Body.Delegations) != 0 {
		t.Errorf("bob's delegation came back: %+v", d.Body.Delegations)
	}
	if r := dan.read(large.ID); len(r.Reviews) != 1 || !r.Reviews[0].Voided {
		t.Errorf("bob's approval came back: %+v", r.Reviews)
	}
	// Rita is provisioned again, by her externalId: active, her name, a new session.
	var back scimUser
	if status, body := entra.do(http.MethodPost, "/scim/v2/Users", entraUser("rita@contoso.com", oid["rita"]), &back); status != http.StatusCreated || !back.Active {
		t.Fatalf("rita again = %d %s", status, body)
	}
	as("rita", "developers")

	// The large VM goes on: a platform admin and the manager SCIM names approve it.
	if status := again.tryApprove(large.ID); status != http.StatusOK {
		t.Fatalf("bob's new approval = %d", status)
	}
	if status := dan.tryApprove(large.ID); status != http.StatusForbidden {
		t.Errorf("dan approved his own request: %d", status)
	}
	if status := mia.tryApprove(large.ID); status != http.StatusOK {
		t.Fatalf("mia's approval = %d", status)
	}
	dan.await(large.ID, "applied")
}

// withCA returns a bundle of the CAs of bundle and caFile.
func withCA(t *testing.T, bundle, caFile string) string {
	t.Helper()
	var out []byte
	for _, f := range []string{bundle, caFile} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	path := filepath.Join(t.TempDir(), "bundle.pem")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSCIMIdentityBinding: SCIM binds a person to their account of the identity provider by its
// immutable identifier, externalId (Entra ID's object ID), never by a name (open question 84). A
// person who signed in before is adopted only by the same account; a name equal but for case is
// refused; a new person never inherits an old one's name, nor what it holds; a rename keeps the
// person; a sign-in whose identifier or name disagrees with SCIM's is refused.
func TestSCIMIdentityBinding(t *testing.T) {
	t.Parallel()
	h, p := scimHarness(t)
	entra := h.provisioning(scimToken)
	ctx := context.Background()

	// Olga signed in before: SCIM adopts her, by her object ID; the groups SCIM provisions apply.
	olgaOID := uuid.NewString()
	h.entraSignIn(p, "olga@contoso.com", olgaOID, "developers", "platform-admins")
	olga := entra.createUser("olga@contoso.com", olgaOID)
	if adopted := h.auditActions("scim.user_created"); len(adopted) != 1 {
		t.Fatalf("created = %v", adopted)
	}
	var adopted bool
	if err := h.store.Pool.QueryRow(ctx, `SELECT (details->>'adopted')::boolean FROM audit_events WHERE action = 'scim.user_created'`).Scan(&adopted); err != nil || !adopted {
		t.Errorf("olga not adopted: %v", err)
	}

	// Victor signed in with another account: his name is not SCIM's to give.
	h.entraSignIn(p, "victor@contoso.com", uuid.NewString(), "developers")
	conflict := func(what, userName, externalID string) {
		t.Helper()
		var e scimError
		if status, body := entra.do(http.MethodPost, "/scim/v2/Users", entraUser(userName, externalID), &e); status != http.StatusConflict || e.ScimType != "uniqueness" {
			t.Errorf("%s = %d %s", what, status, body)
		}
	}
	conflict("another account's name", "victor@contoso.com", uuid.NewString())
	conflict("a name equal but for case", "Olga@contoso.com", uuid.NewString())
	conflict("another userName, the same externalId", "olga2@contoso.com", olgaOID)

	// A rename keeps the person: she signs in by her object ID, with her new name, as olga.
	entra.mustPatch("/scim/v2/Users/"+olga, http.StatusOK, op("Replace", "userName", "olga.new@contoso.com"))
	renamed, status, body := h.trySignIn(p, entraClaims("olga.new@contoso.com", olgaOID, "developers"))
	if _, me := renamed.me(); status != http.StatusOK || me.Body.Name != "olga@contoso.com" {
		t.Errorf("olga renamed signs in = %d %s as %q", status, body, me.Body.Name)
	}
	// Her old userName is not free: it is still her name in Quai.
	conflict("a renamed person's name", "olga@contoso.com", uuid.NewString())
	// Nor is another person's name, nor a reserved one, hers to take.
	for _, name := range []string{"Victor@contoso.com", "breakglass@contoso.com"} {
		var e scimError
		if status, body := entra.do(http.MethodPatch, "/scim/v2/Users/"+olga, map[string]any{"schemas": []string{patchOp},
			"Operations": []any{op("Replace", "userName", name)}}, &e); status != http.StatusConflict || e.ScimType != "uniqueness" {
			t.Errorf("olga renamed %s = %d %s", name, status, body)
		}
	}

	// Sam is provisioned and has never signed in: only his account can take the name.
	samOID := uuid.NewString()
	sam := entra.createUser("sam@contoso.com", samOID)
	if _, status, body := h.trySignIn(p, entraClaims("sam@contoso.com", uuid.NewString(), "platform-admins")); status != http.StatusForbidden ||
		!strings.Contains(body, "gave the name") {
		t.Errorf("another account as sam = %d %s", status, body)
	}
	if _, status, body := h.trySignIn(p, entraClaims("mallory@contoso.com", samOID, "platform-admins")); status != http.StatusForbidden ||
		!strings.Contains(body, "knows you as") {
		t.Errorf("sam's account named mallory = %d %s", status, body)
	}
	if u, err := h.store.GetUser(ctx, "mallory@contoso.com"); err == nil {
		t.Errorf("mallory recorded: %+v", u)
	}
	h.entraSignIn(p, "sam@contoso.com", samOID, "developers")
	// Signed in, his externalId binds for good.
	var e scimError
	if status, body := entra.do(http.MethodPatch, "/scim/v2/Users/"+sam, map[string]any{"schemas": []string{patchOp},
		"Operations": []any{op("Replace", "externalId", uuid.NewString())}}, &e); status != http.StatusBadRequest || e.ScimType != "mutability" {
		t.Errorf("a new externalId = %d %s", status, body)
	}

	// Sam leaves, deleted; a new person with his old userName does not get his name, nor what
	// it holds. Sam himself comes back by his externalId.
	if status, _ := entra.do(http.MethodDelete, "/scim/v2/Users/"+sam, nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete sam = %d", status)
	}
	conflict("a new person with a deleted person's userName", "sam@contoso.com", uuid.NewString())
	var back scimUser
	if status, body := entra.do(http.MethodPost, "/scim/v2/Users", entraUser("sam@contoso.com", samOID), &back); status != http.StatusCreated || back.ID == sam {
		t.Errorf("sam again = %d %s", status, body)
	}
	h.entraSignIn(p, "sam@contoso.com", samOID, "developers")
}

// TestSCIMToken: SCIM takes its own token and nothing else: no session, no API token, nor a SCIM
// token anywhere but SCIM; two tokens while rotating, each named in the audit log and with its
// last use in the access report. Requests are rate limited. An encoded path is not SCIM's.
func TestSCIMToken(t *testing.T) {
	t.Parallel()
	h, p := scimHarness(t)
	admin := h.entraSignIn(p, "ada@contoso.com", uuid.NewString(), "platform-admins")
	apiToken := admin.token("ci", "requests:read")

	unauthorized := func(what string, c scimClient, header ...string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/scim/v2/Users", nil)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Add(header[i], header[i+1])
		}
		client := admin.c // the admin's browser: their session cookie goes too
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Content-Type") != "application/scim+json" ||
			!strings.Contains(string(raw), "a SCIM token is needed") || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s = %d %s", what, resp.StatusCode, raw)
		}
	}
	unauthorized("a session", h.provisioning(""))
	unauthorized("an API token", h.provisioning(apiToken))
	unauthorized("a SCIM token unknown", h.provisioning(scim.TokenPrefix+strings.Repeat("00", 32)))
	unauthorized("the token twice", h.provisioning(""), "Authorization", "Bearer "+scimToken, "Authorization", "Bearer "+scimToken)
	unauthorized("another scheme", h.provisioning(""), "Authorization", "Basic "+scimToken)
	if status := h.withToken(scimToken, http.MethodGet, Prefix+"/me", nil); status != http.StatusUnauthorized {
		t.Errorf("the SCIM token on the API = %d", status)
	}
	// An encoded path is not SCIM's: the API takes it, and refuses the token.
	if status := h.withToken(scimToken, http.MethodGet, "/scim/v2/Users%2F..", nil); status != http.StatusUnauthorized {
		t.Errorf("an encoded path = %d", status)
	}

	// Rotation: both tokens work, each named in the audit log; the access report has their use.
	h.provisioning(scimToken).createUser("one@contoso.com", uuid.NewString())
	h.provisioning(scimNext).createUser("two@contoso.com", uuid.NewString())
	if got := h.auditActions("scim.user_created"); !slices.Equal(got, []string{"scim.user_created scim:entra", "scim.user_created scim:next"}) {
		t.Errorf("created by %v", got)
	}
	var report AccessReportView
	if status, body := admin.do(http.MethodGet, Prefix+"/access-report", nil, &report); status != http.StatusOK || len(report.SCIMTokens) != 2 ||
		report.SCIMTokens[0].Name != "entra" || report.SCIMTokens[0].LastUsedAt.IsZero() {
		t.Errorf("access report = %d %s", status, body)
	}
	for _, u := range report.Users {
		if u.Name == "one@contoso.com" && !u.Provisioned {
			t.Errorf("one is not provisioned: %+v", u)
		}
	}

	// The rate limit: answered before any work.
	h.scim.UseLimit(rate.Every(time.Hour), 1)
	if status, _ := h.provisioning(scimToken).do(http.MethodGet, "/scim/v2/Users", nil, nil); status != http.StatusOK {
		t.Errorf("first = %d", status)
	}
	var e scimError
	if status, body := h.provisioning(scimToken).do(http.MethodGet, "/scim/v2/Users", nil, &e); status != http.StatusTooManyRequests || e.Status != "429" {
		t.Errorf("over the limit = %d %s", status, body)
	}
}
