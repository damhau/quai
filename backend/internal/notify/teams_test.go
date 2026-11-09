package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store/db"
	"github.com/damhau/quai/backend/internal/teamstest"
)

// hostile is what a person may write in a title or a reason: quotes, braces, Markdown links, a
// script.
const hostile = `"}]} {{.Secret}} [click](javascript:alert(1)) <script>alert(1)</script> **bold** \u0000 ` + "`code`"

// teamsTo returns the Teams channel to the fakes, with its secrets in vars, and the clock and the
// waits it uses.
func teamsTo(t *testing.T, fakes *teamstest.Teams, cfg config.Teams, vars map[string]string) (*Teams, *fakeClock) {
	t.Helper()
	network, err := netx.New(fakes.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	sec := secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { v, ok := vars[n]; return v, ok }}})
	tc, err := NewTeams(cfg, network, sec)
	if err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Now()}
	tc.UseClock(clk.now)
	tc.sleep = clk.sleep
	return tc, clk
}

type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	waits []time.Duration
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	return nil
}

func approvalMessage() Message {
	id := uuid.MustParse("0b6e4c43-9a0f-4f53-9d1b-3c4f1f6c2a11")
	page := "https://quai.example.com/requests/" + id.String()
	return Message{Kind: ApprovalNeeded, Subject: "[Quai] Approval needed: #7 " + hostile, Headline: "bob asks for your approval of request #7.",
		Request: &RequestContent{ID: id, State: "pending_approval", ChangeSet: "sha256:" + strings.Repeat("ab", 32), Number: 7, Title: hostile,
			Repository: "infra-live", Requester: "bob", Reason: hostile + "\nsecond line", Waiting: []string{"production-floor (group:platform-admins)"},
			Changes: []FileChange{{Path: "vms/prod.yaml", Op: "modify", Added: 3, Removed: 1}},
			Plans: []PlanContent{{Stack: "prod", Status: "succeeded", Add: 1, Cost: "+12.50 USD a month", Policies: []PolicyResult{{"no-public-ip", "passed"}},
				Resources: []ResourceChange{{"create", "azurerm_linux_virtual_machine.big01"}}}}},
		Link: page, Preferences: "https://quai.example.com/profile", Approve: page + "?link=v1.approve", Deny: page + "?link=v1.deny"}
}

func sendTeams(t *testing.T, c *Teams, to Recipient, m Message) (Receipt, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.Send(ctx, to, m)
}

// TestTeamsCard: an approval card reaches the person by their object ID, with what the request
// says in TextRuns (never Markdown) whatever its text, Approve, Deny and the links to Quai as
// buttons; its update replaces it.
func TestTeamsCard(t *testing.T) {
	t.Parallel()
	fakes := teamstest.Start(t)
	oid := uuid.NewString()
	fakes.Connector.Install(oid)
	tc, _ := teamsTo(t, fakes, config.Teams{Bot: fakes.Bot("env:SECRET")}, map[string]string{"SECRET": fakes.Login.Secret})
	if addr, ok := tc.Address(db.User{Name: "alice", ObjectID: oid}); !ok || addr != oid {
		t.Errorf("Address = %q %v", addr, ok)
	}
	if _, ok := tc.Address(db.User{Name: "fiona", Email: "fiona@example.com"}); ok {
		t.Error("someone without an object ID has an address")
	}
	m := approvalMessage()
	at, err := sendTeams(t, tc, Recipient{ID: "user:alice", Name: "alice", Address: oid}, m)
	if err != nil {
		t.Fatal(err)
	}
	got := fakes.Connector.To(oid)
	if len(got) != 1 || at.Conversation != got[0].Conversation || at.Message != got[0].ID {
		t.Fatalf("sent %+v, receipt %+v", got, at)
	}
	card := got[0].Card
	for _, e := range card.Elements() {
		if e.Markdown {
			t.Errorf("a Markdown element: %+v", e)
		}
		if e.Type != "RichTextBlock" && e.Type != "TextRun" && e.Type != "Container" && e.Type != "Action.OpenUrl" {
			t.Errorf("element %s", e.Type)
		}
	}
	text := card.Text()
	for _, want := range []string{"#7 " + hostile, hostile, "second line", "bob", "modify vms/prod.yaml (+3 -1)", "1 to add, 0 to change, 0 to destroy; +12.50 USD a month",
		"create azurerm_linux_virtual_machine.big01", "passed", "you confirm it there, signed in"} {
		if !strings.Contains(text, want) {
			t.Errorf("the card does not say %q:\n%s", want, text)
		}
	}
	actions := map[string]string{}
	for _, a := range card.Actions() {
		if a.Type != "Action.OpenUrl" || !strings.HasPrefix(a.URL, "https://quai.example.com/") {
			t.Errorf("action %+v", a)
		}
		actions[a.Title] = a.URL
	}
	if actions["Approve"] != m.Approve || actions["Deny"] != m.Deny || actions["Open in Quai"] != m.Link || actions["Notification preferences"] != m.Preferences {
		t.Errorf("actions %v", actions)
	}

	// The update closes it: the status, identifiers, no buttons but the request.
	closed := Message{Kind: ApprovalNeeded, Headline: "Approval of request #7", Status: "You approved it.", Closed: true,
		Request: &RequestContent{Number: 7, Title: hostile, Repository: "infra-live", Reason: "not shown"}, Link: m.Link, Approve: m.Approve, Deny: m.Deny}
	if err := tc.Update(context.Background(), at, closed); err != nil {
		t.Fatal(err)
	}
	got = fakes.Connector.To(oid)
	if len(got) != 1 || got[0].Updates != 1 {
		t.Fatalf("after the update: %+v", got)
	}
	if text := got[0].Card.Text(); !strings.Contains(text, "You approved it.") || strings.Contains(text, "not shown") || strings.Contains(text, "bob") {
		t.Errorf("the closed card:\n%s", text)
	}
	if acts := got[0].Card.Actions(); len(acts) != 1 || acts[0].Title != "Open in Quai" {
		t.Errorf("the closed card's actions: %+v", acts)
	}
}

// TestTeamsToken: the bot's token is asked for once and kept until five minutes before it
// expires; one Teams refuses is replaced once.
func TestTeamsToken(t *testing.T) {
	t.Parallel()
	fakes := teamstest.Start(t)
	oid := uuid.NewString()
	fakes.Connector.Install(oid)
	tc, clk := teamsTo(t, fakes, config.Teams{Bot: fakes.Bot("env:SECRET")}, map[string]string{"SECRET": fakes.Login.Secret})
	to := Recipient{ID: "user:alice", Name: "alice", Address: oid}
	for range 3 {
		if _, err := sendTeams(t, tc, to, approvalMessage()); err != nil {
			t.Fatal(err)
		}
	}
	if n := fakes.Login.Requests(); n != 1 {
		t.Errorf("%d token requests for three cards, want 1", n)
	}
	clk.add(fakes.Login.Lifetime - tokenMargin - time.Second)
	if _, err := sendTeams(t, tc, to, approvalMessage()); err != nil || fakes.Login.Requests() != 1 {
		t.Errorf("just before the margin: %v, %d token requests", err, fakes.Login.Requests())
	}
	clk.add(2 * time.Second)
	if _, err := sendTeams(t, tc, to, approvalMessage()); err != nil || fakes.Login.Requests() != 2 {
		t.Errorf("within the margin: %v, %d token requests", err, fakes.Login.Requests())
	}
	// Revoked before it expires: a new one, once.
	fakes.Login.Expire()
	if _, err := sendTeams(t, tc, to, approvalMessage()); err != nil || fakes.Login.Requests() != 3 {
		t.Errorf("after a revocation: %v, %d token requests", err, fakes.Login.Requests())
	}
}

// TestTeamsErrors: what Teams refuses is an error that says why, without the client secret, the
// token or the webhook's signature.
func TestTeamsErrors(t *testing.T) {
	t.Parallel()
	fakes := teamstest.Start(t)
	installed, blocked, stranger := uuid.NewString(), uuid.NewString(), uuid.NewString()
	fakes.Connector.Install(installed, blocked)
	fakes.Connector.Block(blocked)
	tc, _ := teamsTo(t, fakes, config.Teams{Bot: fakes.Bot("env:SECRET")}, map[string]string{"SECRET": fakes.Login.Secret})
	permanent := func(err error) bool {
		var se *StatusError
		return errors.As(err, &se) && se.Permanent()
	}
	_, err := sendTeams(t, tc, Recipient{ID: "user:stranger", Address: stranger}, approvalMessage())
	if err == nil || !strings.Contains(err.Error(), "ForbiddenOperationException: the app is not installed") || !permanent(err) {
		t.Errorf("not installed: %v", err)
	}
	_, err = sendTeams(t, tc, Recipient{ID: "user:blocked", Address: blocked}, approvalMessage())
	if err == nil || !strings.Contains(err.Error(), "MessageWritesBlocked") || !permanent(err) {
		t.Errorf("blocked: %v", err)
	}

	// A wrong client secret: Entra ID's code, never the secret.
	wrong := "wrong-" + fakes.Login.Secret
	bad, _ := teamsTo(t, fakes, config.Teams{Bot: fakes.Bot("env:SECRET")}, map[string]string{"SECRET": wrong})
	_, err = sendTeams(t, bad, Recipient{ID: "user:alice", Address: installed}, approvalMessage())
	if err == nil || !strings.Contains(err.Error(), "HTTP 401 invalid_client") || strings.Contains(err.Error(), wrong) {
		t.Errorf("wrong secret: %v", err)
	}
	missing, _ := teamsTo(t, fakes, config.Teams{Bot: fakes.Bot("env:NONE")}, nil)
	if _, err = sendTeams(t, missing, Recipient{ID: "user:alice", Address: installed}, approvalMessage()); err == nil || !strings.Contains(err.Error(), "client secret") {
		t.Errorf("no secret: %v", err)
	}
}

// TestTeamsThrottled: a 429 is tried again after Retry-After, or a backoff without it, within a
// send; beyond, or when Retry-After is too long, the send fails for the job's backoff.
func TestTeamsThrottled(t *testing.T) {
	t.Parallel()
	fakes := teamstest.Start(t)
	oid := uuid.NewString()
	fakes.Connector.Install(oid)
	tc, clk := teamsTo(t, fakes, config.Teams{Bot: fakes.Bot("env:SECRET")}, map[string]string{"SECRET": fakes.Login.Secret})
	to := Recipient{ID: "user:alice", Address: oid}
	fakes.Connector.Throttle(1, "3")
	if _, err := sendTeams(t, tc, to, approvalMessage()); err != nil || len(clk.waits) != 1 || clk.waits[0] != 3*time.Second {
		t.Errorf("throttled once: %v, waits %v", err, clk.waits)
	}
	fakes.Connector.Throttle(2, "")
	if _, err := sendTeams(t, tc, to, approvalMessage()); err != nil || len(clk.waits) != 3 || clk.waits[1] < time.Second || clk.waits[2] < 2*time.Second {
		t.Errorf("throttled twice: %v, waits %v", err, clk.waits)
	}
	fakes.Connector.Throttle(maxRetries+1, "")
	var se *StatusError
	if _, err := sendTeams(t, tc, to, approvalMessage()); !errors.As(err, &se) || se.Status != http.StatusTooManyRequests || se.Permanent() {
		t.Errorf("throttled too often: %v", err)
	}
	fakes.Connector.Throttle(1, "3600")
	waits := len(clk.waits)
	if _, err := sendTeams(t, tc, to, approvalMessage()); !errors.As(err, &se) || se.Status != http.StatusTooManyRequests || len(clk.waits) != waits {
		t.Errorf("an hour's Retry-After: %v, waits %v", err, clk.waits)
	}
}

// TestTeamsChannelPost: a channel's post reaches its workflow without an Authorization header,
// and says identifiers only. A webhook's URL is in no error: not its signature, not the URL
// *url.Error prints.
func TestTeamsChannelPost(t *testing.T) {
	t.Parallel()
	fakes := teamstest.Start(t)
	hook := fakes.Workflows.Hook("infra")
	u, _ := url.Parse(hook)
	sig := u.Query().Get("sig")
	cfg := config.Teams{Channels: []config.TeamsChannel{
		{Name: "infra", Webhook: config.SecretRef{Ref: "env:HOOK"}, Repositories: []string{"infra-live"}, Events: []string{"approval_needed", "alert"}},
		{Name: "shop", Webhook: config.SecretRef{Ref: "env:SHOP"}, Teams: []string{"shop"}, Events: []string{"submitted", "approved"}},
	}}
	gone := httptest.NewTLSServer(http.NotFoundHandler())
	gone.Close()
	wrong := "0" + sig[1:]
	if wrong == sig {
		wrong = "1" + sig[1:]
	}
	// Each broken webhook has its reference: the secret store keeps what it resolved for a while.
	broken := map[string]string{
		"badsig": strings.Replace(hook, sig, wrong, 1),                                       // a wrong signature
		"gone":   strings.Replace(hook, u.Host, strings.TrimPrefix(gone.URL, "https://"), 1), // unreachable
		"http":   strings.Replace(hook, "https://", "http://", 1),
		"creds":  strings.Replace(hook, "https://", "https://user:pw@", 1),
		"parse":  "%zz" + hook,
	}
	vars := map[string]string{"HOOK": hook}
	for name, h := range broken {
		vars[strings.ToUpper(name)] = h
		cfg.Channels = append(cfg.Channels, config.TeamsChannel{Name: name, Webhook: config.SecretRef{Ref: "env:" + strings.ToUpper(name)},
			Repositories: []string{"other"}, Events: []string{"alert"}})
	}
	tc, clk := teamsTo(t, fakes, cfg, vars)
	if tc.People() {
		t.Error("channels alone reach people")
	}
	if posts := tc.Posts([]string{Submitted, ApprovalNeeded}, "infra-live", []string{"team:shop"}); len(posts) != 2 ||
		posts[0] != (Post{"infra", ApprovalNeeded}) || posts[1] != (Post{"shop", Submitted}) {
		t.Errorf("Posts = %+v", posts)
	}
	if posts := tc.Posts([]string{Approved}, "infra-live", nil); len(posts) != 0 {
		t.Errorf("Posts of an event no channel takes = %+v", posts)
	}

	m := approvalMessage()
	m.Headline = "Request #7 waits for approval."
	m.Request = &RequestContent{Number: 7, Title: "Create infra-live/vm/prod/big01", Repository: "infra-live", State: "pending_approval"}
	if _, err := sendTeams(t, tc, Recipient{ID: ChannelRecipient + "infra"}, m); err != nil {
		t.Fatal(err)
	}
	posts := fakes.Workflows.Posts("infra")
	if len(posts) != 1 {
		t.Fatalf("posts %+v", posts)
	}
	text := posts[0].Card.Text()
	if !strings.Contains(text, "Request #7 waits for approval.") || !strings.Contains(text, "pending approval") {
		t.Errorf("the post:\n%s", text)
	}
	// Identifiers only: not even the title, which names the resource (open question 64).
	if strings.Contains(text, "big01") {
		t.Errorf("the post shows the request's title:\n%s", text)
	}
	if acts := posts[0].Card.Actions(); len(acts) != 1 || acts[0].Title != "Open in Quai" || acts[0].URL != m.Link {
		t.Errorf("the post's actions: %+v", acts)
	}
	for _, a := range fakes.Workflows.Authorizations() {
		if a != "" {
			t.Errorf("an Authorization header to a workflow: %q", a)
		}
	}

	// Throttled: tried again after Retry-After.
	fakes.Workflows.Throttle(1, "2")
	if _, err := sendTeams(t, tc, Recipient{ID: ChannelRecipient + "infra"}, m); err != nil || len(clk.waits) != 1 || clk.waits[0] != 2*time.Second {
		t.Errorf("throttled: %v, waits %v", err, clk.waits)
	}

	secret := func(err error) {
		t.Helper()
		if err == nil {
			t.Fatal("no error")
		}
		for _, s := range []string{sig, url.QueryEscape(sig), u.Host, u.RawQuery, "/workflows/"} {
			if strings.Contains(err.Error(), s) {
				t.Errorf("the error shows %q: %v", s, err)
			}
		}
	}
	// A wrong signature: refused, the status only.
	_, err := sendTeams(t, tc, Recipient{ID: ChannelRecipient + "badsig"}, m)
	secret(err)
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("a wrong signature: %v", err)
	}
	// Unreachable: *url.Error prints the URL; not here.
	_, err = sendTeams(t, tc, Recipient{ID: ChannelRecipient + "gone"}, m)
	secret(err)
	// Not https, or with credentials: refused before any request, naming the reference.
	for _, name := range []string{"http", "creds", "parse"} {
		_, err = sendTeams(t, tc, Recipient{ID: ChannelRecipient + name}, m)
		secret(err)
		if !strings.Contains(err.Error(), "env:"+strings.ToUpper(name)) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A channel no longer configured.
	if _, err := sendTeams(t, tc, Recipient{ID: ChannelRecipient + "gone"}, m); err == nil {
		t.Error("a post to a channel not configured")
	}
}

// TestRedactHook: what a webhook's error may hold of its URL is cut out.
func TestRedactHook(t *testing.T) {
	t.Parallel()
	hook := "https://prod.example.logic.azure.com/workflows/abc/triggers/manual/paths/invoke?api-version=1&sp=%2Ftriggers%2Fmanual%2Frun&sv=1.0&sig=SECRETSIG123"
	u, _ := url.Parse(hook)
	for _, err := range []error{
		&url.Error{Op: "Post", URL: hook, Err: errors.New("connection refused")},
		errors.New("something about " + hook + " and SECRETSIG123 and " + u.RawQuery),
	} {
		got := redactHook(err, hook, u).Error()
		if strings.Contains(got, "SECRETSIG123") || strings.Contains(got, "logic.azure.com/workflows") {
			t.Errorf("redacted = %q", got)
		}
	}
	timeout := &url.Error{Op: "Post", URL: hook, Err: context.DeadlineExceeded}
	if got := redactHook(timeout, hook, u).Error(); strings.Contains(got, "SECRETSIG123") {
		t.Errorf("timeout = %q", got)
	}
}

// TestCardSize: a card with many files and resources stays under what a webhook takes, leaving
// them out when it must; the counts stay.
func TestCardSize(t *testing.T) {
	t.Parallel()
	m := approvalMessage()
	for i := range 400 {
		m.Request.Changes = append(m.Request.Changes, FileChange{Path: strings.Repeat("d/", 50) + uuid.NewString() + ".yaml", Op: "add", Added: i})
	}
	b, err := json.Marshal(activity(personalCard(m)))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > teamstest.MaxSize {
		t.Errorf("a card of %d bytes", len(b))
	}
	if !strings.Contains(string(b), "Changes (401 files)") {
		t.Error("the count is gone")
	}
}
