package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/slacktest"
	"github.com/damhau/quai/backend/internal/store/db"
)

// slackHostile is what a person may write in a title or a reason for Slack: mentions of everyone
// and of someone, a link with another text, a channel, entities, Markdown.
const slackHostile = `<!channel> <!here> <@U0ADMIN> <https://evil.example.com|click here> <#C0GENERAL> &amp; &lt;b&gt; *bold* _it_ :boom:`

// slackTo returns the Slack channel to the fake, with its secrets in vars, and the clock it waits
// by.
func slackTo(t *testing.T, fake *slacktest.Slack, cfg *config.Slack, vars map[string]string) (*Slack, *fakeClock) {
	t.Helper()
	network, err := netx.New(fake.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	sec := secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { v, ok := vars[n]; return v, ok }}})
	sc, err := NewSlack(*cfg, network, sec)
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Now()}
	sc.UseClock(clk.now)
	sc.sleep = clk.sleep
	return sc, clk
}

func sendSlack(t *testing.T, c *Slack, to Recipient, m Message) (Receipt, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.Send(ctx, to, m)
}

func slackApproval() Message {
	m := approvalMessage()
	m.Subject = "[Quai] Approval needed: #7 " + slackHostile
	m.Request.Title, m.Request.Reason = slackHostile, slackHostile+"\nsecond line"
	m.Request.Requester = "bob <!channel>"
	return m
}

// TestSlackMessage: an approval reaches the person in a direct message, found by their email, with
// what the request says in plain_text objects only, whatever its text, Approve, Deny and the links
// to Quai as buttons; the fallback text has &, < and > escaped. Its update replaces it.
func TestSlackMessage(t *testing.T) {
	t.Parallel()
	fake := slacktest.Start(t)
	alice := fake.Member("Alice@Example.com")
	sc, _ := slackTo(t, fake, fake.Config("env:TOKEN", ""), map[string]string{"TOKEN": fake.Token})
	if addr, ok := sc.Address(db.User{Name: "alice", Email: "alice@example.com"}); !ok || addr != "alice@example.com" {
		t.Errorf("Address = %q %v", addr, ok)
	}
	if _, ok := sc.Address(db.User{Name: "carol"}); ok {
		t.Error("someone without a verified email has an address")
	}
	m := slackApproval()
	at, err := sendSlack(t, sc, Recipient{ID: "user:alice", Name: "alice", Address: "alice@example.com"}, m)
	if err != nil {
		t.Fatal(err)
	}
	got := fake.To(alice)
	if len(got) != 1 || at.Conversation != got[0].Channel || at.Message != got[0].TS || !strings.HasPrefix(at.Conversation, "D") {
		t.Fatalf("sent %+v, receipt %+v", got, at)
	}
	msg := got[0]
	for _, txt := range msg.Texts() {
		if txt.Type != "plain_text" || txt.Emoji == nil || *txt.Emoji {
			t.Errorf("a text object that is not plain_text without emoji: %+v", txt)
		}
	}
	text := msg.PlainText()
	for _, want := range []string{"#7 " + slackHostile, slackHostile, "second line", "bob <!channel>", "modify vms/prod.yaml (+3 -1)",
		"1 to add, 0 to change, 0 to destroy; +12.50 USD a month", "create azurerm_linux_virtual_machine.big01", "Policy no-public-ip: passed",
		"you confirm it there, signed in"} {
		if !strings.Contains(text, want) {
			t.Errorf("the message does not say %q:\n%s", want, text)
		}
	}
	// The fallback text, which Slack formats: escaped, nothing of it acts.
	if strings.ContainsAny(msg.Text, "<>") || !strings.Contains(msg.Text, "&lt;!channel&gt;") || !strings.Contains(msg.Text, "&amp;amp;") ||
		!strings.Contains(msg.Text, "&lt;https://evil.example.com|click here&gt;") {
		t.Errorf("the fallback text: %q", msg.Text)
	}
	buttons := map[string]slacktest.Button{}
	for _, b := range msg.Buttons() {
		buttons[b.Text.Text] = b
	}
	if buttons["Approve"].URL != m.Approve || buttons["Approve"].Style != "primary" || buttons["Deny"].URL != m.Deny || buttons["Deny"].Style != "danger" ||
		buttons["Open in Quai"].URL != m.Link || buttons["Notification preferences"].URL != m.Preferences || len(buttons) != 4 {
		t.Errorf("buttons %+v", buttons)
	}
	// Opened with conversations.open, posted to the DM's ID, never to an email or a user ID.
	var methods []string
	for _, r := range fake.Requests() {
		methods = append(methods, r.Method)
		if r.Method == "chat.postMessage" && !strings.Contains(r.Body, `"channel":"`+at.Conversation+`"`) {
			t.Errorf("posted to %s", r.Body)
		}
	}
	if strings.Join(methods, " ") != "auth.test users.lookupByEmail conversations.open chat.postMessage" {
		t.Errorf("calls %v", methods)
	}

	// The update closes it: the status, identifiers, no buttons but the request.
	closed := Message{Kind: ApprovalNeeded, Headline: "Approval of request #7", Status: "You approved it.", Closed: true,
		Request: &RequestContent{Number: 7, Title: "Create vm", Repository: "infra-live", Reason: "not shown", Requester: "bob"}, Link: m.Link,
		Approve: m.Approve, Deny: m.Deny}
	if err := sc.Update(context.Background(), at, closed); err != nil {
		t.Fatal(err)
	}
	got = fake.To(alice)
	if len(got) != 1 || got[0].Updates != 1 {
		t.Fatalf("after the update: %+v", got)
	}
	if text := got[0].PlainText(); !strings.Contains(text, "You approved it.") || strings.Contains(text, "not shown") || strings.Contains(text, "bob") {
		t.Errorf("the closed message:\n%s", text)
	}
	if bs := got[0].Buttons(); len(bs) != 1 || bs[0].Text.Text != "Open in Quai" {
		t.Errorf("the closed message's buttons: %+v", bs)
	}
	// Gone, or no longer editable: for good.
	fake.Fail("chat.update", "message_not_found")
	var pe interface{ Permanent() bool }
	if err := sc.Update(context.Background(), at, closed); !errors.As(err, &pe) || !pe.Permanent() {
		t.Errorf("a deleted message: %v", err)
	}
	fake.Fail("chat.update", "internal_error")
	if err := sc.Update(context.Background(), at, closed); !errors.As(err, &pe) || pe.Permanent() {
		t.Errorf("an internal error: %v", err)
	}
}

// TestSlackMembers: a message goes to the active full member of the app's workspace with the
// person's email; nobody else holding it is written to (open question 66), and the
// notification is skipped with the reason.
func TestSlackMembers(t *testing.T) {
	t.Parallel()
	fake := slacktest.Start(t)
	no := false
	fake.Member("member@example.com")
	fake.AddUser(slacktest.User{Email: "guest@example.com", Restricted: true})
	fake.AddUser(slacktest.User{Email: "single@example.com", UltraRestricted: true})
	fake.AddUser(slacktest.User{Email: "bot@example.com", Bot: true})
	fake.AddUser(slacktest.User{Email: "stranger@example.com", Stranger: true})
	fake.AddUser(slacktest.User{Email: "other@example.com", TeamID: "T0OTHER"})
	fake.AddUser(slacktest.User{Email: "unconfirmed@example.com", EmailConfirmed: &no})
	fake.AddUser(slacktest.User{Email: "gone@example.com", Deleted: true})
	sc, _ := slackTo(t, fake, fake.Config("env:TOKEN", ""), map[string]string{"TOKEN": fake.Token})
	m := approvalMessage()
	if _, err := sendSlack(t, sc, Recipient{ID: "user:member", Address: "MEMBER@example.com"}, m); err != nil {
		t.Errorf("a member: %v", err)
	}
	for email, why := range map[string]string{
		"nobody@example.com":      "Slack knows no active account with their email",
		"gone@example.com":        "Slack knows no active account with their email",
		"guest@example.com":       "a guest's",
		"single@example.com":      "a guest's",
		"bot@example.com":         "a bot's",
		"stranger@example.com":    "another workspace",
		"other@example.com":       "another workspace",
		"unconfirmed@example.com": "has not confirmed it",
	} {
		_, err := sendSlack(t, sc, Recipient{ID: "user:x", Address: email}, m)
		var u *Unreachable
		if !errors.As(err, &u) || !strings.Contains(u.Reason, why) {
			t.Errorf("%s: %v, want unreachable: %s", email, err, why)
		}
	}
	// Of the app's Enterprise organization, in another of its workspaces: a member.
	fake.Enterprise = "E0QUAI"
	fake.AddUser(slacktest.User{Email: "org@example.com", TeamID: "T0SISTER", EnterpriseID: "E0QUAI"})
	fake.AddUser(slacktest.User{Email: "otherorg@example.com", TeamID: "T0ELSE", EnterpriseID: "E0ELSE"})
	sc2, _ := slackTo(t, fake, fake.Config("env:TOKEN", ""), map[string]string{"TOKEN": fake.Token})
	if _, err := sendSlack(t, sc2, Recipient{ID: "user:org", Address: "org@example.com"}, m); err != nil {
		t.Errorf("of the organization: %v", err)
	}
	var u *Unreachable
	if _, err := sendSlack(t, sc2, Recipient{ID: "user:x", Address: "otherorg@example.com"}, m); !errors.As(err, &u) {
		t.Errorf("of another organization: %v", err)
	}
	// The workspace is asked once per token.
	n := 0
	for _, r := range fake.Requests() {
		if r.Method == "auth.test" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d auth.test for two channels, want 2", n)
	}
}

// TestSlackChannelPost: a channel's post says identifiers only; a channel Slack does not know, or
// the app is not in, is an error that says so.
func TestSlackChannelPost(t *testing.T) {
	t.Parallel()
	fake := slacktest.Start(t)
	fake.AddChannel("C0INFRA001")
	fake.SetChannel(slacktest.Channel{ID: "C0OUTSIDE1"})
	cfg := fake.Config("env:TOKEN", "",
		config.SlackChannel{Name: "infra", Channel: "C0INFRA001", Repositories: []string{"infra-live"}, Events: []string{"approval_needed", "alert"}},
		config.SlackChannel{Name: "shop", Channel: "C0SHOP0001", Teams: []string{"shop"}, Events: []string{"submitted", "approved"}},
		config.SlackChannel{Name: "outside", Channel: "C0OUTSIDE1", Repositories: []string{"other"}, Events: []string{"alert"}})
	sc, _ := slackTo(t, fake, cfg, map[string]string{"TOKEN": fake.Token})
	if posts := sc.Posts([]string{Submitted, ApprovalNeeded}, "infra-live", []string{"team:shop"}); len(posts) != 2 ||
		posts[0] != (Post{"infra", ApprovalNeeded}) || posts[1] != (Post{"shop", Submitted}) {
		t.Errorf("Posts = %+v", posts)
	}
	m := approvalMessage()
	m.Headline = "Request #7 waits for approval."
	m.Request = &RequestContent{Number: 7, Title: "Create infra-live/vm/prod/big01", Repository: "infra-live", State: "pending_approval"}
	if _, err := sendSlack(t, sc, Recipient{ID: GroupRecipient("slack") + "infra"}, m); err != nil {
		t.Fatal(err)
	}
	posts := fake.Posts("C0INFRA001")
	if len(posts) != 1 {
		t.Fatalf("posts %+v", posts)
	}
	text := posts[0].PlainText()
	if !strings.Contains(text, "Request #7 waits for approval.") || !strings.Contains(text, "State: pending approval") || strings.Contains(text, "big01") {
		t.Errorf("the post:\n%s", text)
	}
	if bs := posts[0].Buttons(); len(bs) != 1 || bs[0].Text.Text != "Open in Quai" || bs[0].URL != m.Link {
		t.Errorf("the post's buttons: %+v", bs)
	}
	for name, want := range map[string]string{"shop": "channel_not_found", "outside": "not_in_channel", "nowhere": "no Slack channel nowhere"} {
		if _, err := sendSlack(t, sc, Recipient{ID: GroupRecipient("slack") + name}, m); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %s", name, err, want)
		}
	}
}

// TestSlackThrottled: a 429 is tried again after Retry-After, or a backoff without it, within a
// send; beyond, or when Retry-After is too long, the send fails for the job's backoff.
func TestSlackThrottled(t *testing.T) {
	t.Parallel()
	fake := slacktest.Start(t)
	fake.Member("alice@example.com")
	sc, clk := slackTo(t, fake, fake.Config("env:TOKEN", ""), map[string]string{"TOKEN": fake.Token})
	to := Recipient{ID: "user:alice", Address: "alice@example.com"}
	fake.Throttle(1, "3")
	if _, err := sendSlack(t, sc, to, approvalMessage()); err != nil || len(clk.waits) != 1 || clk.waits[0] != 3*time.Second {
		t.Errorf("throttled once: %v, waits %v", err, clk.waits)
	}
	fake.Throttle(2, "")
	if _, err := sendSlack(t, sc, to, approvalMessage()); err != nil || len(clk.waits) != 3 || clk.waits[1] < time.Second || clk.waits[2] < 2*time.Second {
		t.Errorf("throttled twice: %v, waits %v", err, clk.waits)
	}
	fake.Throttle(maxRetries+1, "1")
	var se *SlackError
	if _, err := sendSlack(t, sc, to, approvalMessage()); !errors.As(err, &se) || se.Status != http.StatusTooManyRequests || se.Code != "ratelimited" || se.Permanent() {
		t.Errorf("throttled too often: %v", err)
	}
	fake.Throttle(1, "3600")
	waits := len(clk.waits)
	if _, err := sendSlack(t, sc, to, approvalMessage()); !errors.As(err, &se) || se.Status != http.StatusTooManyRequests || len(clk.waits) != waits {
		t.Errorf("an hour's Retry-After: %v, waits %v", err, clk.waits)
	}
	// ok false, ratelimited: tried again too.
	fake.Fail("chat.postMessage", "ratelimited")
	if _, err := sendSlack(t, sc, to, approvalMessage()); err != nil {
		t.Errorf("ratelimited once: %v", err)
	}
}

// TestSlackToken: the bot token goes in the Authorization header of every call, never in a URL or
// a body, and is in no error.
func TestSlackToken(t *testing.T) {
	t.Parallel()
	fake := slacktest.Start(t)
	fake.Member("alice@example.com")
	fake.AddChannel("C0INFRA001")
	cfg := fake.Config("env:TOKEN", "", config.SlackChannel{Name: "infra", Channel: "C0INFRA001", Repositories: []string{"r"}, Events: []string{"alert"}})
	sc, _ := slackTo(t, fake, cfg, map[string]string{"TOKEN": " " + fake.Token + "\n"})
	to := Recipient{ID: "user:alice", Address: "alice@example.com"}
	if _, err := sendSlack(t, sc, to, approvalMessage()); err != nil {
		t.Fatal(err)
	}
	if _, err := sendSlack(t, sc, Recipient{ID: GroupRecipient("slack") + "infra"}, approvalMessage()); err != nil {
		t.Fatal(err)
	}
	for _, r := range fake.Requests() {
		if r.Authorization != "Bearer "+fake.Token || strings.Contains(r.RawQuery, fake.Token) || strings.Contains(r.Body, fake.Token) || r.RawQuery != "" ||
			r.HTTPMethod != http.MethodPost {
			t.Errorf("%s: %s ?%s %q", r.Method, r.HTTPMethod, r.RawQuery, r.Authorization)
		}
		if r.Method == "users.lookupByEmail" && (r.ContentType != "application/x-www-form-urlencoded" || r.Body != "email=alice%40example.com") {
			t.Errorf("the email is looked up with %s %q", r.ContentType, r.Body)
		}
	}
	noToken := func(what string, err error, token string) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: no error", what)
			return
		}
		if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), token[strings.LastIndex(token, "-"):]) {
			t.Errorf("%s: the error shows the token: %v", what, err)
		}
	}
	// Refused: Slack's code, never the token.
	fake.Fail("chat.postMessage", "invalid_auth")
	_, err := sendSlack(t, sc, to, approvalMessage())
	noToken("refused", err, fake.Token)
	if !strings.Contains(err.Error(), "invalid_auth") {
		t.Errorf("refused: %v", err)
	}
	wrong := "xoxb-0000-1111-wrongwrongwrong"
	bad, _ := slackTo(t, fake, cfg, map[string]string{"TOKEN": wrong})
	_, err = sendSlack(t, bad, to, approvalMessage())
	noToken("a wrong token", err, wrong)
	// Not a bot token: refused before any call, naming the reference.
	user, _ := slackTo(t, fake, cfg, map[string]string{"TOKEN": "xoxp-a-user-token"})
	_, err = sendSlack(t, user, to, approvalMessage())
	noToken("a user token", err, "xoxp-a-user-token")
	if err == nil || !strings.Contains(err.Error(), "env:TOKEN") {
		t.Errorf("a user token: %v", err)
	}
	missing, _ := slackTo(t, fake, fake.Config("env:NONE", ""), nil)
	if _, err := sendSlack(t, missing, to, approvalMessage()); err == nil || !strings.Contains(err.Error(), "bot token") {
		t.Errorf("no token: %v", err)
	}
	// Unreachable: the error says so, without the token.
	gone := httptest.NewTLSServer(http.NotFoundHandler())
	gone.Close()
	cfg2 := *cfg
	cfg2.APIURL = gone.URL + "/api"
	unreachable, _ := slackTo(t, fake, &cfg2, map[string]string{"TOKEN": fake.Token})
	_, err = sendSlack(t, unreachable, to, approvalMessage())
	noToken("unreachable", err, fake.Token)
	if got := redactToken(fmt.Errorf("Post: Bearer %s refused", fake.Token), fake.Token).Error(); strings.Contains(got, fake.Token) {
		t.Errorf("redacted = %q", got)
	}
}

// TestSlackSize: a message with many files, plans and resources stays within Block Kit's limits
// (the fake checks them) and under maxSlackBlocks, leaving the files and resources out when it
// must; the counts stay. A digest too.
func TestSlackSize(t *testing.T) {
	t.Parallel()
	fake := slacktest.Start(t)
	alice := fake.Member("alice@example.com")
	sc, _ := slackTo(t, fake, fake.Config("env:TOKEN", ""), map[string]string{"TOKEN": fake.Token})
	m := slackApproval()
	m.Request.Reason = strings.Repeat("<!channel> & ", 400)
	for i := range 400 {
		m.Request.Changes = append(m.Request.Changes, FileChange{Path: strings.Repeat("d/", 50) + uuid.NewString() + ".yaml", Op: "add", Added: i})
	}
	for i := range 45 {
		p := PlanContent{Stack: fmt.Sprintf("stack-%d", i), Status: "succeeded", Add: i}
		for j := range 10 {
			p.Resources = append(p.Resources, ResourceChange{"create", strings.Repeat("module.x.", 30) + strconv.Itoa(j)})
		}
		m.Request.Plans = append(m.Request.Plans, p)
	}
	m.Headline = strings.Repeat("A long headline ", 20)
	text, blocks := slackPersonal(m)
	b, _ := json.Marshal(blocks)
	if len(b) > maxSlackBlocks || len(blocks) > slackMaxBlocks || !bytes.Contains(b, []byte("Changes (401 files)")) || !bytes.Contains(b, []byte(fmt.Sprintf("and %d more plans", len(m.Request.Plans)-slackPlans))) {
		t.Errorf("%d blocks, %d bytes", len(blocks), len(b))
	}
	if bytes.Contains(b, []byte("module.x.")) || bytes.Contains(b, []byte(".yaml")) {
		t.Error("the files and resources are still there")
	}
	if units(text) > 4000 {
		t.Errorf("a fallback of %d", units(text))
	}
	if _, err := sendSlack(t, sc, Recipient{ID: "user:alice", Address: "alice@example.com"}, m); err != nil {
		t.Fatal(err)
	}
	d := Message{Kind: DigestKind, Subject: "[Quai] Your digest of Monday 9 November 2026: 40 updates", Headline: "What happened.", Link: "https://quai.example.com/requests",
		Preferences: "https://quai.example.com/profile"}
	for i := range 40 {
		d.Digest = append(d.Digest, DigestItem{Label: "Approval needed", Headline: slackHostile, Request: fmt.Sprintf("#%d %s (infra-live)", i, slackHostile)})
	}
	if _, err := sendSlack(t, sc, Recipient{ID: "user:alice", Address: "alice@example.com"}, d); err != nil {
		t.Fatal(err)
	}
	if got := fake.To(alice); len(got) != 2 || !strings.Contains(got[1].PlainText(), "and 10 more") || strings.ContainsAny(got[1].Text, "<>") {
		t.Errorf("the digest: %+v", got)
	}
}

// TestClipUnits: text is cut on a character boundary, counted as Slack counts.
func TestClipUnits(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   string
		n    int
		want string
	}{{"abc", 3, "abc"}, {"abcdef", 5, "ab..."}, {"😀😀😀", 6, "😀😀😀"}, {"😀😀😀😀", 7, "😀😀..."}, {"éééé", 4, "éééé"}} {
		if got := clipUnits(tt.in, tt.n); got != tt.want || units(got) > tt.n {
			t.Errorf("clipUnits(%q, %d) = %q", tt.in, tt.n, got)
		}
	}
}

// slackEndpoint returns the interaction endpoint with a signing secret, its clock and its log.
func slackEndpoint(t *testing.T, secret string, now time.Time) (*SlackInteractions, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	sec := secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { return secret, n == "SIGNING" && secret != "" }}})
	h := NewSlackInteractions(&config.Slack{SigningSecret: config.SecretRef{Ref: "env:SIGNING"}}, sec, slog.New(slog.NewTextHandler(&logs, nil)))
	h.UseClock(func() time.Time { return now })
	return h, &logs
}

func serveSlack(h http.Handler, body []byte, headers map[string][]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/slack/interactions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// TestSlackInteractions: Slack's example request is verified and acknowledged; a request with a
// signature that does not match, a timestamp more than five minutes away either way, a header
// missing, malformed or sent twice, or a body over the limit is refused, the same way, and logged
// without its body or signature. Without a signing secret there is no endpoint; a flood is cut
// short.
func TestSlackInteractions(t *testing.T) {
	t.Parallel()
	// Slack's example (verifying-requests-from-slack), a slash command's body.
	const (
		secret = "8f742231b10e8888abcd99yyyzzz85a5"
		ts     = "1531420618"
		sig    = "v0=a2114d57b48eac39b9ad189dd8316235a7b4a8d21a10bd27519666489c69b503"
		body   = "token=xyzz0WbapA4vBCDEFasx0q6G&team_id=T1DC2JH3J&team_domain=testteamnow&channel_id=G8PSS9T3V&channel_name=foobar" +
			"&user_id=U2CERLKJA&user_name=roadrunner&command=%2Fwebhook-collect&text=&response_url=https%3A%2F%2Fhooks.slack.com%2Fcommands%2FT1DC2JH3J%2F397700885554%2F96rGlfmibIGlgcZRskXaIFfN" +
			"&trigger_id=398738663015.47445629121.803a0bc887a14d10d2c447fce8b6703c"
	)
	if got := slacktest.Sign(secret, ts, []byte(body)); got != sig {
		t.Fatalf("the fake signs Slack's example %s", got)
	}
	at := time.Unix(1531420618, 0)
	h, logs := slackEndpoint(t, secret, at)
	signed := map[string][]string{"X-Slack-Request-Timestamp": {ts}, "X-Slack-Signature": {sig}}
	if w := serveSlack(h, []byte(body), signed); w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("Slack's example = %d %q", w.Code, w.Body.String())
	}
	for _, d := range []time.Duration{-slackSkew, slackSkew} {
		h, _ := slackEndpoint(t, secret, at.Add(d))
		if w := serveSlack(h, []byte(body), signed); w.Code != http.StatusOK {
			t.Errorf("five minutes away (%v) = %d", d, w.Code)
		}
	}

	other := "v0=" + strings.Repeat("0", 64)
	if other == sig {
		t.Fatal("the same signature")
	}
	flipped := []byte(sig)
	flipped[len(flipped)-1] ^= 1 // '3' -> '2': still hex, another signature
	big := []byte(body + "&pad=" + strings.Repeat("a", maxSlackInteraction))
	cases := map[string]struct {
		at      time.Time
		body    []byte
		headers map[string][]string
	}{
		"another signature":  {at, []byte(body), map[string][]string{"X-Slack-Request-Timestamp": {ts}, "X-Slack-Signature": {other}}},
		"one bit off":        {at, []byte(body), map[string][]string{"X-Slack-Request-Timestamp": {ts}, "X-Slack-Signature": {string(flipped)}}},
		"another body":       {at, []byte(body + "&x=1"), signed},
		"another timestamp":  {at, []byte(body), map[string][]string{"X-Slack-Request-Timestamp": {"1531420619"}, "X-Slack-Signature": {sig}}},
		"old":                {at.Add(slackSkew + time.Second), []byte(body), signed},
		"future":             {at.Add(-slackSkew - time.Second), []byte(body), signed},
		"no signature":       {at, []byte(body), map[string][]string{"X-Slack-Request-Timestamp": {ts}}},
		"no timestamp":       {at, []byte(body), map[string][]string{"X-Slack-Signature": {sig}}},
		"nothing":            {at, []byte(body), nil},
		"two signatures":     {at, []byte(body), map[string][]string{"X-Slack-Request-Timestamp": {ts}, "X-Slack-Signature": {sig, sig}}},
		"two timestamps":     {at, []byte(body), map[string][]string{"X-Slack-Request-Timestamp": {ts, ts}, "X-Slack-Signature": {sig}}},
		"uppercase":          {at, []byte(body), map[string][]string{"X-Slack-Request-Timestamp": {ts}, "X-Slack-Signature": {strings.ToUpper(sig)}}},
		"v1":                 {at, []byte(body), map[string][]string{"X-Slack-Request-Timestamp": {ts}, "X-Slack-Signature": {"v1=" + sig[3:]}}},
		"short":              {at, []byte(body), map[string][]string{"X-Slack-Request-Timestamp": {ts}, "X-Slack-Signature": {sig[:len(sig)-2]}}},
		"signed timestamp":   {at, []byte(body), map[string][]string{"X-Slack-Request-Timestamp": {"+" + ts}, "X-Slack-Signature": {sig}}},
		"timestamp in ms":    {at, []byte(body), map[string][]string{"X-Slack-Request-Timestamp": {ts + "000"}, "X-Slack-Signature": {sig}}},
		"too large, signed":  {at, big, map[string][]string{"X-Slack-Request-Timestamp": {ts}, "X-Slack-Signature": {slacktest.Sign(secret, ts, big)}}},
		"empty body, signed": {at, nil, map[string][]string{"X-Slack-Request-Timestamp": {ts}, "X-Slack-Signature": {sig}}},
	}
	for name, c := range cases {
		h, logs := slackEndpoint(t, secret, c.at)
		w := serveSlack(h, c.body, c.headers)
		if w.Code != http.StatusUnauthorized || w.Body.Len() != 0 || len(w.Header()) != 0 {
			t.Errorf("%s = %d %q %v, want 401 and nothing else", name, w.Code, w.Body.String(), w.Header())
		}
		if l := logs.String(); !strings.Contains(l, "a Slack interaction request was refused") || strings.Contains(l, sig[3:]) || strings.Contains(l, "xyzz0WbapA4vBCDEFasx0q6G") ||
			strings.Contains(l, "roadrunner") || strings.Contains(l, secret) {
			t.Errorf("%s: logged %s", name, l)
		}
	}
	if strings.Contains(logs.String(), "refused") {
		t.Errorf("Slack's example refused: %s", logs.String())
	}

	// No signing secret configured: no endpoint. One that cannot be resolved: no answer either way.
	if NewSlackInteractions(&config.Slack{}, nil, nil) != nil || NewSlackInteractions(nil, nil, nil) != nil {
		t.Error("an endpoint without a signing secret")
	}
	unresolved, _ := slackEndpoint(t, "", at)
	if w := serveSlack(unresolved, []byte(body), signed); w.Code != http.StatusServiceUnavailable {
		t.Errorf("an unresolved secret = %d", w.Code)
	}

	// A flood: the burst, then 429 before any work.
	h, _ = slackEndpoint(t, secret, at)
	codes := map[int]int{}
	for range slackBurst + 10 {
		codes[serveSlack(h, []byte(body), signed).Code]++
	}
	if codes[http.StatusOK] < slackBurst || codes[http.StatusTooManyRequests] == 0 {
		t.Errorf("a flood: %v", codes)
	}
}

// TestSlackInteractionPayload: a block_actions request as the fake builds it, from a message Quai
// sent, stays under the endpoint's limit however hostile the message, and is acknowledged.
func TestSlackInteractionPayload(t *testing.T) {
	t.Parallel()
	fake := slacktest.Start(t)
	alice := fake.Member("alice@example.com")
	sc, _ := slackTo(t, fake, fake.Config("env:TOKEN", ""), map[string]string{"TOKEN": fake.Token})
	m := slackApproval()
	m.Request.Reason = strings.Repeat("<>&\"", 500)
	m.Request.Waiting = []string{strings.Repeat("<&>", 300)}
	for i := range 20 {
		m.Request.Changes = append(m.Request.Changes, FileChange{Path: strings.Repeat("<&>", 60) + strconv.Itoa(i), Op: "add"})
	}
	for i := range 30 {
		m.Request.Plans = append(m.Request.Plans, PlanContent{Stack: strings.Repeat("<", 100), Status: "succeeded", Add: i,
			Policies:  []PolicyResult{{strings.Repeat("&", 100), "passed"}},
			Resources: []ResourceChange{{"create", strings.Repeat("\"<>", 100)}}})
	}
	if _, err := sendSlack(t, sc, Recipient{ID: "user:alice", Address: "alice@example.com"}, m); err != nil {
		t.Fatal(err)
	}
	msg := fake.To(alice)[0]
	approve, _ := msg.Button("Approve")
	req, err := fake.Interaction("https://quai.example.com/slack/interactions", fake.BlockActions(msg, approve, alice), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	_, _ = body.ReadFrom(req.Body)
	if body.Len() > maxSlackInteraction {
		t.Fatalf("a click's request of %d bytes", body.Len())
	}
	now := time.Now()
	h, _ := slackEndpoint(t, fake.SigningSecret, now)
	if w := serveSlack(h, body.Bytes(), req.Header); w.Code != http.StatusOK {
		t.Errorf("a click = %d", w.Code)
	}
}
