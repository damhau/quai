package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Slack sends notifications to Slack (notifications.slack,
// docs/design/11-integrations.md#notifications) through a Slack app: direct messages to people,
// found by their verified email, and posts to Slack channels.
//
// Slack's Web API is at notifications.slack.apiURL, https, reached through network with the
// server's certificate verified; never at an address Slack sends (a response_url). The bot token
// is resolved at each call, goes in the Authorization header only, never in a URL or a body, and
// is taken out of every error. A person is sent to only when Slack's account with their verified
// email is an active full member of the app's workspace (open question 66). Messages are Block Kit
// built from Go values with encoding/json: what people wrote is in plain_text objects, which Slack
// does not format, and the fallback text, which Slack does, has &, < and > escaped.
type Slack struct {
	cfg     config.Slack
	secrets *secrets.Store
	client  *http.Client
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error

	mu        sync.Mutex
	workspace slackWorkspace
}

// slackWorkspace is the workspace of the app, as auth.test answered for a token.
type slackWorkspace struct {
	token            [32]byte // the token's SHA-256
	team, enterprise string
}

// maxSlackAnswer bounds what Quai reads of an answer of the Web API.
const maxSlackAnswer = 1 << 20

// NewSlack returns the Slack channel. Outgoing connections trust what network trusts.
func NewSlack(cfg config.Slack, network *netx.Network, sec *secrets.Store) (*Slack, error) {
	if network == nil {
		var err error
		if network, err = netx.New(""); err != nil {
			return nil, err
		}
	}
	return &Slack{cfg: cfg, secrets: sec, now: time.Now, sleep: sleep,
		client: &http.Client{Transport: network.Transport(), Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// UseClock sets the clock Retry-After dates are read by, for tests.
func (s *Slack) UseClock(now func() time.Time) { s.now = now }

// Name names the channel.
func (s *Slack) Name() string { return "slack" }

// Address is a person's verified email: Slack finds them by it.
func (s *Slack) Address(u db.User) (string, bool) {
	if u.Email == "" {
		return "", false
	}
	if _, err := config.MailAddress(u.Email, false); err != nil {
		return "", false
	}
	return u.Email, true
}

// NoAddress says why someone has no address: no verified email to find them in Slack with.
func (s *Slack) NoAddress() string {
	return "Quai knows no verified email of theirs to find them in Slack with"
}

// Posts returns the Slack channels that post an event of a kind for a repository, or for a
// request whose owners hold team:<name>.
func (s *Slack) Posts(kinds []string, repository string, owners []string) []Post {
	var out []Post
	for _, c := range s.cfg.Channels {
		if k, ok := routes(c.Repositories, c.Teams, c.Events, kinds, repository, owners); ok {
			out = append(out, Post{Channel: c.Name, Kind: k})
		}
	}
	return out
}

// What Slack's answers hold that Quai keeps or sends back: IDs (a DM's D..., a user's U... or
// W...) and a message's timestamp.
var (
	slackIDRe = regexp.MustCompile(`^[A-Z0-9]{2,40}$`)
	slackTSRe = regexp.MustCompile(`^[0-9]{1,20}\.[0-9]{1,12}$`)
)

// Send sends a person a direct message, or posts to a Slack channel (recipients
// slack-channel:<name>). Someone Slack has no full member of the workspace for is unreachable:
// the notification is skipped with the reason.
func (s *Slack) Send(ctx context.Context, to Recipient, m Message) (Receipt, error) {
	if name, ok := strings.CutPrefix(to.ID, GroupRecipient("slack")); ok {
		return Receipt{}, s.post(ctx, name, m)
	}
	member, err := s.member(ctx, to.Address)
	if err != nil {
		return Receipt{}, err
	}
	var open struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := s.call(ctx, "conversations.open", map[string]any{"users": member}, &open); err != nil {
		var se *SlackError
		if errors.As(err, &se) && slices.Contains([]string{"user_not_found", "user_not_visible", "user_disabled"}, se.Code) {
			return Receipt{}, &Unreachable{Reason: "Slack does not let the app write to their account (" + se.Code + ")"}
		}
		return Receipt{}, fmt.Errorf("open a direct message in Slack: %w", err)
	}
	if !slackIDRe.MatchString(open.Channel.ID) {
		return Receipt{}, errors.New("open a direct message in Slack: no conversation ID in the answer")
	}
	text, blocks := slackPersonal(m)
	ts, err := s.postMessage(ctx, open.Channel.ID, text, blocks)
	if err != nil {
		return Receipt{}, fmt.Errorf("send a direct message in Slack: %w", err)
	}
	return Receipt{Conversation: open.Channel.ID, Message: ts}, nil
}

// Update replaces a direct message Quai sent.
func (s *Slack) Update(ctx context.Context, at Receipt, m Message) error {
	text, blocks := slackPersonal(m)
	msg := slackMessage{Channel: at.Conversation, TS: at.Message, Text: text, Blocks: blocks}
	if err := s.call(ctx, "chat.update", msg, nil); err != nil {
		return fmt.Errorf("update a message in Slack: %w", err)
	}
	return nil
}

// post posts to a Slack channel of the configuration.
func (s *Slack) post(ctx context.Context, name string, m Message) error {
	i := slices.IndexFunc(s.cfg.Channels, func(c config.SlackChannel) bool { return c.Name == name })
	if i < 0 {
		return fmt.Errorf("no Slack channel %s in the configuration", name)
	}
	text, blocks := slackChannelPost(m)
	if _, err := s.postMessage(ctx, s.cfg.Channels[i].Channel, text, blocks); err != nil {
		return fmt.Errorf("post to Slack channel %s: %w", name, err)
	}
	return nil
}

// postMessage posts a message, and returns its timestamp.
func (s *Slack) postMessage(ctx context.Context, channel, text string, blocks []slackBlock) (string, error) {
	no := false
	var posted struct {
		TS string `json:"ts"`
	}
	msg := slackMessage{Channel: channel, Text: text, Blocks: blocks, UnfurlLinks: &no, UnfurlMedia: &no}
	if err := s.call(ctx, "chat.postMessage", msg, &posted); err != nil {
		return "", err
	}
	if !slackTSRe.MatchString(posted.TS) {
		return "", errors.New("no message timestamp in the answer")
	}
	return posted.TS, nil
}

// slackUser is what Quai reads of Slack's user object.
type slackUser struct {
	ID                string `json:"id"`
	TeamID            string `json:"team_id"`
	Deleted           bool   `json:"deleted"`
	IsBot             bool   `json:"is_bot"`
	IsAppUser         bool   `json:"is_app_user"`
	IsRestricted      bool   `json:"is_restricted"`
	IsUltraRestricted bool   `json:"is_ultra_restricted"`
	IsStranger        bool   `json:"is_stranger"`
	IsEmailConfirmed  *bool  `json:"is_email_confirmed"`
	Profile           struct {
		Email string `json:"email"`
	} `json:"profile"`
	EnterpriseUser *struct {
		EnterpriseID string `json:"enterprise_id"`
	} `json:"enterprise_user"`
}

// member returns the Slack user ID of the active full member of the app's workspace with an
// email; *Unreachable when there is none (open question 66): Slack knows no one with it, or a
// guest, a bot, a deactivated account or someone of another workspace holds it.
func (s *Slack) member(ctx context.Context, email string) (string, error) {
	ws, err := s.workspaceOf(ctx)
	if err != nil {
		return "", err
	}
	var res struct {
		User slackUser `json:"user"`
	}
	// A form, in the body: the email is in no URL.
	if err := s.call(ctx, "users.lookupByEmail", url.Values{"email": {email}}, &res); err != nil {
		var se *SlackError
		if errors.As(err, &se) && se.Code == "users_not_found" {
			return "", &Unreachable{Reason: "Slack knows no active account with their email in the app's workspace"}
		}
		return "", fmt.Errorf("find someone in Slack by their email: %w", err)
	}
	u := res.User
	enterprise := ws.enterprise != "" && u.EnterpriseUser != nil && u.EnterpriseUser.EnterpriseID == ws.enterprise
	switch {
	case !slackIDRe.MatchString(u.ID) || !strings.EqualFold(u.Profile.Email, email):
		return "", errors.New("find someone in Slack by their email: the answer is not the account with that email")
	case u.Deleted:
		return "", &Unreachable{Reason: "the Slack account with their email is deactivated"}
	case u.IsBot || u.IsAppUser:
		return "", &Unreachable{Reason: "the Slack account with their email is a bot's or an app's"}
	case u.IsRestricted || u.IsUltraRestricted:
		return "", &Unreachable{Reason: "the Slack account with their email is a guest's: Quai writes to full members of the workspace only"}
	case u.IsStranger || u.TeamID != ws.team && !enterprise:
		return "", &Unreachable{Reason: "the Slack account with their email belongs to another workspace than the app's"}
	case u.IsEmailConfirmed != nil && !*u.IsEmailConfirmed:
		return "", &Unreachable{Reason: "the Slack account with their email has not confirmed it"}
	}
	return u.ID, nil
}

// workspaceOf returns the app's workspace (auth.test), asked again when the token changes.
func (s *Slack) workspaceOf(ctx context.Context) (slackWorkspace, error) {
	token, err := s.token(ctx)
	if err != nil {
		return slackWorkspace{}, err
	}
	sum := sha256.Sum256([]byte(token))
	s.mu.Lock()
	ws := s.workspace
	s.mu.Unlock()
	if ws.token == sum && ws.team != "" {
		return ws, nil
	}
	var res struct {
		TeamID       string `json:"team_id"`
		EnterpriseID string `json:"enterprise_id"`
	}
	if err := s.call(ctx, "auth.test", url.Values{}, &res); err != nil {
		return slackWorkspace{}, fmt.Errorf("ask Slack for the app's workspace: %w", err)
	}
	if !slackIDRe.MatchString(res.TeamID) || res.EnterpriseID != "" && !slackIDRe.MatchString(res.EnterpriseID) {
		return slackWorkspace{}, errors.New("ask Slack for the app's workspace: no workspace ID in the answer")
	}
	ws = slackWorkspace{token: sum, team: res.TeamID, enterprise: res.EnterpriseID}
	s.mu.Lock()
	s.workspace = ws
	s.mu.Unlock()
	return ws, nil
}

// slackTokenRe is a bot token: xoxb-, or xoxe.xoxb- when Slack rotates it.
var slackTokenRe = regexp.MustCompile(`^(xoxe\.)?xoxb-[A-Za-z0-9-]{1,250}$`)

// token resolves the bot token.
func (s *Slack) token(ctx context.Context) (string, error) {
	sec, err := s.secrets.Resolve(ctx, s.cfg.BotToken.Ref)
	if err != nil {
		return "", fmt.Errorf("the Slack bot token: %w", err) // names the reference, never the value
	}
	token := strings.TrimSpace(sec.Reveal())
	if !slackTokenRe.MatchString(token) {
		return "", fmt.Errorf("the Slack bot token (%s) is not a bot token: a bot token starts with xoxb-", s.cfg.BotToken.Ref)
	}
	return token, nil
}

// SlackError is an answer of Slack's Web API that is not a success: an HTTP status other than
// 200, or ok false with Slack's error code.
type SlackError struct {
	Method string
	Status int    // the HTTP status when it is not 200
	Code   string // Slack's error
}

func (e *SlackError) Error() string {
	msg := "Slack's " + e.Method + ":"
	if e.Status != 0 {
		msg += fmt.Sprintf(" HTTP %d", e.Status)
	}
	if e.Code != "" {
		msg += " " + e.Code
	}
	return msg
}

// Permanent reports whether trying again cannot help: the message or its conversation is gone, or
// can no longer be edited.
func (e *SlackError) Permanent() bool {
	switch e.Code {
	case "message_not_found", "cant_update_message", "edit_window_closed", "channel_not_found", "is_archived", "not_in_channel",
		"invalid_blocks", "invalid_blocks_format", "msg_blocks_too_long", "msg_too_long":
		return true
	}
	return false
}

// slackCodeRe is what Quai keeps of an error code: what Slack's codes are made of.
var slackCodeRe = regexp.MustCompile(`^[a-z0-9_.]{1,60}$`)

// call calls a method of the Web API, with its arguments as a form (url.Values) or JSON, and reads
// its answer into out. A throttled call (429, ratelimited) or a server error (5xx) is tried again
// after a wait, within maxWait.
func (s *Slack) call(ctx context.Context, method string, args, out any) error {
	token, err := s.token(ctx)
	if err != nil {
		return err
	}
	var (
		body  []byte
		ctype string
	)
	if form, ok := args.(url.Values); ok {
		body, ctype = []byte(form.Encode()), "application/x-www-form-urlencoded"
	} else if body, err = json.Marshal(args); err != nil {
		return err
	} else {
		ctype = "application/json; charset=utf-8"
	}
	u := strings.TrimSuffix(s.cfg.APIURL, "/") + "/" + method
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", ctype)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := s.client.Do(req)
		if err != nil {
			return redactToken(fmt.Errorf("%s: %w", method, err), token)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxSlackAnswer))
		_ = resp.Body.Close()
		var res struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		jerr := json.Unmarshal(data, &res)
		code := ""
		if slackCodeRe.MatchString(res.Error) {
			code = res.Error
		}
		throttled := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 || code == "ratelimited" || code == "rate_limited"
		if throttled && (resp.StatusCode != http.StatusOK || !res.OK) {
			if wait, ok := retryWait(ctx, resp.Header, attempt, s.now()); ok {
				if err := s.sleep(ctx, wait); err != nil {
					return err
				}
				continue
			}
		}
		switch {
		case resp.StatusCode != http.StatusOK:
			return &SlackError{Method: method, Status: resp.StatusCode, Code: code}
		case jerr != nil:
			return fmt.Errorf("%s: an answer that is not JSON", method)
		case !res.OK:
			if code == "" {
				code = "an error"
			}
			return &SlackError{Method: method, Code: code}
		}
		if out != nil {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("%s: an answer of another shape", method)
			}
		}
		return nil
	}
}
