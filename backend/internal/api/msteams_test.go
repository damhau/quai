package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/chatlink"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/mailtest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/teamstest"
)

// The Entra ID object IDs of the people of the Teams tests; fiona and carol have none.
var (
	oidAlice = "5f0c2a8e-1b2d-4c3e-8f4a-0a1b2c3d4e01"
	oidPat   = "5f0c2a8e-1b2d-4c3e-8f4a-0a1b2c3d4e02"
	oidBob   = "5f0c2a8e-1b2d-4c3e-8f4a-0a1b2c3d4e03"
)

// linksKey signs the links of the Teams tests' cards.
const linksKey = "the-links-key-of-the-tests-0123456789abcdef"

// hostileText is what a person may write in a reason: quotes, braces, Markdown links, a script.
const hostileText = `"}]} {{.x}} [click](javascript:alert(1)) <script>alert(1)</script> **bold**`

// withTeams configures notifications to the Teams fakes: the bot, the infra channel posting
// requests of infra-live, and the links' key, keeping email when configured; the people sign in
// with their object IDs. caFile trusts the fakes, and Mailpit when email is configured.
func withTeams(fakes *teamstest.Teams, caFile string) func(*config.Config) {
	return func(c *config.Config) {
		c.Network.CAFile = caFile
		c.Notifications.Teams = &config.Teams{Bot: fakes.Bot("env:TEAMS_CLIENT_SECRET"), Channels: []config.TeamsChannel{{
			Name: "infra", Webhook: config.SecretRef{Ref: "env:TEAMS_HOOK_INFRA"}, Repositories: []string{"infra-live"},
			Events: []string{"submitted", "approval_needed", "approved", "alert"}}}}
		c.Notifications.Links = &config.ChatLinks{Key: config.SecretRef{Ref: "env:QUAI_LINKS_KEY"}}
		c.Auth.Dev.Users = []config.DevUser{
			{Name: "alice", Email: "alice@example.com", Groups: []string{"platform-admins"}, ObjectID: oidAlice},
			{Name: "pat", Email: "pat@example.com", Groups: []string{"platform-admins"}, ObjectID: oidPat},
			{Name: "bob", Email: "bob@example.com", Groups: []string{"developers", "shop"}, ObjectID: oidBob},
			{Name: "fiona", Email: "fiona@example.com", Groups: []string{"finops", "developers"}},
			{Name: "carol", Groups: []string{"shop", "developers"}},
		}
	}
}

// chatHarness is newHarness with notifications to the Teams fakes, then configure's changes, the
// app installed for alice, pat and bob.
func chatHarness(t *testing.T, fakes *teamstest.Teams, caFile string, configure ...func(*config.Config)) *harness {
	t.Helper()
	vars := map[string]string{"TEAMS_CLIENT_SECRET": fakes.Login.Secret, "TEAMS_HOOK_INFRA": fakes.Workflows.Hook("infra"), "QUAI_LINKS_KEY": linksKey}
	sec := secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { v, ok := vars[n]; return v, ok }}})
	dir := t.TempDir()
	bare, work := localRemote(t, dir)
	h := startRepo(t, dir, "{ name: infra-live, title: Infrastructure }", `connector: generic
  url: `+bare+`
  enforcement:
    floorRules: [{ name: production-floor, when: "params.env == 'prod'", approvers: [group:platform-admins] }]
  sync: { minInterval: 1ns }`, repositories.Options{Secrets: sec}, append([]func(*config.Config){withTeams(fakes, caFile)}, configure...)...)
	h.bare, h.work = bare, work
	fakes.Connector.Install(oidAlice, oidPat, oidBob)
	return h
}

// cardOf returns the one card sent to someone, failing the test otherwise.
func cardOf(t *testing.T, fakes *teamstest.Teams, oid string) teamstest.Activity {
	t.Helper()
	got := fakes.Connector.To(oid)
	if len(got) != 1 {
		t.Fatalf("%d cards to %s, want 1: %+v", len(got), oid, got)
	}
	return got[0]
}

// linkOf returns the token of a card's button, checking it opens the request on Quai.
func linkOf(t *testing.T, a teamstest.Activity, button, request string) string {
	t.Helper()
	act, ok := a.Card.Action(button)
	if !ok {
		t.Fatalf("no %s button: %+v", button, a.Card.Actions())
	}
	u, err := url.Parse(act.URL)
	if err != nil || u.Scheme+"://"+u.Host+u.Path != "https://quai.example.com/requests/"+request || len(u.Query()) != 1 || u.Query().Get("link") == "" {
		t.Fatalf("%s opens %q", button, act.URL)
	}
	return u.Query().Get("link")
}

// refused checks that an answer is the refusal of a link, saying why, and returns its detail.
func refused(t *testing.T, what string, status int, body, reason string) {
	t.Helper()
	var p problem
	_ = json.Unmarshal([]byte(body), &p)
	if status != http.StatusForbidden || p.Type != ChatLinkProblemType || !strings.Contains(p.Detail, reason) {
		t.Errorf("%s = %d %s, want the link refused: %q", what, status, body, reason)
	}
}

// TestMicrosoftTeams: a request's approvers get a card each in Teams, with what the request says and
// Approve and Deny; its requester a card; the channel of its repository a post with identifiers
// only. Someone without an object ID gets no card, and the row says why. A link opens the request
// with its decision ready for the person it was sent to, in their session; confirmed, it approves
// the request, and every card is updated to say so. A link is refused, and the refusal audited,
// when it was changed, expired, sent to someone else, used for another decision or change set,
// with an API token, or once the request was decided.
func TestMicrosoftTeams(t *testing.T) {
	t.Parallel()
	fakes := teamstest.Start(t)
	h := chatHarness(t, fakes, fakes.CAFile)
	bob, alice, pat := h.as("bob"), h.as("alice"), h.as("pat")
	h.as("fiona")
	h.as("carol")

	// A large request also waits for finops: fiona, who has no object ID.
	large := newVM("prod", "teams-large", 16)
	large["reason"] = hostileText
	var lr RequestView
	if status, out := bob.do(http.MethodPost, Prefix+"/requests/type", large, &lr); status != http.StatusCreated || lr.State != "pending_approval" {
		t.Fatalf("submit = %d %s", status, out)
	}
	h.quiet(t)
	card := cardOf(t, fakes, oidAlice)
	text := card.Card.Text()
	for _, want := range []string{"bob asks for your approval of request #", hostileText, "Create infra-live/vm/prod/teams-large", "production-floor"} {
		if !strings.Contains(text, want) {
			t.Errorf("alice's card does not say %q:\n%s", want, text)
		}
	}
	for _, e := range card.Card.Elements() {
		if e.Markdown {
			t.Errorf("Markdown in a card: %+v", e)
		}
	}
	linkOf(t, card, "Approve", lr.ID)
	linkOf(t, card, "Deny", lr.ID)
	if c := cardOf(t, fakes, oidBob); !strings.Contains(c.Card.Text(), "was submitted") || len(c.Card.Actions()) != 2 {
		t.Errorf("bob's card: %s %+v", c.Card.Text(), c.Card.Actions())
	}
	var skipped bool
	for _, n := range h.notifications(t, lr.ID) {
		if n.Recipient == "user:fiona" && n.Channel == "teams" {
			skipped = n.State == "skipped" && strings.Contains(n.LastError, "no Entra ID object ID")
		}
	}
	if !skipped {
		t.Errorf("fiona's row: %+v", h.notifications(t, lr.ID))
	}
	posts := fakes.Workflows.Posts("infra")
	if len(posts) != 1 || !strings.Contains(posts[0].Card.Text(), "was submitted") || strings.Contains(posts[0].Card.Text(), "javascript") ||
		strings.Contains(posts[0].Card.Text(), "bob") || strings.Contains(posts[0].Card.Text(), "Create ") {
		t.Errorf("the channel's posts: %+v", posts)
	}
	// Whom notifications reach: not finops' rule, whose only approver has no object ID.
	var view RequestView
	bob.do(http.MethodGet, Prefix+"/requests/"+lr.ID, nil, &view)
	for _, rule := range view.Rules {
		if rule.NoOneToNotify != (strings.Join(rule.Approvers, ",") == "group:finops") {
			t.Errorf("rule %v: noOneToNotify %v", rule.Approvers, rule.NoOneToNotify)
		}
	}
	if view.RequesterNotNotified {
		t.Error("bob, who has an object ID, is not notified")
	}

	// A request alice's approval decides.
	var r RequestView
	if status, out := bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "teams-small", 2), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, out)
	}
	h.quiet(t)
	aliceCard, patCard := fakes.Connector.To(oidAlice)[1], fakes.Connector.To(oidPat)[1]
	approve, deny := linkOf(t, aliceCard, "Approve", r.ID), linkOf(t, aliceCard, "Deny", r.ID)
	patApprove := linkOf(t, patCard, "Approve", r.ID)
	check := func(u *user, token string) (int, string, ChatLinkView) {
		var v ChatLinkView
		status, body := u.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/chat-link", map[string]string{"link": token}, &v)
		return status, body, v
	}
	if status, body, v := check(alice, approve); status != http.StatusOK || v.Decision != "approve" || v.Channel != "teams" ||
		v.ExpiresAt.Before(time.Now().Add(chatlink.Lifetime-time.Minute)) {
		t.Errorf("alice opens her Approve = %d %s", status, body)
	}
	if status, body, v := check(alice, deny); status != http.StatusOK || v.Decision != "deny" {
		t.Errorf("alice opens her Deny = %d %s", status, body)
	}

	// Refused: changed, expired, someone else's, another decision, another change set, a token,
	// another request.
	tampered := approve[:len(approve)-4] + "AAAA"
	if approve[len(approve)-4:] == "AAAA" {
		tampered = approve[:len(approve)-4] + "BBBB"
	}
	status, body, _ := check(alice, tampered)
	refused(t, "a changed link", status, body, "does not match")
	status, body, _ = check(pat, approve)
	refused(t, "alice's link opened by pat", status, body, "sent to someone else")
	status, body = pat.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve", "link": approve}, nil)
	refused(t, "alice's link used by pat", status, body, "sent to someone else")
	status, body = alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "deny", "comment": "no", "link": approve}, nil)
	refused(t, "alice's Approve to deny", status, body, "not the link's")
	keys := chatlink.New(func(context.Context) ([]byte, [][]byte, error) { return []byte(linksKey), nil, nil })
	other, err := keys.Make(context.Background(), chatlink.Link{Request: uuid.MustParse(r.ID), Approver: "alice", Decision: chatlink.Approve,
		ChangeSet: "sha256:" + strings.Repeat("0", 64), Channel: "teams"})
	if err != nil {
		t.Fatal(err)
	}
	status, body, _ = check(alice, other)
	refused(t, "a link for another change set", status, body, "changed since the card was sent")
	status, body = alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve", "link": other}, nil)
	refused(t, "an approval for another change set", status, body, "changed since the card was sent")
	status, body = alice.do(http.MethodPost, Prefix+"/requests/"+lr.ID+"/chat-link", map[string]string{"link": approve}, nil)
	refused(t, "a link for another request", status, body, "another request")
	var tok CreatedTokenOutput
	if status, body := alice.do(http.MethodPost, Prefix+"/tokens", map[string]any{"name": "ci", "scopes": []string{"requests:write"}}, &tok.Body); status != http.StatusCreated {
		t.Fatalf("create token = %d %s", status, body)
	}
	status, body = withToken(t, h, tok.Body.Token, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve", "link": approve})
	refused(t, "a link with an API token", status, body, "not with an API token")
	h.clock.set(time.Now().Add(chatlink.Lifetime + time.Minute))
	status, body, _ = check(alice, approve)
	refused(t, "an expired link", status, body, "expired")
	h.clock.set(time.Time{})
	if alice.do(http.MethodGet, Prefix+"/requests/"+r.ID, nil, &view); view.State != "pending_approval" || len(view.Reviews) != 0 {
		t.Fatalf("after the refusals: %s %+v", view.State, view.Reviews)
	}
	ev, personal := h.audited("request.link_refused", r.ID)
	codes := map[string]int{}
	for _, e := range ev {
		codes[e.Details["code"].(string)]++
	}
	want := map[string]int{chatlink.Signature: 1, chatlink.Approver: 2, chatlink.Decision: 1, chatlink.Changed: 2, chatlink.Session: 1, chatlink.Expired: 1}
	for code, n := range want {
		if codes[code] != n {
			t.Errorf("%d refusals %s audited, want %d: %v", codes[code], code, n, codes)
		}
	}
	for _, e := range ev {
		if e.Details["code"] == chatlink.Approver {
			byPerson(t, e, personal, "pat")
			if e.Details["sentTo"] == "alice" || e.Details["decision"] != "approve" || e.Details["channel"] != "teams" {
				t.Errorf("the refusal of alice's link to pat: %+v", e.Details)
			}
		}
	}

	// Confirmed in alice's session, her link approves the request; every card says so.
	var approved RequestView
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve", "link": approve}, &approved); status != http.StatusOK ||
		len(approved.Reviews) != 1 || approved.Reviews[0].Reviewer != "alice" {
		t.Fatalf("alice's approval = %d %s", status, body)
	}
	h.quiet(t)
	aliceCard, patCard = fakes.Connector.To(oidAlice)[1], fakes.Connector.To(oidPat)[1]
	if aliceCard.Updates != 1 || !strings.Contains(aliceCard.Card.Text(), "You approved it.") || len(aliceCard.Card.Actions()) != 1 {
		t.Errorf("alice's card after her approval (%d updates): %s %+v", aliceCard.Updates, aliceCard.Card.Text(), aliceCard.Card.Actions())
	}
	if patCard.Updates != 1 || !strings.Contains(patCard.Card.Text(), "It no longer waits for approval") || len(patCard.Card.Actions()) != 1 {
		t.Errorf("pat's card after alice's approval (%d updates): %s", patCard.Updates, patCard.Card.Text())
	}
	if c := fakes.Connector.To(oidAlice)[0]; c.Updates != 0 {
		t.Errorf("alice's card of the large request was updated: %s", c.Card.Text())
	}
	status, body, _ = check(pat, patApprove)
	refused(t, "pat's link once decided", status, body, "no longer waits for approval")
	if posts := fakes.Workflows.Posts("infra"); len(posts) != 3 || !strings.Contains(posts[2].Card.Text(), "was approved") {
		t.Errorf("the channel's posts: %d", len(posts))
	}
}

// TestMicrosoftTeamsStepUp: a link to a request that needs a step-up asks for it first; signed in again,
// the approver confirms it.
func TestMicrosoftTeamsStepUp(t *testing.T) {
	t.Parallel()
	fakes := teamstest.Start(t)
	h := chatHarness(t, fakes, fakes.CAFile, func(c *config.Config) { c.Auth.StepUp = &config.StepUp{MaxAge: 5 * time.Minute} })
	clk := &testClock{t: time.Now().Truncate(time.Second)}
	h.auth.UseClock(clk.now)
	vm, err := os.ReadFile(filepath.Join(h.work, ".quai/types/vm.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const rules = "  approval:\n    rules:\n"
	if !strings.Contains(string(vm), rules) {
		t.Fatal("the example vm type changed its approval")
	}
	h.push(".quai/types/vm.yaml", strings.Replace(string(vm), rules, "  approval:\n    stepUp: \"params.env == 'prod'\"\n    rules:\n", 1))
	bob, alice := h.as("bob"), h.as("alice")
	h.as("pat")
	var r RequestView
	if status, out := bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "teams-stepup", 2), &r); status != http.StatusCreated || !r.StepUp {
		t.Fatalf("submit = %d %s", status, out)
	}
	h.quiet(t)
	approve := linkOf(t, cardOf(t, fakes, oidAlice), "Approve", r.ID)
	clk.add(6 * time.Minute)
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/chat-link", map[string]string{"link": approve}, nil); status != http.StatusOK {
		t.Errorf("opening needs no step-up: %d %s", status, body)
	}
	status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve", "link": approve}, nil)
	stepUpRefused(t, "alice's old approval from her card", status, body, "more than 5m0s ago")
	alice = h.as("alice")
	var approved RequestView
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve", "link": approve}, &approved); status != http.StatusOK ||
		len(approved.Reviews) != 1 || !approved.Reviews[0].StepUp {
		t.Fatalf("alice's approval, signed in again = %d %s", status, body)
	}
}

// TestMicrosoftTeamsReach: with email and Teams, an approver who turned approvals off by email is reached
// on Teams, with an object ID; without one, they are not, and the request page says so.
func TestMicrosoftTeamsReach(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	fakes := teamstest.Start(t)
	pem, err := os.ReadFile(mail.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, append(pem, fakes.PEM(t)...), 0o600); err != nil {
		t.Fatal(err)
	}
	h := chatHarness(t, fakes, ca, func(c *config.Config) {
		c.Notifications.Email = &config.Email{Host: mail.Host, Port: mail.Port, Transport: config.EmailSTARTTLS, From: "Quai <quai@example.com>"}
	})
	bob, alice, pat := h.as("bob"), h.as("alice"), h.as("pat")
	off := map[string]any{"events": []map[string]string{{"kind": "approval_needed", "channel": "email", "mode": "off"}},
		"digest": map[string]any{"hour": 8, "timeZone": "UTC"}}
	alice.setPrefs(off)
	pat.setPrefs(off)
	var r RequestView
	if status, out := bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "teams-reach", 2), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, out)
	}
	noOne := func() bool {
		t.Helper()
		var v RequestView
		bob.do(http.MethodGet, Prefix+"/requests/"+r.ID, nil, &v)
		for _, rule := range v.Rules {
			if strings.Join(rule.Approvers, ",") == "group:platform-admins" {
				return rule.NoOneToNotify
			}
		}
		t.Fatalf("no platform-admins rule: %+v", v.Rules)
		return false
	}
	if noOne() {
		t.Error("alice and pat, on Teams, are not notified")
	}
	h.quiet(t)
	if len(fakes.Connector.To(oidAlice)) != 1 || len(fakes.Connector.To(oidPat)) != 1 || len(mail.To(t, "alice@example.com")) != 0 {
		t.Errorf("alice's cards %d, pat's %d, alice's emails %d", len(fakes.Connector.To(oidAlice)), len(fakes.Connector.To(oidPat)), len(mail.To(t, "alice@example.com")))
	}
	// Signed in without Entra ID since, they have no object ID: nothing reaches them.
	if _, err := h.store.Pool.Exec(context.Background(), "UPDATE users SET object_id = '' WHERE name IN ('alice', 'pat')"); err != nil {
		t.Fatal(err)
	}
	if !noOne() {
		t.Error("approvers with approvals off by email and no object ID are notified")
	}
}
