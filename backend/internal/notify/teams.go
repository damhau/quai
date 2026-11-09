package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Teams sends notifications to Microsoft Teams (notifications.teams,
// docs/design/11-integrations.md#notifications): personal cards through the bot, to a person's
// Entra ID object ID, and channel posts through Workflows webhooks.
//
// Every endpoint comes from the configuration, https, never from what Teams answers (a
// conversation's serviceUrl included), and is reached through network with the server's
// certificate verified. The bot's client secret is resolved at each token request; its tokens
// stay in memory, are cached until five minutes before they expire, and go to the Bot Connector
// only. A webhook's URL is a credential (its sig): it is resolved at each post, never logged, and
// taken out of every error. Cards are built from Go values with encoding/json; what people wrote
// (a title, a reason, a name) goes in TextRuns, which Teams does not render as Markdown.
type Teams struct {
	cfg     config.Teams
	secrets *secrets.Store
	client  *http.Client // the Bot Connector's and the token endpoint's
	hooks   *http.Client // the webhooks': no redirects
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error

	mu     sync.Mutex
	token  string
	expiry time.Time
}

// tokenMargin is how long before a bot token expires Quai gets another.
const tokenMargin = 5 * time.Minute

// maxWait is the longest Quai waits for Teams to take a request again (429, 412, 502, 504) within
// a send; longer, the send fails and is tried again with the notifications' backoff.
const maxWait = 10 * time.Second

// maxRetries is how often a send retries a throttled request within one attempt.
const maxRetries = 2

// NewTeams returns the Teams channel. Outgoing connections trust what network trusts.
func NewTeams(cfg config.Teams, network *netx.Network, sec *secrets.Store) (*Teams, error) {
	if network == nil {
		var err error
		if network, err = netx.New(""); err != nil {
			return nil, err
		}
	}
	t := &Teams{cfg: cfg, secrets: sec, now: time.Now, sleep: sleep}
	t.client = &http.Client{Transport: network.Transport(), Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.hooks = &http.Client{Transport: network.Transport(), Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return t, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// UseClock sets the clock tokens expire by, for tests.
func (t *Teams) UseClock(now func() time.Time) { t.now = now }

// Name names the channel.
func (t *Teams) Name() string { return "teams" }

// People reports whether the channel reaches people: with a bot.
func (t *Teams) People() bool { return t.cfg.Bot != nil }

// Address is a person's Entra ID object ID (oid), from their last sign-in, with the bot.
func (t *Teams) Address(u db.User) (string, bool) {
	if t.cfg.Bot == nil {
		return "", false
	}
	if _, err := uuid.Parse(u.ObjectID); err != nil {
		return "", false
	}
	return u.ObjectID, true
}

// NoAddress says why someone has no address: they never signed in with Entra ID.
func (t *Teams) NoAddress() string {
	return "Quai knows no Entra ID object ID (oid) of theirs: they have not signed in with Entra ID"
}

// Posts returns the Teams channels that post an event of a kind for a repository, or for a
// request whose owners hold team:<name>.
func (t *Teams) Posts(kinds []string, repository string, owners []string) []Post {
	var out []Post
	for _, c := range t.cfg.Channels {
		mine := slices.Contains(c.Repositories, repository) || slices.ContainsFunc(c.Teams, func(team string) bool {
			return slices.Contains(owners, "team:"+team)
		})
		if !mine {
			continue
		}
		for _, k := range kinds {
			if slices.Contains(c.Events, k) {
				out = append(out, Post{Channel: c.Name, Kind: k})
				break
			}
		}
	}
	return out
}

// Post is an event a Teams channel gets, as a kind of notification.
type Post struct{ Channel, Kind string }

// Send sends a personal card, or posts to a channel (recipients teams-channel:<name>).
func (t *Teams) Send(ctx context.Context, to Recipient, m Message) (Receipt, error) {
	if name, ok := strings.CutPrefix(to.ID, ChannelRecipient); ok {
		return Receipt{}, t.post(ctx, name, m)
	}
	if t.cfg.Bot == nil {
		return Receipt{}, errors.New("the Teams bot is not configured")
	}
	if _, err := uuid.Parse(to.Address); err != nil {
		return Receipt{}, errors.New("not an Entra ID object ID")
	}
	b := t.cfg.Bot
	var conv struct {
		ID string `json:"id"`
	}
	params := map[string]any{"bot": map[string]any{"id": "28:" + b.AppID}, "isGroup": false, "members": []map[string]any{{"id": to.Address}},
		"tenantId": b.TenantID, "channelData": map[string]any{"tenant": map[string]any{"id": b.TenantID}}}
	if err := t.connector(ctx, http.MethodPost, "v3/conversations", params, &conv); err != nil {
		return Receipt{}, fmt.Errorf("start a conversation in Teams: %w", err)
	}
	if conv.ID == "" {
		return Receipt{}, errors.New("start a conversation in Teams: no conversation ID in the answer")
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := t.connector(ctx, http.MethodPost, "v3/conversations/"+url.PathEscape(conv.ID)+"/activities", activity(personalCard(m)), &res); err != nil {
		return Receipt{}, fmt.Errorf("send a card in Teams: %w", err)
	}
	return Receipt{Conversation: conv.ID, Message: res.ID}, nil
}

// Update replaces a personal card Quai sent.
func (t *Teams) Update(ctx context.Context, at Receipt, m Message) error {
	if t.cfg.Bot == nil {
		return errors.New("the Teams bot is not configured")
	}
	path := "v3/conversations/" + url.PathEscape(at.Conversation) + "/activities/" + url.PathEscape(at.Message)
	if err := t.connector(ctx, http.MethodPut, path, activity(personalCard(m)), nil); err != nil {
		return fmt.Errorf("update a card in Teams: %w", err)
	}
	return nil
}

// ChannelRecipient starts the recipients that are Teams channels.
const ChannelRecipient = "teams-channel:"

// StatusError is an answer of Teams that is not a success.
type StatusError struct {
	Status int
	Code   string // the ErrorResponse's code, or Entra ID's error
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("HTTP %d", e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	return msg
}

// Permanent reports whether trying again cannot help: the request was refused (400, 403, 404).
func (e *StatusError) Permanent() bool {
	return e.Status == http.StatusBadRequest || e.Status == http.StatusForbidden || e.Status == http.StatusNotFound
}

// connector calls the Bot Connector with the bot's token: once more with a new token when it is
// refused (401), and again after a wait when Teams throttles (429) or a gateway fails (412, 502,
// 504), within maxWait.
func (t *Teams) connector(ctx context.Context, method, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	u := strings.TrimSuffix(t.cfg.Bot.ServiceURL, "/") + "/" + path
	renewed := false
	for attempt := 0; ; attempt++ {
		token, err := t.botToken(ctx)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(b))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := t.client.Do(req)
		if err != nil {
			return redactToken(err, token)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if out != nil && len(bytes.TrimSpace(data)) > 0 {
				if err := json.Unmarshal(data, out); err != nil {
					return fmt.Errorf("an answer that is not JSON: %w", err)
				}
			}
			return nil
		case resp.StatusCode == http.StatusUnauthorized && !renewed:
			// Revoked or expired early: a new token, once.
			t.forgetToken(token)
			renewed = true
			continue
		}
		e := &StatusError{Status: resp.StatusCode, Code: connectorCode(data)}
		if wait, ok := t.retry(ctx, resp, attempt); ok {
			if err := t.sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}
		return e
	}
}

// retry says whether to try a throttled request again, and after how long: what Retry-After
// says, or an exponential backoff with jitter, when it is within maxWait and the deadline.
func (t *Teams) retry(ctx context.Context, resp *http.Response, attempt int) (time.Duration, bool) {
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusPreconditionFailed, http.StatusBadGateway, http.StatusGatewayTimeout:
	default:
		return 0, false
	}
	if attempt >= maxRetries {
		return 0, false
	}
	wait := time.Second<<attempt + rand.N(500*time.Millisecond) // #nosec G404 -- jitter, not a secret
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			wait = time.Duration(n) * time.Second
		} else if at, err := http.ParseTime(s); err == nil {
			wait = at.Sub(t.now())
		}
	}
	if wait > maxWait {
		return 0, false
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait+5*time.Second {
		return 0, false
	}
	return max(wait, 0), true
}

// connectorCode is the code of an ErrorResponse ({"error": {"code"}}), or the subCode of a blocked
// conversation, cut to a length an error may hold.
func connectorCode(body []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		ErrorCode int    `json:"errorCode"`
		Message   string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	code := e.Error.Code
	if code == "" && e.ErrorCode != 0 {
		code = fmt.Sprintf("errorCode %d", e.ErrorCode)
		if strings.Contains(e.Message, "MessageWritesBlocked") {
			code += " MessageWritesBlocked: the person blocked the bot"
		}
	}
	if code == "ForbiddenOperationException" {
		code += ": the app is not installed for the person"
	}
	return oneLine(code, 120)
}

// botToken returns the bot's token: the cached one until tokenMargin before it expires, a new one
// otherwise, from the token endpoint with the client secret.
func (t *Teams) botToken(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.now().Before(t.expiry.Add(-tokenMargin)) {
		return t.token, nil
	}
	b := t.cfg.Bot
	s, err := t.secrets.Resolve(ctx, b.ClientSecret.Ref)
	if err != nil {
		return "", fmt.Errorf("the Teams bot's client secret: %w", err) // names the reference, never the value
	}
	secret := s.Reveal()
	cc := clientcredentials.Config{ClientID: b.AppID, ClientSecret: secret, TokenURL: b.TokenURL, Scopes: []string{b.Scope},
		AuthStyle: oauth2.AuthStyleInParams}
	fetched, started := t.now(), time.Now()
	tok, err := cc.Token(context.WithValue(ctx, oauth2.HTTPClient, t.client))
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			// Entra ID's error and its code; not its description, which can be long.
			return "", fmt.Errorf("the Teams bot's token: HTTP %d %s", re.Response.StatusCode, oneLine(re.ErrorCode, 60))
		}
		return "", redact(fmt.Errorf("the Teams bot's token: %w", err), secret)
	}
	if tok.AccessToken == "" || tok.Expiry.IsZero() {
		return "", errors.New("the Teams bot's token: the answer has no token or expiry")
	}
	// oauth2 sets Expiry from expires_in by the system's clock; the cache keeps it by Quai's.
	t.token, t.expiry = tok.AccessToken, fetched.Add(tok.Expiry.Sub(started))
	return t.token, nil
}

// forgetToken drops the cached token when it is the one refused.
func (t *Teams) forgetToken(token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token == token {
		t.token, t.expiry = "", time.Time{}
	}
}

// redactToken removes a bearer token from an error's text.
func redactToken(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "[redacted]"))
}

// post posts a card to a Teams channel's workflow. Nothing of the webhook's URL is in an error.
func (t *Teams) post(ctx context.Context, name string, m Message) error {
	i := slices.IndexFunc(t.cfg.Channels, func(c config.TeamsChannel) bool { return c.Name == name })
	if i < 0 {
		return fmt.Errorf("no Teams channel %s in the configuration", name)
	}
	ch := t.cfg.Channels[i]
	s, err := t.secrets.Resolve(ctx, ch.Webhook.Ref)
	if err != nil {
		return fmt.Errorf("the webhook of Teams channel %s: %w", name, err) // names the reference, never the value
	}
	hook := strings.TrimSpace(s.Reveal())
	u, err := url.Parse(hook)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		// Not url.Parse's error: it holds the URL.
		return fmt.Errorf("the webhook of Teams channel %s (%s) is not an https URL without credentials", name, ch.Webhook.Ref)
	}
	b, err := json.Marshal(activity(channelCard(m)))
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("post to Teams channel %s: %w", name, redactHook(err, hook, u))
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := t.hooks.Do(req)
		if err != nil {
			return fmt.Errorf("post to Teams channel %s: %w", name, redactHook(err, hook, u))
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if wait, ok := t.retry(ctx, resp, attempt); ok {
			if err := t.sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}
		// The body is not in the error: a workflow's answer may echo the request.
		return fmt.Errorf("post to Teams channel %s: %w", name, &StatusError{Status: resp.StatusCode})
	}
}

// redactHook returns an error whose text holds nothing of a webhook's URL: *url.Error prints the
// whole URL, its signature included, so its operation and URL are dropped; the URL, its query and
// each of its query's values are then cut out of what is left.
func redactHook(err error, hook string, u *url.URL) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
		if ue.Timeout() {
			return errors.New("timed out")
		}
	}
	msg := err.Error()
	secrets := []string{hook, u.RawQuery, u.String()}
	for _, vs := range u.Query() {
		for _, v := range vs {
			if len(v) >= 4 {
				secrets = append(secrets, v, url.QueryEscape(v))
			}
		}
	}
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
		}
	}
	return errors.New(msg)
}

// The cards: Adaptive Cards 1.2, built as Go values. Text that comes from people or from
// configuration is in TextRuns of RichTextBlocks, which do not render Markdown; there is no
// TextBlock nor FactSet, whose texts Teams renders as Markdown.

type card struct {
	Schema  string     `json:"$schema"`
	Type    string     `json:"type"`
	Version string     `json:"version"`
	Body    []element  `json:"body"`
	Actions []action   `json:"actions,omitempty"`
	MSTeams *cardTeams `json:"msteams,omitempty"`
}

type cardTeams struct {
	Width string `json:"width"`
}

type element struct {
	Type      string    `json:"type"` // RichTextBlock or Container
	Inlines   []run     `json:"inlines,omitempty"`
	Items     []element `json:"items,omitempty"`
	Separator bool      `json:"separator,omitempty"`
	Spacing   string    `json:"spacing,omitempty"`
	Wrap      bool      `json:"wrap,omitempty"`
}

type run struct {
	Type     string `json:"type"` // TextRun
	Text     string `json:"text"`
	Weight   string `json:"weight,omitempty"`
	Size     string `json:"size,omitempty"`
	IsSubtle bool   `json:"isSubtle,omitempty"`
	FontType string `json:"fontType,omitempty"`
}

type action struct {
	Type  string `json:"type"` // Action.OpenUrl
	Title string `json:"title"`
	URL   string `json:"url"`
	Style string `json:"style,omitempty"`
}

type attachment struct {
	ContentType string  `json:"contentType"`
	ContentURL  *string `json:"contentUrl"` // always null
	Content     card    `json:"content"`
}

type cardMessage struct {
	Type        string       `json:"type"`
	Attachments []attachment `json:"attachments"`
}

// activity is a message with one card.
func activity(c card) cardMessage {
	return cardMessage{Type: "message", Attachments: []attachment{{ContentType: "application/vnd.microsoft.card.adaptive", Content: c}}}
}

func newCard(body []element, actions []action) card {
	return card{Schema: "http://adaptivecards.io/schemas/adaptive-card.json", Type: "AdaptiveCard", Version: "1.2", Body: body,
		Actions: actions, MSTeams: &cardTeams{Width: "Full"}}
}

// text is a line of plain text.
func text(s string, opts ...func(*run)) element {
	r := run{Type: "TextRun", Text: s}
	for _, o := range opts {
		o(&r)
	}
	return element{Type: "RichTextBlock", Inlines: []run{r}}
}

func bold(r *run)      { r.Weight = "Bolder" }
func large(r *run)     { r.Size = "Medium"; r.Weight = "Bolder" }
func subtle(r *run)    { r.IsSubtle = true }
func monospace(r *run) { r.FontType = "Monospace" }

// field is a labelled value on a line.
func field(label, value string) element {
	return element{Type: "RichTextBlock", Inlines: []run{{Type: "TextRun", Text: label + ": ", Weight: "Bolder"}, {Type: "TextRun", Text: value}}}
}

// paragraph is text of several lines, a block per line, at most maxLines.
func paragraph(label, s string) []element {
	const maxLines = 20
	lines := strings.Split(strings.ReplaceAll(clip(s, 2000), "\r\n", "\n"), "\n")
	if len(lines) > maxLines {
		lines = append(lines[:maxLines], "...")
	}
	out := []element{field(label, oneLine(lines[0], 500))}
	for _, l := range lines[1:] {
		out = append(out, text(oneLine(l, 500)))
	}
	return out
}

// cardItems bounds the files and resources a card lists; the request page has them all.
const cardItems = 20

// maxCard is the size above which a card leaves its files and plans' resources out: under the
// 28 KB a webhook takes, with room for the envelope.
const maxCard = 24 * 1024

// personalCard is a person's card: what the email says, in short, with Approve and Deny for an
// approval, and a link to the request.
func personalCard(m Message) card {
	if m.Kind == DigestKind {
		return digestCard(m)
	}
	c := fullCard(m, true)
	if b, err := json.Marshal(c); err == nil && len(b) > maxCard {
		c = fullCard(m, false)
	}
	return c
}

func fullCard(m Message, details bool) card {
	body := []element{text(oneLine(m.Headline, 400), large)}
	if m.Status != "" {
		body = append(body, text(oneLine(m.Status, 400), bold))
	}
	if rc := m.Request; rc != nil {
		body = append(body, field("Request", fmt.Sprintf("#%d %s", rc.Number, oneLine(rc.Title, 200))), field("Repository", oneLine(rc.Repository, 200)))
		if !m.Closed {
			body = append(body, field("Requested by", oneLine(rc.Requester, 200)))
			if rc.Reason != "" {
				body = append(body, paragraph("Reason", rc.Reason)...)
			}
			if len(rc.Waiting) > 0 {
				body = append(body, field("Waiting for", oneLine(strings.Join(rc.Waiting, "; "), 500)))
			}
			body = append(body, requestDetails(rc, details)...)
		}
	}
	if a := m.Alert; a != nil {
		body = append(body, field("Repository", oneLine(a.Repository, 200)))
		for _, d := range a.Details {
			body = append(body, field(d.Label, oneLine(d.Value, 300)))
		}
	}
	var actions []action
	if m.Approve != "" && m.Deny != "" && !m.Closed {
		actions = append(actions, action{Type: "Action.OpenUrl", Title: "Approve", URL: m.Approve, Style: "positive"},
			action{Type: "Action.OpenUrl", Title: "Deny", URL: m.Deny, Style: "destructive"})
	}
	actions = append(actions, action{Type: "Action.OpenUrl", Title: "Open in Quai", URL: m.Link})
	if m.Preferences != "" && !m.Closed {
		actions = append(actions, action{Type: "Action.OpenUrl", Title: "Notification preferences", URL: m.Preferences})
	}
	if m.Approve != "" && !m.Closed {
		body = append(body, text("Approve and Deny open the request in Quai with the decision ready: you confirm it there, signed in.", subtle))
	}
	return newCard(body, actions)
}

// requestDetails lists a request's files and plans: the counts always, the files and resources
// when details is true.
func requestDetails(rc *RequestContent, details bool) []element {
	var out []element
	if len(rc.Changes) > 0 {
		out = append(out, element{Type: "Container", Separator: true, Spacing: "Medium", Items: []element{text(fmt.Sprintf("Changes (%d files)", len(rc.Changes)), bold)}})
		if details {
			for i, c := range rc.Changes {
				if i == cardItems {
					out = append(out, text(fmt.Sprintf("and %d more", len(rc.Changes)-cardItems), subtle))
					break
				}
				out = append(out, text(fmt.Sprintf("%s %s (+%d -%d)", c.Op, oneLine(c.Path, 200), c.Added, c.Removed), monospace))
			}
		}
	}
	for _, p := range rc.Plans {
		line := fmt.Sprintf("%d to add, %d to change, %d to destroy", p.Add, p.Change, p.Destroy)
		if p.Cost != "" {
			line += "; " + p.Cost
		}
		out = append(out, element{Type: "Container", Separator: true, Spacing: "Medium", Items: []element{
			text(fmt.Sprintf("Plan of %s (%s)", oneLine(p.Stack, 100), oneLine(p.Status, 40)), bold), text(line)}})
		for _, pol := range p.Policies {
			out = append(out, field("Policy "+oneLine(pol.Name, 100), oneLine(pol.Result, 40)))
		}
		if details {
			for i, r := range p.Resources {
				if i == cardItems {
					out = append(out, text(fmt.Sprintf("and %d more", len(p.Resources)-cardItems+p.More), subtle))
					break
				}
				out = append(out, text(oneLine(r.Action+" "+r.Address, 300), monospace))
			}
			if len(p.Resources) <= cardItems && p.More > 0 {
				out = append(out, text(fmt.Sprintf("and %d more", p.More), subtle))
			}
		}
	}
	return out
}

func digestCard(m Message) card {
	body := []element{text(oneLine(m.Subject, 200), large), text(oneLine(m.Headline, 400))}
	for i, it := range m.Digest {
		if i == 30 {
			body = append(body, text(fmt.Sprintf("and %d more: the requests page has them", len(m.Digest)-30), subtle))
			break
		}
		items := []element{text(oneLine(it.Label, 60)+": "+oneLine(it.Headline, 300), bold)}
		if it.Request != "" {
			items = append(items, text(oneLine(it.Request, 300)))
		}
		body = append(body, element{Type: "Container", Separator: true, Items: items})
	}
	actions := []action{{Type: "Action.OpenUrl", Title: "Open in Quai", URL: m.Link}}
	if m.Preferences != "" {
		actions = append(actions, action{Type: "Action.OpenUrl", Title: "Notification preferences", URL: m.Preferences})
	}
	return newCard(body, actions)
}

// channelCard is a channel's post: identifiers, the state and a link; never what people wrote, a
// diff or a plan (open question 64).
func channelCard(m Message) card {
	body := []element{text(oneLine(m.Headline, 400), large)}
	if rc := m.Request; rc != nil {
		body = append(body, field("Request", fmt.Sprintf("#%d", rc.Number)), field("Repository", oneLine(rc.Repository, 200)))
		if rc.State != "" {
			body = append(body, field("State", strings.ReplaceAll(rc.State, "_", " ")))
		}
	}
	if a := m.Alert; a != nil {
		body = append(body, field("Repository", oneLine(a.Repository, 200)))
		for _, d := range a.Details {
			body = append(body, field(d.Label, oneLine(d.Value, 300)))
		}
	}
	return newCard(body, []action{{Type: "Action.OpenUrl", Title: "Open in Quai", URL: m.Link}})
}
