package auth_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"slices"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/store/db"
)

func TestDirectory(t *testing.T) {
	d := auth.NewDirectory(config.Auth{
		Teams: []config.TeamMapping{
			{Group: "sg-quai-shop-ops", Team: "shop"}, // before the pattern: the first match decides
			{Group: "sg-quai-*", Team: "*"},
			{Group: "*-admins", Team: "admins-*"},
			{Group: "ad-*-rw", Team: "*"},
			{Group: "Platform Engineers", Team: "platform"},
			{Group: "sg-quai-blog", Team: "never"}, // after a pattern that matches it
		},
		Roles: []config.RoleMapping{
			{Group: "developers", Role: "requester"},
			{Group: "sg-quai-shop", Role: "operator", Scope: "team:shop"},
			{Group: "sg-quai-shop-ops", Role: "operator", Scope: "team:shop"},
			{Group: "developers", Role: "requester"}, // twice: once in the grants
		},
	})
	for group, team := range map[string]string{
		"sg-quai-shop-ops":   "shop",
		"sg-quai-shop":       "shop",
		"sg-quai-blog":       "blog",
		"sg-quai-":           "", // * matches one character at least
		"db-admins":          "admins-db",
		"ad-data-rw":         "data",
		"ad-rw":              "",
		"Platform Engineers": "platform",
		"platform engineers": "", // names are matched with their case
		"developers":         "", // no entry maps it: no team, its name notwithstanding
		"shop":               "",
	} {
		if got, ok := d.Team(group); got != team || ok != (team != "") {
			t.Errorf("Team(%q) = %q, %v, want %q", group, got, ok, team)
		}
	}
	if got := d.Teams([]string{"sg-quai-shop-ops", "developers", "sg-quai-shop", "db-admins", "shop"}); !slices.Equal(got, []string{"admins-db", "shop"}) {
		t.Errorf("Teams = %v", got)
	}
	if got := d.Teams([]string{"developers"}); got != nil {
		t.Errorf("Teams of unmapped groups = %v", got)
	}
	if got := d.Teams(nil); got != nil {
		t.Errorf("Teams(nil) = %v", got)
	}

	// A last entry {group: "*", team: "*"} makes the groups no other entry maps the teams of their names.
	all := auth.NewDirectory(config.Auth{Teams: []config.TeamMapping{{Group: "sg-quai-*", Team: "*"}, {Group: "*", Team: "*"}}})
	if got := all.Teams([]string{"sg-quai-shop", "developers", "x"}); !slices.Equal(got, []string{"developers", "shop", "x"}) {
		t.Errorf("Teams with a catch-all = %v", got)
	}
	if got, ok := all.Team(""); ok {
		t.Errorf("Team(\"\") = %q", got) // * matches one character at least
	}
	// "*" alone in the team is what the group's * matched; around it, a name.
	if got, _ := auth.NewDirectory(config.Auth{Teams: []config.TeamMapping{{Group: "*", Team: "team-*"}}}).Team("web"); got != "team-web" {
		t.Errorf("Team = %q", got)
	}
	shop := authz.Grant{Role: authz.Operator, Scope: authz.Scope{Kind: authz.ScopeTeam, Name: "shop"}}
	want := []auth.Granted{
		{Grant: authz.Grant{Role: authz.Requester, Scope: authz.Global}, Group: "developers"},
		{Grant: shop, Group: "sg-quai-shop"},
		{Grant: shop, Group: "sg-quai-shop-ops"},
	}
	if got := d.Grants([]string{"sg-quai-shop-ops", "developers", "sg-quai-shop", "blog"}); !slices.Equal(got, want) {
		t.Errorf("Grants = %+v", got)
	}
	if got := auth.ScopeName(shop.Scope) + " " + auth.ScopeName(authz.Global); got != "team:shop global" {
		t.Errorf("ScopeName = %s", got)
	}
	// Without auth.teams, each group is the team of its name; the zero directory maps nothing.
	var zero auth.Directory
	if team, ok := zero.Team("shop"); team != "shop" || !ok || zero.Grants([]string{"developers"}) != nil {
		t.Error("the zero directory maps")
	}
	if got := zero.Teams([]string{"shop", "developers", "shop"}); !slices.Equal(got, []string{"developers", "shop"}) {
		t.Errorf("Teams without auth.teams = %v", got)
	}
}

// TestUsers: each login keeps the person in the directory as the identity provider describes
// them, with only an email it vouches for; a name held by another subject, and a deactivated
// person, cannot sign in.
func TestUsers(t *testing.T) {
	p := newIDP(t)
	srv, c, s := quaiWith(t, p, func(cfg *config.Config) {
		cfg.Auth.Teams = []config.TeamMapping{{Group: "sh*", Team: "retail-*"}}
	})
	ctx := context.Background()
	login := func(claims map[string]any) (*http.Client, int, string) {
		t.Helper()
		p.claims = claims
		jar, _ := cookiejar.New(nil)
		browser := &http.Client{Jar: jar, Transport: c.Transport}
		resp, err := browser.Get(srv.URL + "/auth/login")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.Request.URL.Path == "/" { // signed in and sent home, which this server does not serve
			_, status := getMe(t, browser, srv.URL)
			return browser, status, ""
		}
		return browser, resp.StatusCode, string(body)
	}

	bob := map[string]any{"preferred_username": "bob", "sub": "sub-bob", "oid": "oid-bob", "email": "bob@example.com",
		"email_verified": true, "groups": []string{"shop", "developers"}}
	browser, status, body := login(bob)
	if status != http.StatusOK {
		t.Fatalf("login = %d %s", status, body)
	}
	// The teams come from auth.teams: shop is mapped, developers no team.
	if m, _ := getMe(t, browser, srv.URL); !slices.Equal(m.Teams, []string{"retail-op"}) {
		t.Errorf("me = %+v", m)
	}
	u, err := s.GetUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "bob@example.com" || u.Subject != "sub-bob" || u.ObjectID != "oid-bob" || !slices.Equal(u.Groups, []string{"shop", "developers"}) ||
		!u.LastLogin.Valid || !u.AuthTime.Valid || u.DeactivatedAt.Valid {
		t.Errorf("bob = %+v", u)
	}
	first := u.LastLogin.Time

	// The next login updates the row; an email the identity provider does not vouch for is not kept.
	bob["email_verified"], bob["groups"] = false, []string{"developers"}
	if _, status, body := login(bob); status != http.StatusOK {
		t.Fatalf("second login = %d %s", status, body)
	}
	if u, _ := s.GetUser(ctx, "bob"); u.Email != "" || !slices.Equal(u.Groups, []string{"developers"}) || u.LastLogin.Time.Before(first) {
		t.Errorf("bob after the second login = %+v", u)
	}

	// Another account of the identity provider with the same name is refused: it would act as bob.
	impostor := map[string]any{"preferred_username": "bob", "sub": "sub-other", "groups": []string{"platform-admins"}}
	browser, status, body = login(impostor)
	if status != http.StatusForbidden || !strings.Contains(body, `the name \"bob\" belongs to another account`) {
		t.Errorf("same name, another subject = %d %s", status, body)
	}
	if _, status := getMe(t, browser, srv.URL); status != http.StatusUnauthorized {
		t.Errorf("signed in anyway: %d", status)
	}
	if u, _ := s.GetUser(ctx, "bob"); u.Subject != "sub-bob" || !slices.Equal(u.Groups, []string{"developers"}) {
		t.Errorf("the refused login changed bob: %+v", u)
	}
	// The query refuses it too, for a login that looked before the name was taken.
	record := func(subject string) int64 {
		t.Helper()
		n, err := s.RecordUserLogin(ctx, db.RecordUserLoginParams{Name: "bob", Subject: subject, Groups: []string{"never-recorded"}})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := record("sub-other"); n != 0 {
		t.Errorf("another subject recorded: %d", n)
	}
	// An administrator releases the name: the next login takes it.
	if _, err := s.Pool.Exec(ctx, "UPDATE users SET subject = '' WHERE name = 'bob'"); err != nil {
		t.Fatal(err)
	}
	if _, status, body := login(impostor); status != http.StatusOK {
		t.Errorf("after the name is released = %d %s", status, body)
	}
	if u, _ := s.GetUser(ctx, "bob"); u.Subject != "sub-other" || u.ObjectID != "oid-bob" {
		t.Errorf("bob's new subject = %+v", u)
	}

	// A deactivated person cannot sign in.
	if _, err := s.Pool.Exec(ctx, "UPDATE users SET deactivated_at = now() WHERE name = 'bob'"); err != nil {
		t.Fatal(err)
	}
	browser, status, body = login(impostor)
	if status != http.StatusForbidden || !strings.Contains(body, "deactivated") {
		t.Errorf("deactivated = %d %s", status, body)
	}
	if n := record("sub-other"); n != 0 {
		t.Errorf("a deactivated person's login recorded: %d", n)
	}
	if u, _ := s.GetUser(ctx, "bob"); slices.Contains(u.Groups, "never-recorded") {
		t.Errorf("refused logins changed bob: %+v", u)
	}
	if _, status := getMe(t, browser, srv.URL); status != http.StatusUnauthorized {
		t.Errorf("deactivated, signed in anyway: %d", status)
	}
}

// TestDevUsers: a development login keeps the person too, and the identity provider's identifiers
// known from an earlier login.
func TestDevUsers(t *testing.T) {
	srv, c, s := quaiWith(t, nil, nil)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, "INSERT INTO users (name, subject, object_id) VALUES ('alice', 'sub-alice', 'oid-alice')"); err != nil {
		t.Fatal(err)
	}
	if resp := send(t, c, http.MethodPost, srv.URL+"/auth/dev/login", map[string]any{"name": "alice"}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("dev login = %d", resp.StatusCode)
	}
	u, err := s.GetUser(ctx, "alice")
	if err != nil || u.Subject != "sub-alice" || u.ObjectID != "oid-alice" || !slices.Equal(u.Groups, []string{"platform-admins"}) || !u.LastLogin.Valid {
		t.Errorf("alice = %+v, %v", u, err)
	}
}
