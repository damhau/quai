package api

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/slacktest"
)

// slackHostileText is what a person may write in a reason for Slack: mentions of everyone and of
// someone, a link with another text, Markdown.
const slackHostileText = `<!channel> <@U0ADMIN> <https://evil.example.com|click here> *bold* &amp;`

// slackInfra is the Slack channel of infra-live's requests.
const slackInfra = "C0INFRA001"

// withSlack configures notifications to the Slack fake: the app, its signing secret, the infra
// channel posting requests of infra-live, and the links' key. fiona has an email Slack does not
// know; carol has no email.
func withSlack(fake *slacktest.Slack) func(*config.Config) {
	return func(c *config.Config) {
		c.Network.CAFile = fake.CAFile
		c.Notifications.Slack = fake.Config("env:SLACK_BOT_TOKEN", "env:SLACK_SIGNING_SECRET", config.SlackChannel{Name: "infra", Channel: slackInfra,
			Repositories: []string{"infra-live"}, Events: []string{"submitted", "approval_needed", "approved", "alert"}})
		c.Notifications.Links = &config.ChatLinks{Key: config.SecretRef{Ref: "env:QUAI_LINKS_KEY"}}
		c.Auth.Dev.Users = []config.DevUser{
			{Name: "alice", Email: "alice@example.com", Groups: []string{"platform-admins"}},
			{Name: "pat", Email: "pat@example.com", Groups: []string{"platform-admins"}},
			{Name: "bob", Email: "bob@example.com", Groups: []string{"developers", "shop"}},
			{Name: "fiona", Email: "fiona@example.com", Groups: []string{"finops", "developers"}},
			{Name: "carol", Groups: []string{"shop", "developers"}},
		}
	}
}

// slackHarness is newHarness with notifications to the Slack fake, then configure's changes;
// alice, pat and bob are members of the workspace. It returns their Slack user IDs.
func slackHarness(t *testing.T, fake *slacktest.Slack, configure ...func(*config.Config)) (*harness, map[string]string) {
	t.Helper()
	vars := map[string]string{"SLACK_BOT_TOKEN": fake.Token, "SLACK_SIGNING_SECRET": fake.SigningSecret, "QUAI_LINKS_KEY": linksKey}
	sec := secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { v, ok := vars[n]; return v, ok }}})
	dir := t.TempDir()
	bare, work := localRemote(t, dir)
	h := startRepo(t, dir, "{ name: infra-live, title: Infrastructure }", `connector: generic
  url: `+bare+`
  enforcement:
    floorRules: [{ name: production-floor, when: "params.env == 'prod'", approvers: [group:platform-admins] }]
  sync: { minInterval: 1ns }`, repositories.Options{Secrets: sec}, append([]func(*config.Config){withSlack(fake)}, configure...)...)
	h.bare, h.work = bare, work
	fake.AddChannel(slackInfra)
	return h, map[string]string{"alice": fake.Member("alice@example.com"), "pat": fake.Member("pat@example.com"), "bob": fake.Member("bob@example.com")}
}

// dmOf returns the one direct message to someone, failing the test otherwise.
func dmOf(t *testing.T, fake *slacktest.Slack, user string) slacktest.Message {
	t.Helper()
	got := fake.To(user)
	if len(got) != 1 {
		t.Fatalf("%d messages to %s, want 1: %+v", len(got), user, got)
	}
	return got[0]
}

// slackLinkOf returns the token of a message's button, checking it opens the request on Quai.
func slackLinkOf(t *testing.T, m slacktest.Message, button, request string) string {
	t.Helper()
	b, ok := m.Button(button)
	if !ok {
		t.Fatalf("no %s button: %+v", button, m.Buttons())
	}
	u, err := url.Parse(b.URL)
	if err != nil || u.Scheme+"://"+u.Host+u.Path != "https://quai.example.com/requests/"+request || len(u.Query()) != 1 || u.Query().Get("link") == "" {
		t.Fatalf("%s opens %q", button, b.URL)
	}
	return u.Query().Get("link")
}

// interact sends Slack's request of a click to Quai with a client, and returns the answer's status,
// its body and whether it set a cookie.
func interact(t *testing.T, c *http.Client, req *http.Request) (int, string, bool) {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), len(resp.Header.Values("Set-Cookie")) > 0
}

// TestSlack: a request's approvers get a direct message each in Slack, with what the request says in
// plain text and Approve and Deny; its requester a message; the channel of its repository a post
// with identifiers only. Someone Slack does not know gets none, and the row says why. A link opens
// the request with its decision ready, in the person's session; confirmed, it approves the
// request, and every message is updated to say so. Slack's request for a click is verified and
// acknowledged, and changes nothing, whoever sends it; one not signed, signed with another
// secret, or too old or too far ahead is refused. The exemption from sessions and CSRF tokens is
// that one path's only.
func TestSlack(t *testing.T) {
	t.Parallel()
	fake := slacktest.Start(t)
	h, ids := slackHarness(t, fake)
	bob, alice, pat := h.as("bob"), h.as("alice"), h.as("pat")
	h.as("fiona")
	h.as("carol")

	// A large request also waits for finops: fiona, whom Slack does not know.
	large := newVM("prod", "slack-large", 16)
	large["reason"] = slackHostileText
	var lr RequestView
	if status, out := bob.do(http.MethodPost, Prefix+"/requests/type", large, &lr); status != http.StatusCreated || lr.State != "pending_approval" {
		t.Fatalf("submit = %d %s", status, out)
	}
	h.quiet(t)
	dm := dmOf(t, fake, ids["alice"])
	text := dm.PlainText()
	for _, want := range []string{"bob asks for your approval of request #", slackHostileText, "Create infra-live/vm/prod/slack-large", "production-floor"} {
		if !strings.Contains(text, want) {
			t.Errorf("alice's message does not say %q:\n%s", want, text)
		}
	}
	for _, txt := range dm.Texts() {
		if txt.Type != "plain_text" {
			t.Errorf("formatted text in a message: %+v", txt)
		}
	}
	if strings.ContainsAny(dm.Text, "<>") {
		t.Errorf("the fallback text is not escaped: %q", dm.Text)
	}
	slackLinkOf(t, dm, "Approve", lr.ID)
	slackLinkOf(t, dm, "Deny", lr.ID)
	if m := dmOf(t, fake, ids["bob"]); !strings.Contains(m.PlainText(), "was submitted") || len(m.Buttons()) != 2 {
		t.Errorf("bob's message: %s %+v", m.PlainText(), m.Buttons())
	}
	var skipped bool
	for _, n := range h.notifications(t, lr.ID) {
		if n.Recipient == "user:fiona" && n.Channel == "slack" {
			skipped = n.State == "skipped" && strings.Contains(n.LastError, "Slack knows no active account with their email")
		}
	}
	if !skipped {
		t.Errorf("fiona's row: %+v", h.notifications(t, lr.ID))
	}
	posts := fake.Posts(slackInfra)
	if len(posts) != 1 || !strings.Contains(posts[0].PlainText(), "was submitted") || strings.Contains(posts[0].PlainText(), "evil") ||
		strings.Contains(posts[0].PlainText(), "bob") || strings.Contains(posts[0].PlainText(), "Create ") {
		t.Errorf("the channel's posts: %+v", posts)
	}

	// A request alice's approval decides.
	var r RequestView
	if status, out := bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "slack-small", 2), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, out)
	}
	h.quiet(t)
	aliceDM, patDM := fake.To(ids["alice"])[1], fake.To(ids["pat"])[1]
	approve := slackLinkOf(t, aliceDM, "Approve", r.ID)
	patApprove := slackLinkOf(t, patDM, "Approve", r.ID)
	var link ChatLinkView
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/chat-link", map[string]string{"link": approve}, &link); status != http.StatusOK ||
		link.Decision != "approve" || link.Channel != "slack" {
		t.Errorf("alice opens her Approve = %d %s", status, body)
	}
	status, body := pat.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve", "link": approve}, nil)
	refused(t, "alice's link used by pat", status, body, "sent to someone else")

	// Slack's request for alice's click on Approve: acknowledged, and nothing else, even with her
	// session's cookie.
	endpoint := h.srv.URL + "/slack/interactions"
	approveButton, _ := aliceDM.Button("Approve")
	click := fake.BlockActions(aliceDM, approveButton, ids["alice"])
	req, err := fake.Interaction(endpoint, click, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if status, body, cookie := interact(t, alice.c, req); status != http.StatusOK || body != "" || cookie {
		t.Errorf("Slack's request = %d %q, cookie %v", status, body, cookie)
	}
	var view RequestView
	if alice.do(http.MethodGet, Prefix+"/requests/"+r.ID, nil, &view); view.State != "pending_approval" || len(view.Reviews) != 0 {
		t.Fatalf("after Slack's request: %s %+v", view.State, view.Reviews)
	}

	// Refused, the same way: another secret, a changed body, too old, too far ahead, no signature.
	other := slacktest.Start(t) // another app: another signing secret
	refusals := map[string]func() *http.Request{
		"another secret": func() *http.Request { r, _ := other.Interaction(endpoint, click, time.Now()); return r },
		"a changed body": func() *http.Request {
			r, _ := fake.Interaction(endpoint, click, time.Now())
			signed, _ := fake.Interaction(endpoint, map[string]any{"type": "block_actions"}, time.Now())
			r.Header = signed.Header
			return r
		},
		"six minutes old": func() *http.Request {
			r, _ := fake.Interaction(endpoint, click, time.Now().Add(-6*time.Minute))
			return r
		},
		"six minutes ahead": func() *http.Request {
			r, _ := fake.Interaction(endpoint, click, time.Now().Add(6*time.Minute))
			return r
		},
		"no signature": func() *http.Request {
			r, _ := fake.Interaction(endpoint, click, time.Now())
			r.Header.Del("X-Slack-Signature")
			return r
		},
	}
	for name, make := range refusals {
		if status, body, cookie := interact(t, http.DefaultClient, make()); status != http.StatusUnauthorized || body != "" || cookie {
			t.Errorf("%s = %d %q, cookie %v", name, status, body, cookie)
		}
	}
	// By the server's clock: a request signed now is old once the clock is six minutes ahead.
	req, _ = fake.Interaction(endpoint, click, time.Now())
	h.clock.set(time.Now().Add(6 * time.Minute))
	status, _, _ = interact(t, http.DefaultClient, req)
	h.clock.set(time.Time{})
	if status != http.StatusUnauthorized {
		t.Errorf("by the server's clock, six minutes old = %d", status)
	}

	// The exemption is that path's only: elsewhere, alice's session without its CSRF token is
	// refused; there, Slack's handler refuses what Slack did not sign.
	noCSRF := &user{h: h, c: alice.c}
	for _, path := range []string{"/slack/interactions/", "/slack/interactionsx", "/slack/%69nteractions", "/slack//interactions", Prefix + "/requests/" + r.ID + "/review"} {
		status, body := noCSRF.do(http.MethodPost, path, map[string]string{"decision": "approve", "link": approve}, nil)
		if status != http.StatusForbidden || !strings.Contains(body, "X-CSRF-Token") {
			t.Errorf("%s without a CSRF token = %d %s", path, status, body)
		}
	}
	if status, body := noCSRF.do(http.MethodPost, "/slack/interactions", map[string]string{"decision": "approve"}, nil); status != http.StatusUnauthorized || body != "" {
		t.Errorf("/slack/interactions with a session, unsigned = %d %s", status, body)
	}
	if alice.do(http.MethodGet, Prefix+"/requests/"+r.ID, nil, &view); view.State != "pending_approval" || len(view.Reviews) != 0 {
		t.Fatalf("after the refusals: %s %+v", view.State, view.Reviews)
	}

	// Confirmed in alice's session, her link approves the request; every message says so.
	var approved RequestView
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve", "link": approve}, &approved); status != http.StatusOK ||
		len(approved.Reviews) != 1 || approved.Reviews[0].Reviewer != "alice" {
		t.Fatalf("alice's approval = %d %s", status, body)
	}
	h.quiet(t)
	aliceDM, patDM = fake.To(ids["alice"])[1], fake.To(ids["pat"])[1]
	if aliceDM.Updates != 1 || !strings.Contains(aliceDM.PlainText(), "You approved it.") || len(aliceDM.Buttons()) != 1 {
		t.Errorf("alice's message after her approval (%d updates): %s %+v", aliceDM.Updates, aliceDM.PlainText(), aliceDM.Buttons())
	}
	if patDM.Updates != 1 || !strings.Contains(patDM.PlainText(), "It no longer waits for approval") || len(patDM.Buttons()) != 1 {
		t.Errorf("pat's message after alice's approval (%d updates): %s", patDM.Updates, patDM.PlainText())
	}
	if m := fake.To(ids["alice"])[0]; m.Updates != 0 {
		t.Errorf("alice's message of the large request was updated: %s", m.PlainText())
	}
	status, body = pat.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/chat-link", map[string]string{"link": patApprove}, nil)
	refused(t, "pat's link once decided", status, body, "no longer waits for approval")
	if posts := fake.Posts(slackInfra); len(posts) != 3 || !strings.Contains(posts[2].PlainText(), "was approved") {
		t.Errorf("the channel's posts: %d", len(posts))
	}

	// Every call to Slack carried the token in its header; none is in the log, nor the signing
	// secret, nor a refused request's signature.
	for _, c := range fake.Requests() {
		if c.Authorization != "Bearer "+fake.Token || c.RawQuery != "" || strings.Contains(c.Body, fake.Token) {
			t.Errorf("%s: %q ?%s", c.Method, c.Authorization, c.RawQuery)
		}
	}
	logs := h.log.String()
	if !strings.Contains(logs, "a Slack interaction request was refused") {
		t.Error("no refusal logged")
	}
	for _, s := range []string{fake.Token, fake.SigningSecret, other.SigningSecret, "payload="} {
		if strings.Contains(logs, s) {
			t.Errorf("the log shows %q", s)
		}
	}
}

// TestSlackPreferences: Slack is a channel of people's preferences: a requester who turned
// submissions off on Slack gets no message there; approvals cannot be off on the only channel.
// Without a signing secret, there is no endpoint for Slack's requests.
func TestSlackPreferences(t *testing.T) {
	t.Parallel()
	fake := slacktest.Start(t)
	h, ids := slackHarness(t, fake, func(c *config.Config) { c.Notifications.Slack.SigningSecret = config.SecretRef{} })
	bob, alice := h.as("bob"), h.as("alice")
	h.as("pat")
	digest := map[string]any{"hour": 8, "timeZone": "UTC"}
	prefs := bob.setPrefs(map[string]any{"events": []map[string]string{{"kind": "submitted", "channel": "slack", "mode": "off"}}, "digest": digest})
	if strings.Join(prefs.Body.Channels, ",") != "slack" {
		t.Errorf("channels %v", prefs.Body.Channels)
	}
	if status, body := alice.do(http.MethodPut, Prefix+"/me/preferences", map[string]any{"events": []map[string]string{{"kind": "approval_needed", "channel": "slack", "mode": "off"}},
		"digest": digest}, nil); status != http.StatusUnprocessableEntity || !strings.Contains(body, "approvers must hear") {
		t.Errorf("approvals off on Slack alone = %d %s", status, body)
	}
	var r RequestView
	if status, out := bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "slack-prefs", 2), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, out)
	}
	h.quiet(t)
	if len(fake.To(ids["bob"])) != 0 || len(fake.To(ids["alice"])) != 1 || len(fake.To(ids["pat"])) != 1 {
		t.Errorf("bob's messages %d, alice's %d, pat's %d", len(fake.To(ids["bob"])), len(fake.To(ids["alice"])), len(fake.To(ids["pat"])))
	}
	req, err := fake.Interaction(h.srv.URL+"/slack/interactions", map[string]any{"type": "block_actions"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _ := interact(t, alice.c, req); status != http.StatusNotFound {
		t.Errorf("Slack's request without a signing secret = %d", status)
	}
}
