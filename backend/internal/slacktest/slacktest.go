// Package slacktest fakes Slack's Web API, the methods Quai's Slack notifications call, and signs
// the interaction requests Slack sends, for tests (docs/plan/phase-4-backlog.md#services-with-no-account):
// there is no Slack workspace to test with. The fake is an HTTPS server with a certificate of its
// own (CAFile trusts it) for one workspace and one app, that implements only what Quai calls,
// refuses what the documentation says is refused, and keeps what it received for the test to
// read:
//
//   - POST /api/{method}, the arguments as a form (application/x-www-form-urlencoded) or JSON
//     (application/json), the bot token in Authorization: Bearer and nowhere else; the answer is
//     JSON with ok, and error when ok is false, with HTTP 200.
//   - auth.test: the workspace (team_id, and enterprise_id in an Enterprise organization).
//   - users.lookupByEmail: a form with email; {"ok": true, "user": <a user object>}, or
//     users_not_found, also for a deactivated account.
//   - conversations.open: users, one user ID; {"ok": true, "channel": {"id": "D..."}}, the same
//     for the same person; user_not_found, user_disabled.
//   - chat.postMessage: channel (a channel ID, a DM's ID, or a user ID, which opens a DM), text,
//     blocks; {"ok": true, "channel", "ts", "message"}; channel_not_found, not_in_channel,
//     is_archived, invalid_blocks, no_text.
//   - chat.update: channel (a DM's ID, never a user ID) and ts of a message the app posted, text,
//     blocks; message_not_found, channel_not_found, msg_too_long (text over 4,000 characters).
//
// It follows Slack's documentation, read on 2026-11-09:
//   - Web API basics (https://docs.slack.dev/apis/web-api/): arguments as a GET query, a POST
//     form or, for most write methods, a JSON body with Content-type application/json; the
//     token preferably in the Authorization header, never in a query string or a JSON body; ok,
//     error and warning in every answer.
//   - Rate limits (https://docs.slack.dev/apis/web-api/rate-limits): HTTP 429 Too Many Requests
//     with a Retry-After header in seconds.
//   - The methods' pages (https://docs.slack.dev/reference/methods/auth.test,
//     .../users.lookupByEmail, .../conversations.open, .../chat.postMessage, .../chat.update):
//     their arguments, answers, scopes and errors.
//   - The user object (https://docs.slack.dev/reference/objects/user-object): deleted, is_bot,
//     is_app_user, is_restricted (a guest), is_ultra_restricted (a single-channel guest),
//     is_stranger (another workspace), team_id, enterprise_user, profile.email,
//     is_email_confirmed.
//   - Block Kit (https://docs.slack.dev/reference/block-kit/blocks and the pages of the header,
//     section, context and actions blocks, the button element and the text object): 50 blocks a
//     message; a header's plain_text of 150 characters; a section's text of 3,000, or up to 10
//     fields of 2,000; up to 10 elements in a context, 25 in an actions block; a button's
//     plain_text of 75, its action_id of 255, url of 3,000, style primary or danger; a text
//     object's type plain_text or mrkdwn, its text 1 to 3,000 characters, emoji with plain_text
//     only, verbatim with mrkdwn only.
//   - Formatting message text (https://docs.slack.dev/messaging/formatting-message-text): &, <
//     and > escaped as &amp;, &lt; and &gt;.
//   - Verifying requests from Slack (https://docs.slack.dev/authentication/verifying-requests-from-slack):
//     X-Slack-Request-Timestamp, X-Slack-Signature v0= and the hex HMAC-SHA256 of
//     "v0:<timestamp>:<body>" with the signing secret; the example Sign is tested with.
//   - Handling user interaction (https://docs.slack.dev/interactivity/handling-user-interaction)
//     and the block_actions payload (https://docs.slack.dev/reference/interaction-payloads/block_actions-payload):
//     a form with payload, the JSON of the interaction; a button with a url sends one too, to be
//     acknowledged with HTTP 200 within three seconds.
//
// What the pages do not say, the fake assumes, and it is not verified against Slack: the body of
// a 429 ({"ok": false, "error": "ratelimited"}); that users.lookupByEmail matches the email
// without regard to case and also takes a POST form, as every method does; the answers' fields
// beyond those the pages' examples show; that chat.postMessage's text over 40,000 characters is
// msg_too_long rather than truncated.
package slacktest

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/damhau/quai/backend/internal/config"
)

// Slack is a fake of Slack's Web API for one workspace and one app.
type Slack struct {
	*httptest.Server
	// Team is the workspace's ID; Enterprise its organization's, empty by default.
	Team, Enterprise string
	// Token is the app's bot token, SigningSecret its signing secret.
	Token, SigningSecret string
	// CAFile is a PEM file of the fake's certificate, for netx.New.
	CAFile string

	mu         sync.Mutex
	users      []*User
	channels   map[string]*Channel
	dms        map[string]string // DM -> user ID
	messages   []*Message
	requests   []Request
	throttle   int
	retryAfter string
	failures   map[string][]string // method -> errors to answer next
	ts         int64
}

// Start starts the fake for the test, with a workspace, a bot token and a signing secret of its
// own.
func Start(t testing.TB) *Slack {
	t.Helper()
	s := &Slack{Team: "T" + strings.ToUpper(random(5)), Token: "xoxb-" + random(6) + "-" + random(6) + "-" + random(12), SigningSecret: random(16),
		channels: map[string]*Channel{}, dms: map[string]string{}, failures: map[string][]string{}, ts: time.Now().Unix()}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	ca := filepath.Join(t.TempDir(), "slack-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	s.CAFile = ca
	return s
}

// APIURL is the Web API's base, for notifications.slack.apiURL.
func (s *Slack) APIURL() string { return s.URL + "/api" }

// Config returns the configuration of the app, its bot token and signing secret at these
// references (no signing secret when signingRef is empty).
func (s *Slack) Config(tokenRef, signingRef string, channels ...config.SlackChannel) *config.Slack {
	c := &config.Slack{BotToken: config.SecretRef{Ref: tokenRef}, APIURL: s.APIURL(), Channels: channels}
	if signingRef != "" {
		c.SigningSecret = config.SecretRef{Ref: signingRef}
	}
	return c
}

// PEM returns the PEM of the fake's certificate, to put in one CA file with others' (Mailpit's).
func (s *Slack) PEM(tb testing.TB) []byte {
	tb.Helper()
	b, err := os.ReadFile(s.CAFile)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// User is an account of the workspace, or of another one (Stranger, TeamID).
type User struct {
	ID, Email string
	// TeamID is the workspace's ID by default.
	TeamID string
	// EnterpriseID, for a user of an Enterprise organization (enterprise_user.enterprise_id).
	EnterpriseID string

	// Deleted, Bot, Restricted (a guest), UltraRestricted (a single-channel guest) and Stranger
	// (of another workspace) are the user object's flags.
	Deleted, Bot, Restricted, UltraRestricted, Stranger bool
	// EmailConfirmed is is_email_confirmed; absent when nil.
	EmailConfirmed *bool
}

// AddUser adds an account; its ID is made up when empty, and returned.
func (s *Slack) AddUser(u User) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.ID == "" {
		u.ID = "U" + strings.ToUpper(random(5))
	}
	if u.TeamID == "" {
		u.TeamID = s.Team
	}
	s.users = append(s.users, &u)
	return u.ID
}

// Member adds a full member of the workspace with an email, and returns their ID.
func (s *Slack) Member(email string) string { return s.AddUser(User{Email: email}) }

// Channel is a Slack channel.
type Channel struct {
	ID string
	// Member says whether the app is in it; Archived whether it is archived.
	Member, Archived bool
}

// AddChannel adds a channel the app is in.
func (s *Slack) AddChannel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels[id] = &Channel{ID: id, Member: true}
}

// SetChannel adds or changes a channel.
func (s *Slack) SetChannel(c Channel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels[c.ID] = &c
}

// Throttle makes the next n calls answer 429 Too Many Requests, with Retry-After when it is not
// empty.
func (s *Slack) Throttle(n int, retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttle, s.retryAfter = n, retryAfter
}

// Fail makes the next calls of a method answer ok false with these errors, one each.
func (s *Slack) Fail(method string, errs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[method] = append(s.failures[method], errs...)
}

// Request is a call the fake received.
type Request struct {
	Method        string // the Web API's method
	HTTPMethod    string
	RawQuery      string
	Authorization string
	ContentType   string
	Body          string
}

// Requests returns the calls received, in order.
func (s *Slack) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Message is a message the app posted, as it is now.
type Message struct {
	Channel string // the channel's or DM's ID
	User    string // the DM's user; empty in a channel
	TS      string
	Text    string // the fallback text
	Blocks  []Block
	Updates int // how often it was updated
}

// Block is a block of a message.
type Block struct {
	Type     string            `json:"type"`
	BlockID  string            `json:"block_id,omitempty"`
	Text     *Text             `json:"text,omitempty"`
	Fields   []Text            `json:"fields,omitempty"`
	Elements []json.RawMessage `json:"elements,omitempty"`
	Level    int               `json:"level,omitempty"`
}

// Text is a text object.
type Text struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Emoji    *bool  `json:"emoji,omitempty"`
	Verbatim *bool  `json:"verbatim,omitempty"`
}

// Button is a button element.
type Button struct {
	Type     string `json:"type"`
	Text     Text   `json:"text"`
	ActionID string `json:"action_id"`
	URL      string `json:"url"`
	Value    string `json:"value"`
	Style    string `json:"style"`
}

// Texts returns the message's text objects, in order: the blocks' texts, fields and context
// elements, and the buttons' texts.
func (m *Message) Texts() []Text {
	var out []Text
	for _, b := range m.Blocks {
		if b.Text != nil {
			out = append(out, *b.Text)
		}
		out = append(out, b.Fields...)
		for _, e := range b.Elements {
			var t struct {
				Type string          `json:"type"`
				Text json.RawMessage `json:"text"`
			}
			_ = json.Unmarshal(e, &t)
			if t.Type == "plain_text" || t.Type == "mrkdwn" {
				var x Text
				_ = json.Unmarshal(e, &x)
				out = append(out, x)
			} else if t.Type == "button" {
				var x Button
				_ = json.Unmarshal(e, &x)
				out = append(out, x.Text)
			}
		}
	}
	return out
}

// PlainText returns what the message's blocks say, a line per text object.
func (m *Message) PlainText() string {
	var lines []string
	for _, t := range m.Texts() {
		lines = append(lines, t.Text)
	}
	return strings.Join(lines, "\n")
}

// Buttons returns the message's buttons.
func (m *Message) Buttons() []Button {
	var out []Button
	for _, b := range m.Blocks {
		if b.Type != "actions" {
			continue
		}
		for _, e := range b.Elements {
			var x Button
			if json.Unmarshal(e, &x) == nil && x.Type == "button" {
				out = append(out, x)
			}
		}
	}
	return out
}

// Button returns the button with a text.
func (m *Message) Button(text string) (Button, bool) {
	for _, b := range m.Buttons() {
		if b.Text.Text == text {
			return b, true
		}
	}
	return Button{}, false
}

// To returns the direct messages to a user, oldest first.
func (s *Slack) To(user string) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.messages {
		if m.User == user {
			out = append(out, *m)
		}
	}
	return out
}

// Posts returns the messages posted to a channel, oldest first.
func (s *Slack) Posts(channel string) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Message
	for _, m := range s.messages {
		if m.Channel == channel && m.User == "" {
			out = append(out, *m)
		}
	}
	return out
}

// answer writes an answer of the Web API.
func answer(w http.ResponseWriter, v map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code string) { answer(w, map[string]any{"ok": false, "error": code}) }

func (s *Slack) serve(w http.ResponseWriter, r *http.Request) {
	method, ok := strings.CutPrefix(r.URL.Path, "/api/")
	if !ok || method == "" || strings.Contains(method, "/") {
		http.NotFound(w, r)
		return
	}
	var body bytes.Buffer
	_, _ = body.ReadFrom(http.MaxBytesReader(w, r.Body, 1<<20))
	ctype, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Request{Method: method, HTTPMethod: r.Method, RawQuery: r.URL.RawQuery, Authorization: r.Header.Get("Authorization"),
		ContentType: r.Header.Get("Content-Type"), Body: body.String()})
	if s.throttle > 0 {
		s.throttle--
		if s.retryAfter != "" {
			w.Header().Set("Retry-After", s.retryAfter)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "ratelimited"})
		return
	}
	// The token: in the Authorization header, never in the query string.
	switch auth := r.Header.Get("Authorization"); {
	case r.URL.Query().Has("token"), auth == "":
		fail(w, "not_authed")
		return
	case auth != "Bearer "+s.Token:
		fail(w, "invalid_auth")
		return
	}
	if errs := s.failures[method]; len(errs) > 0 {
		s.failures[method] = errs[1:]
		fail(w, errs[0])
		return
	}
	// The arguments: a query string, a form or a JSON body; never a mix.
	args := map[string]any{}
	switch {
	case r.Method != http.MethodPost && r.Method != http.MethodGet:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	case ctype == "application/json":
		if r.URL.RawQuery != "" {
			fail(w, "invalid_arguments")
			return
		}
		if err := json.Unmarshal(body.Bytes(), &args); err != nil {
			fail(w, "invalid_json")
			return
		}
	case ctype == "application/x-www-form-urlencoded" || body.Len() == 0:
		form, err := url.ParseQuery(body.String())
		if err != nil {
			fail(w, "invalid_form_data")
			return
		}
		for k, v := range r.URL.Query() {
			form[k] = append(form[k], v...)
		}
		for k, v := range form {
			args[k] = v[0]
		}
	default:
		fail(w, "invalid_post_type")
		return
	}
	str := func(k string) string { v, _ := args[k].(string); return v }
	switch method {
	case "auth.test":
		res := map[string]any{"ok": true, "url": "https://quai-test.slack.com/", "team": "Quai test", "user": "quai", "team_id": s.Team,
			"user_id": "U0QUAIBOT", "bot_id": "B0QUAIBOT"}
		if s.Enterprise != "" {
			res["enterprise_id"] = s.Enterprise
		}
		answer(w, res)
	case "users.lookupByEmail":
		if ctype == "application/json" {
			fail(w, "invalid_arguments") // form-encoded only
			return
		}
		email := str("email")
		if email == "" {
			fail(w, "invalid_arguments")
			return
		}
		i := slices.IndexFunc(s.users, func(u *User) bool { return strings.EqualFold(u.Email, email) && !u.Deleted })
		if i < 0 {
			fail(w, "users_not_found")
			return
		}
		answer(w, map[string]any{"ok": true, "user": userObject(s.users[i])})
	case "conversations.open":
		users := strings.Split(str("users"), ",")
		if len(users) != 1 || users[0] == "" {
			fail(w, "invalid_arguments") // a multi-person DM: Quai opens none
			return
		}
		dm, code := s.open(users[0])
		if code != "" {
			fail(w, code)
			return
		}
		answer(w, map[string]any{"ok": true, "channel": map[string]any{"id": dm}})
	case "chat.postMessage":
		s.postMessage(w, args)
	case "chat.update":
		s.update(w, args)
	default:
		fail(w, "unknown_method")
	}
}

// userObject is a user as Slack answers it.
func userObject(u *User) map[string]any {
	name, _, _ := strings.Cut(u.Email, "@")
	o := map[string]any{"id": u.ID, "team_id": u.TeamID, "name": name, "deleted": u.Deleted, "real_name": name, "tz": "Europe/Zurich",
		"is_admin": false, "is_owner": false, "is_bot": u.Bot, "is_app_user": false, "is_restricted": u.Restricted,
		"is_ultra_restricted": u.UltraRestricted, "updated": 1502138686, "profile": map[string]any{"email": u.Email, "real_name": name, "display_name": name}}
	if u.Stranger {
		o["is_stranger"] = true
	}
	if u.EmailConfirmed != nil {
		o["is_email_confirmed"] = *u.EmailConfirmed
	}
	if u.EnterpriseID != "" {
		o["enterprise_user"] = map[string]any{"id": u.ID, "enterprise_id": u.EnterpriseID, "enterprise_name": "Quai test", "teams": []string{u.TeamID}}
	}
	return o
}

// open opens the DM with a user: the same for the same person.
func (s *Slack) open(user string) (string, string) {
	i := slices.IndexFunc(s.users, func(u *User) bool { return u.ID == user })
	switch {
	case i < 0:
		return "", "user_not_found"
	case s.users[i].Deleted:
		return "", "user_disabled"
	}
	for dm, u := range s.dms {
		if u == user {
			return dm, ""
		}
	}
	dm := "D" + user[1:]
	s.dms[dm] = user
	return dm, ""
}

func (s *Slack) postMessage(w http.ResponseWriter, args map[string]any) {
	channel, _ := args["channel"].(string)
	user := ""
	switch c, ok := s.channels[channel]; {
	case strings.HasPrefix(channel, "U") || strings.HasPrefix(channel, "W"):
		dm, code := s.open(channel)
		if code != "" {
			fail(w, "channel_not_found")
			return
		}
		user, channel = channel, dm
	case strings.HasPrefix(channel, "D") && s.dms[channel] != "":
		user = s.dms[channel]
	case !ok:
		fail(w, "channel_not_found")
		return
	case c.Archived:
		fail(w, "is_archived")
		return
	case !c.Member:
		fail(w, "not_in_channel")
		return
	}
	text, blocks, code := content(args, 40000)
	if code != "" {
		fail(w, code)
		return
	}
	s.ts++
	m := &Message{Channel: channel, User: user, TS: strconv.FormatInt(s.ts, 10) + ".000100", Text: text, Blocks: blocks}
	s.messages = append(s.messages, m)
	answer(w, map[string]any{"ok": true, "channel": channel, "ts": m.TS, "message": map[string]any{"type": "message", "text": text, "blocks": blocks,
		"bot_id": "B0QUAIBOT", "ts": m.TS}})
}

func (s *Slack) update(w http.ResponseWriter, args map[string]any) {
	channel, _ := args["channel"].(string)
	ts, _ := args["ts"].(string)
	if _, ok := s.channels[channel]; !ok && s.dms[channel] == "" {
		fail(w, "channel_not_found")
		return
	}
	i := slices.IndexFunc(s.messages, func(m *Message) bool { return m.Channel == channel && m.TS == ts })
	if i < 0 {
		fail(w, "message_not_found")
		return
	}
	text, blocks, code := content(args, 4000)
	if code != "" {
		fail(w, code)
		return
	}
	m := s.messages[i]
	m.Text, m.Blocks = text, blocks
	m.Updates++
	answer(w, map[string]any{"ok": true, "channel": channel, "ts": ts, "text": text, "message": map[string]any{"text": text, "blocks": blocks}})
}

// content reads and checks a message's text and blocks.
func content(args map[string]any, maxText int) (string, []Block, string) {
	text, _ := args["text"].(string)
	var raw []byte
	switch b := args["blocks"].(type) {
	case string: // a form: the blocks' JSON
		raw = []byte(b)
	case nil:
	default:
		raw, _ = json.Marshal(b)
	}
	var blocks []Block
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&blocks); err != nil {
			return "", nil, "invalid_blocks_format"
		}
	}
	switch {
	case text == "" && len(blocks) == 0:
		return "", nil, "no_text"
	case units(text) > maxText:
		return "", nil, "msg_too_long"
	case len(blocks) > 50:
		return "", nil, "invalid_blocks"
	}
	for _, b := range blocks {
		if !validBlock(b) {
			return "", nil, "invalid_blocks"
		}
	}
	return text, blocks, ""
}

func units(s string) int {
	n := 0
	for _, r := range s {
		n += max(utf16.RuneLen(r), 1)
	}
	return n
}

// validText checks a text object, of the types allowed, at most n characters.
func validText(t *Text, n int, types ...string) bool {
	return t != nil && slices.Contains(types, t.Type) && t.Text != "" && units(t.Text) <= n &&
		(t.Emoji == nil || t.Type == "plain_text") && (t.Verbatim == nil || t.Type == "mrkdwn")
}

// validBlock checks a block of the types Quai sends against Block Kit's limits.
func validBlock(b Block) bool {
	if len(b.BlockID) > 255 {
		return false
	}
	switch b.Type {
	case "header":
		return validText(b.Text, 150, "plain_text") && len(b.Fields) == 0 && len(b.Elements) == 0
	case "section":
		if b.Text == nil && len(b.Fields) == 0 || len(b.Fields) > 10 || len(b.Elements) > 0 {
			return false
		}
		if b.Text != nil && !validText(b.Text, 3000, "plain_text", "mrkdwn") {
			return false
		}
		for _, f := range b.Fields {
			if !validText(&f, 2000, "plain_text", "mrkdwn") {
				return false
			}
		}
		return true
	case "context":
		if b.Text != nil || len(b.Elements) == 0 || len(b.Elements) > 10 {
			return false
		}
		for _, e := range b.Elements {
			var t Text
			dec := json.NewDecoder(bytes.NewReader(e))
			dec.DisallowUnknownFields()
			if dec.Decode(&t) != nil || !validText(&t, 3000, "plain_text", "mrkdwn") {
				return false
			}
		}
		return true
	case "actions":
		if b.Text != nil || len(b.Elements) == 0 || len(b.Elements) > 25 {
			return false
		}
		ids := map[string]bool{}
		for _, e := range b.Elements {
			var x Button
			dec := json.NewDecoder(bytes.NewReader(e))
			dec.DisallowUnknownFields()
			if dec.Decode(&x) != nil || x.Type != "button" || !validText(&x.Text, 75, "plain_text") || len(x.ActionID) > 255 || ids[x.ActionID] ||
				len(x.URL) > 3000 || len(x.Value) > 2000 || x.Style != "" && x.Style != "primary" && x.Style != "danger" {
				return false
			}
			if x.ActionID != "" {
				ids[x.ActionID] = true
			}
		}
		return true
	case "divider":
		return b.Text == nil && len(b.Fields) == 0 && len(b.Elements) == 0
	}
	return false
}

// Sign returns the X-Slack-Signature of a request body sent at a timestamp, with a signing secret.
func Sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "v0:%s:", timestamp)
	mac.Write(body)
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// Interaction returns the request Slack sends to an interaction endpoint at a moment: a form with
// payload, the interaction's JSON, signed with the app's signing secret.
func (s *Slack) Interaction(endpoint string, payload any, at time.Time) (*http.Request, error) {
	p, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	body := []byte(url.Values{"payload": {string(p)}}.Encode())
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	ts := strconv.FormatInt(at.Unix(), 10)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", Sign(s.SigningSecret, ts, body))
	req.Header.Set("User-Agent", "Slackbot 1.0 (+https://api.slack.com/robots)")
	return req, nil
}

// BlockActions returns the block_actions payload Slack sends when someone clicks a button of a
// message: who clicked, the message as it is, the action, a response_url.
func (s *Slack) BlockActions(m Message, b Button, user string) map[string]any {
	return map[string]any{
		"type": "block_actions", "user": map[string]any{"id": user, "username": "someone", "name": "someone", "team_id": s.Team},
		"api_app_id": "A0QUAIAPP", "token": random(12), "trigger_id": "13345224609.738474920.8088930838d88f008e0",
		"container": map[string]any{"type": "message", "message_ts": m.TS, "channel_id": m.Channel, "is_ephemeral": false},
		"team":      map[string]any{"id": s.Team, "domain": "quai-test"}, "enterprise": nil, "is_enterprise_install": false,
		"channel": map[string]any{"id": m.Channel, "name": "directmessage"},
		"message": map[string]any{"type": "message", "text": m.Text, "blocks": m.Blocks, "bot_id": "B0QUAIBOT", "ts": m.TS},
		"state":   map[string]any{"values": map[string]any{}}, "response_url": "https://hooks.slack.com/actions/" + s.Team + "/1/" + random(12),
		"actions": []map[string]any{{"action_id": b.ActionID, "block_id": "actions", "text": b.Text, "url": b.URL, "style": b.Style, "type": "button",
			"action_ts": "1548426417.840180"}},
	}
}
