// Package teamstest fakes the Microsoft services Quai's Teams notifications call, for tests
// (docs/plan/phase-4-backlog.md#services-with-no-account): there is no Microsoft 365 tenant to
// test with. Each fake is an HTTPS server with a certificate of its own (CAFile trusts them all)
// that implements only what Quai calls, refuses what the documentation says is refused, and keeps
// what it received for the test to read:
//
//   - Login, the Microsoft identity platform's token endpoint, for the client credentials grant:
//     POST /{tenant}/oauth2/v2.0/token, a form with grant_type=client_credentials, client_id,
//     client_secret (or HTTP Basic) and scope https://api.botframework.com/.default; it answers
//     {token_type, expires_in, ext_expires_in, access_token}, or {error, error_description,
//     error_codes, timestamp, trace_id, correlation_id}.
//   - Connector, the Bot Connector service of Teams (service URL /teams/): create a conversation
//     (POST /teams/v3/conversations, a ConversationParameters with the bot 28:<app ID>, one
//     member by Entra ID object ID and channelData.tenant.id; a ConversationResourceResponse),
//     send an activity (POST …/conversations/{id}/activities; a ResourceResponse) and update one
//     (PUT …/activities/{activityId}). Each needs Authorization: Bearer with a token Login issued
//     that has not expired; errors are an ErrorResponse {"error": {"code", "message"}}.
//   - Workflows, the "When a Teams webhook request is received" trigger of a workflow anyone may
//     call: POST /workflows/{id}/triggers/manual/paths/invoke with api-version, sp, sv and sig,
//     the shared access signature, in the query; no Authorization header; a JSON body
//     {"type": "message", "attachments": [{"contentType": "application/vnd.microsoft.card.adaptive",
//     "contentUrl": null, "content": <an Adaptive Card>}]} of at most 28 KB; 202 Accepted.
//
// It follows Microsoft's documentation, read on 2026-11-09:
//   - Microsoft identity platform and the OAuth 2.0 client credentials flow
//     (https://learn.microsoft.com/en-us/entra/identity-platform/v2-oauth2-client-creds-grant-flow).
//   - Authenticate requests with the Bot Connector API
//     (https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-authentication?view=azure-bot-service-4.0):
//     the token request, its scope, the Bearer header, a token to be cached until it expires.
//   - Bot Framework Connector service REST API reference
//     (https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-api-reference?view=azure-bot-service-4.0):
//     the operations, ConversationParameters, ConversationResourceResponse, ResourceResponse,
//     ErrorResponse, and the status codes.
//   - Send proactive messages
//     (https://learn.microsoft.com/en-us/microsoftteams/platform/bots/how-to/conversations/send-proactive-messages):
//     creating a personal conversation by the user's Entra ID object ID and tenant, the global
//     service URL, 403 when the app is not installed for the user (ForbiddenOperationException) or
//     the user blocked the bot (errorCode 209, MessageWritesBlocked).
//   - Optimize your bot with rate limiting and best practices in Teams
//     (https://learn.microsoft.com/en-us/microsoftteams/platform/bots/how-to/rate-limit): 429 Too
//     Many Requests, to retry with exponential backoff and jitter.
//   - Create an Incoming Webhook
//     (https://learn.microsoft.com/en-us/microsoftteams/platform/webhooks-and-connectors/how-to/add-incoming-webhook),
//     Microsoft Teams connector, its "Microsoft Teams Webhook" section
//     (https://learn.microsoft.com/en-us/connectors/teams/), and Create callable or nestable
//     workflows (https://learn.microsoft.com/en-us/azure/logic-apps/logic-apps-http-endpoint): the
//     body, the 28 KB limit, no authentication header with "Anyone", the shared access signature
//     in the URL, 202 Accepted without a Response action.
//
// What the pages do not say, the fake assumes, and it is not verified against Microsoft's
// services: the status and body of a refused signature (401, a Logic Apps-style error), of a body
// over 28 KB (413) and of an expired or unknown bot token (401, code "Unauthorized"); a
// Retry-After header with a 429 (the rate-limit page does not mention it; Quai also backs off
// without it); and that creating a personal conversation twice with one person answers the same
// conversation ID.
package teamstest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/damhau/quai/backend/internal/config"
)

// Teams is the three fakes, for one tenant and one bot.
type Teams struct {
	Login     *Login
	Connector *Connector
	Workflows *Workflows
	// CAFile is a PEM file of the fakes' certificates, for netx.New.
	CAFile string
}

// Start starts the fakes for the test, with a tenant, an application and a client secret of their
// own.
func Start(t testing.TB) *Teams {
	t.Helper()
	login := &Login{Tenant: uuid.NewString(), ClientID: uuid.NewString(), Secret: random(20), Lifetime: 3599 * time.Second,
		tokens: map[string]time.Time{}, now: time.Now}
	login.Server = httptest.NewTLSServer(http.HandlerFunc(login.serve))
	t.Cleanup(login.Close)
	conn := &Connector{login: login, conversations: map[string]string{}, installed: map[string]bool{}, blocked: map[string]bool{}}
	conn.Server = httptest.NewTLSServer(http.HandlerFunc(conn.serve))
	t.Cleanup(conn.Close)
	wf := &Workflows{hooks: map[string]*hook{}}
	wf.Server = httptest.NewTLSServer(http.HandlerFunc(wf.serve))
	t.Cleanup(wf.Close)

	var certs []byte
	for _, s := range []*httptest.Server{login.Server, conn.Server, wf.Server} {
		c := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
		if !bytes.Contains(certs, c) {
			certs = append(certs, c...)
		}
	}
	ca := filepath.Join(t.TempDir(), "teams-ca.pem")
	if err := os.WriteFile(ca, certs, 0o600); err != nil {
		t.Fatal(err)
	}
	return &Teams{Login: login, Connector: conn, Workflows: wf, CAFile: ca}
}

// Bot returns the configuration of a bot that uses the fakes, its client secret at secretRef.
func (t *Teams) Bot(secretRef string) *config.TeamsBot {
	return &config.TeamsBot{AppID: t.Login.ClientID, TenantID: t.Login.Tenant, ClientSecret: config.SecretRef{Ref: secretRef},
		TokenURL: t.Login.TokenURL(), Scope: config.DefaultTeamsScope, ServiceURL: t.Connector.ServiceURL()}
}

// PEM returns the PEM of the fakes' certificates, to put in one CA file with others' (Mailpit's).
func (t *Teams) PEM(tb testing.TB) []byte {
	tb.Helper()
	b, err := os.ReadFile(t.CAFile)
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

// Login is a fake token endpoint of the Microsoft identity platform, for one tenant and one
// application.
type Login struct {
	*httptest.Server
	Tenant, ClientID, Secret string
	// Lifetime is the tokens' expires_in.
	Lifetime time.Duration

	mu       sync.Mutex
	tokens   map[string]time.Time // token -> when it expires
	requests int
	now      func() time.Time
}

// TokenURL is the tenant's v2.0 token endpoint.
func (l *Login) TokenURL() string { return l.URL + "/" + l.Tenant + "/oauth2/v2.0/token" }

// Requests returns how many token requests it took, granted or not.
func (l *Login) Requests() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.requests
}

// Expire makes every token issued so far expired: the Connector refuses them.
func (l *Login) Expire() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for t := range l.tokens {
		l.tokens[t] = time.Time{}
	}
}

// valid reports whether a token was issued and has not expired.
func (l *Login) valid(token string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	exp, ok := l.tokens[token]
	return ok && l.now().Before(exp)
}

var tokenPath = regexp.MustCompile(`^/([^/]+)/oauth2/v2\.0/token$`)

func (l *Login) serve(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	l.requests++
	l.mu.Unlock()
	fail := func(status int, code string, aadsts int, desc string) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": code, "error_description": fmt.Sprintf("AADSTS%d: %s", aadsts, desc),
			"error_codes": []int{aadsts}, "timestamp": time.Now().UTC().Format("2006-01-02 15:04:05Z"), "trace_id": uuid.NewString(),
			"correlation_id": uuid.NewString()})
	}
	m := tokenPath.FindStringSubmatch(r.URL.Path)
	switch {
	case m == nil:
		http.NotFound(w, r)
		return
	case r.Method != http.MethodPost:
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	case m[1] != l.Tenant:
		fail(http.StatusBadRequest, "invalid_request", 90002, "Tenant '"+m[1]+"' not found.")
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/x-www-form-urlencoded" {
		fail(http.StatusBadRequest, "invalid_request", 900144, "The request body must contain the following parameter: 'grant_type'.")
		return
	}
	if err := r.ParseForm(); err != nil {
		fail(http.StatusBadRequest, "invalid_request", 90014, "The request body is not a form.")
		return
	}
	id, secret := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	if u, p, ok := r.BasicAuth(); ok {
		id, secret = u, p
	}
	switch {
	case r.PostForm.Get("grant_type") != "client_credentials":
		fail(http.StatusBadRequest, "unsupported_grant_type", 70003, "The app requested an unsupported grant type.")
	case id != l.ClientID:
		fail(http.StatusBadRequest, "unauthorized_client", 700016, "Application with identifier '"+id+"' was not found in the directory.")
	case secret != l.Secret:
		fail(http.StatusUnauthorized, "invalid_client", 7000215, "Invalid client secret provided.")
	case r.PostForm.Get("scope") != config.DefaultTeamsScope:
		fail(http.StatusBadRequest, "invalid_scope", 70011, "The provided value for the input parameter 'scope' is not valid.")
	default:
		token := random(32)
		l.mu.Lock()
		l.tokens[token] = l.now().Add(l.Lifetime)
		l.mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		secs := int(l.Lifetime / time.Second)
		_ = json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer", "expires_in": secs, "ext_expires_in": secs, "access_token": token})
	}
}

// Activity is a message the bot sent to someone, as it is now.
type Activity struct {
	ID           string
	Conversation string
	Member       string // the person's Entra ID object ID
	Card         Card
	Updates      int // how often it was updated
}

// Connector is a fake Bot Connector service of Teams.
type Connector struct {
	*httptest.Server
	login *Login

	mu            sync.Mutex
	conversations map[string]string // conversation -> member
	installed     map[string]bool   // members the app is installed for
	blocked       map[string]bool   // members who blocked the bot
	activities    []*Activity
	requests      int
	throttle      int
	retryAfter    string
}

// ServiceURL is the Connector's base URL, as the global service URL ends with /teams/.
func (c *Connector) ServiceURL() string { return c.URL + "/teams/" }

// Install installs the bot's app for people, by Entra ID object ID: the Connector refuses to start
// a conversation with anyone else.
func (c *Connector) Install(oids ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, o := range oids {
		c.installed[o] = true
	}
}

// Block makes a person block the bot.
func (c *Connector) Block(oid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocked[oid] = true
}

// Throttle answers the next n requests 429, with Retry-After when retryAfter is not empty.
func (c *Connector) Throttle(n int, retryAfter string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.throttle, c.retryAfter = n, retryAfter
}

// Requests returns how many requests reached the Connector.
func (c *Connector) Requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

// Activities returns the messages sent, as they are now, in the order they were sent.
func (c *Connector) Activities() []Activity {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Activity, 0, len(c.activities))
	for _, a := range c.activities {
		out = append(out, *a)
	}
	return out
}

// To returns the messages sent to a person.
func (c *Connector) To(oid string) []Activity {
	var out []Activity
	for _, a := range c.Activities() {
		if a.Member == oid {
			out = append(out, a)
		}
	}
	return out
}

var (
	conversationsPath = regexp.MustCompile(`^/teams/v3/conversations$`)
	activitiesPath    = regexp.MustCompile(`^/teams/v3/conversations/([^/]+)/activities$`)
	activityPath      = regexp.MustCompile(`^/teams/v3/conversations/([^/]+)/activities/([^/]+)$`)
)

// connectorError writes an ErrorResponse.
func connectorError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func (c *Connector) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests++
	throttled := c.throttle > 0
	if throttled {
		c.throttle--
	}
	retryAfter := c.retryAfter
	c.mu.Unlock()
	if throttled {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		connectorError(w, http.StatusTooManyRequests, "TooManyRequests", "Too many requests")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !c.login.valid(token) {
		connectorError(w, http.StatusUnauthorized, "Unauthorized", "Authorization has been denied for this request.")
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		connectorError(w, http.StatusBadRequest, "BadArgument", "the body must be JSON")
		return
	}
	switch {
	case conversationsPath.MatchString(r.URL.Path) && r.Method == http.MethodPost:
		c.createConversation(w, r)
	case activitiesPath.MatchString(r.URL.Path) && r.Method == http.MethodPost:
		c.send(w, r, activitiesPath.FindStringSubmatch(r.URL.Path)[1])
	case activityPath.MatchString(r.URL.Path) && r.Method == http.MethodPut:
		m := activityPath.FindStringSubmatch(r.URL.Path)
		c.update(w, r, m[1], m[2])
	case conversationsPath.MatchString(r.URL.Path), activitiesPath.MatchString(r.URL.Path), activityPath.MatchString(r.URL.Path):
		connectorError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "The channel doesn't support the requested operation.")
	default:
		connectorError(w, http.StatusNotFound, "NotFound", "no such operation")
	}
}

type account struct {
	ID string `json:"id"`
}

func (c *Connector) createConversation(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Bot         account   `json:"bot"`
		Members     []account `json:"members"`
		IsGroup     bool      `json:"isGroup"`
		TenantID    string    `json:"tenantId"`
		ChannelData struct {
			Tenant struct {
				ID string `json:"id"`
			} `json:"tenant"`
		} `json:"channelData"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		connectorError(w, http.StatusBadRequest, "BadArgument", "the body is not a ConversationParameters: "+err.Error())
		return
	}
	switch {
	case p.Bot.ID != "28:"+c.login.ClientID:
		connectorError(w, http.StatusBadRequest, "BadArgument", "bot.id must be the bot's: 28:<app ID>")
	case len(p.Members) != 1 || p.IsGroup:
		connectorError(w, http.StatusBadRequest, "BadArgument", "a personal conversation has one member")
	case p.ChannelData.Tenant.ID != c.login.Tenant || p.TenantID != "" && p.TenantID != c.login.Tenant:
		connectorError(w, http.StatusBadRequest, "BadArgument", "channelData.tenant.id must be the bot's tenant")
	default:
		oid := p.Members[0].ID
		c.mu.Lock()
		installed, blocked := c.installed[oid], c.blocked[oid]
		id := ""
		for conv, m := range c.conversations {
			if m == oid {
				id = conv
			}
		}
		if installed && !blocked && id == "" {
			id = "a:" + random(16)
			c.conversations[id] = oid
		}
		c.mu.Unlock()
		switch {
		case !installed:
			connectorError(w, http.StatusForbidden, "ForbiddenOperationException", "Bot is not installed in user's personal scope")
		case blocked:
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errorCode":209,"message":"{\"subCode\": \"MessageWritesBlocked\"}"}`))
		default:
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		}
	}
}

// activity reads an activity's body: a message with one Adaptive Card.
func activity(r *http.Request) (Card, error) {
	var a struct {
		Type        string       `json:"type"`
		Attachments []attachment `json:"attachments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		return nil, err
	}
	if a.Type != "message" {
		return nil, fmt.Errorf("type must be message, not %q", a.Type)
	}
	if len(a.Attachments) != 1 {
		return nil, fmt.Errorf("a message of Quai has one card, not %d", len(a.Attachments))
	}
	return a.Attachments[0].card()
}

func (c *Connector) send(w http.ResponseWriter, r *http.Request, conv string) {
	c.mu.Lock()
	member, ok := c.conversations[conv]
	c.mu.Unlock()
	if !ok {
		connectorError(w, http.StatusNotFound, "ConversationNotFound", "Conversation not found.")
		return
	}
	card, err := activity(r)
	if err != nil {
		connectorError(w, http.StatusBadRequest, "BadArgument", err.Error())
		return
	}
	a := &Activity{ID: "1:" + random(12), Conversation: conv, Member: member, Card: card}
	c.mu.Lock()
	c.activities = append(c.activities, a)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": a.ID})
}

func (c *Connector) update(w http.ResponseWriter, r *http.Request, conv, id string) {
	c.mu.Lock()
	i := slices.IndexFunc(c.activities, func(a *Activity) bool { return a.Conversation == conv && a.ID == id })
	c.mu.Unlock()
	if i < 0 {
		connectorError(w, http.StatusNotFound, "ActivityNotFound", "Activity not found.")
		return
	}
	card, err := activity(r)
	if err != nil {
		connectorError(w, http.StatusBadRequest, "BadArgument", err.Error())
		return
	}
	c.mu.Lock()
	c.activities[i].Card = card
	c.activities[i].Updates++
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
}

// Post is a message a workflow took.
type Post struct {
	Hook string
	Card Card
}

// Workflows is a fake of workflows with the "When a Teams webhook request is received" trigger,
// anyone may call: each hook is a channel's.
type Workflows struct {
	*httptest.Server

	mu         sync.Mutex
	hooks      map[string]*hook // by workflow ID
	posts      []Post
	requests   []string // each request's Authorization header, "" without one
	throttle   int
	retryAfter string
}

type hook struct{ name, sig string }

// Hook adds a workflow, named for the test, and returns its trigger's URL, with its signature.
func (wf *Workflows) Hook(name string) string {
	id, sig := random(16), random(24)
	wf.mu.Lock()
	wf.hooks[id] = &hook{name: name, sig: sig}
	wf.mu.Unlock()
	return wf.URL + "/workflows/" + id + "/triggers/manual/paths/invoke?api-version=1&sp=%2Ftriggers%2Fmanual%2Frun&sv=1.0&sig=" + sig
}

// Posts returns the messages a hook took, in order; all hooks' when name is empty.
func (wf *Workflows) Posts(name string) []Post {
	wf.mu.Lock()
	defer wf.mu.Unlock()
	var out []Post
	for _, p := range wf.posts {
		if name == "" || p.Hook == name {
			out = append(out, p)
		}
	}
	return out
}

// Authorizations returns the Authorization header of each request, "" for none.
func (wf *Workflows) Authorizations() []string {
	wf.mu.Lock()
	defer wf.mu.Unlock()
	return slices.Clone(wf.requests)
}

// Throttle answers the next n requests 429, with Retry-After when retryAfter is not empty.
func (wf *Workflows) Throttle(n int, retryAfter string) {
	wf.mu.Lock()
	defer wf.mu.Unlock()
	wf.throttle, wf.retryAfter = n, retryAfter
}

// MaxSize is the largest message a workflow takes.
const MaxSize = 28 * 1024

var hookPath = regexp.MustCompile(`^/workflows/([0-9a-f]+)/triggers/manual/paths/invoke$`)

func (wf *Workflows) serve(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, code, message string) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message}})
	}
	wf.mu.Lock()
	wf.requests = append(wf.requests, r.Header.Get("Authorization"))
	throttled := wf.throttle > 0
	if throttled {
		wf.throttle--
	}
	retryAfter := wf.retryAfter
	m := hookPath.FindStringSubmatch(r.URL.Path)
	var h *hook
	if m != nil {
		h = wf.hooks[m[1]]
	}
	wf.mu.Unlock()
	q := r.URL.Query()
	switch {
	case h == nil:
		fail(http.StatusNotFound, "WorkflowNotFound", "The workflow could not be found.")
		return
	case q.Get("sig") != h.sig || q.Get("sv") != "1.0" || q.Get("sp") != "/triggers/manual/run" || q.Get("api-version") == "":
		fail(http.StatusUnauthorized, "DirectApiInvalidAuthorizationSignature", "The provided signature is not valid.")
		return
	case r.Header.Get("Authorization") != "":
		// Documented: with "Anyone", a request with an authentication token header fails.
		fail(http.StatusUnauthorized, "DirectApiAuthorizationRequired", "A workflow anyone may trigger takes no Authorization header.")
		return
	case r.Method != http.MethodPost:
		fail(http.StatusMethodNotAllowed, "TriggerRequestMethodNotValid", "The trigger expects POST.")
		return
	case throttled:
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		fail(http.StatusTooManyRequests, "WorkflowRequestsThrottled", "Too many requests.")
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		fail(http.StatusBadRequest, "InvalidRequestContent", "The body must be JSON.")
		return
	}
	body := http.MaxBytesReader(w, r.Body, MaxSize+1)
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(body); err != nil || buf.Len() > MaxSize {
		fail(http.StatusRequestEntityTooLarge, "MessageSizeExceeded", "The message size limit is 28 KB.")
		return
	}
	var msg struct {
		Type        string       `json:"type"`
		Attachments []attachment `json:"attachments"`
	}
	if err := json.Unmarshal(buf.Bytes(), &msg); err != nil || msg.Type != "message" || len(msg.Attachments) == 0 {
		fail(http.StatusBadRequest, "InvalidRequestContent", `The body must be {"type": "message", "attachments": [...]}.`)
		return
	}
	for _, a := range msg.Attachments {
		card, err := a.card()
		if err != nil {
			fail(http.StatusBadRequest, "InvalidRequestContent", err.Error())
			return
		}
		wf.mu.Lock()
		wf.posts = append(wf.posts, Post{Hook: h.name, Card: card})
		wf.mu.Unlock()
	}
	w.WriteHeader(http.StatusAccepted)
}

// attachment is an Adaptive Card attachment.
type attachment struct {
	ContentType string          `json:"contentType"`
	ContentURL  json.RawMessage `json:"contentUrl"`
	Content     json.RawMessage `json:"content"`
}

func (a attachment) card() (Card, error) {
	if a.ContentType != "application/vnd.microsoft.card.adaptive" {
		return nil, fmt.Errorf("contentType must be application/vnd.microsoft.card.adaptive, not %q", a.ContentType)
	}
	if len(a.ContentURL) > 0 && string(a.ContentURL) != "null" {
		return nil, fmt.Errorf("contentUrl must be null")
	}
	var c struct {
		Type    string `json:"type"`
		Version string `json:"version"`
		Body    []any  `json:"body"`
	}
	if err := json.Unmarshal(a.Content, &c); err != nil || c.Type != "AdaptiveCard" || len(c.Body) == 0 {
		return nil, fmt.Errorf("content must be an Adaptive Card with a body")
	}
	if _, err := strconv.ParseFloat(c.Version, 64); err != nil {
		return nil, fmt.Errorf("the card's version %q is not one", c.Version)
	}
	return Card(slices.Clone(a.Content)), nil
}

// Card is an Adaptive Card as received.
type Card json.RawMessage

// Element is an element of a card: its type, and the text, URL or title it has.
type Element struct {
	Type, Text, Title, URL string
	Markdown               bool // a TextBlock or a fact, whose text Teams renders as Markdown
}

// Elements returns the card's elements, depth first, actions among them.
func (c Card) Elements() []Element {
	var v any
	_ = json.Unmarshal(c, &v)
	var out []Element
	var walk func(v any, inFacts bool)
	walk = func(v any, inFacts bool) {
		switch x := v.(type) {
		case map[string]any:
			str := func(k string) string { s, _ := x[k].(string); return s }
			if t := str("type"); t != "" && t != "AdaptiveCard" {
				out = append(out, Element{Type: t, Text: str("text"), Title: str("title"), URL: str("url"), Markdown: t == "TextBlock"})
			}
			if inFacts {
				out = append(out, Element{Type: "Fact", Title: str("title"), Text: str("value"), Markdown: true})
			}
			for _, k := range []string{"body", "items", "columns", "inlines", "actions", "facts"} {
				if list, ok := x[k].([]any); ok {
					for _, e := range list {
						walk(e, k == "facts")
					}
				}
			}
		case []any:
			for _, e := range x {
				walk(e, inFacts)
			}
		}
	}
	walk(v, false)
	return out
}

// Texts returns the card's texts, in order.
func (c Card) Texts() []string {
	var out []string
	for _, e := range c.Elements() {
		if e.Text != "" {
			out = append(out, e.Text)
		}
	}
	return out
}

// Text returns the card's texts on one line each.
func (c Card) Text() string { return strings.Join(c.Texts(), "\n") }

// Actions returns the card's buttons.
func (c Card) Actions() []Element {
	var out []Element
	for _, e := range c.Elements() {
		if strings.HasPrefix(e.Type, "Action.") {
			out = append(out, e)
		}
	}
	return out
}

// Action returns the button with a title, or false.
func (c Card) Action(title string) (Element, bool) {
	for _, a := range c.Actions() {
		if a.Title == title {
			return a, true
		}
	}
	return Element{}, false
}
